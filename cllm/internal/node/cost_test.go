package node

import "testing"

// TestEstimateCostBackwardCompatible confirms EstimateCost (no KV
// estimator) keeps KVCost == TotalCost — byte-for-byte the pre-Phase-4
// shape callers depend on.
func TestEstimateCostBackwardCompatible(t *testing.T) {
	c := EstimateCost(50, 32, nil)
	if c.TotalCost != 82 {
		t.Fatalf("TotalCost = %d, want 82", c.TotalCost)
	}
	if c.KVCost != c.TotalCost {
		t.Fatalf("KVCost = %d, want == TotalCost (%d)", c.KVCost, c.TotalCost)
	}
}

// TestEstimateCostWithKVColdMirrorsTotal confirms a cold KV estimator
// falls back to TotalCost — callers should see no behavioural diff
// during warm-up.
func TestEstimateCostWithKVColdMirrorsTotal(t *testing.T) {
	kv := NewCompletionEstimator(64, 50)
	c := EstimateCostWithKV(50, 32, nil, kv, 1.0)
	if c.KVCost != c.TotalCost {
		t.Fatalf("cold KVCost = %d, want == TotalCost (%d)", c.KVCost, c.TotalCost)
	}
}

// TestEstimateCostWithKVWarmDecouples drives the KV estimator with a
// distribution that diverges from the compute estimator and confirms
// the resulting KVCost reflects the KV stream rather than the compute
// stream.
func TestEstimateCostWithKVWarmDecouples(t *testing.T) {
	costEst := NewCompletionEstimator(64, 4)
	kvEst := NewCompletionEstimator(64, 4)

	// Cost estimator sees long completions; KV estimator sees short
	// ones (mimicking prefix-cache amortization or short-residency
	// decode where KV cost decouples from compute cost).
	for i := 0; i < 20; i++ {
		costEst.Observe(100)
		kvEst.Observe(10)
	}

	c := EstimateCostWithKV(40, 200, costEst, kvEst, 1.0)
	if c.TotalCost != 40+100 {
		t.Fatalf("TotalCost = %d, want 140", c.TotalCost)
	}
	if c.KVCost != 40+10 {
		t.Fatalf("KVCost = %d, want 50 (decoupled from cost)", c.KVCost)
	}
}

// TestEstimateCostWithKVFactorAmortizes confirms a factor < 1.0
// amortizes the KV estimator output, the operator's calibration knob.
func TestEstimateCostWithKVFactorAmortizes(t *testing.T) {
	kvEst := NewCompletionEstimator(64, 4)
	for i := 0; i < 10; i++ {
		kvEst.Observe(80)
	}
	c := EstimateCostWithKV(20, 200, nil, kvEst, 0.5)
	// Expected: 20 + int(80 * 0.5) = 60.
	if c.KVCost != 60 {
		t.Fatalf("KVCost = %d, want 60 (factor 0.5)", c.KVCost)
	}
}

// TestEstimateCostWithKVFactorClampsToMaxTokens confirms the KV
// estimator output is bounded by maxTokens just like the compute path.
func TestEstimateCostWithKVFactorClampsToMaxTokens(t *testing.T) {
	kvEst := NewCompletionEstimator(64, 4)
	for i := 0; i < 10; i++ {
		kvEst.Observe(500)
	}
	c := EstimateCostWithKV(20, 32, nil, kvEst, 1.0)
	// kvP95 = 500 but maxTokens = 32, so the KV estimate is clamped.
	if c.KVCost != 20+32 {
		t.Fatalf("KVCost = %d, want %d (clamped to maxTokens)", c.KVCost, 20+32)
	}
}

// TestEstimateCostWithKVFactorZeroFallsBackToOne confirms factor=0 is
// treated as factor=1 (the documented backward-compat default).
func TestEstimateCostWithKVFactorZeroFallsBackToOne(t *testing.T) {
	kvEst := NewCompletionEstimator(64, 4)
	for i := 0; i < 10; i++ {
		kvEst.Observe(40)
	}
	c0 := EstimateCostWithKV(10, 200, nil, kvEst, 0)
	c1 := EstimateCostWithKV(10, 200, nil, kvEst, 1.0)
	if c0.KVCost != c1.KVCost {
		t.Fatalf("factor=0 KVCost (%d) != factor=1 KVCost (%d)", c0.KVCost, c1.KVCost)
	}
}
