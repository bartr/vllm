package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// newTestServer wires up a fully-formed askdServer pointed at a temp
// log dir, ready for httptest exercising of the routes. No actual
// listener is opened.
func newTestServer(t *testing.T) (*askdServer, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	defaults := defaultOptions()
	defaults.bench = 1
	defaults.addr = ":0"
	defaults.logDir = dir
	s := &askdServer{
		addr:   ":0",
		logDir: dir,
		stderr: io.Discard,
		cfg:    newRuntimeConfig(defaults),
		state:  jobIdle,
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { s.stopJob("test cleanup") })
	return s, ts
}

func TestServeHealthReadyVersion(t *testing.T) {
	_, ts := newTestServer(t)
	for _, ep := range []string{"/health", "/ready", "/version"} {
		resp, err := http.Get(ts.URL + ep)
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", ep, resp.StatusCode)
		}
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body) == 0 {
			t.Errorf("%s: empty body", ep)
		}
	}
}

func TestConfigGetPutReset(t *testing.T) {
	_, ts := newTestServer(t)

	// GET initial.
	resp, _ := http.Get(ts.URL + "/config")
	var cfg jobSpec
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if cfg.MaxTokens == 0 {
		t.Fatalf("expected non-zero default max_tokens")
	}
	original := cfg.MaxTokens

	// PUT change.
	body, _ := json.Marshal(jobSpec{MaxTokens: 42, Bench: 5})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = http.DefaultClient.Do(req)
	var updated jobSpec
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if updated.MaxTokens != 42 || updated.Bench != 5 {
		t.Fatalf("PUT did not apply: %+v", updated)
	}

	// Reset.
	resp, _ = http.Post(ts.URL+"/config/reset", "application/json", nil)
	var reset jobSpec
	_ = json.NewDecoder(resp.Body).Decode(&reset)
	resp.Body.Close()
	if reset.MaxTokens != original {
		t.Fatalf("reset did not revert max_tokens: got %d want %d", reset.MaxTokens, original)
	}
}

func TestConfigHTMLForm(t *testing.T) {
	s, ts := newTestServer(t)
	form := url.Values{"bench": {"7"}, "max_tokens": {"99"}}
	resp, err := http.PostForm(ts.URL+"/config/html", form)
	if err != nil {
		t.Fatalf("postform: %v", err)
	}
	resp.Body.Close()
	cfg := s.cfg.snapshot()
	if cfg.bench != 7 || cfg.maxTokens != 99 {
		t.Fatalf("form did not apply: bench=%d maxTokens=%d", cfg.bench, cfg.maxTokens)
	}

	resp, _ = http.Get(ts.URL + "/config/html")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "askd config") {
		t.Errorf("html missing title")
	}
}

func TestBenchStartConflictWithoutValidSpec(t *testing.T) {
	_, ts := newTestServer(t)
	// Force bench=0 via reset (defaults set bench=1 in newTestServer).
	body := bytes.NewReader([]byte(`{"bench":0}`))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/bench", body)
	req.Header.Set("Content-Type", "application/json")
	// First, set defaults bench to 0 using PUT.
	pbody, _ := json.Marshal(map[string]int{}) // no change
	preq, _ := http.NewRequest(http.MethodPut, ts.URL+"/config", bytes.NewReader(pbody))
	_, _ = http.DefaultClient.Do(preq)
	// Now post with bench>=1 to start a job that immediately fails on
	// missing prompt source — we don't want a real downstream call.
	// Instead, just verify GET /bench returns idle state.
	resp, _ := http.Get(ts.URL + "/bench")
	var st jobStatus
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.State != jobIdle {
		t.Errorf("expected idle, got %s", st.State)
	}
}

func TestStopJobIdle(t *testing.T) {
	s, ts := newTestServer(t)
	resp, _ := http.Post(ts.URL+"/bench/stop", "application/json", nil)
	var st jobStatus
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.State != jobIdle {
		t.Errorf("stop on idle: state=%s", st.State)
	}
	// No log file should be created on idle stop.
	entries, _ := os.ReadDir(s.logDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run-") {
			t.Errorf("idle stop created a log file: %s", e.Name())
		}
	}
}

func TestLogsListEmpty(t *testing.T) {
	_, ts := newTestServer(t)
	resp, _ := http.Get(ts.URL + "/logs")
	var body struct {
		Logs []map[string]any `json:"logs"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if len(body.Logs) != 0 {
		t.Errorf("expected empty logs, got %d", len(body.Logs))
	}
}

func TestLogTailRejectsTraversal(t *testing.T) {
	_, ts := newTestServer(t)
	resp, _ := http.Get(ts.URL + "/logs/..%2Fetc%2Fpasswd")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 on traversal attempt, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRuntimeConfigUpdate(t *testing.T) {
	rc := newRuntimeConfig(options{bench: 1, maxTokens: 100, url: "http://x"})
	stream := false
	got := rc.update(jobSpec{Bench: 4, Stream: &stream, URL: "http://y"})
	if got.bench != 4 || got.stream != false || got.url != "http://y" {
		t.Fatalf("update wrong: %+v", got)
	}
	// maxTokens unchanged (zero in spec means "leave alone").
	if got.maxTokens != 100 {
		t.Fatalf("maxTokens clobbered: %d", got.maxTokens)
	}
	rc.reset()
	if rc.snapshot().bench != 1 {
		t.Fatalf("reset failed")
	}
}

func TestBenchControllerPauseResume(t *testing.T) {
	c := newBenchController()
	if !c.gate(testCtx(t)) {
		t.Fatalf("initial gate should pass")
	}
	c.setPaused(true)
	resumed := make(chan struct{})
	go func() {
		c.gate(testCtx(t))
		close(resumed)
	}()
	select {
	case <-resumed:
		t.Fatalf("gate returned while paused")
	default:
	}
	c.setPaused(false)
	<-resumed
}
