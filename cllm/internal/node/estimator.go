package node

import (
	"sort"
	"sync"
)

// CompletionEstimator is a small rolling-window p95 estimator over recently
// observed completion token counts. It is lock-protected and safe for
// concurrent use.
//
// Until the window contains at least minSamples observations, P95 returns
// (0, false) and callers should fall back to a worst-case cost.
type CompletionEstimator struct {
	mu         sync.Mutex
	window     []int
	cursor     int
	count      int
	maxSamples int
	minSamples int
}

// NewCompletionEstimator returns a CompletionEstimator with the given
// rolling-window size and warm-up minimum. Defaults: maxSamples=256,
// minSamples=50.
func NewCompletionEstimator(maxSamples, minSamples int) *CompletionEstimator {
	if maxSamples < 1 {
		maxSamples = 256
	}
	if minSamples < 1 {
		minSamples = 50
	}
	return &CompletionEstimator{
		window:     make([]int, maxSamples),
		maxSamples: maxSamples,
		minSamples: minSamples,
	}
}

// Observe records one completed request's actual completion token count.
func (e *CompletionEstimator) Observe(completionTokens int) {
	if completionTokens < 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.window[e.cursor] = completionTokens
	e.cursor = (e.cursor + 1) % e.maxSamples
	if e.count < e.maxSamples {
		e.count++
	}
}

// P95 returns the 95th percentile of recent observations. ok is false if
// the estimator has fewer than minSamples observations.
func (e *CompletionEstimator) P95() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.count < e.minSamples {
		return 0, false
	}
	snapshot := make([]int, e.count)
	copy(snapshot, e.window[:e.count])
	sort.Ints(snapshot)
	idx := int(float64(e.count) * 0.95)
	if idx >= e.count {
		idx = e.count - 1
	}
	return snapshot[idx], true
}
