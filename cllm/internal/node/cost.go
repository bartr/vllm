package node

// RequestCost is the conservative token cost we charge a request to the
// admission budget. It is computed before admission from the request payload
// and a p95 completion estimator, then reconciled on completion.
//
//	cost = prompt_tokens + min(max_tokens, p95_completion_tokens)
//
// Bounded by max_tokens so a request asking for 50 always pays <= 50.
//
// Field names use the lowercase-with-camelCase convention exported through
// matching getters; the struct literal form (e.g. RequestCost{TotalCost: 80})
// is preserved for backward compatibility with existing test fixtures.
type RequestCost struct {
	PromptTokens    int
	EstimatedTokens int // the min(max_tokens, p95) component
	TotalCost       int // PromptTokens + EstimatedTokens

	// KVCost is the KV-cache token charge for this request. Defaults
	// to TotalCost; the Phase 4 path (EstimateCostWithKV) replaces
	// this with a per-node KV-estimator-driven prediction when the
	// routed node has KV modeling enabled and its KV estimator is
	// warm. Override paths: `:dsl kv-cost=N` sets it directly,
	// `:dsl no-kv` sets it to -1 (sentinel: skip the KV gate).
	KVCost int

	// Priority is the admission-queue priority for this request
	// (Phase 14C). Higher = preferred when capacity frees up. Defaults
	// to 0 (FIFO equivalent); the handler maps the resolved class /
	// `:dsl priority=` directive to a small integer (low=-1, medium=0,
	// high=+1) and stores it here just before calling
	// scheduler.AcquireOnNode. Pre-Phase-14C call sites that construct
	// RequestCost{} directly stay at 0 \u2014 byte-for-byte FIFO behavior.
	Priority int
}

// EstimateCost computes the admission cost from the primitive inputs.
// estimator may be nil (treated as cold-start: fall back to maxTokens).
//
// maxTokens must already be normalised to a positive value by the caller;
// promptTokens must already include any system-prompt accounting.
func EstimateCost(promptTokens, maxTokens int, estimator *CompletionEstimator) RequestCost {
	return EstimateCostWithKV(promptTokens, maxTokens, estimator, nil, 0)
}

// EstimateCostWithKV is the Phase 4 form of EstimateCost: it accepts an
// optional per-node KV estimator and a completion-factor multiplier so
// KVCost can decouple from TotalCost. When kvEstimator is nil OR cold,
// KVCost falls back to PromptTokens + EstimatedTokens — byte-for-byte
// today's behavior.
//
// kvFactor scales the KV estimator's p95 completion prediction; 0 falls
// back to 1.0 and values are clamped to (0, 4.0]. The factor models
// amortization (e.g., prefix-cache hits) on hardware where peak KV
// residency is below prompt+completion.
func EstimateCostWithKV(promptTokens, maxTokens int, estimator, kvEstimator *CompletionEstimator, kvFactor float64) RequestCost {
	if maxTokens < 1 {
		maxTokens = 1
	}
	estimate := maxTokens
	if estimator != nil {
		if p95, ok := estimator.P95(); ok && p95 < estimate {
			estimate = p95
		}
	}
	totalCost := promptTokens + estimate

	kvCost := totalCost
	if kvEstimator != nil {
		if kvP95, ok := kvEstimator.P95(); ok {
			factor := kvFactor
			if factor <= 0 {
				factor = 1.0
			}
			if factor > 4.0 {
				factor = 4.0
			}
			scaled := int(float64(kvP95) * factor)
			if scaled < 0 {
				scaled = 0
			}
			if scaled > maxTokens {
				scaled = maxTokens
			}
			kvCost = promptTokens + scaled
			if kvCost < promptTokens {
				kvCost = promptTokens
			}
		}
	}

	return RequestCost{
		PromptTokens:    promptTokens,
		EstimatedTokens: estimate,
		TotalCost:       totalCost,
		KVCost:          kvCost,
	}
}
