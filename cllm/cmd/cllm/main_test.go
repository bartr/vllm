package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"cllm/internal/buildinfo"
	"cllm/internal/config"
)

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	for _, want := range []string{"Usage: cllm [options]", "--downstream-url", "--downstream-token", "--downstream-model", "--models-cache-ttl", "--replay-delay", "--version", "-h, --help", `Default system prompt for /ask (default "You are a helpful assistant.")`, "Default max tokens for /ask (default 4000)", "Default temperature for /ask (default 0.2)"} {
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
