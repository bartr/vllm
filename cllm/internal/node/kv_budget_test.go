package node

import "testing"

func TestKVBudgetTryChargeAndRelease(t *testing.T) {
	b := NewKVBudget(100)
	if ok, reason := b.TryCharge(40); !ok || reason != "" {
		t.Fatalf("first charge: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := b.TryCharge(60); !ok || reason != "" {
		t.Fatalf("second charge filling capacity: ok=%v reason=%q", ok, reason)
	}
	if cap, inFlight := b.Stats(); cap != 100 || inFlight != 100 {
		t.Fatalf("stats = (%d,%d), want (100,100)", cap, inFlight)
	}
	// At capacity. A new request should be kv_pressure (would fit if
	// we had room), not kv_oversize (charge alone <= capacity).
	if ok, reason := b.TryCharge(1); ok || reason != "kv_pressure" {
		t.Fatalf("charge at capacity: ok=%v reason=%q, want kv_pressure", ok, reason)
	}

	b.Release(40)
	if cap, inFlight := b.Stats(); cap != 100 || inFlight != 60 {
		t.Fatalf("after release stats = (%d,%d), want (100,60)", cap, inFlight)
	}
	if ok, reason := b.TryCharge(40); !ok || reason != "" {
		t.Fatalf("recharge after release: ok=%v reason=%q", ok, reason)
	}
}

func TestKVBudgetOversizeIsDistinguishedFromPressure(t *testing.T) {
	b := NewKVBudget(100)
	if ok, reason := b.TryCharge(150); ok || reason != "kv_oversize" {
		t.Fatalf("oversize on empty budget: ok=%v reason=%q, want kv_oversize", ok, reason)
	}
	// Oversize must not consume any capacity.
	if _, inFlight := b.Stats(); inFlight != 0 {
		t.Fatalf("oversize charged budget: inFlight=%d, want 0", inFlight)
	}
	// And a normal request still admits.
	if ok, _ := b.TryCharge(50); !ok {
		t.Fatalf("normal charge after oversize attempt should still admit")
	}
}

func TestKVBudgetReleaseDoesNotGoNegative(t *testing.T) {
	b := NewKVBudget(100)
	b.Release(50) // release without prior charge
	if _, inFlight := b.Stats(); inFlight != 0 {
		t.Fatalf("inFlight after spurious release = %d, want 0", inFlight)
	}
}

func TestKVBudgetReconfigureKeepsInFlight(t *testing.T) {
	b := NewKVBudget(100)
	b.TryCharge(80)
	b.Reconfigure(50) // shrink below current inFlight
	cap, inFlight := b.Stats()
	if cap != 50 {
		t.Fatalf("capacity after shrink = %d, want 50", cap)
	}
	if inFlight != 80 {
		t.Fatalf("inFlight after shrink = %d, want 80 (admitted requests are not evicted)", inFlight)
	}
	// Existing inFlight blocks new charges until release.
	if ok, reason := b.TryCharge(1); ok || reason != "kv_pressure" {
		t.Fatalf("post-shrink charge: ok=%v reason=%q, want kv_pressure", ok, reason)
	}
	b.Release(80)
	if ok, _ := b.TryCharge(40); !ok {
		t.Fatalf("post-release charge should admit")
	}
}

func TestKVBudgetClampsCapacityToOne(t *testing.T) {
	b := NewKVBudget(0)
	if cap, _ := b.Stats(); cap != 1 {
		t.Fatalf("zero capacity clamped to %d, want 1", cap)
	}
	b.Reconfigure(-5)
	if cap, _ := b.Stats(); cap != 1 {
		t.Fatalf("negative reconfigure clamped to %d, want 1", cap)
	}
}
