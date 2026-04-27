package httpapi

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCompletionEstimatorColdStart(t *testing.T) {
	e := newCompletionEstimator(10, 5)
	if _, ok := e.p95(); ok {
		t.Fatal("p95 should be unavailable before minSamples")
	}
	for i := 0; i < 4; i++ {
		e.observe(100)
	}
	if _, ok := e.p95(); ok {
		t.Fatal("p95 should still be unavailable at 4 samples")
	}
	e.observe(100)
	if v, ok := e.p95(); !ok || v != 100 {
		t.Fatalf("p95 = (%d, %v); want (100, true)", v, ok)
	}
}

func TestCompletionEstimatorP95Value(t *testing.T) {
	e := newCompletionEstimator(100, 10)
	for i := 1; i <= 100; i++ {
		e.observe(i)
	}
	v, ok := e.p95()
	if !ok {
		t.Fatal("p95 unavailable")
	}
	// p95 of 1..100 is index 95 in 0-indexed sorted slice = value 96.
	if v != 96 {
		t.Fatalf("p95 = %d; want 96", v)
	}
}

func TestCompletionEstimatorRollingWindow(t *testing.T) {
	e := newCompletionEstimator(5, 3)
	for _, v := range []int{1000, 1000, 1000, 1, 1} {
		e.observe(v)
	}
	// After 5 observations, window is full; p95 of {1000,1000,1000,1,1}
	// sorted = {1,1,1000,1000,1000} -> idx int(5*0.95)=4 -> 1000.
	if v, _ := e.p95(); v != 1000 {
		t.Fatalf("p95 with mixed values = %d; want 1000", v)
	}
	// Push three more small values to evict the 1000s.
	e.observe(2)
	e.observe(2)
	e.observe(2)
	// Window now {2,2,2,1,1} -> sorted {1,1,2,2,2} -> idx 4 -> 2.
	if v, _ := e.p95(); v != 2 {
		t.Fatalf("p95 after eviction = %d; want 2", v)
	}
}

func TestEstimateRequestCostColdFallback(t *testing.T) {
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hello world"}},
		MaxTokens: 200,
	}
	cost := estimateRequestCost(payload, nil)
	// No estimator -> fall back to max_tokens.
	if cost.estimatedTokens != 200 {
		t.Fatalf("estimated = %d; want 200", cost.estimatedTokens)
	}
	if cost.totalCost != cost.promptTokens+200 {
		t.Fatalf("totalCost = %d; want %d", cost.totalCost, cost.promptTokens+200)
	}
}

func TestEstimateRequestCostWarmEstimator(t *testing.T) {
	e := newCompletionEstimator(100, 5)
	for i := 0; i < 10; i++ {
		e.observe(50)
	}
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hello"}},
		MaxTokens: 1000,
	}
	cost := estimateRequestCost(payload, e)
	if cost.estimatedTokens != 50 {
		t.Fatalf("estimated = %d; want 50", cost.estimatedTokens)
	}
}

func TestEstimateRequestCostBoundedByMaxTokens(t *testing.T) {
	e := newCompletionEstimator(100, 5)
	for i := 0; i < 10; i++ {
		e.observe(500)
	}
	payload := chatCompletionRequest{
		Messages:  []chatCompletionMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 10,
	}
	cost := estimateRequestCost(payload, e)
	if cost.estimatedTokens != 10 {
		t.Fatalf("estimated = %d; want 10 (max_tokens cap)", cost.estimatedTokens)
	}
}

func TestTokenBudgetImmediateAdmit(t *testing.T) {
	b := newTokenBudget(100, 10)
	waited, ok := b.acquire(context.Background(), 30)
	if !ok || waited != 0 {
		t.Fatalf("immediate admit failed: ok=%v waited=%v", ok, waited)
	}
	_, in, _, _ := b.stats()
	if in != 30 {
		t.Fatalf("inFlight = %d; want 30", in)
	}
}

func TestTokenBudgetBlocksUntilRelease(t *testing.T) {
	b := newTokenBudget(100, 10)
	b.acquire(context.Background(), 80)

	var wg sync.WaitGroup
	wg.Add(1)
	admitted := make(chan time.Duration, 1)
	go func() {
		defer wg.Done()
		w, ok := b.acquire(context.Background(), 30)
		if !ok {
			t.Errorf("blocked acquire failed")
			return
		}
		admitted <- w
	}()

	// Give the goroutine time to enqueue.
	time.Sleep(20 * time.Millisecond)
	_, _, waiting, _ := b.stats()
	if waiting != 1 {
		t.Fatalf("waiting = %d; want 1", waiting)
	}

	b.release(80)
	wg.Wait()
	w := <-admitted
	if w < 10*time.Millisecond {
		t.Fatalf("waited = %v; expected >= 10ms", w)
	}
}

func TestTokenBudgetRejectsWhenQueueFull(t *testing.T) {
	b := newTokenBudget(10, 1)
	// Fill capacity.
	b.acquire(context.Background(), 10)
	// First waiter enqueues.
	enqueued := make(chan struct{})
	go func() {
		close(enqueued)
		b.acquire(context.Background(), 5)
	}()
	<-enqueued
	time.Sleep(10 * time.Millisecond)
	// Second waiter should be rejected.
	if _, ok := b.acquire(context.Background(), 5); ok {
		t.Fatal("acquire should have been rejected (queue full)")
	}
}

func TestTokenBudgetRejectsOversizedRequest(t *testing.T) {
	b := newTokenBudget(100, 10)
	if _, ok := b.acquire(context.Background(), 200); ok {
		t.Fatal("oversized request should reject")
	}
}

func TestTokenBudgetContextCancelInQueue(t *testing.T) {
	b := newTokenBudget(10, 5)
	b.acquire(context.Background(), 10)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := b.acquire(ctx, 5)
		done <- ok
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if ok := <-done; ok {
		t.Fatal("cancelled acquire should not succeed")
	}
	_, _, waiting, _ := b.stats()
	if waiting != 0 {
		t.Fatalf("waiter should be removed; waiting = %d", waiting)
	}
}

func TestTokenBudgetFIFOOrder(t *testing.T) {
	b := newTokenBudget(10, 10)
	b.acquire(context.Background(), 10)

	results := make(chan int, 3)
	for i := 1; i <= 3; i++ {
		i := i
		go func() {
			b.acquire(context.Background(), 5)
			results <- i
		}()
		time.Sleep(5 * time.Millisecond)
	}
	// Release 5 at a time so only one waiter wakes per release; that
	// makes the ordering observable through the result channel.
	b.release(5)
	if got := <-results; got != 1 {
		t.Fatalf("first wake: got %d; want 1", got)
	}
	b.release(5)
	if got := <-results; got != 2 {
		t.Fatalf("second wake: got %d; want 2", got)
	}
	b.release(5)
	if got := <-results; got != 3 {
		t.Fatalf("third wake: got %d; want 3", got)
	}
}

func TestTokenBudgetReconfigureGrowsAdmitsWaiters(t *testing.T) {
	b := newTokenBudget(10, 5)
	b.acquire(context.Background(), 10)

	admitted := make(chan struct{})
	go func() {
		b.acquire(context.Background(), 50)
		close(admitted)
	}()
	time.Sleep(10 * time.Millisecond)
	b.reconfigure(100, 5)
	select {
	case <-admitted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("waiter not admitted after capacity grew")
	}
}

func TestSchedulerObserveFeedsEstimator(t *testing.T) {
	s := newRequestScheduler(10000, 10)
	// Fewer than minSamples (50) observations: estimator stays cold.
	for i := 0; i < 49; i++ {
		s.Observe(42)
	}
	if _, ok := s.Estimator().p95(); ok {
		t.Fatal("estimator should be cold below minSamples")
	}
	s.Observe(42)
	if v, ok := s.Estimator().p95(); !ok || v != 42 {
		t.Fatalf("warm p95 = (%d,%v); want (42,true)", v, ok)
	}
}

func TestSchedulerCostBasedRejectionAndAdmit(t *testing.T) {
	s := newRequestScheduler(100, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rel, _, ok := s.Acquire(ctx, requestCost{totalCost: 80}, "/a")
	if !ok {
		t.Fatal("admit /a")
	}
	// Oversized request rejects immediately even with empty queue.
	if _, _, ok := s.Acquire(ctx, requestCost{totalCost: 200}, "/oversize"); ok {
		t.Fatal("oversize should reject")
	}
	// Cost 30 doesn't fit (80+30>100); blocks until release.
	queued := make(chan bool, 1)
	go func() {
		_, _, ok := s.Acquire(ctx, requestCost{totalCost: 30}, "/b")
		queued <- ok
	}()
	rel()
	if !<-queued {
		t.Fatal("queued /b should admit after release")
	}
}
