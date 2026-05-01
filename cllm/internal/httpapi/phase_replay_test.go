package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// phaseReplayCachedSSE returns a streaming-style cachedVLLMResponse
// with `n` single-token content chunks. Token counts are 1 per chunk
// since the synthetic content "x" tokenizes to one segment-token in
// the parser.
func phaseReplayCachedSSE(n int) cachedVLLMResponse {
	var b strings.Builder
	b.WriteString(`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString(`data: [DONE]` + "\n\n")
	return cachedVLLMResponse{
		statusCode:  200,
		contentType: "text/event-stream",
		streaming:   true,
		body:        []byte(b.String()),
	}
}

// TestCachedReplayDelayPicksPhaseRate verifies that an active envelope
// switches the rate at the boundary. Below the boundary the
// initial-phase rate is used; at and after the boundary the sustained
// rate kicks in.
func TestCachedReplayDelayPicksPhaseRate(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)

	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 10,
		InitialTPS:    1000,
		SustainedTPS:  10,
		Source:        "class",
	}}

	// Below boundary: should use 1000 tps -> very small delay.
	belowBoundary := h.cachedReplayDelay(1, 0, overrides)
	// At boundary: should use 10 tps -> much larger delay.
	atBoundary := h.cachedReplayDelay(1, 10, overrides)
	// Far above: same as at-boundary since phase B is sustained.
	farAbove := h.cachedReplayDelay(1, 100, overrides)

	if !(belowBoundary < atBoundary) {
		t.Fatalf("expected below(%s) < at(%s)", belowBoundary, atBoundary)
	}
	if atBoundary != farAbove {
		t.Fatalf("expected at(%s) == farAbove(%s) (both phase B)", atBoundary, farAbove)
	}
}

// TestCachedReplayDelayDSLTpsBeatsPhase: when both `:dsl tps=N` and a
// class envelope are present, the DSL override wins (single-rate
// semantics, per design §4.2).
func TestCachedReplayDelayDSLTpsBeatsPhase(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)

	overrides := replayOverrides{
		tpsOverride: 500,
		phase: phaseEnvelope{
			InitialTokens: 10,
			InitialTPS:    1000,
			SustainedTPS:  10,
			Source:        "class",
		},
	}

	below := h.cachedReplayDelay(1, 0, overrides)
	above := h.cachedReplayDelay(1, 100, overrides)
	if below != above {
		t.Fatalf("DSL tps must short-circuit phase: below(%s) above(%s)", below, above)
	}
}

// TestCachedReplayDelayInheritsBaseTPSWhenPhaseRateZero: a phase
// envelope with InitialTokens > 0 but SustainedTPS == 0 inherits the
// handler base TPS for the sustained phase (the consumer-time fallback
// promised in classes.go validation).
func TestCachedReplayDelayInheritsBaseTPSWhenPhaseRateZero(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)

	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 10,
		InitialTPS:    1000,
		// SustainedTPS unset -> inherit handler base TPS (64).
	}}

	wantBase := h.cachedReplayDelay(1, 0, replayOverrides{}) // base 64 tps
	gotPhaseB := h.cachedReplayDelay(1, 50, overrides)
	if wantBase != gotPhaseB {
		t.Fatalf("phase B inherits base: want(%s) got(%s)", wantBase, gotPhaseB)
	}
}

// TestReplayCachedStreamEmitsPhaseTransition runs the full replay loop
// against a synthetic SSE cache, asserts the phase_transition counter
// fires exactly once with the expected labels, and confirms the
// first-phase tokens emit fast while phase-B tokens are paced slow.
func TestReplayCachedStreamEmitsPhaseTransition(t *testing.T) {
	h := NewHandler()
	// Base TPS is irrelevant; the envelope rates dominate.
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	cached := phaseReplayCachedSSE(20)
	recorder := httptest.NewRecorder()
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    100000, // ~10us per token: phase A is ~free.
		SustainedTPS:  100000, // make phase B fast too so the test stays quick.
		Source:        "class",
	}}

	h.replayCachedStream(context.Background(), recorder, cached, replayOptions{
		stream:       true,
		includeUsage: true,
		overrides:    overrides,
		class:        "interactive",
	})

	got := testutil.ToFloat64(h.metrics.phaseTransitionsTotal.WithLabelValues("interactive", "phase_a", "phase_b"))
	if got != 1 {
		t.Fatalf("phase_transitions_total{interactive, phase_a, phase_b} = %v, want 1", got)
	}

	// No spurious other-class label.
	bleed := testutil.ToFloat64(h.metrics.phaseTransitionsTotal.WithLabelValues(defaultClassName, "phase_a", "phase_b"))
	if bleed != 0 {
		t.Fatalf("default class should not have transitions: got %v", bleed)
	}
}

// TestReplayCachedStreamNoTransitionForShortStream: a stream that
// finishes before reaching the boundary emits zero transition events.
// Absence of the transition is itself diagnostic ("interactive
// request was a short ack").
func TestReplayCachedStreamNoTransitionForShortStream(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	cached := phaseReplayCachedSSE(3)
	recorder := httptest.NewRecorder()
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 100,
		InitialTPS:    100000,
		SustainedTPS:  100000,
		Source:        "class",
	}}

	h.replayCachedStream(context.Background(), recorder, cached, replayOptions{
		stream:    true,
		overrides: overrides,
		class:     "interactive",
	})

	got := testutil.ToFloat64(h.metrics.phaseTransitionsTotal.WithLabelValues("interactive", "phase_a", "phase_b"))
	if got != 0 {
		t.Fatalf("short stream should not emit a transition, got %v", got)
	}
}

// TestReplayCachedStreamLegacyRequestUnchanged: a request with no
// phase envelope (and no DSL phase fields) emits no transition counter
// activity at all — back-compat guarantee for Phase 13.2.
func TestReplayCachedStreamLegacyRequestUnchanged(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	cached := phaseReplayCachedSSE(50)
	recorder := httptest.NewRecorder()

	h.replayCachedStream(context.Background(), recorder, cached, replayOptions{
		stream:    true,
		overrides: replayOverrides{}, // no phase envelope
		class:     defaultClassName,
	})

	for _, c := range []string{defaultClassName, "interactive", "batch"} {
		if got := testutil.ToFloat64(h.metrics.phaseTransitionsTotal.WithLabelValues(c, "phase_a", "phase_b")); got != 0 {
			t.Fatalf("legacy stream emitted transition for %q: %v", c, got)
		}
	}
}

// TestReplayCachedStreamPhaseATimingFasterThanPhaseB exercises the
// rate switch end-to-end: with a wide initial/sustained spread, the
// first segment's pacing must be observably faster than a
// post-boundary segment. We use a stub sleep that records the
// per-segment delays so the assertion is deterministic.
func TestReplayCachedStreamPhaseATimingFasterThanPhaseB(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	var sleeps []time.Duration
	h.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	cached := phaseReplayCachedSSE(10)
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    1000, // fast
		SustainedTPS:  10,   // slow
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), cached, replayOptions{
		stream:    true,
		overrides: overrides,
		class:     "interactive",
	})

	// We expect 11 sleep calls (role chunk + 10 content tokens, each
	// 1 token). Assert that sleeps[0..4] (phase A) are strictly less
	// than sleeps[5..9] (phase B). The role-chunk segment carries
	// tokenCount=0 so it gets skipped from sleep entirely; the first
	// recorded sleep is for the first content token.
	if len(sleeps) < 10 {
		t.Fatalf("got %d sleeps, want >=10: %v", len(sleeps), sleeps)
	}
	// First content sleep: tokensSoFar=0 (phase A, fast).
	first := sleeps[0]
	// A clearly post-boundary sleep: index 5 corresponds to the 6th
	// content token (tokensSoFar=5, exactly at boundary -> phase B).
	postBoundary := sleeps[5]
	if !(first < postBoundary) {
		t.Fatalf("expected first(%s) < postBoundary(%s)", first, postBoundary)
	}
	// Sanity check: first should be roughly 1/1000s, post-boundary
	// roughly 1/10s. Use a 50% slack to absorb degradation curve.
	if first > 5*time.Millisecond {
		t.Fatalf("phase A sleep too slow: %s (expected <5ms at 1000 tps)", first)
	}
	if postBoundary < 50*time.Millisecond {
		t.Fatalf("phase B sleep too fast: %s (expected ~100ms at 10 tps)", postBoundary)
	}
}

// TestReplayCachedStreamRecordsPhaseTokenCounts verifies the per-class
// phase-A / phase-B token counters tally a stream that crosses the
// boundary and a short stream that stays in phase A.
func TestReplayCachedStreamRecordsPhaseTokenCounts(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	// Long stream: 20 tokens, boundary at 5 -> 5 phase A, 15 phase B.
	long := phaseReplayCachedSSE(20)
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    100000,
		SustainedTPS:  100000,
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), long, replayOptions{
		stream: true, overrides: overrides, class: "interactive",
	})

	gotA := testutil.ToFloat64(h.metrics.phaseATokensTotal.WithLabelValues("interactive"))
	gotB := testutil.ToFloat64(h.metrics.phaseBTokensTotal.WithLabelValues("interactive"))
	if gotA != 5 || gotB != 15 {
		t.Fatalf("token counts = (A=%v, B=%v), want (5, 15)", gotA, gotB)
	}

	// Short stream: 3 tokens, boundary at 100 -> 3 phase A, 0 phase B.
	short := phaseReplayCachedSSE(3)
	overridesShort := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 100,
		InitialTPS:    100000,
		SustainedTPS:  100000,
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), short, replayOptions{
		stream: true, overrides: overridesShort, class: "interactive",
	})

	gotA2 := testutil.ToFloat64(h.metrics.phaseATokensTotal.WithLabelValues("interactive"))
	gotB2 := testutil.ToFloat64(h.metrics.phaseBTokensTotal.WithLabelValues("interactive"))
	if gotA2 != 8 || gotB2 != 15 {
		t.Fatalf("token counts after short = (A=%v, B=%v), want (8, 15)", gotA2, gotB2)
	}
}

// TestReplayCachedStreamRecordsPhaseReclaim asserts the reclaim
// counter accumulates (initial_tps - sustained_tps) * phaseBTokens
// when the envelope yields capacity, and stays zero when it does not.
func TestReplayCachedStreamRecordsPhaseReclaim(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)
	h.sleep = func(_ context.Context, _ time.Duration) error { return nil }

	cached := phaseReplayCachedSSE(20)
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    100000,
		SustainedTPS:  100000 / 2,
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), cached, replayOptions{
		stream: true, overrides: overrides, class: "interactive",
	})

	got := testutil.ToFloat64(h.metrics.phaseReclaimTokenSecondsTotal.WithLabelValues("interactive"))
	want := float64(100000-50000) * 15
	if got != want {
		t.Fatalf("reclaim = %v, want %v", got, want)
	}

	// Degenerate: sustained >= initial -> no reclaim.
	cached2 := phaseReplayCachedSSE(20)
	overrides2 := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    10,
		SustainedTPS:  10,
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), cached2, replayOptions{
		stream: true, overrides: overrides2, class: "batch",
	})
	if r := testutil.ToFloat64(h.metrics.phaseReclaimTokenSecondsTotal.WithLabelValues("batch")); r != 0 {
		t.Fatalf("batch reclaim should be 0, got %v", r)
	}
}

// TestReplayCachedStreamSetsEffectiveTPSGauges verifies the per-class
// effective-TPS gauges follow the resolved envelope rates.
func TestReplayCachedStreamSetsEffectiveTPSGauges(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	cached := phaseReplayCachedSSE(20)
	overrides := replayOverrides{phase: phaseEnvelope{
		InitialTokens: 5,
		InitialTPS:    500,
		SustainedTPS:  100,
		Source:        "class",
	}}
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), cached, replayOptions{
		stream: true, overrides: overrides, class: "interactive",
	})

	gotI := testutil.ToFloat64(h.metrics.phaseInitialTPSEffective.WithLabelValues("interactive"))
	gotS := testutil.ToFloat64(h.metrics.phaseSustainedTPSEffective.WithLabelValues("interactive"))
	wantI := h.effectiveClassRate(500)
	wantS := h.effectiveClassRate(100)
	if gotI != wantI || gotS != wantS {
		t.Fatalf("effective TPS = (I=%v, S=%v), want (%v, %v)", gotI, gotS, wantI, wantS)
	}
}

// TestReplayCachedStreamLegacyDoesNotTouchPhaseMetrics: a request
// with no envelope contributes nothing to any phase counter or gauge.
func TestReplayCachedStreamLegacyDoesNotTouchPhaseMetrics(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(10, 10)
	h.SetStreamRealism(0, 0, 0, 0, 0)

	cached := phaseReplayCachedSSE(50)
	h.replayCachedStream(context.Background(), httptest.NewRecorder(), cached, replayOptions{
		stream: true, overrides: replayOverrides{}, class: defaultClassName,
	})

	if got := testutil.ToFloat64(h.metrics.phaseATokensTotal.WithLabelValues(defaultClassName)); got != 0 {
		t.Fatalf("legacy phase_a tokens = %v, want 0", got)
	}
	if got := testutil.ToFloat64(h.metrics.phaseBTokensTotal.WithLabelValues(defaultClassName)); got != 0 {
		t.Fatalf("legacy phase_b tokens = %v, want 0", got)
	}
	if got := testutil.ToFloat64(h.metrics.phaseReclaimTokenSecondsTotal.WithLabelValues(defaultClassName)); got != 0 {
		t.Fatalf("legacy reclaim = %v, want 0", got)
	}
}
