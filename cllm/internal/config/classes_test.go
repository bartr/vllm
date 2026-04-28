package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadClassesFileAcceptsPhaseFields ensures the loader accepts
// the new phase-aware token-allocation fields (Phase 13.1) and
// preserves them through to the returned ClassSpec.
func TestReadClassesFileAcceptsPhaseFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "classes.yaml")
	body := `
classes:
  default:
    priority: medium
  interactive:
    priority: high
    max_queue_ms: 500
    initial_tokens: 100
    initial_tps: 32
    sustained_tps: 16
  batch:
    priority: low
    sustained_tps: 32
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := readClassesFile(path, true)
	if err != nil {
		t.Fatalf("readClassesFile: %v", err)
	}
	got := out["interactive"]
	if got.InitialTokens != 100 || got.InitialTPS != 32 || got.SustainedTPS != 16 {
		t.Fatalf("interactive envelope mismatch: %+v", got)
	}
	if out["batch"].InitialTokens != 0 || out["batch"].SustainedTPS != 32 {
		t.Fatalf("batch envelope mismatch: %+v", out["batch"])
	}
	if out["default"].InitialTokens != 0 || out["default"].InitialTPS != 0 || out["default"].SustainedTPS != 0 {
		t.Fatalf("default should have zero phase fields: %+v", out["default"])
	}
}

func TestReadClassesFileRejectsNegativePhaseFields(t *testing.T) {
	cases := map[string]string{
		"initial_tokens": "initial_tokens: -1",
		"initial_tps":    "initial_tps: -1",
		"sustained_tps":  "sustained_tps: -1",
	}
	for label, line := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "classes.yaml")
			body := "classes:\n  bad:\n    priority: medium\n    " + line + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := readClassesFile(path, true)
			if err == nil {
				t.Fatalf("expected error for negative %s", label)
			}
			if !strings.Contains(err.Error(), label) {
				t.Fatalf("error %v does not mention %s", err, label)
			}
		})
	}
}

// TestReadClassesFileRejectsBoundaryWithoutRates: an envelope that
// declares a non-zero phase boundary but leaves both rates at 0 has
// no observable timing effect — the loader rejects it so misconfig
// surfaces at boot rather than as silent metric noise.
func TestReadClassesFileRejectsBoundaryWithoutRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "classes.yaml")
	body := `
classes:
  bad:
    priority: medium
    initial_tokens: 50
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readClassesFile(path, true)
	if err == nil {
		t.Fatal("expected misconfig error")
	}
	if !strings.Contains(err.Error(), "initial_tokens") {
		t.Fatalf("error %v should mention initial_tokens", err)
	}
}

// TestReadClassesFileAllowsBoundaryWithSingleRate: a boundary plus
// just one rate is a valid configuration — the unset rate inherits
// the handler base TPS at consumption time.
func TestReadClassesFileAllowsBoundaryWithSingleRate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "classes.yaml")
	body := `
classes:
  a:
    initial_tokens: 100
    initial_tps: 32
  b:
    initial_tokens: 100
    sustained_tps: 16
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := readClassesFile(path, true)
	if err != nil {
		t.Fatalf("readClassesFile: %v", err)
	}
	if out["a"].InitialTPS != 32 || out["a"].SustainedTPS != 0 {
		t.Fatalf("class a: %+v", out["a"])
	}
	if out["b"].SustainedTPS != 16 || out["b"].InitialTPS != 0 {
		t.Fatalf("class b: %+v", out["b"])
	}
}

// TestShippedClassesYAMLParses guards the in-tree configs/classes.yaml
// example against regressions introduced by new validation rules.
func TestShippedClassesYAMLParses(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "classes.yaml")
	out, err := readClassesFile(path, true)
	if err != nil {
		t.Fatalf("shipped classes.yaml: %v", err)
	}
	if _, ok := out["default"]; !ok {
		t.Fatalf("shipped classes.yaml missing default class")
	}
}
