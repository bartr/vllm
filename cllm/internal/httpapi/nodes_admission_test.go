package httpapi

import (
	"context"
	"strings"
	"testing"

	"cllm/internal/node"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// makeTestNode constructs a synthetic *node.Node with the given id, class,
// and capacity. Realism/Upstream are zeroed; the node is suitable for
// admission-path tests that don't need downstream forwarding.
func makeTestNode(id, class string, capacity int64) *node.Node {
	return &node.Node{
		ID:        id,
		Class:     class,
		Budget:    node.NewTokenBudget(capacity, 16),
		Estimator: node.NewCompletionEstimator(256, 50),
		Capacity:  node.Capacity{MaxTokensInFlight: capacity, MaxWaitingRequests: 16},
	}
}

func TestAcquireRequestSlotOnNodeChargesNodeBudget(t *testing.T) {
	handler := NewHandler()
	a := makeTestNode("a", "H100", 100)
	b := makeTestNode("b", "A10", 100)
	handler.SetNodes([]*node.Node{a, b}, "least-loaded")

	release, _, ok, _ := handler.acquireRequestSlotOnNode(context.Background(), node.RequestCost{TotalCost: 50}, "/v1/chat/completions", a)
	if !ok {
		t.Fatalf("expected admission to succeed")
	}
	defer release()

	_, inFlightA, _, _ := a.Budget.Stats()
	_, inFlightB, _, _ := b.Budget.Stats()
	if inFlightA != 50 {
		t.Fatalf("node a in_flight = %d, want 50", inFlightA)
	}
	if inFlightB != 0 {
		t.Fatalf("node b in_flight = %d, want 0 (admission charged a only)", inFlightB)
	}

	// Per-node admission counter should record exactly one admit on a.
	got := testutil.ToFloat64(handler.metrics.nodeAdmissionsTotal.WithLabelValues("a", "H100", "admitted"))
	if got != 1 {
		t.Fatalf("nodeAdmissionsTotal{a,H100,admitted} = %v, want 1", got)
	}
}

func TestAcquireRequestSlotOnNodeNilFallsBackToScheduler(t *testing.T) {
	handler := NewHandler()
	// Default single-node fleet from NewHandler. Passing nil should
	// route through scheduler, which uses h.scheduler.node.
	release, _, ok, _ := handler.acquireRequestSlotOnNode(context.Background(), node.RequestCost{TotalCost: 1}, "/x", nil)
	if !ok {
		t.Fatalf("expected admission to succeed")
	}
	defer release()
	_, inFlight, _, _ := handler.scheduler.node.Budget.Stats()
	if inFlight != 1 {
		t.Fatalf("scheduler node in_flight = %d, want 1", inFlight)
	}
	// Per-node metric must NOT fire for the default fallback.
	got := testutil.ToFloat64(handler.metrics.nodeAdmissionsTotal.WithLabelValues("default", "default", "admitted"))
	if got != 0 {
		t.Fatalf("nodeAdmissionsTotal default fallback = %v, want 0 (single-node path)", got)
	}
}

func TestAcquireRequestSlotOnNodeRejectsWhenBudgetFull(t *testing.T) {
	handler := NewHandler()
	a := makeTestNode("a", "H100", 5)
	handler.SetNodes([]*node.Node{a, makeTestNode("b", "A10", 100)}, "least-loaded")

	// Saturate a; queue depth = 16 so the next acquire would queue, not reject.
	release1, _, ok, _ := handler.acquireRequestSlotOnNode(context.Background(), node.RequestCost{TotalCost: 5}, "/x", a)
	if !ok {
		t.Fatalf("first admission failed")
	}
	defer release1()

	// Cancel context immediately so the queued acquire returns false.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, ok2, _ := handler.acquireRequestSlotOnNode(ctx, node.RequestCost{TotalCost: 5}, "/x", a)
	if ok2 {
		t.Fatalf("expected rejection when budget full and ctx cancelled")
	}
	got := testutil.ToFloat64(handler.metrics.nodeAdmissionsTotal.WithLabelValues("a", "H100", "rejected"))
	if got != 1 {
		t.Fatalf("nodeAdmissionsTotal{a,H100,rejected} = %v, want 1", got)
	}
}

func TestSetNodesUpdatesNodeIDs(t *testing.T) {
	handler := NewHandler()
	if ids := handler.NodeIDs(); len(ids) != 1 || ids[0] != "default" {
		t.Fatalf("default fleet IDs = %v, want [default]", ids)
	}
	handler.SetNodes([]*node.Node{
		makeTestNode("rtx-2000-0", "rtx-2000", 100),
		makeTestNode("h100-0", "H100", 1000),
	}, "least-loaded")
	got := strings.Join(handler.NodeIDs(), ",")
	if got != "rtx-2000-0,h100-0" {
		t.Fatalf("multi-node fleet IDs = %q, want rtx-2000-0,h100-0", got)
	}
}

func TestNodeFleetCollectorEmitsOnlyForMultiNode(t *testing.T) {
	// Single-node default: no per-node series.
	single := NewHandler()
	out := testutil.CollectAndCount(nodeFleetCollector{handler: single})
	if out != 0 {
		t.Fatalf("single-node collector emitted %d series, want 0", out)
	}
	// Multi-node: expect 3 series (in_flight + max + waiting) per node.
	multi := NewHandler()
	multi.SetNodes([]*node.Node{
		makeTestNode("a", "H100", 100),
		makeTestNode("b", "A10", 100),
	}, "least-loaded")
	got := testutil.CollectAndCount(nodeFleetCollector{handler: multi})
	if got != 6 {
		t.Fatalf("multi-node collector emitted %d series, want 6", got)
	}
}
