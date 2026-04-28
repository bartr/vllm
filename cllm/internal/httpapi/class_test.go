package httpapi

import (
	"strings"
	"testing"
)

func TestResolveClassHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", defaultClassName},
		{"whitespace", "   ", defaultClassName},
		{"oversize", strings.Repeat("a", maxClassNameLen+1), defaultClassName},
		{"interactive lowercase", "interactive", "interactive"},
		{"mixed case folds", "InterActive", "interactive"},
		{"hyphen and digits", "batch-1", "batch-1"},
		{"underscore", "eval_run", "eval_run"},
		{"trailing space trimmed", "  batch  ", "batch"},
		{"illegal chars route default", "batch!", defaultClassName},
		{"slash routes default", "a/b", defaultClassName},
		{"unicode routes default", "café", defaultClassName},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveClassHeader(tc.raw); got != tc.want {
				t.Fatalf("resolveClassHeader(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestClassRegistryDefaultsAndUnknown(t *testing.T) {
	t.Parallel()
	r := newClassRegistry()
	if got := r.resolve("").name; got != defaultClassName {
		t.Fatalf("empty header → %q; want %q", got, defaultClassName)
	}
	if got := r.resolve("does-not-exist").name; got != defaultClassName {
		t.Fatalf("unknown class → %q; want %q", got, defaultClassName)
	}
	def := r.resolve("")
	if def.config.Priority != "medium" {
		t.Fatalf("default priority = %q; want %q", def.config.Priority, "medium")
	}
}

func TestClassRegistryConfigure(t *testing.T) {
	t.Parallel()
	r := newClassRegistry()
	r.configure(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxQueueMs: 500},
		"batch":       {Priority: "low", MaxQueueMs: 10000},
		// Default deliberately omitted → must be auto-injected.
	})
	if got := r.resolve("interactive").config.Priority; got != "high" {
		t.Fatalf("interactive priority = %q; want high", got)
	}
	if got := r.resolve("batch").config.MaxQueueMs; got != 10000 {
		t.Fatalf("batch max_queue_ms = %d; want 10000", got)
	}
	if got := r.resolve("").name; got != defaultClassName {
		t.Fatalf("empty header → %q; want %q (default auto-injected)", got, defaultClassName)
	}

	// Names sorted with default first.
	names := r.names()
	if len(names) == 0 || names[0] != defaultClassName {
		t.Fatalf("names[0] = %v; want default first", names)
	}
}

func TestClassRegistryConfigureRejectsMalformedNames(t *testing.T) {
	t.Parallel()
	r := newClassRegistry()
	r.configure(map[string]ClassConfig{
		"bad name!": {Priority: "high"},
		"good":      {Priority: "low"},
	})
	// Malformed name silently dropped (do not pollute label cardinality).
	if got := r.resolve("bad name!").name; got != defaultClassName {
		t.Fatalf("malformed class resolved to %q; want default", got)
	}
	if got := r.resolve("good").config.Priority; got != "low" {
		t.Fatalf("good priority = %q; want low", got)
	}
}

func TestParseDSLWorkloadClassOverride(t *testing.T) {
	t.Parallel()
	msgs := []chatCompletionMessage{
		{Role: "user", Content: "hello :dsl workload-class=interactive"},
	}
	_, ov := parseDSL(msgs, nil)
	if ov.workloadClass != "interactive" {
		t.Fatalf("workloadClass = %q; want interactive", ov.workloadClass)
	}
	wantDirective := "workload-class=interactive"
	found := false
	for _, d := range ov.directives {
		if d == wantDirective {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("directives = %v; want %q recorded", ov.directives, wantDirective)
	}
}

func TestParseDSLWorkloadClassFirstWins(t *testing.T) {
	t.Parallel()
	// Two directives across two messages; first occurrence (interactive) wins.
	msgs := []chatCompletionMessage{
		{Role: "system", Content: "ctx :dsl workload-class=interactive"},
		{Role: "user", Content: "ask :dsl workload-class=batch"},
	}
	_, ov := parseDSL(msgs, nil)
	if ov.workloadClass != "interactive" {
		t.Fatalf("workloadClass = %q; want interactive (first-wins)", ov.workloadClass)
	}
}

func TestParseDSLWorkloadClassRejectsMalformed(t *testing.T) {
	t.Parallel()
	msgs := []chatCompletionMessage{
		{Role: "user", Content: "hi :dsl workload-class=bad!name"},
	}
	_, ov := parseDSL(msgs, nil)
	if ov.workloadClass != "" {
		t.Fatalf("workloadClass = %q; want empty (malformed name dropped)", ov.workloadClass)
	}
	for _, d := range ov.directives {
		if strings.HasPrefix(d, "workload-class=") {
			t.Fatalf("directives = %v; malformed class should not be recorded", ov.directives)
		}
	}
}

func TestParseDSLWorkloadClassEmptyValueDropped(t *testing.T) {
	t.Parallel()
	msgs := []chatCompletionMessage{
		{Role: "user", Content: "hi :dsl workload-class="},
	}
	_, ov := parseDSL(msgs, nil)
	if ov.workloadClass != "" {
		t.Fatalf("workloadClass = %q; want empty for value-less directive", ov.workloadClass)
	}
}
