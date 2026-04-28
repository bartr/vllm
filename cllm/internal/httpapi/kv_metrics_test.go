package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cllm/internal/node"
)

// TestCombinedLoadDrivesDegradationWhenKVEnabled is the canonical
// "memory-bound vs compute-bound" experiment from
// docs/design-memory-pressure.md: at the same admitted token cost, a
// long-context request degrades a KV-enabled node faster than a
// short-context request because kv_load > cost_load.
func TestCombinedLoadDrivesDegradationWhenKVEnabled(t *testing.T) {
	short := makeKVTestNode("short", "H100", 10000, 1000)
	long := makeKVTestNode("long", "H100", 10000, 1000)

	short.Budget.Acquire(context.Background(), 500)
	long.Budget.Acquire(context.Background(), 500)

	short.KV.TryCharge(50)
	long.KV.TryCharge(800)

	shortLoad := combinedLoadOf(short)
	longLoad := combinedLoadOf(long)

	// short: max(500/10000=0.05, 50/1000=0.05) = 0.05
	// long:  max(500/10000=0.05, 800/1000=0.80) = 0.80
	if shortLoad >= longLoad {
		t.Fatalf("short combined_load (%v) should be < long (%v)", shortLoad, longLoad)
	}
	if shortLoad >= 0.10 {
		t.Fatalf("short combined_load = %v, want < 0.10 (no degradation)", shortLoad)
	}
	if longLoad <= 0.10 {
		t.Fatalf("long combined_load = %v, want > 0.10 (KV pressure visible)", longLoad)
	}

	const baseTPS = 32
	const maxDeg = 50
	shortDeg, shortTPS := degradationFromLoad(shortLoad, baseTPS, maxDeg)
	longDeg, longTPS := degradationFromLoad(longLoad, baseTPS, maxDeg)

	if shortDeg != 0 {
		t.Fatalf("short_deg = %v, want 0 (below threshold)", shortDeg)
	}
	if longDeg <= shortDeg {
		t.Fatalf("long_deg (%v) should exceed short_deg (%v)", longDeg, shortDeg)
	}
	if longTPS >= shortTPS {
		t.Fatalf("long_tps (%v) should be < short_tps (%v) once KV pressure binds", longTPS, shortTPS)
	}
}

// TestCombinedLoadFallsBackToCostWhenKVDisabled enforces the backward-
// compat contract: combinedLoadOf on a KV-disabled node equals
// cost_load alone.
func TestCombinedLoadFallsBackToCostWhenKVDisabled(t *testing.T) {
	plain := makeTestNode("plain", "default", 100)
	plain.Budget.Acquire(context.Background(), 50)

	got := combinedLoadOf(plain)
	want := 0.50
	if got != want {
		t.Fatalf("combinedLoadOf(no-KV) = %v, want %v", got, want)
	}
}

// TestKVMetricsExposedOnlyWhenEnabled enforces the cardinality
// contract: KV-axis Prometheus series are emitted only for nodes with
// KV modeling enabled.
func TestKVMetricsExposedOnlyWhenEnabled(t *testing.T) {
	handler := NewHandler()
	withKV := makeKVTestNode("with-kv", "H100", 10000, 1000)
	withoutKV := makeTestNode("plain", "A10", 10000)
	handler.SetNodes([]*node.Node{withKV, withoutKV}, "least-loaded")

	withKV.Budget.Acquire(context.Background(), 100)
	withKV.KV.TryCharge(200)
	withoutKV.Budget.Acquire(context.Background(), 100)

	body := scrapeMetrics(t, handler)

	mustContain(t, body, `cllm_node_kv_tokens_in_flight{class="H100",node="with-kv"} 200`)
	mustContain(t, body, `cllm_node_max_kv_tokens{class="H100",node="with-kv"} 1000`)
	mustContain(t, body, `cllm_node_combined_load{class="H100",node="with-kv"}`)
	mustNotContain(t, body, `cllm_node_kv_tokens_in_flight{class="A10"`)
	mustNotContain(t, body, `cllm_node_max_kv_tokens{class="A10"`)
	mustNotContain(t, body, `cllm_node_combined_load{class="A10"`)

	mustContain(t, body, `cllm_node_tokens_in_flight{class="H100",node="with-kv"} 100`)
	mustContain(t, body, `cllm_node_tokens_in_flight{class="A10",node="plain"} 100`)
}

// scrapeMetrics gathers the in-process Prometheus registry and renders
// a minimal text body sufficient for substring assertions.
func scrapeMetrics(t *testing.T, handler *Handler) string {
	t.Helper()
	mfs, err := handler.metrics.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			sb.WriteString(mf.GetName())
			sb.WriteString("{")
			for i, lp := range m.Label {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `%s="%s"`, lp.GetName(), lp.GetValue())
			}
			sb.WriteString("} ")
			switch {
			case m.Gauge != nil:
				fmt.Fprintf(&sb, "%g", m.Gauge.GetValue())
			case m.Counter != nil:
				fmt.Fprintf(&sb, "%g", m.Counter.GetValue())
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("metrics output missing %q\n---\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("metrics output unexpectedly contains %q", needle)
	}
}
