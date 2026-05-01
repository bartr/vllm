package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cllm/internal/node"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseDSLMaxQueueMsOverride(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: "hi :dsl max-queue-ms=250"}}, nil)
	if ov.maxQueueMsOverride != 250 {
		t.Fatalf("maxQueueMsOverride = %d; want 250", ov.maxQueueMsOverride)
	}
	found := false
	for _, d := range ov.directives {
		if d == "max-queue-ms=250" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("directives = %v; want max-queue-ms=250 recorded", ov.directives)
	}
}

func TestParseDSLMaxQueueMsRejectsNegative(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: "hi :dsl max-queue-ms=-5"}}, nil)
	if ov.maxQueueMsOverride != 0 {
		t.Fatalf("maxQueueMsOverride = %d; want 0 (negative rejected)", ov.maxQueueMsOverride)
	}
}

// saturateBudget pre-charges all remaining capacity on the handler's
// default node so the next admit must queue. Returns a cleanup that
// releases the charged cost. Does not interact with upstream or the
// HTTP path.
func saturateBudget(t *testing.T, h *Handler) func() {
	t.Helper()
	capacity, inFlight, _, _ := h.scheduler.node.Budget.Stats()
	remaining := capacity - inFlight
	if remaining <= 0 {
		t.Fatalf("budget already saturated: capacity=%d in_flight=%d", capacity, inFlight)
	}
	waited, ok := h.scheduler.node.Budget.Acquire(context.Background(), remaining)
	if !ok {
		t.Fatalf("saturateBudget: pre-charge failed (remaining=%d, waited=%v)", remaining, waited)
	}
	return func() { h.scheduler.node.Budget.Release(remaining) }
}

// TestClassQueueTimeoutRejection: with the budget saturated, a request
// whose class deadline is shorter than how long it would have to wait
// must be rejected with reason=class_queue_timeout and a 429 body of
// "class queue timeout".
func TestClassQueueTimeoutRejection(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxQueueMs: 30},
	})
	routes := h.Routes()

	releaseSat := saturateBudget(t, h)
	defer releaseSat()

	body := `{"messages":[{"role":"user","content":"hi :dsl workload-class=interactive"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	routes.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "class queue timeout") {
		t.Fatalf("expected class queue timeout body, got %q", rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "interactive", "class_queue_timeout"))
	if got != 1 {
		t.Fatalf("rejections{default,interactive,class_queue_timeout} = %v; want 1", got)
	}
	// Sanity: the request actually waited near the deadline. Tolerate
	// schedulers that fire the timeout slightly early or late.
	if elapsed < 15*time.Millisecond {
		t.Fatalf("rejected too early: elapsed=%v (deadline wasn't reached)", elapsed)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("rejected too late: elapsed=%v (deadline didn't fire)", elapsed)
	}
}

// TestClassQueueTimeoutOverrideViaDSL: a `:dsl max-queue-ms=N` directive
// must override the resolved class's MaxQueueMs. Here the default class
// has MaxQueueMs=0 (no cap) and the override forces 30ms.
func TestClassQueueTimeoutOverrideViaDSL(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	routes := h.Routes()

	releaseSat := saturateBudget(t, h)
	defer releaseSat()

	body := `{"messages":[{"role":"user","content":"hi :dsl max-queue-ms=30"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "class queue timeout") {
		t.Fatalf("expected class queue timeout body, got %q", rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, defaultClassName, "class_queue_timeout"))
	if got != 1 {
		t.Fatalf("rejections{default,default,class_queue_timeout} = %v; want 1", got)
	}
}

// TestClassQueueTimeoutNotAppliedWhenZero: with class.MaxQueueMs=0 and
// no DSL override, an unsaturated request must succeed with no
// premature rejection.
func TestClassQueueTimeoutNotAppliedWhenZero(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetClasses(map[string]ClassConfig{
		"unbounded": {Priority: "low", MaxQueueMs: 0},
	})
	routes := h.Routes()
	rec := classChatRequest(t, routes, "unbounded", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "unbounded", "class_queue_timeout"))
	if got != 0 {
		t.Fatalf("rejections{unbounded,class_queue_timeout} = %v; want 0 (no cap)", got)
	}
}

// TestOverCapacityNotMisclassifiedAsClassTimeout: an immediate over-
// capacity rejection (request larger than the entire budget) must keep
// reason=over_capacity even when the class has a deadline. The deadline
// only fires while waiting; immediate rejections never reach Acquire's
// select loop.
func TestOverCapacityNotMisclassifiedAsClassTimeout(t *testing.T) {
	h := NewHandler()
	h.SetRequestProcessingLimits(5, 0) // capacity=5, no waiting room

	cost := node.RequestCost{TotalCost: 9999} // bigger than capacity
	release, _, ok, reason := h.acquireRequestSlotOnNode(context.Background(), cost, "/x", nil)
	if ok {
		release()
		t.Fatalf("expected over-capacity rejection")
	}
	if reason != "over_capacity" {
		t.Fatalf("reason = %q; want over_capacity", reason)
	}
}
