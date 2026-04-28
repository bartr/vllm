package httpapi

import "testing"

// TestResolvePhaseEnvelopeNilClass returns the zero value rather than
// panicking; used by handler paths that may receive a nil class
// pointer in degenerate setups (defense-in-depth).
func TestResolvePhaseEnvelopeNilClass(t *testing.T) {
	env := resolvePhaseEnvelope(nil, replayOverrides{})
	if env.active() {
		t.Fatalf("nil class should yield inactive envelope: %+v", env)
	}
}

// TestResolvePhaseEnvelopeFromClass: a class with non-zero phase
// fields produces an active envelope sourced "class".
func TestResolvePhaseEnvelopeFromClass(t *testing.T) {
	c := newClassState("interactive", ClassConfig{
		Priority:      "high",
		InitialTokens: 100,
		InitialTPS:    32,
		SustainedTPS:  16,
	})
	env := resolvePhaseEnvelope(c, replayOverrides{})
	if !env.active() {
		t.Fatalf("expected active envelope, got %+v", env)
	}
	if env.InitialTokens != 100 || env.InitialTPS != 32 || env.SustainedTPS != 16 {
		t.Fatalf("envelope mismatch: %+v", env)
	}
	if env.Source != "class" {
		t.Fatalf("source = %q, want class", env.Source)
	}
}

// TestResolvePhaseEnvelopeLegacyClass: a class with no phase fields
// (today's default) yields a zero-value envelope so the cache replay
// path stays on its single-rate branch.
func TestResolvePhaseEnvelopeLegacyClass(t *testing.T) {
	c := newClassState("default", ClassConfig{Priority: "medium"})
	env := resolvePhaseEnvelope(c, replayOverrides{})
	if env.active() {
		t.Fatalf("legacy class should be inactive: %+v", env)
	}
	if env != (phaseEnvelope{}) {
		t.Fatalf("legacy class should be zero value: %+v", env)
	}
}

// TestResolvePhaseEnvelopeRatesOnly: a class that pins rates but no
// boundary is treated as single-rate (active() == false). 13.4 will
// re-evaluate this once DSL-level overrides land — the class is
// effectively a sustained-only envelope.
func TestResolvePhaseEnvelopeRatesOnly(t *testing.T) {
	c := newClassState("batch", ClassConfig{
		Priority:     "low",
		SustainedTPS: 32,
	})
	env := resolvePhaseEnvelope(c, replayOverrides{})
	if env.active() {
		t.Fatalf("rates-without-boundary must be inactive: %+v", env)
	}
	if env.SustainedTPS != 32 || env.Source != "class" {
		t.Fatalf("rates carried through but envelope inactive: %+v", env)
	}
}
