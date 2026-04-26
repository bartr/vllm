package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cllm/internal/buildinfo"
	"cllm/internal/config"
	"cllm/internal/httpapi"
)

// syncBuffer is a thread-safe wrapper around bytes.Buffer for tests where a
// background goroutine writes to the same buffer the test goroutine reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	for _, want := range []string{"Usage: cllm [options]", "--cache-file-path", "--downstream-url", "--downstream-token", "--downstream-model", "--max-tokens-per-second", "--max-concurrent-requests", "--max-waiting-requests", "--max-degradation", "--version", "-h, --help", `Default system prompt for chat completions (default "You are a helpful assistant.")`, "Default max tokens for chat completions (default 4000)", "Default temperature for chat completions (default 0.2)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output %q does not contain %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunShortHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"-h"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: cllm [options]") {
		t.Fatalf("help output %q does not contain usage", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	originalVersion := buildinfo.Version
	buildinfo.Version = "1.2.3"
	defer func() { buildinfo.Version = originalVersion }()

	exitCode := run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	if stdout.String() != "cllm 1.2.3\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "cllm 1.2.3\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestNewServerDisablesWriteTimeoutForStreaming(t *testing.T) {
	server := newServer(config.Config{Addr: ":8080"}, http.NewServeMux())

	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want 0", server.WriteTimeout)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, 5*time.Second)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %s, want %s", server.IdleTimeout, 60*time.Second)
	}
}

func TestResolveStartupDownstreamModelUsesConfiguredModel(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))

	model, err := resolveStartupDownstreamModel(context.Background(), logger, config.Config{
		DownstreamURL:   "http://localhost:8000",
		DownstreamModel: "configured-model",
	})
	if err != nil {
		t.Fatalf("resolveStartupDownstreamModel() error = %v", err)
	}
	if model != "configured-model" {
		t.Fatalf("model = %q, want %q", model, "configured-model")
	}
	if logBuffer.Len() != 0 {
		t.Fatalf("log output = %q, want empty", logBuffer.String())
	}
}

func TestResolveStartupDownstreamModelFetchesAndLogsSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("authorization = %q, want %q", got, "Bearer secret-token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fallback-model"},{"id":"preferred-model","default":true},{"id":"extra-model"}]}`))
	}))
	defer server.Close()

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))

	model, err := resolveStartupDownstreamModel(context.Background(), logger, config.Config{
		DownstreamURL:   server.URL,
		DownstreamToken: "secret-token",
	})
	if err != nil {
		t.Fatalf("resolveStartupDownstreamModel() error = %v", err)
	}
	if model != "preferred-model" {
		t.Fatalf("model = %q, want %q", model, "preferred-model")
	}

	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="resolved downstream model from downstream /v1/models"`,
		`selected_model=preferred-model`,
		`selection_source=default`,
		`msg="multiple downstream models available"`,
		`available_models="[fallback-model preferred-model extra-model]"`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestResolveStartupDownstreamModelFailsWhenNoModelAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{}]}`))
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := resolveStartupDownstreamModel(context.Background(), logger, config.Config{DownstreamURL: server.URL})
	if err == nil {
		t.Fatal("resolveStartupDownstreamModel() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "models response did not include a model id") {
		t.Fatalf("error = %q, want models response error", err.Error())
	}
}

func TestLoadStartupCacheLoadsPersistedEntries(t *testing.T) {
	tempDir := t.TempDir()
	cacheFilePath := tempDir + "/cache.json"
	cacheFileBody := []byte("{\n  \"version\": 1,\n  \"entries\": [\n    {\n      \"key\": \"persisted-key\",\n      \"status_code\": 201,\n      \"body\": \"eyJjaG9pY2VzIjpbeyJtZXNzYWdlIjp7ImNvbnRlbnQiOiJsb2FkZWQgZnJvbSBkaXNrIn19XSwidXNhZ2UiOnsiY29tcGxldGlvbl90b2tlbnMiOjJ9fQ==\",\n      \"content_type\": \"application/json\",\n      \"streaming\": false\n    }\n  ]\n}\n")
	if err := os.WriteFile(cacheFilePath, cacheFileBody, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))
	handler := httpapi.NewHandler()

	if err := loadStartupCache(logger, handler, cacheFilePath); err != nil {
		t.Fatalf("loadStartupCache() error = %v", err)
	}

	summaryRecorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(summaryRecorder, httptest.NewRequest(http.MethodGet, "/cache", nil))
	if summaryRecorder.Code != http.StatusOK {
		t.Fatalf("summary status code = %d, want %d", summaryRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"enabled":true`, `"cache_size":100`, `"cache_entries":1`, `"cache_file_path":"` + cacheFilePath + `"`} {
		if !strings.Contains(summaryRecorder.Body.String(), want) {
			t.Fatalf("summary body = %q, want substring %q", summaryRecorder.Body.String(), want)
		}
	}

	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/cache/persisted-key", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"status_code":201`, `"content":"loaded from disk"`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("body = %q, want substring %q", recorder.Body.String(), want)
		}
	}
	if !strings.Contains(logBuffer.String(), `msg="startup cache loaded"`) {
		t.Fatalf("log output = %q, want startup cache loaded message", logBuffer.String())
	}
}

func TestStartQueueDepthLoggerLogsQueueDepth(t *testing.T) {
	var logBuffer syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))
	handler := httpapi.NewHandler()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		startQueueDepthLogger(ctx, logger, handler, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuffer.String(), `msg="queue depth"`) {
			cancel()
			<-done
			logOutput := logBuffer.String()
			for _, want := range []string{
				`msg="queue depth"`,
				`concurrent_requests=0`,
				`max_concurrent_requests=512`,
				`waiting_requests=0`,
				`max_waiting_requests=1023`,
				`effective_tokens_per_second=32.8`,
				`computed_degradation_percentage=0`,
			} {
				if !strings.Contains(logOutput, want) {
					t.Fatalf("log output %q does not contain %q", logOutput, want)
				}
			}
			return
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	<-done
	t.Fatalf("log output %q does not contain queue depth entry", logBuffer.String())
}
