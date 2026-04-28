package httpapi

import (
	"cllm/internal/node"
)

// estimateRequestCost computes the admission cost for a payload. estimator
// may be nil (cold-start: fall back to max_tokens). This wraps
// node.EstimateCost with the httpapi-local prompt-token estimator and
// default-max-tokens normalisation.
func estimateRequestCost(payload chatCompletionRequest, estimator *node.CompletionEstimator) node.RequestCost {
	prompt := estimatePromptTokensFromRequest(payload)
	maxTokens := payload.MaxTokens
	if maxTokens < 1 {
		maxTokens = defaultMaxTokens
	}
	return node.EstimateCost(prompt, maxTokens, estimator)
}
