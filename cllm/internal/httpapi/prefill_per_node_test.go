package httpapi

import (
	"context"
	"testing"

	"cllm/internal/node"
)

// TestComputePrefillDelayUsesPerNodeRate locks the per-node prefill
// behavior added in 0.14.0 (item 16). When the routed node carries
// Capacity.PerRequestTPS > 0 and Realism.PrefillRateMultiplier > 0,
// the prefill rate is `PerRequestRate(ConcurrentRequests()) ×
// Realism.PrefillRateMultiplier` regardless of the handler-global
// max-tokens-per-second / scheduler degradation curve.
func TestComputePrefillDelayUsesPerNodeRate(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(1024, 10)
	handler.SetPrefillSimulation(2.0, 0, 0, 0) // global multiplier 2x, no jitter / cap
	handler.jitterSource = func() float64 { return 0 }

	n := &node.Node{
		ID:    "perreq",
		Class: "perreq",
		Capacity: node.Capacity{
			PerRequestTPS:        50, // overrides global 100 tps
			DegradationThreshold: 0,  // pacing flat in band
			MaxConcurrency:       0,
		},
		Realism: node.Realism{
			PrefillRateMultiplier: 4, // overrides global 2x
		},
	}

	got := handler.computePrefillDelay(200, replayOverrides{routedNode: n})
	// Expected = 200 tokens / (50 tps × 4) = 200 / 200 = 1.0 s.
	wantNs := int64(1_000_000_000)
	delta := int64(got) - wantNs
	if delta < -1_000_000 || delta > 1_000_000 { // ±1ms tolerance
		t.Fatalf("delay = %s, want ~1s (50tps × 4 multiplier × 200 tokens)", got)
	}
}

// TestComputePrefillDelayDegradesWithNodeConcurrency verifies the
// per-node prefill delay grows as live concurrency rises. This is the
// regression trap for "TTFT doesn't track per-node concurrency" \u2014
// at concurrency = MaxConcurrency the prefill rate must equal
// PerRequestTPS × (1 - MaxDegradation/100).
func TestComputePrefillDelayDegradesWithNodeConcurrency(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(1024, 10)
	handler.SetPrefillSimulation(1.0, 0, 0, 0)
	handler.jitterSource = func() float64 { return 0 }

	n := &node.Node{
		ID:    "deg",
		Class: "deg",
		Concurrency: node.NewTokenBudget(8, 8),
		Capacity: node.Capacity{
			PerRequestTPS:        40,
			DegradationThreshold: 1,
			MaxConcurrency:       8,
		},
		Degradation: node.Degradation{MaxDegradation: 50},
		Realism:     node.Realism{PrefillRateMultiplier: 1},
	}

	cold := handler.computePrefillDelay(40, replayOverrides{routedNode: n})

	// Drive concurrency to the saturation point.
	for i := 0; i < 8; i++ {
		_, _, ok := n.Concurrency.AcquireWithPriority(context.Background(), 1, 0)
		if !ok {
			t.Fatalf("concurrency acquire %d: failed", i)
		}
	}
	hot := handler.computePrefillDelay(40, replayOverrides{routedNode: n})

	if !(hot > cold) {
		t.Fatalf("expected hot (%s) > cold (%s) once concurrency saturates", hot, cold)
	}
	// At c=MaxConcurrency, rate halves (MaxDegradation=50). So delay
	// should approximately double.
	if !(hot >= cold*3/2) {
		t.Fatalf("hot (%s) should be roughly 2× cold (%s) at full saturation; ratio %.2fx", hot, cold, float64(hot)/float64(cold))
	}
}

// TestPredictTTFTmsUsesPerNodeRate verifies that the admission-time
// TTFT prediction (used by class max_ttft_ms gate) also picks up the
// per-node rate.
func TestPredictTTFTmsUsesPerNodeRate(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(1024, 10)
	handler.SetPrefillSimulation(0, 0, 0, 0) // disable prefill component
	handler.jitterSource = func() float64 { return 0 }

	fast := &node.Node{
		ID:       "fast",
		Class:    "fast",
		Capacity: node.Capacity{PerRequestTPS: 100},
	}
	slow := &node.Node{
		ID:       "slow",
		Class:    "slow",
		Capacity: node.Capacity{PerRequestTPS: 10},
	}

	fastTTFT := handler.predictTTFTms(0, replayOverrides{routedNode: fast})
	slowTTFT := handler.predictTTFTms(0, replayOverrides{routedNode: slow})
	if !(slowTTFT > fastTTFT) {
		t.Fatalf("slow TTFT (%dms) should exceed fast TTFT (%dms) at the same concurrency", slowTTFT, fastTTFT)
	}
}
