package main

import (
	"bytes"
	"strings"
	"testing"

	"cache/internal/buildinfo"
)

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}
	for _, want := range []string{"Usage: cache [options]", "--models-cache-ttl", "--version", "-h, --help"} {
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
	if !strings.Contains(stdout.String(), "Usage: cache [options]") {
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
	if stdout.String() != "cache 1.2.3\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "cache 1.2.3\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
