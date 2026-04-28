package httpapi

import (
	"strings"
	"testing"
)

// TestParseDSLInitialTokensOverride verifies the directive parses
// cleanly, lands on the dslInitialTokens field, sets the presence
// flag, and is recorded in the directives slice.
func TestParseDSLInitialTokensOverride(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tokens=80"},
	}, nil)
	if !ov.dslInitialTokensSet || ov.dslInitialTokens != 80 {
		t.Fatalf("got set=%v tokens=%d; want true, 80", ov.dslInitialTokensSet, ov.dslInitialTokens)
	}
	if !directiveRecorded(ov.directives, "initial-tokens=80") {
		t.Fatalf("directives = %v; want initial-tokens=80", ov.directives)
	}
}

// TestParseDSLInitialTokensZeroIsValid: 0 is a meaningful value
// ("skip phase A") so the parser must record it and set the presence
// flag, distinguishing it from the unset case.
func TestParseDSLInitialTokensZeroIsValid(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tokens=0"},
	}, nil)
	if !ov.dslInitialTokensSet {
		t.Fatalf("dslInitialTokensSet = false; want true")
	}
	if ov.dslInitialTokens != 0 {
		t.Fatalf("dslInitialTokens = %d; want 0", ov.dslInitialTokens)
	}
}

func TestParseDSLInitialTokensRejectsNegative(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tokens=-5"},
	}, nil)
	if ov.dslInitialTokensSet {
		t.Fatalf("negative initial-tokens should be dropped: set=%v", ov.dslInitialTokensSet)
	}
}

func TestParseDSLInitialTPSOverride(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tps=200"},
	}, nil)
	if ov.dslInitialTPSOverride != 200 {
		t.Fatalf("dslInitialTPSOverride = %d; want 200", ov.dslInitialTPSOverride)
	}
}

func TestParseDSLInitialTPSRejectsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, val := range []string{"0", "-1", "5000"} {
		_, ov := parseDSL([]chatCompletionMessage{
			{Role: "user", Content: "hi :dsl initial-tps=" + val},
		}, nil)
		if ov.dslInitialTPSOverride != 0 {
			t.Fatalf("initial-tps=%s accepted (got %d); want rejected", val, ov.dslInitialTPSOverride)
		}
	}
}

func TestParseDSLSustainedTPSOverride(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl sustained-tps=64"},
	}, nil)
	if ov.dslSustainedTPSOverride != 64 {
		t.Fatalf("dslSustainedTPSOverride = %d; want 64", ov.dslSustainedTPSOverride)
	}
}

// TestParseDSLPhaseAllThree: a complete envelope from DSL alone.
func TestParseDSLPhaseAllThree(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tokens=100 initial-tps=500 sustained-tps=50"},
	}, nil)
	if ov.dslInitialTokens != 100 || ov.dslInitialTPSOverride != 500 || ov.dslSustainedTPSOverride != 50 {
		t.Fatalf("unexpected: tokens=%d initial=%d sustained=%d",
			ov.dslInitialTokens, ov.dslInitialTPSOverride, ov.dslSustainedTPSOverride)
	}
}

// TestParseDSLNoPhaseClaimsAllThreeClasses: a no-phase token before
// any of the per-field directives forces single-rate. Subsequent
// initial-tokens/initial-tps/sustained-tps tokens must be ignored
// (first-wins per directive class).
func TestParseDSLNoPhaseClaimsAllThreeClasses(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl no-phase initial-tokens=100 initial-tps=500 sustained-tps=50"},
	}, nil)
	if !ov.noPhase {
		t.Fatalf("noPhase should be true")
	}
	if ov.dslInitialTokensSet || ov.dslInitialTPSOverride != 0 || ov.dslSustainedTPSOverride != 0 {
		t.Fatalf("no-phase should suppress per-field overrides: %+v", ov)
	}
	if !directiveRecorded(ov.directives, "no-phase") {
		t.Fatalf("no-phase not recorded: %v", ov.directives)
	}
}

// TestParseDSLPhaseFirstWins: a `:dsl initial-tps=100 initial-tps=200`
// keeps the first occurrence per directive class.
func TestParseDSLPhaseFirstWins(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl initial-tps=100 initial-tps=200"},
	}, nil)
	if ov.dslInitialTPSOverride != 100 {
		t.Fatalf("first-wins violated: got %d; want 100", ov.dslInitialTPSOverride)
	}
}

// TestResolvePhaseEnvelopeDSLOverlaysClass: DSL override fields take
// precedence per-field; unspecified DSL fields inherit class.
func TestResolvePhaseEnvelopeDSLOverlaysClass(t *testing.T) {
	t.Parallel()
	c := newClassState("interactive", ClassConfig{
		InitialTokens: 100,
		InitialTPS:    32,
		SustainedTPS:  16,
	})
	dsl := replayOverrides{
		dslSustainedTPSOverride: 4,
	}
	env := resolvePhaseEnvelope(c, dsl)
	if env.InitialTokens != 100 || env.InitialTPS != 32 {
		t.Fatalf("class fields lost: %+v", env)
	}
	if env.SustainedTPS != 4 {
		t.Fatalf("DSL sustained override lost: %+v", env)
	}
	if env.Source != "mixed" {
		t.Fatalf("Source = %q; want mixed", env.Source)
	}
}

// TestResolvePhaseEnvelopeNoPhaseShortCircuits: even with a fully
// configured class, no-phase forces a zero-value envelope.
func TestResolvePhaseEnvelopeNoPhaseShortCircuits(t *testing.T) {
	t.Parallel()
	c := newClassState("interactive", ClassConfig{
		InitialTokens: 100,
		InitialTPS:    32,
		SustainedTPS:  16,
	})
	env := resolvePhaseEnvelope(c, replayOverrides{noPhase: true})
	if env.active() {
		t.Fatalf("no-phase should yield inactive envelope: %+v", env)
	}
	if env != (phaseEnvelope{}) {
		t.Fatalf("no-phase should yield zero value: %+v", env)
	}
}

// TestResolvePhaseEnvelopeDSLOnly: with no class envelope and
// per-request DSL fields, the resolver builds an envelope sourced
// "dsl".
func TestResolvePhaseEnvelopeDSLOnly(t *testing.T) {
	t.Parallel()
	c := newClassState("default", ClassConfig{Priority: "medium"})
	dsl := replayOverrides{
		dslInitialTokensSet:     true,
		dslInitialTokens:        50,
		dslInitialTPSOverride:   200,
		dslSustainedTPSOverride: 20,
	}
	env := resolvePhaseEnvelope(c, dsl)
	if !env.active() || env.InitialTokens != 50 || env.InitialTPS != 200 || env.SustainedTPS != 20 {
		t.Fatalf("DSL-only envelope mismatch: %+v", env)
	}
	if env.Source != "dsl" {
		t.Fatalf("Source = %q; want dsl", env.Source)
	}
}

// TestResolvePhaseEnvelopeDSLInitialTokensZeroSkipsPhaseA: a class
// envelope with initial_tokens=100 plus a per-request `initial-tokens=0`
// must collapse to single-rate (active() == false).
func TestResolvePhaseEnvelopeDSLInitialTokensZeroSkipsPhaseA(t *testing.T) {
	t.Parallel()
	c := newClassState("interactive", ClassConfig{
		InitialTokens: 100,
		InitialTPS:    32,
		SustainedTPS:  16,
	})
	dsl := replayOverrides{
		dslInitialTokensSet: true,
		dslInitialTokens:    0,
	}
	env := resolvePhaseEnvelope(c, dsl)
	if env.active() {
		t.Fatalf("initial-tokens=0 should disable phase A: %+v", env)
	}
}

func directiveRecorded(directives []string, want string) bool {
	for _, d := range directives {
		if strings.EqualFold(d, want) {
			return true
		}
	}
	return false
}
