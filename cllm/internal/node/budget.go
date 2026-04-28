package node

import (
	"context"
	"sync"
	"time"
)

// DefaultPriorityAgingStepMs is the default aging tick used by the node
// loader (Phase 14C). Every step of queue time grants the waiter +1 to
// effective priority, so a low-priority request waiting longer than
// 2*step out-ranks a fresh high-priority request and avoids starvation.
// 1000 ms balances "observable in the smoke flow" against "doesn't
// scramble sub-second admission ordering".
const DefaultPriorityAgingStepMs = 1000

// TokenBudget is a semaphore over int64 cost units. Callers Acquire(cost)
// to charge the budget; when total in-flight cost would exceed capacity,
// they block until enough cost is released.
//
// Phase 14C: when a slot frees, the waiter with the highest *effective*
// priority that currently fits is promoted (not strictly the queue head).
// Effective priority = base priority + age boost, where age boost grows by
// +1 every agingStepMs milliseconds spent in the queue. Among ties, the
// earlier-arriving waiter wins (stable). Pure-FIFO behavior is preserved
// when every waiter shares the same priority and aging is disabled
// (agingStepMs == 0).
//
// A bounded wait queue prevents unbounded memory growth: once the queue
// holds maxWaiting waiters, additional Acquire calls fail immediately
// with ok=false.
type TokenBudget struct {
	mu          sync.Mutex
	capacity    int64
	inFlight    int64
	maxWaiting  int
	agingStepMs int
	skipCount   uint64
	queue       []*budgetWaiter
}

type budgetWaiter struct {
	cost       int64
	priority   int
	enqueuedAt time.Time
	signal     chan struct{}
	done       bool // protected by TokenBudget.mu
	skipped    bool // set at promote time if this waiter jumped ahead of others (Phase 14C)
}

// NewTokenBudget returns a TokenBudget with the given capacity and maximum
// waiting-queue depth. Capacity is clamped to a minimum of 1; maxWaiting is
// clamped to a minimum of 0. Aging is disabled (agingStepMs=0) — call
// SetAgingStepMs to enable Phase 14C aging.
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

// SetAgingStepMs configures the priority-aging tick (Phase 14C). When
// stepMs > 0, a waiter's effective priority gains +1 for every stepMs of
// queue time, so a low-priority request waiting long enough eventually
// out-ranks a fresh high-priority request and avoids starvation.
// stepMs <= 0 disables aging (pure base-priority + FIFO ordering).
func (b *TokenBudget) SetAgingStepMs(stepMs int) {
	if stepMs < 0 {
		stepMs = 0
	}
	b.mu.Lock()
	b.agingStepMs = stepMs
	b.mu.Unlock()
}

// Acquire is a backwards-compatible wrapper that admits at the default
// (zero) priority. Pre-Phase-14C callers and tests stay on this entry
// point; new code that needs priority-weighted dequeue should call
// AcquireWithPriority directly.
func (b *TokenBudget) Acquire(ctx context.Context, cost int64) (waited time.Duration, ok bool) {
	waited, _, ok = b.AcquireWithPriority(ctx, cost, 0)
	return waited, ok
}

// AcquireWithPriority charges cost against the budget at the given
// priority (Phase 14C). Higher numeric priority = preferred when a slot
// frees. Returns skipped=true if this waiter was promoted past one or
// more older waiters (used by callers to surface
// `cllm_admission_priority_skips_total`). skipped is meaningless when
// ok=false.
func (b *TokenBudget) AcquireWithPriority(ctx context.Context, cost int64, priority int) (waited time.Duration, skipped bool, ok bool) {
	if cost < 1 {
		cost = 1
	}
	b.mu.Lock()
	if cost > b.capacity {
		// A single request larger than total capacity can never admit;
		// reject rather than block forever.
		b.mu.Unlock()
		return 0, false, false
	}
	if b.inFlight+cost <= b.capacity && len(b.queue) == 0 {
		b.inFlight += cost
		b.mu.Unlock()
		return 0, false, true
	}
	if len(b.queue) >= b.maxWaiting {
		b.mu.Unlock()
		return 0, false, false
	}
	w := &budgetWaiter{
		cost:       cost,
		priority:   priority,
		enqueuedAt: time.Now(),
		signal:     make(chan struct{}),
	}
	b.queue = append(b.queue, w)
	b.mu.Unlock()

	start := time.Now()
	select {
	case <-w.signal:
		b.mu.Lock()
		wasSkipped := w.skipped
		b.mu.Unlock()
		return time.Since(start), wasSkipped, true
	case <-ctx.Done():
		// We were cancelled. Mark ourselves done and remove from the
		// queue if still present. If we were already woken, refund.
		b.mu.Lock()
		if w.done {
			b.inFlight -= w.cost
			b.promoteLocked()
			b.mu.Unlock()
			return time.Since(start), false, false
		}
		for i, q := range b.queue {
			if q == w {
				b.queue = append(b.queue[:i], b.queue[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		return time.Since(start), false, false
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

// promoteLocked wakes the highest-effective-priority waiters that fit
// (Phase 14C). Effective priority = base + waited_ms / agingStepMs (when
// agingStepMs > 0). Within a tier, earliest enqueue time wins (stable).
// When a non-head waiter is chosen, the budget's skipCount counter is
// incremented and the waiter is tagged so the caller can surface a
// metric. Caller must hold b.mu.
func (b *TokenBudget) promoteLocked() {
	now := time.Now()
	for len(b.queue) > 0 {
		bestIdx := -1
		var bestEff int
		for i, w := range b.queue {
			if b.inFlight+w.cost > b.capacity {
				continue
			}
			eff := w.priority
			if b.agingStepMs > 0 {
				waitedMs := int(now.Sub(w.enqueuedAt) / time.Millisecond)
				if waitedMs > 0 {
					eff += waitedMs / b.agingStepMs
				}
			}
			if bestIdx == -1 || eff > bestEff {
				bestIdx = i
				bestEff = eff
			}
		}
		if bestIdx == -1 {
			return
		}
		w := b.queue[bestIdx]
		if bestIdx > 0 {
			w.skipped = true
			b.skipCount++
		}
		b.queue = append(b.queue[:bestIdx], b.queue[bestIdx+1:]...)
		b.inFlight += w.cost
		w.done = true
		close(w.signal)
	}
}

// PrioritySkips returns the cumulative count of out-of-FIFO promotions
// since the budget was created (Phase 14C). Cheap snapshot; safe for
// metric scrapers.
func (b *TokenBudget) PrioritySkips() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.skipCount
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
