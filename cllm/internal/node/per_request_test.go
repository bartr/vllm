package node

import (
	"context"
	"testing"
	"time"
)

// TestPerRequestRateThreeRegime walks the three-regime curve in the
// vLLM-shaped pacing model (item 15, 0.13.0):
//
//	c <= threshold       -> base rate, constant
//	threshold < c <= max -> linear degradation toward base*(1-deg/100)
//	c > max              -> caller queues; rate at c==max returned
//
// Disabled when PerRequestTPS == 0.
func TestPerRequestRateThreeRegime(t *testing.T) {
	n := &Node{
		Capacity: Capacity{
			PerRequestTPS:        32,
			DegradationThreshold: 32,
			MaxConcurrency:       64,
		},
		Degradation: Degradation{MaxDegradation: 20},
	}

	cases := []struct {
		concurrency int
		want        float64
	}{
		{0, 32},     // empty
		{1, 32},     // 1 request
		{32, 32},    // at threshold, no degradation yet
		{48, 32 * (1 - 0.20*0.5)}, // halfway: 28.8
		{64, 32 * (1 - 0.20)},     // at max: 25.6
		{200, 32 * (1 - 0.20)},    // past max: clamped at max degradation
	}
	for _, tc := range cases {
		got := n.PerRequestRate(tc.concurrency)
		if got != tc.want {
			t.Errorf("PerRequestRate(%d) = %v, want %v", tc.concurrency, got, tc.want)
		}
	}
}

// TestPerRequestRateDisabled confirms a node with PerRequestTPS == 0
// returns 0 (caller falls back to the legacy fleet-divided pacer).
func TestPerRequestRateDisabled(t *testing.T) {
	n := &Node{Capacity: Capacity{PerRequestTPS: 0, MaxConcurrency: 64}}
	if got := n.PerRequestRate(10); got != 0 {
		t.Fatalf("disabled node should return 0, got %v", got)
	}
}

// TestConcurrencyGateAdmissionAndQueueing verifies the per-node
// request-slot semaphore: MaxConcurrency requests admit immediately;
// the next admits up to MaxWaitingRequests queue; further requests
// reject with ok=false.
func TestConcurrencyGateAdmissionAndQueueing(t *testing.T) {
	cap := Capacity{
		MaxTokensInFlight:  1_000_000,
		MaxWaitingRequests: 2,
		PerRequestTPS:      32,
		MaxConcurrency:     2,
	}
	n := &Node{
		ID:          "test",
		Capacity:    cap,
		Budget:      NewTokenBudget(cap.MaxTokensInFlight, cap.MaxWaitingRequests),
		Concurrency: NewTokenBudget(int64(cap.MaxConcurrency), cap.MaxWaitingRequests),
	}
	ctx := context.Background()

	// 2 admit immediately.
	if _, ok := n.Concurrency.Acquire(ctx, 1); !ok {
		t.Fatal("first admit failed")
	}
	if _, ok := n.Concurrency.Acquire(ctx, 1); !ok {
		t.Fatal("second admit failed")
	}
	if got := n.ConcurrentRequests(); got != 2 {
		t.Fatalf("ConcurrentRequests = %d, want 2", got)
	}
	// 3rd request would queue; verify rejected immediately when ctx
	// is already deadlined (used by TestRejection).
	deadlined, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	if _, ok := n.Concurrency.Acquire(deadlined, 1); ok {
		t.Fatal("expected rejection on saturated gate with deadlined ctx")
	}
}
