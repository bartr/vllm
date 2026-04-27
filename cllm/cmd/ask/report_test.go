package main

import (
	"math"
	"testing"
	"time"
)

// TestWindowDequeStableUnderConcurrentLoad reproduces the original
// reported bug: 20 concurrent workers each producing tokens at 32 tps
// (target total 640 tps) was reporting 400..541 with the EWMA-mean lag
// (in-flight requests still under-filling the most recent buckets).
// With a decaying-max-lifetime lag, the steady-state reading must
// converge to ~640 tps with very low variance.
func TestWindowDequeStableUnderConcurrentLoad(t *testing.T) {
	const (
		workers     = 20
		perReqTPS   = 32.0
		reqDuration = 8 * time.Second // each request runs 8s, produces 256 tokens
		windowAge   = 15 * time.Second
		simSeconds  = 120
	)
	tokensPerReq := int(perReqTPS * reqDuration.Seconds()) // 256
	expected := workers * perReqTPS                        // 640

	w := windowDeque{maxAge: windowAge}
	base := time.Unix(0, 0)

	stagger := reqDuration / time.Duration(workers)
	type next struct {
		nextStart time.Time
	}
	state := make([]next, workers)
	for i := 0; i < workers; i++ {
		state[i].nextStart = base.Add(stagger * time.Duration(i))
	}

	tick := 100 * time.Millisecond
	end := base.Add(time.Duration(simSeconds) * time.Second)
	var samples []float64
	for now := base; !now.After(end); now = now.Add(tick) {
		for i := 0; i < workers; i++ {
			reqEnd := state[i].nextStart.Add(reqDuration)
			for !reqEnd.After(now) {
				w.add(state[i].nextStart, reqEnd, tokensPerReq)
				state[i].nextStart = reqEnd
				reqEnd = state[i].nextStart.Add(reqDuration)
			}
		}
		if int(now.Sub(base).Milliseconds())%1000 == 0 && now.After(base) {
			v, ready := w.tps(now)
			if ready {
				samples = append(samples, v)
			} else {
				samples = append(samples, math.NaN())
			}
		}
	}

	if len(samples) < simSeconds-1 {
		t.Fatalf("not enough samples: %d", len(samples))
	}
	// Steady state takes lag (~maxLife = 8s) + one full window to wash
	// out warm-up. Skip the first 40s to be safe.
	stable := samples[40:]
	for i, s := range stable {
		if math.IsNaN(s) {
			t.Errorf("sample %d (t=%ds): not ready (warmup gate too long)", i, 40+i+1)
			continue
		}
		if math.Abs(s-expected)/expected > 0.03 {
			t.Errorf("sample %d (t=%ds): tps=%.2f want ~%.2f (within 3%%)",
				i, 40+i+1, s, expected)
		}
	}
}

// TestWindowDequeProRatesStraddler verifies that an in-window request
// has its tokens distributed across its lifetime in 1-second buckets,
// and that the lagged-read picks up only the buckets actually inside
// the read window.
func TestWindowDequeProRatesStraddler(t *testing.T) {
	// Single 10s request producing 1000 tokens (100 tokens/sec),
	// running [t=10, t=20]. maxLife = 10s, lag = 10s (uncapped: cap is
	// maxAge - 1 = 14s).
	w := windowDeque{maxAge: 15 * time.Second}
	base := time.Unix(0, 0)
	w.add(base.Add(10*time.Second), base.Add(20*time.Second), 1000)
	// At t=40, lag=10s, readEnd=30.0, readStart=15.0.
	// Request buckets [10..19] (each holds 100 tokens). Overlap:
	// bucket 10..14 outside window (sec<15 → bucket [10,11) outside);
	// bucket 15: overlap [15,16) ∩ [15,30) = 1.0 → 100 tokens;
	// buckets 16..19 fully inside → 400 tokens. Total 500/15 ≈ 33.33.
	// (Bucket 15 holds tokens generated during [15,16) of the
	// request; with uniform 100/s production that's 100 tokens.)
	got, ready := w.tps(base.Add(40 * time.Second))
	if !ready {
		t.Fatalf("expected ready at t=40 (elapsed=30s, maxAge+lag=25s)")
	}
	want := 500.0 / 15.0
	if math.Abs(got-want) > 0.5 {
		t.Errorf("partial-overlap tps=%.2f want %.2f", got, want)
	}

	// At t=50, lag=10s, readEnd=40, window=[25,40]. Request fully
	// out → 0.
	got, _ = w.tps(base.Add(50 * time.Second))
	if got != 0 {
		t.Errorf("evicted tps=%.2f want 0", got)
	}
}

// TestWindowDequeWarmupCoverage checks the warmup gate: tps reports
// not-ready until elapsed >= maxAge + lag, then becomes ready with
// coverage capped at maxAge.
func TestWindowDequeWarmupCoverage(t *testing.T) {
	w := windowDeque{maxAge: 15 * time.Second}
	base := time.Unix(0, 0)
	// Short instantaneous request anchors first at t=0.
	w.add(base, base.Add(time.Millisecond), 1)
	// Add a request inside the window so we have something to count.
	w.add(base.Add(10*time.Second), base.Add(10*time.Second+time.Millisecond), 100)
	// Warmup gate: not ready until elapsed >= maxAge + lag.
	if _, ready := w.tps(base.Add(1 * time.Second)); ready {
		t.Errorf("expected NOT ready at t=1s (warmup gate)")
	}
	if _, ready := w.tps(base.Add(14 * time.Second)); ready {
		t.Errorf("expected NOT ready at t=14s (warmup gate)")
	}
	// At t=20s, elapsed=20 >= 15+tiny lag → ready. readEnd≈20,
	// window=[5,20]. Buckets at 0 (1 tok) and 10 (100 tok) both
	// inside → 101/15 ≈ 6.73.
	got, ready := w.tps(base.Add(20 * time.Second))
	if !ready {
		t.Fatalf("expected ready at t=20s")
	}
	want := 101.0 / 15.0
	if math.Abs(got-want)/want > 0.05 {
		t.Errorf("post-warmup tps=%.2f want %.2f", got, want)
	}
}
