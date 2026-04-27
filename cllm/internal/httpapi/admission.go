package httpapi

import (
	"context"
	"sort"
	"sync"
	"time"
)

// requestCost is the conservative token cost we charge a request to the
// admission budget. It is computed before admission from the request payload
// and the global p95 completion estimator, then reconciled on completion.
//
// cost = prompt_tokens + min(max_tokens, p95_completion_tokens)
//
// Bounded by max_tokens so a request asking for 50 always pays <= 50.
type requestCost struct {
	promptTokens     int
	estimatedTokens  int // the min(max_tokens, p95) component
	totalCost        int // promptTokens + estimatedTokens
}

// estimateRequestCost computes the admission cost for a payload. estimator
// may be nil (treated as cold-start: fall back to max_tokens).
func estimateRequestCost(payload chatCompletionRequest, estimator *completionEstimator) requestCost {
	prompt := estimatePromptTokensFromRequest(payload)
	maxTokens := payload.MaxTokens
	if maxTokens < 1 {
		maxTokens = defaultMaxTokens
	}

	estimate := maxTokens
	if estimator != nil {
		if p95, ok := estimator.p95(); ok && p95 < estimate {
			estimate = p95
		}
	}

	return requestCost{
		promptTokens:    prompt,
		estimatedTokens: estimate,
		totalCost:       prompt + estimate,
	}
}

// completionEstimator is a small rolling-window p95 estimator over recently
// observed completion token counts. It is lock-protected and safe for
// concurrent use.
//
// Until the window contains at least minSamples observations, p95 returns
// (0, false) and callers should fall back to a worst-case cost.
type completionEstimator struct {
	mu         sync.Mutex
	window     []int
	cursor     int
	count      int
	maxSamples int
	minSamples int
}

func newCompletionEstimator(maxSamples, minSamples int) *completionEstimator {
	if maxSamples < 1 {
		maxSamples = 256
	}
	if minSamples < 1 {
		minSamples = 50
	}
	return &completionEstimator{
		window:     make([]int, maxSamples),
		maxSamples: maxSamples,
		minSamples: minSamples,
	}
}

// observe records one completed request's actual completion token count.
func (e *completionEstimator) observe(completionTokens int) {
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

// p95 returns the 95th percentile of recent observations. ok is false if
// the estimator has fewer than minSamples observations.
func (e *completionEstimator) p95() (int, bool) {
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

// tokenBudget is a semaphore over int64 cost units. Callers acquire(cost)
// to charge the budget; when total in-flight cost would exceed capacity,
// they block (FIFO, in arrival order) until enough cost is released.
//
// A bounded wait queue prevents unbounded memory growth: once the queue
// holds maxWaiting waiters, additional acquire calls fail immediately
// with ok=false.
type tokenBudget struct {
	mu         sync.Mutex
	capacity   int64
	inFlight   int64
	maxWaiting int
	queue      []*budgetWaiter
}

type budgetWaiter struct {
	cost   int64
	signal chan struct{}
	done   bool // protected by tokenBudget.mu
}

func newTokenBudget(capacity int64, maxWaiting int) *tokenBudget {
	if capacity < 1 {
		capacity = 1
	}
	if maxWaiting < 0 {
		maxWaiting = 0
	}
	return &tokenBudget{
		capacity:   capacity,
		maxWaiting: maxWaiting,
	}
}

// acquire charges cost against the budget. Returns ok=false if the wait
// queue is full or ctx is cancelled before admission. waited is the time
// spent in the queue; 0 if admitted immediately.
func (b *tokenBudget) acquire(ctx context.Context, cost int64) (waited time.Duration, ok bool) {
	if cost < 1 {
		cost = 1
	}
	b.mu.Lock()
	if cost > b.capacity {
		// A single request larger than total capacity can never admit;
		// reject rather than block forever.
		b.mu.Unlock()
		return 0, false
	}
	if b.inFlight+cost <= b.capacity && len(b.queue) == 0 {
		b.inFlight += cost
		b.mu.Unlock()
		return 0, true
	}
	if len(b.queue) >= b.maxWaiting {
		b.mu.Unlock()
		return 0, false
	}
	w := &budgetWaiter{cost: cost, signal: make(chan struct{})}
	b.queue = append(b.queue, w)
	b.mu.Unlock()

	start := time.Now()
	select {
	case <-w.signal:
		return time.Since(start), true
	case <-ctx.Done():
		// We were cancelled. Mark ourselves done and remove from the
		// queue if still present. If we were already woken, refund.
		b.mu.Lock()
		if w.done {
			b.inFlight -= w.cost
			b.promoteLocked()
			b.mu.Unlock()
			return time.Since(start), false
		}
		for i, q := range b.queue {
			if q == w {
				b.queue = append(b.queue[:i], b.queue[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		return time.Since(start), false
	}
}

// release returns cost to the budget and wakes waiters who can now fit.
func (b *tokenBudget) release(cost int64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	b.inFlight -= cost
	if b.inFlight < 0 {
		b.inFlight = 0
	}
	b.promoteLocked()
	b.mu.Unlock()
}

// promoteLocked wakes as many head-of-queue waiters as currently fit.
// Caller must hold b.mu.
func (b *tokenBudget) promoteLocked() {
	for len(b.queue) > 0 {
		head := b.queue[0]
		if b.inFlight+head.cost > b.capacity {
			return
		}
		b.queue = b.queue[1:]
		b.inFlight += head.cost
		head.done = true
		close(head.signal)
	}
}

// reconfigure changes capacity and/or wait-queue depth. Already-admitted
// requests keep their slots; if capacity grew, queued waiters may now fit.
func (b *tokenBudget) reconfigure(capacity int64, maxWaiting int) {
	if capacity < 1 {
		capacity = 1
	}
	if maxWaiting < 0 {
		maxWaiting = 0
	}
	b.mu.Lock()
	b.capacity = capacity
	b.maxWaiting = maxWaiting
	b.promoteLocked()
	b.mu.Unlock()
}

// stats returns a snapshot of current budget state.
func (b *tokenBudget) stats() (capacity, inFlight int64, waiting, maxWaiting int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity, b.inFlight, len(b.queue), b.maxWaiting
}
