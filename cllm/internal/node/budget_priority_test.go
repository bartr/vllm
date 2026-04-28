package node

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBudgetPriorityWeightedDequeue confirms that when capacity frees,
// the highest-priority waiter is promoted, not the FIFO head.
//
// Setup: capacity 1, 3 waiters all of cost 1: low, low, high (in arrival
// order). After the initial holder releases, the high-priority waiter
// should win the slot even though it arrived last.
func TestBudgetPriorityWeightedDequeue(t *testing.T) {
	t.Parallel()
	b := NewTokenBudget(1, 8)
	ctx := context.Background()

	// Pin capacity.
	if _, _, ok := b.AcquireWithPriority(ctx, 1, 0); !ok {
		t.Fatal("initial acquire failed")
	}

	type result struct {
		label   string
		waited  time.Duration
		skipped bool
		ok      bool
	}
	results := make(chan result, 3)
	enqueue := func(label string, prio int) {
		go func() {
			waited, skipped, ok := b.AcquireWithPriority(ctx, 1, prio)
			results <- result{label, waited, skipped, ok}
		}()
	}

	enqueue("low-1", -1)
	time.Sleep(5 * time.Millisecond) // ensure deterministic enqueue order
	enqueue("low-2", -1)
	time.Sleep(5 * time.Millisecond)
	enqueue("high", 1)
	time.Sleep(20 * time.Millisecond) // let all three enqueue

	// Free the slot once at a time. Each Release(1) drops inFlight to 0
	// and promoteLocked picks the next best-priority waiter — so a single
	// Release per iteration is enough to thread the three waiters out.
	got := []string{}
	for i := 0; i < 3; i++ {
		b.Release(1)
		select {
		case r := <-results:
			got = append(got, r.label)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for promotion %d (got=%v)", i, got)
		}
	}

	if got[0] != "high" {
		t.Fatalf("first dequeue = %q; want high (priority should win)", got[0])
	}
}

// TestBudgetPrioritySkippedFlag asserts that the waiter promoted out of
// FIFO order observes skipped=true, while the FIFO head observes
// skipped=false.
func TestBudgetPrioritySkippedFlag(t *testing.T) {
	t.Parallel()
	b := NewTokenBudget(1, 8)
	ctx := context.Background()

	if _, _, ok := b.AcquireWithPriority(ctx, 1, 0); !ok {
		t.Fatal("initial acquire failed")
	}

	type result struct {
		label   string
		skipped bool
	}
	out := make(chan result, 2)
	go func() {
		_, sk, _ := b.AcquireWithPriority(ctx, 1, -1)
		out <- result{"low", sk}
	}()
	time.Sleep(10 * time.Millisecond)
	go func() {
		_, sk, _ := b.AcquireWithPriority(ctx, 1, 1)
		out <- result{"high", sk}
	}()
	time.Sleep(20 * time.Millisecond)

	b.Release(1) // promotes high (skipped over low)
	first := <-out
	if first.label != "high" || !first.skipped {
		t.Fatalf("first promote = %+v; want {high, skipped=true}", first)
	}
	b.Release(1) // promotes low (FIFO head, skipped=false)
	second := <-out
	if second.label != "low" || second.skipped {
		t.Fatalf("second promote = %+v; want {low, skipped=false}", second)
	}

	if got := b.PrioritySkips(); got != 1 {
		t.Fatalf("PrioritySkips = %d; want 1", got)
	}
}

// TestBudgetFIFOWithinSamePriority confirms that with all waiters at the
// same priority, ordering remains arrival-order FIFO.
func TestBudgetFIFOWithinSamePriority(t *testing.T) {
	t.Parallel()
	b := NewTokenBudget(1, 8)
	ctx := context.Background()
	if _, _, ok := b.AcquireWithPriority(ctx, 1, 0); !ok {
		t.Fatal("initial acquire failed")
	}

	out := make(chan string, 3)
	enqueue := func(label string) {
		go func() {
			_, _, _ = b.AcquireWithPriority(ctx, 1, 0)
			out <- label
		}()
	}
	for _, name := range []string{"a", "b", "c"} {
		enqueue(name)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)

	// First Release wakes the FIFO head 'a'; second wakes 'b'; third wakes 'c'.
	got := []string{}
	for i := 0; i < 3; i++ {
		b.Release(1)
		got = append(got, <-out)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("FIFO order broken: %v", got)
	}
	if b.PrioritySkips() != 0 {
		t.Fatalf("PrioritySkips = %d; want 0 (no skips)", b.PrioritySkips())
	}
}

// TestBudgetPriorityAgingBoost confirms aging: a long-waiting low waiter
// out-ranks a fresh high waiter once age boost lifts its effective
// priority. Step 30 ms => after ~70 ms, low's eff = -1 + 2 = 1 = high.
// After ~95 ms, low's eff = -1 + 3 = 2 > 1, so low wins.
func TestBudgetPriorityAgingBoost(t *testing.T) {
	t.Parallel()
	b := NewTokenBudget(1, 8)
	b.SetAgingStepMs(30)
	ctx := context.Background()
	if _, _, ok := b.AcquireWithPriority(ctx, 1, 0); !ok {
		t.Fatal("initial acquire failed")
	}

	type result struct {
		label string
	}
	out := make(chan result, 2)
	go func() {
		_, _, _ = b.AcquireWithPriority(ctx, 1, -1)
		out <- result{"low"}
	}()
	time.Sleep(95 * time.Millisecond) // age low past high's static priority
	go func() {
		_, _, _ = b.AcquireWithPriority(ctx, 1, 1)
		out <- result{"high"}
	}()
	time.Sleep(5 * time.Millisecond)

	b.Release(1)
	first := <-out
	if first.label != "low" {
		t.Fatalf("first = %q; want low (aging boost should outrank fresh high)", first.label)
	}
}

// TestBudgetPriorityCancelDoesNotBreakOrdering ensures that a cancelled
// waiter (Phase 14B class_queue_timeout path) is removed cleanly from
// the priority queue without disturbing remaining waiters.
func TestBudgetPriorityCancelDoesNotBreakOrdering(t *testing.T) {
	t.Parallel()
	b := NewTokenBudget(1, 8)
	ctx := context.Background()
	if _, _, ok := b.AcquireWithPriority(ctx, 1, 0); !ok {
		t.Fatal("initial acquire failed")
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = b.AcquireWithPriority(cancelCtx, 1, 1) // high, will cancel
	}()

	time.Sleep(10 * time.Millisecond)
	out := make(chan string, 1)
	go func() {
		_, _, _ = b.AcquireWithPriority(ctx, 1, 0)
		out <- "medium"
	}()
	time.Sleep(10 * time.Millisecond)

	cancel()    // remove the high-pri waiter from the queue
	wg.Wait()
	b.Release(1)

	select {
	case got := <-out:
		if got != "medium" {
			t.Fatalf("got %q after cancel; want medium", got)
		}
	case <-time.After(time.Second):
		t.Fatal("medium waiter never woke after cancel + release")
	}
}
