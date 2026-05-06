package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cllm/internal/buildinfo"
)

// askdServer holds the state of the askd HTTP control plane.
type askdServer struct {
	addr   string
	logDir string
	stderr io.Writer

	cfg *runtimeConfig

	mu        sync.Mutex
	state     jobState
	startedAt time.Time
	pausedAt  time.Time
	stoppedAt time.Time
	curSpec   *jobSpec
	ctl       *benchController
	cancel    context.CancelFunc
	jobDone   chan struct{}
	curLog    *runLog
	message   string
}

// runServe is the askd entry point. It is called from main when --serve
// is set. It blocks until SIGTERM/SIGINT.
func runServe(opts options, stdout, stderr io.Writer) error {
	opts = applyServeDefaults(opts)
	if err := os.MkdirAll(opts.logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir %s: %w", opts.logDir, err)
	}
	s := &askdServer{
		addr:   opts.addr,
		logDir: opts.logDir,
		stderr: stderr,
		cfg:    newRuntimeConfig(opts),
		state:  jobIdle,
	}

	mux := s.routes()
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stderr, "askd %s listening on %s (log-dir=%s)\n", buildinfo.Version, s.addr, s.logDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Auto-start a benchmark on pod start using the runtime defaults.
	// Disable via ASK_AUTOSTART=false (e.g. for tests / manual mode).
	if envBoolOr("ASK_AUTOSTART", true) {
		go s.autoStartOnReady(stderr)
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "askd: shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	s.stopJob("askd shutdown")
	return nil
}

func (s *askdServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/config/reset", s.handleConfigReset)
	mux.HandleFunc("/config/html", s.handleConfigHTML)
	mux.HandleFunc("/bench", s.handleBench)
	mux.HandleFunc("/bench/pause", s.handleBenchPause)
	mux.HandleFunc("/bench/start", s.handleBenchStart)
	mux.HandleFunc("/bench/stop", s.handleBenchStop)
	mux.HandleFunc("/bench/restart", s.handleBenchRestart)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/logs/", s.handleLogTail)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// ----- basic handlers -----

func (s *askdServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *askdServer) handleReady(w http.ResponseWriter, r *http.Request) {
	// askd is "ready" once the server is up. Job state is independent
	// (a paused or running job does not affect readiness).
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "job_state": string(s.snapshotState().State)})
}

func (s *askdServer) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"name":    "askd",
		"version": buildinfo.Version,
	})
}

func (s *askdServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "askd %s\n\nendpoints:\n", buildinfo.Version)
	for _, p := range []string{
		"GET  /health",
		"GET  /ready",
		"GET  /version",
		"GET  /config            (JSON snapshot of current defaults)",
		"PUT  /config            (JSON merge into current defaults)",
		"POST /config/reset      (revert to startup defaults)",
		"GET  /config/html       (HTML form view + edit)",
		"POST /config/html       (form-encoded merge)",
		"GET  /bench             (current job status)",
		"POST /bench             (start a new job; JSON jobSpec)",
		"POST /bench/pause       (drain in-flight, stop accepting new prompts)",
		"POST /bench/start       (resume from pause; or start a new run if idle)",
		"POST /bench/stop        (drain, close current log file, return to idle)",
		"POST /bench/restart     (stop + start a fresh run with current config)",
		"GET  /logs              (list per-run log files)",
		"GET  /logs/<name>       (tail/raw of one run log)",
	} {
		fmt.Fprintf(w, "  %s\n", p)
	}
}

// ----- config -----

func (s *askdServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if preferHTML(r.Header.Get("Accept")) {
			s.renderConfigForm(w, r, "")
			return
		}
		writeJSON(w, http.StatusOK, optionsToJobSpec(s.cfg.snapshot()))
	case http.MethodPut, http.MethodPost:
		var spec jobSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		updated := s.cfg.update(spec)
		writeJSON(w, http.StatusOK, optionsToJobSpec(updated))
	default:
		w.Header().Set("Allow", "GET, PUT, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *askdServer) handleConfigReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.cfg.reset()
	writeJSON(w, http.StatusOK, optionsToJobSpec(s.cfg.snapshot()))
}

// ----- bench job lifecycle -----

func (s *askdServer) handleBench(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.snapshotState())
	case http.MethodPost:
		var spec jobSpec
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		if err := s.startJob(spec, false); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, s.snapshotState())
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *askdServer) handleBenchPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.pauseJob(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotState())
}

// /bench/start has two modes: resume from pause, or (if idle) start
// using current defaults with no body.
func (s *askdServer) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	switch state {
	case jobPaused:
		s.resumeJob()
		writeJSON(w, http.StatusOK, s.snapshotState())
	case jobIdle:
		var spec jobSpec
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		if err := s.startJob(spec, false); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, s.snapshotState())
	default:
		writeError(w, http.StatusConflict, "job is already running; use /bench/pause or /bench/restart")
	}
}

func (s *askdServer) handleBenchStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.stopJob("stop requested via /bench/stop")
	writeJSON(w, http.StatusOK, s.snapshotState())
}

func (s *askdServer) handleBenchRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var spec jobSpec
	s.mu.Lock()
	if s.curSpec != nil {
		spec = *s.curSpec
	}
	s.mu.Unlock()
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	s.stopJob("restart requested via /bench/restart")
	if err := s.startJob(spec, true); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.snapshotState())
}

// startJob spins up a new bench job. If forceWarmup is true (restart),
// the spec.Warmup flag is forced on so the run looks like a fresh
// process start (warm-up + headers).
func (s *askdServer) startJob(spec jobSpec, forceWarmup bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != jobIdle {
		return fmt.Errorf("job already %s; stop or pause first", s.state)
	}

	// Compose effective options: current defaults overlaid with spec.
	effective := s.cfg.update(spec)
	if forceWarmup {
		effective.warmup = true
	}
	if effective.bench < 1 {
		return errors.New("bench must be >= 1")
	}

	// Open a per-run log file.
	rl, err := newRunLog(s.logDir)
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctl := newBenchController()
	s.state = jobRunning
	s.startedAt = time.Now().UTC()
	s.stoppedAt = time.Time{}
	s.pausedAt = time.Time{}
	s.curSpec = &spec
	s.ctl = ctl
	s.cancel = cancel
	s.curLog = rl
	s.jobDone = make(chan struct{})
	s.message = "running"

	specSnap := spec
	out := io.MultiWriter(s.stderr, rl)
	go func(opts options) {
		defer close(s.jobDone)
		canonical := opts.promptText
		fmt.Fprintf(out, "=== askd run started %s spec=%s ===\n", time.Now().UTC().Format(time.RFC3339), mustJSON(specSnap))
		err := runBenchControlled(ctx, opts, canonical, out, out, ctl)
		s.mu.Lock()
		switch {
		case err == nil:
			s.message = "completed"
		case errors.Is(err, errJobStopped):
			s.message = "stopped"
		default:
			s.message = "error: " + err.Error()
		}
		s.stoppedAt = time.Now().UTC()
		fmt.Fprintf(out, "=== askd run ended %s message=%s ===\n", s.stoppedAt.Format(time.RFC3339), s.message)
		_ = rl.Close()
		s.state = jobIdle
		s.ctl = nil
		s.cancel = nil
		s.curLog = nil
		s.mu.Unlock()
	}(effective)

	return nil
}

func (s *askdServer) pauseJob() error {
	s.mu.Lock()
	if s.state != jobRunning {
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("cannot pause from state %s", st)
	}
	ctl := s.ctl
	rl := s.curLog
	s.mu.Unlock()
	if ctl == nil {
		return errors.New("no active controller")
	}
	ctl.setPaused(true)
	// Drain in-flight requests before declaring "paused".
	drainCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctl.waitDrain(drainCtx)
	s.mu.Lock()
	s.state = jobPaused
	s.pausedAt = time.Now().UTC()
	s.message = "paused"
	s.mu.Unlock()
	if rl != nil {
		fmt.Fprintf(rl, "=== askd PAUSED %s (in-flight drained) ===\n", time.Now().UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(s.stderr, "askd: PAUSED\n")
	return nil
}

func (s *askdServer) resumeJob() {
	s.mu.Lock()
	if s.state != jobPaused {
		s.mu.Unlock()
		return
	}
	ctl := s.ctl
	rl := s.curLog
	s.state = jobRunning
	s.pausedAt = time.Time{}
	s.message = "running"
	s.mu.Unlock()
	if ctl != nil {
		ctl.setPaused(false)
	}
	if rl != nil {
		fmt.Fprintf(rl, "=== askd RESUMED %s ===\n", time.Now().UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(s.stderr, "askd: STARTED (resumed from pause)\n")
}

// stopJob drains in-flight, signals workers to exit, waits for the job
// goroutine to close the log file. Safe to call when idle (no-op).
func (s *askdServer) stopJob(reason string) {
	s.mu.Lock()
	if s.state == jobIdle {
		s.mu.Unlock()
		return
	}
	ctl := s.ctl
	cancel := s.cancel
	jobDone := s.jobDone
	rl := s.curLog
	s.mu.Unlock()

	if rl != nil {
		fmt.Fprintf(rl, "=== askd STOP requested %s reason=%q ===\n", time.Now().UTC().Format(time.RFC3339), reason)
	}
	if ctl != nil {
		// Stop accepting new prompts but let in-flight finish.
		ctl.setPaused(true)
		drainCtx, dcancel := context.WithTimeout(context.Background(), 60*time.Second)
		ctl.waitDrain(drainCtx)
		dcancel()
		ctl.setStopped()
	}
	if cancel != nil {
		cancel()
	}
	if jobDone != nil {
		select {
		case <-jobDone:
		case <-time.After(30 * time.Second):
		}
	}
	fmt.Fprintf(s.stderr, "askd: STOPPED (%s)\n", reason)
}

// ----- helpers -----

func (s *askdServer) snapshotState() jobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := jobStatus{State: s.state, Message: s.message}
	if !s.startedAt.IsZero() {
		st.StartedAt = s.startedAt.Format(time.RFC3339)
	}
	if !s.pausedAt.IsZero() {
		st.PausedAt = s.pausedAt.Format(time.RFC3339)
	}
	if !s.stoppedAt.IsZero() {
		st.StoppedAt = s.stoppedAt.Format(time.RFC3339)
	}
	if s.curSpec != nil {
		spec := *s.curSpec
		st.Spec = &spec
	}
	if s.curLog != nil {
		st.LogFile = s.curLog.name
	}
	if s.ctl != nil {
		st.InflightReqs = s.ctl.inflight.Load()
		st.CompletedReqs = s.ctl.completed.Load()
	}
	return st
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// preferHTML returns true when the Accept header lists text/html with a
// quality factor higher than (or before) application/json. Mirrors the
// cllm httpapi.preferHTML helper so browsers hitting /config get the
// form view while curl (which sends `*/*` or no Accept) keeps the JSON
// contract.
func preferHTML(accept string) bool {
	if accept == "" {
		return false
	}
	lower := strings.ToLower(accept)
	htmlIdx := strings.Index(lower, "text/html")
	if htmlIdx < 0 {
		return false
	}
	jsonIdx := strings.Index(lower, "application/json")
	if jsonIdx >= 0 && jsonIdx < htmlIdx {
		return false
	}
	return true
}

// applyServeDefaults fills in askd-specific defaults that don't apply to
// the CLI single-shot/bench paths. The configmap (or explicit flags) can
// override any of these.
func applyServeDefaults(opts options) options {
	if opts.bench <= 0 {
		opts.bench = envIntOr("ASK_BENCH", 120)
	}
	if len(opts.files) == 0 {
		if f := envOr("ASK_FILES", "/configs/prompts.yaml"); f != "" {
			opts.files = []string{f}
		}
	}
	if !opts.loop {
		opts.loop = envBoolOr("ASK_LOOP", true)
	}
	return opts
}

// autoStartOnReady waits until the HTTP listener accepts a /health
// connection, then submits an empty jobSpec — startJob picks up the
// runtime defaults (which already include the askd-mode bench/files/loop
// values from applyServeDefaults).
func (s *askdServer) autoStartOnReady(stderr io.Writer) {
	deadline := time.Now().Add(60 * time.Second)
	addr := s.addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	url := "http://" + addr + "/health"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := s.startJob(jobSpec{}, false); err != nil {
		fmt.Fprintf(stderr, "askd: autostart skipped: %v\n", err)
		return
	}
	fmt.Fprintf(stderr, "askd: autostart triggered (bench=%d)\n", s.cfg.snapshot().bench)
}

func envBoolOr(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off", "":
		return false
	}
	return def
}

// ----- /logs -----

func (s *askdServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime string `json:"modified"`
	}
	out := []item{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "run-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, item{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"log_dir": s.logDir, "logs": out})
}

func (s *askdServer) handleLogTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/logs/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "invalid log name")
		return
	}
	path := filepath.Join(s.logDir, name)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer f.Close()

	// Optional ?tail=N returns the last N bytes (default: full file).
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if tail > 0 {
		info, err := f.Stat()
		if err == nil && info.Size() > int64(tail) {
			_, _ = f.Seek(info.Size()-int64(tail), io.SeekStart)
		}
	}
	_, _ = io.Copy(w, f)
}
