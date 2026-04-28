package httpapi

import (
	"context"
	"log/slog"
	"time"
)

// phaseEnvelope is the resolved per-request pacing envelope for
// phase-aware token allocation (system-design §14, item 13).
//
// The envelope is computed at admit time from the resolved workload
// class plus any per-request DSL overrides, then threaded through
// `replayOverrides.phase` to the cache replay loop. Phase 13.1
// introduces only the type, the resolver, and the plumbing; later
// phases consume it:
//
//	13.2  cachedReplayDelay reads InitialTokens/InitialTPS/SustainedTPS
//	      and switches rates at the phase boundary.
//	13.4  parseDSL fills DSL override fields below.
//
// A zero-value phaseEnvelope (InitialTokens == 0) means "single-rate"
// — legacy behavior is preserved byte-for-byte.
type phaseEnvelope struct {
	// InitialTokens is the count of phase-A output tokens. 0 means
	// no phase A (sustained-only).
	InitialTokens int
	// InitialTPS is the rate during phase A. 0 means inherit the
	// handler base TPS.
	InitialTPS int
	// SustainedTPS is the rate during phase B. 0 means inherit the
	// handler base TPS.
	SustainedTPS int
	// Source records where the envelope came from for lifecycle
	// events and diagnostics: "class", "dsl", "mixed", or "" when
	// disabled.
	Source string
}

// active reports whether the envelope describes a two-phase request.
// Single-rate (legacy) callers see false.
func (p phaseEnvelope) active() bool { return p.InitialTokens > 0 }

// resolvePhaseEnvelope folds class config and per-request DSL
// overrides into a single envelope. Precedence (matches the design
// doc and mirrors the existing kv-cost / max-queue-ms patterns):
//
//	1. `:dsl no-phase` short-circuits everything to a zero-value
//	   envelope (force single-rate).
//	2. DSL field overrides (`initial-tokens=`, `initial-tps=`,
//	   `sustained-tps=`) overlay the class envelope per-field.
//	3. Resolved class fields, if non-zero.
//	4. Single-rate legacy (zero-value envelope).
//
// The Source string records provenance for diagnostics: "" when
// inactive, "class" when only the class contributed, "dsl" when only
// DSL fields contributed, "mixed" when both did.
func resolvePhaseEnvelope(class *classState, dsl replayOverrides) phaseEnvelope {
	if dsl.noPhase {
		return phaseEnvelope{}
	}

	var classCfg ClassConfig
	if class != nil {
		classCfg = class.config
	}

	classContributed := classCfg.InitialTokens > 0 || classCfg.InitialTPS > 0 || classCfg.SustainedTPS > 0
	dslContributed := dsl.dslInitialTokensSet || dsl.dslInitialTPSOverride > 0 || dsl.dslSustainedTPSOverride > 0

	if !classContributed && !dslContributed {
		return phaseEnvelope{}
	}

	env := phaseEnvelope{
		InitialTokens: classCfg.InitialTokens,
		InitialTPS:    classCfg.InitialTPS,
		SustainedTPS:  classCfg.SustainedTPS,
	}
	if dsl.dslInitialTokensSet {
		env.InitialTokens = dsl.dslInitialTokens
	}
	if dsl.dslInitialTPSOverride > 0 {
		env.InitialTPS = dsl.dslInitialTPSOverride
	}
	if dsl.dslSustainedTPSOverride > 0 {
		env.SustainedTPS = dsl.dslSustainedTPSOverride
	}

	switch {
	case classContributed && dslContributed:
		env.Source = "mixed"
	case dslContributed:
		env.Source = "dsl"
	default:
		env.Source = "class"
	}
	// A class that pins only one of the two rates is allowed (the
	// unset rate inherits handler base TPS at consumption time).
	// A class that pins rates without setting InitialTokens is also
	// allowed — it is effectively a class-scoped sustained-only
	// override and stays single-rate (active() returns false).
	return env
}

// emitPhaseTransition records the one-shot crossing from phase A to
// phase B for a request. Caller is responsible for ensuring the event
// fires at most once per stream. The class label defaults to
// defaultClassName for callers that do not carry the resolved class
// (defense-in-depth; the cache replay path always passes it).
func (h *Handler) emitPhaseTransition(ctx context.Context, class string, env phaseEnvelope, tokensEmitted int, sincePhaseStart time.Duration) {
	if class == "" {
		class = defaultClassName
	}
	const fromPhase = "phase_a"
	const toPhase = "phase_b"
	h.metrics.observePhaseTransition(class, fromPhase, toPhase)
	h.emitLifecycleEvent(ctx, slog.LevelInfo, "phase_transition", "", "phase boundary crossed",
		"class", class,
		"from", fromPhase,
		"to", toPhase,
		"tokens_emitted", tokensEmitted,
		"initial_tokens", env.InitialTokens,
		"initial_tps", env.InitialTPS,
		"sustained_tps", env.SustainedTPS,
		"phase_a_duration_ms", float64(sincePhaseStart)/float64(time.Millisecond),
	)
}

// recordPhaseSummary emits per-stream phase metrics at the end of a
// cache-replay. Called from a defer in replayCachedStream so it fires
// on every exit path. A no-op when the envelope is not active or when
// the stream produced no content.
func (h *Handler) recordPhaseSummary(opts replayOptions, contentEmitted int) {
	env := opts.overrides.phase
	if !env.active() || contentEmitted <= 0 {
		return
	}
	class := opts.class
	if class == "" {
		class = defaultClassName
	}
	phaseATokens := contentEmitted
	if phaseATokens > env.InitialTokens {
		phaseATokens = env.InitialTokens
	}
	phaseBTokens := contentEmitted - phaseATokens
	h.metrics.observePhaseTokens(class, phaseATokens, phaseBTokens)

	// Effective rates after degradation. Using the per-phase
	// configured rate (or the inherited base) ensures the gauge
	// reports what the streamer actually paced at, not the YAML
	// intent.
	effInitial := h.effectiveClassRate(env.InitialTPS)
	effSustained := h.effectiveClassRate(env.SustainedTPS)
	h.metrics.observePhaseEffectiveTPS(class, effInitial, effSustained)

	// Reclaim: design §6/§7 — (initial_tps - sustained_tps) *
	// tokens_in_phase_b. Clamp at zero so the counter never goes
	// backwards when an operator sets sustained > initial (degenerate
	// envelope). We use the configured rates here (not effective)
	// because reclaim measures envelope intent vs. actual tokens
	// emitted in phase B; degradation scales both rates uniformly so
	// the ratio is preserved.
	initialRate := env.InitialTPS
	sustainedRate := env.SustainedTPS
	if initialRate > 0 && sustainedRate > 0 && phaseBTokens > 0 && initialRate > sustainedRate {
		reclaim := float64(initialRate-sustainedRate) * float64(phaseBTokens)
		h.metrics.observePhaseReclaim(class, reclaim)
	}
}

// effectiveClassRate returns the post-degradation tokens-per-second
// the streamer would actually use for `rate`. Mirrors the rate-pick
// logic in cachedReplayDelay so the reported gauge matches the live
// pacing decision. A zero `rate` inherits the handler base TPS.
func (h *Handler) effectiveClassRate(rate int) float64 {
	h.configMu.RLock()
	base := h.maxTokensPerSecond
	maxDeg := h.maxDegradation
	sched := h.scheduler
	h.configMu.RUnlock()

	effRate := rate
	if effRate <= 0 {
		effRate = base
	}
	if effRate <= 0 {
		return 0
	}
	if sched != nil {
		return sched.effectiveTokensPerSecond(effRate, maxDeg)
	}
	return calibratedTokensPerSecond(effRate)
}
