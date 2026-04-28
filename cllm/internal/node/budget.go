package node

import (
	"context"
	"sync"
	"time"
)

// TokenBudget is a semaphore over int64 cost units. Callers Acquire(cost)
// to charge the budget; when total in-flight cost would exceed capacity,
// they block (FIFO, in arrival order) until enough cost is released.
//
// A bounded wait queue prevents unbounded memory growth: once the queue
// holds maxWaiting waiters, additional Acquire calls fail immediately
// with ok=false.
type TokenBudget struct {
	mu         sync.Mutex
	capacity   int64
	inFlight   int64
	maxWaiting int
	queue      []*budgetWaiter
}

type budgetWaiter struct {
	cost   int64
	signal chan struct{}
	done   bool // protected by TokenBudget.mu
}

// NewTokenBudget returns a TokenBudget with the given capacity and maximum
// waiting-queue depth. Capacity is clamped to a minimum of 1; maxWaiting is
// clamped to a minimum of 0.
func NewTokenBudget(capacity int64, maxWaiting int) *TokenBudget {
	if capacity < 1 {
		capacity = 1
	}
	if maxWaiting < 0 {
		maxWaiting = 0
	}
	return &TokenBudget{
		capacity:   capacity,
		maxWaiting: maxWaiting,
	}
}

// Acquire charges cost against the budget. Returns ok=false if the wait
// queue is full or ctx is cancelled before admission. waited is the time
// spent in the queue; 0 if admitted immediately.
func (b *TokenBudget) Acquire(ctx context.Context, cost int64) (waited time.Duration, ok bool) {
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

// Release returns cost to the budget and wakes waiters who can now fit.
func (b *TokenBudget) Release(cost int64) {
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
func (b *TokenBudget) promoteLocked() {
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

// Reconfigure changes capacity and/or wait-queue depth. Already-admitted
// requests keep their slots; if capacity grew, queued waiters may now fit.
func (b *TokenBudget) Reconfigure(capacity int64, maxWaiting int) {
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

// Stats returns a snapshot of current budget state.
func (b *TokenBudget) Stats() (capacity, inFlight int64, waiting, maxWaiting int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity, b.inFlight, len(b.queue), b.maxWaiting
}
