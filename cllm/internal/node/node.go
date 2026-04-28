package node

// Capacity holds the per-node admission and streaming limits.
type Capacity struct {
	MaxTokensInFlight  int64
	MaxTokensPerSecond int
	MaxWaitingRequests int

	// MaxKVTokens is the per-node KV-cache occupancy ceiling, in KV
	// tokens. 0 disables the KV admission axis entirely (§10 of
	// docs/design-memory-pressure.md): no charge, no gate, no metrics.
	MaxKVTokens int64

	// KVWeight scales kv_load when computing the combined load fed
	// into f(load). 0 falls back to 1.0 (KV pressure dominates equally
	// with compute pressure once both pass the 10% deadband).
	KVWeight float64
}

// Degradation holds the per-node f(load) curve parameters.
//
// Phase 1: shape is a fixed piecewise-linear curve in code; only
// MaxDegradation is operator-tunable. Future: pluggable shapes (see §14
// item 2 in system-design.md).
type Degradation struct {
	Shape          string // "piecewise_linear" for now
	MaxDegradation int    // percent, 0-95
}

// Realism holds the per-node opt-in realism knobs (prefill simulation and
// stream perturbations). Mirrors the runtime-tunable knobs documented in
// §7.1 and §7.2 of system-design.md.
type Realism struct {
	PrefillRateMultiplier   float64
	PrefillBaseOverheadMs   int
	PrefillJitterPercent    int
	PrefillMaxMs            int
	StreamVariabilityPct    int
	StreamJitterPct         int
	StreamStallProbPct      int
	StreamStallMinMs        int
	StreamStallMaxMs        int
}

// Upstream describes an OpenAI-compatible backend a node may pass requests
// through to. A Node with Upstream == nil is purely synthetic; a Node with
// Upstream != nil is a pass-through node (e.g., a real GPU-backed vLLM
// instance).
type Upstream struct {
	URL   string
	Token string
	Model string
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

	// KV is the per-node KV-cache occupancy budget. nil when the node
	// has Capacity.MaxKVTokens == 0 (KV modeling disabled). Admission
	// paths must nil-check before consulting it.
	KV *KVBudget

	Capacity    Capacity
	Degradation Degradation
	Realism     Realism

	Upstream *Upstream // nil = pure synthetic
}
