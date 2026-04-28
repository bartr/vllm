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

	// KVCost is the KV-cache token charge for this request. In v1 it
	// mirrors TotalCost (§4.1 of docs/design-memory-pressure.md); a
	// future per-node KV estimator or a :dsl kv-cost= directive can
	// override it without changing the call sites that construct
	// RequestCost via EstimateCost.
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
	if maxTokens < 1 {
		maxTokens = 1
	}
	estimate := maxTokens
	if estimator != nil {
		if p95, ok := estimator.P95(); ok && p95 < estimate {
			estimate = p95
		}
	}
	return RequestCost{
		PromptTokens:    promptTokens,
		EstimatedTokens: estimate,
		TotalCost:       promptTokens + estimate,
		KVCost:          promptTokens + estimate,
	}
}
