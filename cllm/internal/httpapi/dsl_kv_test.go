package httpapi

import "testing"

func TestParseDSLKVCostOverride(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl kv-cost=512"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hi" {
		t.Fatalf("content = %q", out[0].Content)
	}
	if !ov.active() || ov.kvCostOverride != 512 {
		t.Fatalf("kvCostOverride = %d, want 512 (directives=%v)", ov.kvCostOverride, ov.directives)
	}
	if ov.noKV {
		t.Fatalf("noKV must remain false on kv-cost=N")
	}
}

func TestParseDSLNoKVSetsSentinel(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl no-kv"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hi" {
		t.Fatalf("content = %q", out[0].Content)
	}
	if !ov.noKV {
		t.Fatalf("no-kv should set noKV=true (directives=%v)", ov.directives)
	}
}

func TestParseDSLNoKVWinsOverLaterKVCost(t *testing.T) {
	// Directive class "kv-cost" is shared between no-kv and kv-cost=N;
	// first-wins semantics mean a later kv-cost=N must NOT clobber an
	// earlier no-kv claim.
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl no-kv kv-cost=999"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if !ov.noKV {
		t.Fatalf("no-kv should win first")
	}
	if ov.kvCostOverride != 0 {
		t.Fatalf("kvCostOverride must remain 0 when no-kv claimed first; got %d", ov.kvCostOverride)
	}
}

func TestParseDSLKVCostRejectsZeroAndNegative(t *testing.T) {
	for _, tc := range []string{
		"hi :dsl kv-cost=0",
		"hi :dsl kv-cost=-50",
	} {
		_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: tc}}, func() float64 { return 0 })
		if ov.kvCostOverride != 0 || ov.noKV {
			t.Fatalf("%q: malformed value should be rejected; got override=%d noKV=%v", tc, ov.kvCostOverride, ov.noKV)
		}
	}
}
