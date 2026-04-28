package httpapi

import (
	"context"
	"testing"

	"cllm/internal/node"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// makeKVTestNode is like makeTestNode but also enables the KV admission
// axis with the supplied KV capacity.
func makeKVTestNode(id, class string, computeCap, kvCap int64) *node.Node {
	n := &node.Node{
		ID:        id,
		Class:     class,
		Budget:    node.NewTokenBudget(computeCap, 16),
		Estimator: node.NewCompletionEstimator(256, 50),
		Capacity: node.Capacity{
			MaxTokensInFlight:  computeCap,
			MaxWaitingRequests: 16,
			MaxKVTokens:        kvCap,
			KVWeight:           1.0,
		},
	}
	n.KV = node.NewKVBudget(kvCap)
	// Phase 4: KV-modeled nodes carry an independent KV estimator
	// stream; matching the loader so handler-level tests see the
	// same shape.
	n.KVEstimator = node.NewCompletionEstimator(256, 50)
	return n
}

func TestKVAdmissionRejectsKVPressure(t *testing.T) {
	handler := NewHandler()
	a := makeKVTestNode("a", "H100", 1000, 100)
	handler.SetNodes([]*node.Node{a, makeTestNode("b", "A10", 1000)}, "least-loaded")

	// First request: compute=50, kv=80. Both fit.
	rel1, _, ok, reason := handler.acquireRequestSlotOnNode(
		context.Background(),
		node.RequestCost{TotalCost: 50, KVCost: 80},
		"/v1/chat/completions", a)
	if !ok {
		t.Fatalf("first admission failed: reason=%q", reason)
	}
	defer rel1()

	// Second request: compute fits (50+50=100 <= 1000), but KV would
	// overflow (80+30=110 > 100) so it must be rejected with
	// kv_pressure rather than queuing on compute.
	_, _, ok2, reason2 := handler.acquireRequestSlotOnNode(
		context.Background(),
		node.RequestCost{TotalCost: 50, KVCost: 30},
		"/v1/chat/completions", a)
	if ok2 {
		t.Fatalf("expected KV pressure rejection")
	}
	if reason2 != "kv_pressure" {
		t.Fatalf("reason = %q, want kv_pressure", reason2)
	}

	// Compute slot must have been refunded (work-conserving unwind).
	_, inFlightCompute, _, _ := a.Budget.Stats()
	if inFlightCompute != 50 {
		t.Fatalf("compute in_flight after KV-pressure unwind = %d, want 50", inFlightCompute)
	}

	// Per-node admission counter records exactly one rejected.
	got := testutil.ToFloat64(handler.metrics.nodeAdmissionsTotal.WithLabelValues("a", "H100", "rejected"))
	if got != 1 {
		t.Fatalf("nodeAdmissionsTotal{a,H100,rejected} = %v, want 1", got)
	}
}

func TestKVAdmissionRejectsKVOversize(t *testing.T) {
	handler := NewHandler()
	a := makeKVTestNode("a", "H100", 100000, 1000)
	handler.SetNodes([]*node.Node{a, makeTestNode("b", "A10", 1000)}, "least-loaded")

	// kv_cost alone exceeds MaxKVTokens (5000 > 1000). Reject
	// immediately with kv_oversize, not kv_pressure.
	_, _, ok, reason := handler.acquireRequestSlotOnNode(
		context.Background(),
		node.RequestCost{TotalCost: 50, KVCost: 5000},
		"/v1/chat/completions", a)
	if ok {
		t.Fatalf("expected kv_oversize rejection")
	}
	if reason != "kv_oversize" {
		t.Fatalf("reason = %q, want kv_oversize", reason)
	}

	// Compute budget must be untouched.
	_, inFlightCompute, _, _ := a.Budget.Stats()
	if inFlightCompute != 0 {
		t.Fatalf("compute in_flight after kv_oversize = %d, want 0", inFlightCompute)
	}
	// KV budget must be untouched.
	_, inFlightKV := a.KV.Stats()
	if inFlightKV != 0 {
		t.Fatalf("kv in_flight after kv_oversize = %d, want 0", inFlightKV)
	}
}

func TestKVAdmissionReleaseRefundsBothBudgets(t *testing.T) {
	handler := NewHandler()
	a := makeKVTestNode("a", "H100", 1000, 1000)
	handler.SetNodes([]*node.Node{a, makeTestNode("b", "A10", 1000)}, "least-loaded")

	rel, _, ok, _ := handler.acquireRequestSlotOnNode(
		context.Background(),
		node.RequestCost{TotalCost: 100, KVCost: 200},
		"/v1/chat/completions", a)
	if !ok {
		t.Fatalf("admission failed")
	}
	_, inFlightCompute, _, _ := a.Budget.Stats()
	_, inFlightKV := a.KV.Stats()
	if inFlightCompute != 100 || inFlightKV != 200 {
		t.Fatalf("post-admit (compute,kv) = (%d,%d), want (100,200)", inFlightCompute, inFlightKV)
	}

	rel()
	_, inFlightCompute, _, _ = a.Budget.Stats()
	_, inFlightKV = a.KV.Stats()
	if inFlightCompute != 0 || inFlightKV != 0 {
		t.Fatalf("post-release (compute,kv) = (%d,%d), want (0,0)", inFlightCompute, inFlightKV)
	}
}

func TestKVAdmissionDisabledByDefault(t *testing.T) {
	handler := NewHandler()
	// Plain node without KV; admission must behave as today.
	a := makeTestNode("a", "H100", 1000)
	handler.SetNodes([]*node.Node{a, makeTestNode("b", "A10", 1000)}, "least-loaded")

	rel, _, ok, reason := handler.acquireRequestSlotOnNode(
		context.Background(),
		node.RequestCost{TotalCost: 50, KVCost: 9999999}, // enormous, would oversize if KV were on
		"/v1/chat/completions", a)
	if !ok {
		t.Fatalf("admission failed with KV disabled: reason=%q", reason)
	}
	defer rel()

	if a.KV != nil {
		t.Fatalf("expected node.KV == nil when MaxKVTokens=0")
	}
}
