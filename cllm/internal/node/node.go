package node

// Capacity holds the per-node admission and streaming limits.
type Capacity struct {
	MaxTokensInFlight  int64 `json:"max_tokens_in_flight"`
	MaxWaitingRequests int   `json:"max_waiting_requests"`

	// PerRequestTPS is the per-request decode rate (tokens/second) the
	// node applies during cache-replay pacing. Models real vLLM
	// behavior where a single request decodes at ~32 tok/s regardless
	// of how much aggregate fleet capacity exists.
	//
	// 0 disables per-request pacing (byte-for-byte legacy behavior:
	// the global handler-level max-tokens-per-second / scheduler
	// degradation curve is used instead). Used by `passthrough` style
	// nodes that should add no shaping to upstream traffic.
	PerRequestTPS int `json:"per_request_tokens_per_second"`

	// DegradationThreshold is the per-node concurrent-request count at
	// which per-request rate begins to degrade. Below this, every
	// request paces at PerRequestTPS. Between this and MaxConcurrency,
	// per-request rate falls linearly toward
	// PerRequestTPS * (1 - MaxDegradation/100).
	//
	// 0 disables the soft band (rate is constant = PerRequestTPS up to
	// MaxConcurrency, then queueing).
	DegradationThreshold int `json:"degradation_threshold"`

	// MaxConcurrency is the maximum number of concurrent decoding
	// requests the node will admit. Past this, additional requests
	// queue (up to MaxWaitingRequests) and then 429.
	//
	// 0 disables the request-slot gate (legacy: only the token-cost
	// gate enforces admission). Models a real GPU's batch-slot limit:
	// at MaxConcurrency the per-request rate has fully degraded by
	// MaxDegradation%, and any further request waits for a slot.
	MaxConcurrency int `json:"max_concurrency"`

	// BypassCache, when true, makes every request routed to this node
	// behave as if `:dsl no-cache` were set: the response cache is
	// neither consulted nor written. Used for "real GPU baseline"
	// passthrough lanes (e.g. the `vllm` node) so cache hits from
	// other lanes never contaminate the upstream-only measurement.
	//
	// The bypass is applied immediately after routing by stamping
	// `replayDSL.noCache = true`; downstream behavior (TTFT-budget
	// gate skip, cache-write skip, metrics) follows the existing
	// no-cache path byte-for-byte.
	BypassCache bool `json:"bypass_cache"`

	// MaxKVTokens is the per-node KV-cache occupancy ceiling, in KV
	// tokens. 0 disables the KV admission axis entirely (§10 of
	// docs/spec-n-memory-pressure.md): no charge, no gate, no metrics.
	MaxKVTokens int64 `json:"max_kv_tokens"`

	// KVWeight scales kv_load when computing the combined load fed
	// into f(load). 0 falls back to 1.0 (KV pressure dominates equally
	// with compute pressure once both pass the 10% deadband).
	KVWeight float64 `json:"kv_weight"`

	// KVCompletionFactor scales the KV estimator's p95 completion
	// prediction when computing per-request KVCost (Phase 4 of
	// docs/spec-n-memory-pressure.md). A factor < 1.0 models
	// amortization (prefix cache, mid-stream KV release) on hardware
	// where peak KV residency is below prompt+completion. 0 falls
	// back to 1.0; values are clamped to (0, 4.0]. Only consulted
	// when the node has KV modeling enabled and KVEstimator is warm.
	KVCompletionFactor float64 `json:"kv_completion_factor"`
}

// Degradation holds the per-node f(load) curve parameters.
//
// Phase 1: shape is a fixed piecewise-linear curve in code; only
// MaxDegradation is operator-tunable. Future: pluggable shapes (see §14
// item 2 in docs/system-design.md).
type Degradation struct {
	Shape          string `json:"f_load_shape"`    // "piecewise_linear" for now
	MaxDegradation int    `json:"max_degradation"` // percent, 0-95
}

// Realism holds the per-node opt-in realism knobs (prefill simulation and
// stream perturbations). Mirrors the runtime-tunable knobs documented in
// §7.1 and §7.2 of docs/system-design.md.
type Realism struct {
	PrefillRateMultiplier float64 `json:"prefill_rate_multiplier"`
	PrefillBaseOverheadMs int     `json:"prefill_base_overhead_ms"`
	PrefillJitterPercent  int     `json:"prefill_jitter_percent"`
	PrefillMaxMs          int     `json:"prefill_max_ms"`
	StreamVariabilityPct  int     `json:"stream_variability_percent"`
	StreamJitterPct       int     `json:"stream_jitter_percent"`
	StreamStallProbPct    int     `json:"stream_stall_probability_percent"`
	StreamStallMinMs      int     `json:"stream_stall_min_ms"`
	StreamStallMaxMs      int     `json:"stream_stall_max_ms"`
}

// Upstream describes a Chat Completions API backend a node may pass requests
// through to. A Node with Upstream == nil is purely synthetic; a Node with
// Upstream != nil is a pass-through node (e.g., a real GPU-backed vLLM
// instance).
type Upstream struct {
	URL   string `json:"upstream"`
	Token string `json:"token,omitempty"`
	Model string `json:"model,omitempty"`
}

// Node models a single vLLM-like instance: an admission stock (Budget), a
// rolling completion-token estimator, capacity limits, realism knobs, and
// an optional upstream. A real fleet has N independent vLLM instances each
// with its own scheduler and queue; cLLM models that with N Node values
// inside a single process.
//
// In Phase 1 the handler holds individual fields rather than a Node value;
// this struct exists so Phase 2 can lift those fields into a list of nodes
// without further moves.
type Node struct {
	ID    string
	Class string

	Budget    *TokenBudget
	Estimator *CompletionEstimator

	// KVEstimator is the per-node KV-residency p95 estimator (Phase 4
	// of docs/spec-n-memory-pressure.md). nil when MaxKVTokens == 0.
	// Observes actual completion-token counts on a separate sample
	// stream from Estimator so KV cost can decouple from compute cost
	// (e.g., when the operator sets KVCompletionFactor < 1 to model
	// prefix-cache amortization, or when future per-node calibration
	// data lands). When nil OR cold, KVCost falls back to
	// PromptTokens + EstimatedTokens — byte-for-byte today's behavior.
	KVEstimator *CompletionEstimator

	// KV is the per-node KV-cache occupancy budget. nil when the node
	// has Capacity.MaxKVTokens == 0 (KV modeling disabled). Admission
	// paths must nil-check before consulting it.
	KV *KVBudget

	// Concurrency is the per-node request-slot admission gate. nil
	// when Capacity.MaxConcurrency == 0 (gate disabled). Acquired in
	// addition to (and after) the token-cost Budget; uses cost=1 per
	// request. Queue depth is Capacity.MaxWaitingRequests, shared
	// with the token-cost Budget.
	Concurrency *TokenBudget

	Capacity    Capacity
	Degradation Degradation
	Realism     Realism

	Upstream *Upstream // nil = pure synthetic
}

// New constructs a Node from already-resolved effective configuration. It is
// used by both file loading and runtime CRUD so every node gets identical
// budget, estimator, KV, and concurrency initialization.
func New(id, class string, capacity Capacity, degradation Degradation, realism Realism, upstream *Upstream) *Node {
	n := &Node{
		ID:          id,
		Class:       class,
		Budget:      NewTokenBudget(capacity.MaxTokensInFlight, capacity.MaxWaitingRequests),
		Estimator:   NewCompletionEstimator(256, 50),
		Capacity:    capacity,
		Degradation: degradation,
		Realism:     realism,
		Upstream:    upstream,
	}
	n.Budget.SetAgingStepMs(DefaultPriorityAgingStepMs)
	if capacity.MaxKVTokens > 0 {
		n.KV = NewKVBudget(capacity.MaxKVTokens)
		n.KVEstimator = NewCompletionEstimator(256, 50)
	}
	if capacity.MaxConcurrency > 0 {
		n.Concurrency = NewTokenBudget(int64(capacity.MaxConcurrency), capacity.MaxWaitingRequests)
		n.Concurrency.SetAgingStepMs(DefaultPriorityAgingStepMs)
	}
	return n
}

// PerRequestRate returns the simulated per-request decode rate
// (tokens/second) the node would deliver at the given concurrency
// level. Implements the three-regime vLLM-shaped curve (item 15,
// 0.13.0):
//
//	c <= DegradationThreshold      -> PerRequestTPS
//	DegradationThreshold < c <= MaxConcurrency
//	    -> PerRequestTPS * (1 - MaxDegradation/100 * (c-T)/(M-T))
//	c > MaxConcurrency             -> queueing (caller should not pace)
//
// Returns 0 when per-request pacing is disabled (PerRequestTPS == 0)
// — caller must fall back to the legacy fleet-divided pacer.
func (n *Node) PerRequestRate(concurrency int) float64 {
	if n == nil {
		return 0
	}
	cap := n.Capacity
	if cap.PerRequestTPS <= 0 {
		return 0
	}
	base := float64(cap.PerRequestTPS)
	// Threshold defaults to MaxConcurrency when unset (no soft band).
	threshold := cap.DegradationThreshold
	maxCon := cap.MaxConcurrency
	if threshold <= 0 || threshold > maxCon {
		threshold = maxCon
	}
	if concurrency < 0 {
		concurrency = 0
	}
	if maxCon <= 0 || concurrency <= threshold {
		return base
	}
	// In degradation band: linear from base at threshold to
	// base*(1-maxDeg/100) at maxCon. Clamp at maxCon (caller should
	// have queued past that).
	if concurrency >= maxCon {
		concurrency = maxCon
	}
	maxDeg := n.Degradation.MaxDegradation
	if maxDeg < 0 {
		maxDeg = 0
	}
	if maxDeg > 100 {
		maxDeg = 100
	}
	span := maxCon - threshold
	if span <= 0 {
		return base * (1 - float64(maxDeg)/100)
	}
	frac := float64(concurrency-threshold) / float64(span)
	return base * (1 - float64(maxDeg)/100*frac)
}

// ConcurrentRequests returns the live count of in-flight admitted
// requests on the node's per-request concurrency gate. Returns 0 when
// the gate is disabled (Concurrency == nil); in that case callers
// should not pace by per-request rate.
func (n *Node) ConcurrentRequests() int {
	if n == nil || n.Concurrency == nil {
		return 0
	}
	_, inFlight, _, _ := n.Concurrency.Stats()
	return int(inFlight)
}
