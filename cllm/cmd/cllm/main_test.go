package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cllm/internal/buildinfo"
	"cllm/internal/config"
	"cllm/internal/httpapi"
)

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	for _, want := range []string{"Usage: cllm [options]", "--downstream-url", "--downstream-token", "--downstream-model", "--max-tokens-per-second", "--max-concurrent-requests", "--max-waiting-requests", "--max-degradation", "--version", "-h, --help", `Default system prompt for /ask (default "You are a helpful assistant.")`, "Default max tokens for /ask (default 4000)", "Default temperature for /ask (default 0.2)"} {
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
		DownstreamURL:   "http://127.0.0.1:32080",
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

func TestStartQueueDepthLoggerLogsQueueDepth(t *testing.T) {
	var logBuffer bytes.Buffer
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
