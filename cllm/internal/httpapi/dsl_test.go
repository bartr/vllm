package httpapi

import (
	"strings"
	"testing"
	"time"
)

func TestParseDSLNoMarkerLeavesMessagesUnchanged(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hello world"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hello world" {
		t.Fatalf("content = %q, want unchanged", out[0].Content)
	}
	if ov.active() {
		t.Fatalf("expected no overrides; got %v", ov.directives)
	}
}

func TestParseDSLStripsMarkerAndDirectives(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hello :dsl no-jitter segment=50"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hello" {
		t.Fatalf("content = %q, want %q", out[0].Content, "hello")
	}
	if !ov.active() || len(ov.directives) != 2 {
		t.Fatalf("directives = %v, want 2", ov.directives)
	}
	if ov.resolveJitterPercent(15) != 0 {
		t.Fatalf("jitter not zeroed by no-jitter")
	}
	if ov.delayScaleFn == nil {
		t.Fatalf("delayScaleFn missing")
	}
	if got := ov.delayScaleFn(); got != 1.5 {
		t.Fatalf("delayScaleFn() = %v, want 1.5", got)
	}
}

func TestParseDSLNoDelayMacroExpands(t *testing.T) {
	// no-delay is a macro: claims prefill+jitter+variability+stall classes
	// but DOES NOT stop further parsing and DOES NOT disable TPS pacing.
	// Subsequent tps= and segment= directives still apply.
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl no-delay tps=100 segment=50"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hi" {
		t.Fatalf("content = %q", out[0].Content)
	}
	if !ov.noPrefill {
		t.Fatalf("no-delay should set noPrefill")
	}
	if ov.jitterFn == nil || ov.jitterFn(50) != 0 {
		t.Fatalf("no-delay should zero jitter")
	}
	if ov.variabilityFn == nil || ov.variabilityFn(50) != 0 {
		t.Fatalf("no-delay should zero variability")
	}
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("no-delay should zero stall")
	}
	if ov.noTPS {
		t.Fatalf("no-delay should NOT set noTPS")
	}
	if ov.tpsOverride != 100 {
		t.Fatalf("tps=100 should still apply after no-delay, got %d", ov.tpsOverride)
	}
	if ov.delayScaleFn == nil || ov.delayScaleFn() != 1.5 {
		t.Fatalf("segment=50 should still apply after no-delay")
	}
}

func TestParseDSLNoTPSDisablesPacing(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-tps"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if !ov.noTPS {
		t.Fatalf("no-tps should set noTPS")
	}
}

func TestParseDSLNoTPSConflictsWithTPSOverride(t *testing.T) {
	// First-wins: no-tps before tps=N keeps no-tps and ignores tps=.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-tps tps=100"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if !ov.noTPS {
		t.Fatalf("no-tps should win")
	}
	if ov.tpsOverride != 0 {
		t.Fatalf("tps=100 should be ignored, got %d", ov.tpsOverride)
	}
	// Reverse order: tps=N first wins, no-tps ignored.
	in2 := []chatCompletionMessage{{Role: "user", Content: ":dsl tps=100 no-tps"}}
	_, ov2 := parseDSL(in2, func() float64 { return 0 })
	if ov2.noTPS {
		t.Fatalf("no-tps after tps= should be ignored")
	}
	if ov2.tpsOverride != 100 {
		t.Fatalf("tps=100 should win, got %d", ov2.tpsOverride)
	}
}

func TestParseDSLFirstOccurrenceWins(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "x :dsl segment=50 segment=-50 jitter=10 jitter=-30"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	// segment=50 should win over segment=-50 (delay class)
	if ov.delayScaleFn() != 1.5 {
		t.Fatalf("first delay directive should be segment=50, got scale %v", ov.delayScaleFn())
	}
	// jitter=10 should win over jitter=-30
	if ov.resolveJitterPercent(15) != 25 {
		t.Fatalf("jitter = %d, want 25 (15+10)", ov.resolveJitterPercent(15))
	}
}

func TestParseDSLTPSOverride(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl tps=64"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.tpsOverride != 64 {
		t.Fatalf("tps = %d, want 64", ov.tpsOverride)
	}
}

func TestParseDSLRangeUsesDraw(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment=30:50"}}
	// draw=0 -> r=0.5 -> pct = 30 + 0.5*20 = 40 -> 1.4
	_, ov := parseDSL(in, func() float64 { return 0 })
	got := ov.delayScaleFn()
	if got != 1.4 {
		t.Fatalf("scale = %v, want 1.4", got)
	}
}

func TestParseDSLNegativeRange(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment=-50:-30"}}
	// draw=-1 -> r=0 -> pct=lo=-50 -> 1 + (-50/100) = 0.5
	_, ov := parseDSL(in, func() float64 { return -1 })
	got := ov.delayScaleFn()
	if got != 0.5 {
		t.Fatalf("scale = %v, want 0.5", got)
	}
}

func TestParseDSLUnknownTokensIgnored(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl nonsense tps=foo segment=abc no-jitter"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if len(ov.directives) != 1 || ov.directives[0] != "no-jitter" {
		t.Fatalf("directives = %v, want [no-jitter]", ov.directives)
	}
}

func TestParseDSLNoEqualsForm(t *testing.T) {
	// `segment 50` should be equivalent to `segment=50`.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment 50 tps 100"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.delayScaleFn == nil || ov.delayScaleFn() != 1.5 {
		t.Fatalf("expected delay scale 1.5 from `segment 50`")
	}
	if ov.tpsOverride != 100 {
		t.Fatalf("tps = %d, want 100 from `tps 100`", ov.tpsOverride)
	}
}

func TestParseDSLNoEqualsFormWithRange(t *testing.T) {
	// `segment 30:50` with draw=0 -> r=0.5 -> pct=40 -> 1.4
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment 30:50"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if got := ov.delayScaleFn(); got != 1.4 {
		t.Fatalf("scale = %v, want 1.4", got)
	}
}

func TestParseDSLNoEqualsFormSkipsNonNumericNext(t *testing.T) {
	// `jitter` followed by `no-stall` is not a value, so jitter is treated
	// as a no-op and no-stall is processed independently.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl jitter no-stall"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("expected no-stall to apply")
	}
	if ov.jitterFn != nil {
		t.Fatalf("bare `jitter` without value should not set jitterFn")
	}
}

func TestParseDSLMixedSignRange(t *testing.T) {
	// segment=-20:20: lo=-20, hi=20. draw=-1 -> r=0 -> pct=-20 -> 0.8.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment=-20:20"}}
	_, ov := parseDSL(in, func() float64 { return -1 })
	if got := ov.delayScaleFn(); got != 0.8 {
		t.Fatalf("scale = %v, want 0.8", got)
	}
	// draw=1 -> r=1 -> pct=20 -> 1.2.
	_, ov2 := parseDSL(in, func() float64 { return 1 })
	if got := ov2.delayScaleFn(); got != 1.2 {
		t.Fatalf("scale = %v, want 1.2", got)
	}
}

func TestParseDSLNegativeSingleValue(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl jitter=-30"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	// jitter handler value 50 + (-30) = 20.
	if got := ov.resolveJitterPercent(50); got != 20 {
		t.Fatalf("jitter = %d, want 20", got)
	}
}

func TestParseDSLLoHiSwap(t *testing.T) {
	// lo > hi should normalize to lo=20, hi=50.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl segment=50:20"}}
	_, ov := parseDSL(in, func() float64 { return -1 })
	// After swap: lo=20 hi=50; draw=-1 -> r=0 -> pct=20 -> 1.2.
	if got := ov.delayScaleFn(); got != 1.2 {
		t.Fatalf("scale = %v, want 1.2", got)
	}
}

func TestParseDSLAcrossMultipleMessages(t *testing.T) {
	in := []chatCompletionMessage{
		{Role: "system", Content: "be helpful :dsl no-prefill"},
		{Role: "user", Content: "hello :dsl tps=50"},
	}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "be helpful" || out[1].Content != "hello" {
		t.Fatalf("cleaned messages = %+v", out)
	}
	if !ov.noPrefill {
		t.Fatalf("noPrefill not set")
	}
	if ov.tpsOverride != 50 {
		t.Fatalf("tps = %d, want 50", ov.tpsOverride)
	}
}

func TestParseDSLClampsPercentDeltas(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl jitter=200"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.resolveJitterPercent(15) != 100 {
		t.Fatalf("jitter = %d, want clamped to 100", ov.resolveJitterPercent(15))
	}
}

func TestComputePrefillDelayHonorsNoPrefill(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	if d := handler.computePrefillDelay(100, replayOverrides{noPrefill: true}); d != 0 {
		t.Fatalf("delay = %s, want 0 with noPrefill", d)
	}
}

func TestCachedReplayDelayHonorsNoTPS(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	if d := handler.cachedReplayDelay(50, 0, replayOverrides{noTPS: true}); d != 0 {
		t.Fatalf("delay = %s, want 0 with noTPS", d)
	}
}

func TestCachedReplayDelayUsesTPSOverride(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	withDefault := handler.cachedReplayDelay(100, 0, replayOverrides{})
	withOverride := handler.cachedReplayDelay(100, 0, replayOverrides{tpsOverride: 1000})
	if !(withOverride < withDefault) {
		t.Fatalf("override (%s) should be faster than default (%s)", withOverride, withDefault)
	}
}

func TestComputeStreamSegmentDelayAppliesDelayScale(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(0, 0, 0, 0, 0)
	handler.jitterSource = func() float64 { return 0 }

	base, _ := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})
	scaled, _ := handler.computeStreamSegmentDelay(10, 0, replayOverrides{
		delayScaleFn: func() float64 { return 2.0 },
	})
	if scaled != 2*base {
		t.Fatalf("scaled = %s, want 2x of %s", scaled, base)
	}
}

func TestParseDSLLeavesPromptCleanWhenStopped(t *testing.T) {
	in := []chatCompletionMessage{
		{Role: "user", Content: "first :dsl no-delay extra-tokens"},
		{Role: "user", Content: "second :dsl tps=200"},
	}
	out, _ := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "first" {
		t.Fatalf("msg0 = %q", out[0].Content)
	}
	if !strings.HasPrefix(out[1].Content, "second") || strings.Contains(out[1].Content, "tps=200") {
		t.Fatalf("msg1 = %q, want stripped", out[1].Content)
	}
}

func TestComputePrefillDelayAppliesScale(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(2.5, 100, 0, 60000) // disable jitter for determinism
	base := handler.computePrefillDelay(50, replayOverrides{})
	doubled := handler.computePrefillDelay(50, replayOverrides{prefillDurationScale: 2.0})
	want := 2 * base
	// Allow small rounding difference
	if d := doubled - want; d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("doubled = %s, want %s", doubled, want)
	}
}

func TestParseDSLNoCacheCoexistsWithNoDelay(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "x :dsl no-delay no-cache tps=50"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if !ov.noCache {
		t.Fatalf("noCache not set")
	}
	if !ov.noPrefill {
		t.Fatalf("no-delay should set noPrefill")
	}
	// no-delay no longer stops parsing: tps=50 still applies.
	if ov.tpsOverride != 50 {
		t.Fatalf("tps = %d, want 50 (no-delay no longer halts parsing)", ov.tpsOverride)
	}
	if len(ov.directives) != 3 {
		t.Fatalf("directives = %v, want exactly [no-cache, no-delay, tps=50]", ov.directives)
	}
}

func TestParseDSLMaxTokensOverride(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl max-tokens=128"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.maxTokensOverride != 128 {
		t.Fatalf("maxTokensOverride = %d, want 128", ov.maxTokensOverride)
	}
}

func TestParseDSLMaxTokensRejectsBadValue(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl max-tokens=0 max-tokens=foo"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.maxTokensOverride != 0 {
		t.Fatalf("maxTokensOverride = %d, want 0", ov.maxTokensOverride)
	}
}

func TestParseDSLNoCacheStripsFromPrompt(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hello :dsl no-cache"}}
	out, _ := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hello" {
		t.Fatalf("content = %q, want %q", out[0].Content, "hello")
	}
}

// TestParseDSLReCacheSetsFlag verifies re-cache produces a distinct
// override from no-cache: it does NOT set noCache, but DOES set reCache.
func TestParseDSLReCacheSetsFlag(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hello :dsl re-cache"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hello" {
		t.Fatalf("content = %q, want %q", out[0].Content, "hello")
	}
	if ov.noCache {
		t.Fatalf("noCache should be false for re-cache")
	}
	if !ov.reCache {
		t.Fatalf("reCache should be true")
	}
	if !contains(ov.directives, "re-cache") {
		t.Fatalf("directives missing re-cache: %v", ov.directives)
	}
}

// TestParseDSLCacheClassFirstWins verifies no-cache and re-cache share a
// class so a second cache directive is ignored.
func TestParseDSLCacheClassFirstWins(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-cache re-cache"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if !ov.noCache || ov.reCache {
		t.Fatalf("expected noCache only; got noCache=%v reCache=%v", ov.noCache, ov.reCache)
	}
}

// TestParseDSLPrefixedTerminatesAtNewline reproduces a regression: when
// the ask CLI prepended `:dsl ...\n<prompt>` (the bench --dsl path), the
// splitter consumed the prompt body as DSL tokens, leaving an empty
// user message that vLLM responded to with ~54 generic tokens.
func TestParseDSLPrefixedTerminatesAtNewline(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-cache max-tokens=512\nExplain Azure"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "Explain Azure" {
		t.Fatalf("content = %q, want %q", out[0].Content, "Explain Azure")
	}
	if !ov.noCache {
		t.Fatalf("noCache = false, want true")
	}
	if ov.maxTokensOverride != 512 {
		t.Fatalf("maxTokensOverride = %d, want 512", ov.maxTokensOverride)
	}
}

// TestParseDSLPrefixedNoBody covers the degenerate case of a `:dsl …`
// line with no following prompt text.
func TestParseDSLPrefixedNoBody(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-cache"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "" {
		t.Fatalf("content = %q, want empty", out[0].Content)
	}
	if !ov.noCache {
		t.Fatalf("noCache = false, want true")
	}
}

func TestParseDSLProfileExpandsBundle(t *testing.T) {
	profiles := map[string][]string{"interactive": {"no-stall", "no-jitter"}}
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl profile=interactive"}}
	_, ov := parseDSLWithProfiles(in, func() float64 { return 0 }, profiles)
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("expected no-stall override from profile")
	}
	if ov.jitterFn == nil || ov.jitterFn(50) != 0 {
		t.Fatalf("expected no-jitter override from profile")
	}
	if !contains(ov.directives, "profile=interactive") {
		t.Fatalf("directives missing profile marker: %v", ov.directives)
	}
}

func TestParseDSLProfileLowerPrecedenceThanExplicit(t *testing.T) {
	profiles := map[string][]string{"interactive": {"no-stall", "no-jitter"}}
	// Explicit tps=50 should be preserved alongside interactive's no-stall/no-jitter.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl tps=50 profile=interactive"}}
	_, ov := parseDSLWithProfiles(in, func() float64 { return 0 }, profiles)
	if ov.tpsOverride != 50 {
		t.Fatalf("tpsOverride = %d, want 50 (explicit wins over profile)", ov.tpsOverride)
	}
	// But other parts of the profile bundle should still apply.
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("expected no-stall override from profile baseline")
	}
}

func TestParseDSLProfileSurvivesNoDelay(t *testing.T) {
	profiles := map[string][]string{"batch": {"no-prefill", "no-jitter", "no-variability", "no-stall"}}
	// no-delay no longer halts parsing, so the profile is recorded normally
	// and its bundle still expands afterward.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl no-delay profile=batch"}}
	_, ov := parseDSLWithProfiles(in, func() float64 { return 0 }, profiles)
	if !ov.noPrefill {
		t.Fatalf("no-delay should set noPrefill")
	}
	if !contains(ov.directives, "profile=batch") {
		t.Fatalf("directives missing batch profile: %v", ov.directives)
	}
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("expected no-stall override from batch profile")
	}
}

func TestParseDSLUnknownProfileIgnored(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl profile=does-not-exist tps=77"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.tpsOverride != 77 {
		t.Fatalf("tpsOverride = %d, want 77", ov.tpsOverride)
	}
	for _, d := range ov.directives {
		if strings.HasPrefix(d, "profile=") {
			t.Fatalf("unknown profile should not be recorded, got %v", ov.directives)
		}
	}
}

func TestParseDSLProfileFirstWinsAcrossProfiles(t *testing.T) {
	profiles := map[string][]string{
		"interactive": {"no-stall", "no-jitter"},
		"stall-heavy": {"stall=50", "jitter=30", "variability=20"},
	}
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl profile=interactive profile=stall-heavy"}}
	_, ov := parseDSLWithProfiles(in, func() float64 { return 0 }, profiles)
	// interactive sets stall to 0; stall-heavy would bump it. First wins.
	if ov.stallFn == nil || ov.stallFn(50) != 0 {
		t.Fatalf("expected interactive's no-stall to win over stall-heavy")
	}
	if !contains(ov.directives, "profile=interactive") {
		t.Fatalf("first profile not recorded: %v", ov.directives)
	}
	if contains(ov.directives, "profile=stall-heavy") {
		t.Fatalf("second profile should be ignored: %v", ov.directives)
	}
}

func TestParseDSLWithProfilesCustomMap(t *testing.T) {
	custom := map[string][]string{"slow": {"tps=5", "stall=40"}}
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl profile=slow"}}
	_, ov := parseDSLWithProfiles(in, func() float64 { return 0 }, custom)
	if ov.tpsOverride != 5 {
		t.Fatalf("tpsOverride = %d, want 5", ov.tpsOverride)
	}
	if ov.stallFn == nil || ov.stallFn(0) != 40 {
		t.Fatalf("expected stall=40 from custom profile")
	}
	// Built-in 'interactive' should NOT be available.
	in2 := []chatCompletionMessage{{Role: "user", Content: ":dsl profile=interactive"}}
	_, ov2 := parseDSLWithProfiles(in2, func() float64 { return 0 }, custom)
	if ov2.tpsOverride != 0 {
		t.Fatalf("tpsOverride = %d, want 0 (interactive not in custom map)", ov2.tpsOverride)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// timeUnused keeps the time import alive when other helpers don't reference it.
var _ = time.Now

func TestDSLDirectiveFamily(t *testing.T) {
	cases := map[string]string{
		"no-cache":            "no-cache",
		"no-delay":            "no-delay",
		"no-prefill":          "no-prefill",
		"tps=512":             "tps",
		"tps=1":               "tps",
		"max-tokens=64":       "max-tokens",
		"jitter=25":           "jitter",
		"jitter=-30":          "jitter",
		"variability=10":      "variability",
		"stall=5":             "stall",
		"prefill=200":         "prefill",
		"segment=30":          "segment",
		"segment=-50":         "segment",
		"segment=30:50":       "segment",
		"segment=-20:20":      "segment",
		"+30":                 "other",
		"-50":                 "other",
		"profile=interactive": "profile=interactive",
		"profile=batch":       "profile=batch",
		"":                    "",
		"garbage":             "other",
	}
	for in, want := range cases {
		if got := dslDirectiveFamily(in); got != want {
			t.Errorf("dslDirectiveFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDSLFamiliesEmptyReturnsNoneBaseline(t *testing.T) {
	got := dslFamilies(nil)
	if len(got) != 1 || got[0] != "none" {
		t.Fatalf("dslFamilies(nil) = %v, want [none]", got)
	}
}

func TestDSLFamiliesDeduplicates(t *testing.T) {
	// tps=512 and tps=100 collapse to the same family.
	got := dslFamilies([]string{"tps=512", "tps=100", "no-cache", "no-cache"})
	if len(got) != 2 || got[0] != "tps" || got[1] != "no-cache" {
		t.Fatalf("dslFamilies = %v, want [tps no-cache]", got)
	}
}

func TestParseDSLMarkerIsCaseInsensitive(t *testing.T) {
	for _, marker := range []string{":dsl", ":DSL", ":Dsl", ":dSl"} {
		in := []chatCompletionMessage{{Role: "user", Content: "hello " + marker + " no-delay"}}
		out, ov := parseDSL(in, func() float64 { return 0 })
		if out[0].Content != "hello" {
			t.Fatalf("marker %q: cleaned content = %q, want %q", marker, out[0].Content, "hello")
		}
		if !ov.noPrefill {
			t.Fatalf("marker %q: no-delay should set noPrefill", marker)
		}
	}
}

func TestParseDSLWithDefaultProfileAppliesWhenNoMarker(t *testing.T) {
	profiles := map[string][]string{"slow": {"tps=5", "stall=40"}}
	in := []chatCompletionMessage{{Role: "user", Content: "hello world"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "slow")
	if ov.tpsOverride != 5 {
		t.Fatalf("tpsOverride = %d, want 5 from default profile", ov.tpsOverride)
	}
	if !contains(ov.directives, "profile=slow") {
		t.Fatalf("expected profile=slow in directives, got %v", ov.directives)
	}
}

func TestParseDSLWithDefaultProfileSuppressedByExplicitMarker(t *testing.T) {
	profiles := map[string][]string{"slow": {"tps=5"}}
	// Even an empty :dsl marker should suppress the default profile.
	in := []chatCompletionMessage{{Role: "user", Content: "hello :dsl no-cache"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "slow")
	if ov.tpsOverride != 0 {
		t.Fatalf("tpsOverride = %d, want 0 (default suppressed)", ov.tpsOverride)
	}
	if contains(ov.directives, "profile=slow") {
		t.Fatalf("default profile should not have been applied: %v", ov.directives)
	}
}

func TestParseDSLWithDefaultProfileExplicitProfileWins(t *testing.T) {
	profiles := map[string][]string{
		"slow": {"tps=5"},
		"fast": {"tps=99"},
	}
	in := []chatCompletionMessage{{Role: "user", Content: "hi :dsl profile=fast"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "slow")
	if ov.tpsOverride != 99 {
		t.Fatalf("tpsOverride = %d, want 99 from explicit profile=fast", ov.tpsOverride)
	}
	if contains(ov.directives, "profile=slow") {
		t.Fatalf("default profile should be overridden: %v", ov.directives)
	}
}

func TestParseDSLWithDefaultProfilePromptDirectiveWinsFirst(t *testing.T) {
	// Prompt sets tps=7; default profile would set tps=5. Default profile is
	// still applied (records profile=slow), but tps stays at 7.
	profiles := map[string][]string{"slow": {"tps=5", "stall=40"}}
	in := []chatCompletionMessage{{Role: "user", Content: "hi"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "slow")
	if ov.tpsOverride != 5 {
		t.Fatalf("tpsOverride = %d, want 5 from default profile", ov.tpsOverride)
	}
	// Sanity: stall also expanded from the bundle.
	if ov.stallFn == nil || ov.stallFn(0) != 40 {
		t.Fatalf("expected stall=40 from default profile bundle")
	}
}

func TestParseDSLWithDefaultProfileEmptyName(t *testing.T) {
	profiles := map[string][]string{"slow": {"tps=5"}}
	in := []chatCompletionMessage{{Role: "user", Content: "hi"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "")
	if ov.tpsOverride != 0 {
		t.Fatalf("tpsOverride = %d, want 0 (no default)", ov.tpsOverride)
	}
	if len(ov.directives) != 0 {
		t.Fatalf("expected no directives, got %v", ov.directives)
	}
}

func TestParseDSLWithDefaultProfileUnknownNameIgnored(t *testing.T) {
	profiles := map[string][]string{"slow": {"tps=5"}}
	in := []chatCompletionMessage{{Role: "user", Content: "hi"}}
	_, ov := parseDSLWithDefaultProfile(in, func() float64 { return 0 }, profiles, "does-not-exist")
	if ov.tpsOverride != 0 {
		t.Fatalf("tpsOverride = %d, want 0 (unknown default)", ov.tpsOverride)
	}
}

func TestParseDSLNodePin(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: "hello :dsl node=h100-0"}}
	out, ov := parseDSL(in, func() float64 { return 0 })
	if out[0].Content != "hello" {
		t.Fatalf("content = %q, want %q", out[0].Content, "hello")
	}
	if ov.nodeID != "h100-0" {
		t.Fatalf("nodeID = %q, want h100-0", ov.nodeID)
	}
	if ov.nodeClass != "" {
		t.Fatalf("nodeClass = %q, want empty", ov.nodeClass)
	}
	if !contains(ov.directives, "node=h100-0") {
		t.Fatalf("directives = %v, want contains node=h100-0", ov.directives)
	}
}

func TestParseDSLNodeClassPin(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl node-class=A10"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.nodeClass != "a10" {
		t.Fatalf("nodeClass = %q, want a10 (lowercased)", ov.nodeClass)
	}
	if ov.nodeID != "" {
		t.Fatalf("nodeID = %q, want empty", ov.nodeID)
	}
}

func TestParseDSLNodeIDWinsOverClass(t *testing.T) {
	// Both in same prompt; document order is node-class first then
	// node= — first-wins means node-class= sticks. Reverse order
	// should make node= win.
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl node=h100-0 node-class=A10"}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.nodeID != "h100-0" {
		t.Fatalf("nodeID = %q, want h100-0", ov.nodeID)
	}
	if ov.nodeClass != "" {
		t.Fatalf("nodeClass = %q, want empty (node= seen first)", ov.nodeClass)
	}

	in2 := []chatCompletionMessage{{Role: "user", Content: ":dsl node-class=A10 node=h100-0"}}
	_, ov2 := parseDSL(in2, func() float64 { return 0 })
	if ov2.nodeClass != "a10" {
		t.Fatalf("nodeClass = %q, want a10", ov2.nodeClass)
	}
	if ov2.nodeID != "" {
		t.Fatalf("nodeID = %q, want empty (node-class= seen first)", ov2.nodeID)
	}
}

func TestParseDSLNodeEmptyValueIgnored(t *testing.T) {
	in := []chatCompletionMessage{{Role: "user", Content: ":dsl node= node-class="}}
	_, ov := parseDSL(in, func() float64 { return 0 })
	if ov.nodeID != "" || ov.nodeClass != "" {
		t.Fatalf("expected both empty, got id=%q class=%q", ov.nodeID, ov.nodeClass)
	}
}

