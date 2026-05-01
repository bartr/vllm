package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestParseDSLMaxTTFTMsOverride: positive integer parses, set flag is
// true, directive is recorded.
func TestParseDSLMaxTTFTMsOverride(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: "hi :dsl max-ttft-ms=750"}}, nil)
	if !ov.maxTTFTMsSet {
		t.Fatalf("maxTTFTMsSet = false; want true")
	}
	if ov.maxTTFTMsOverride != 750 {
		t.Fatalf("maxTTFTMsOverride = %d; want 750", ov.maxTTFTMsOverride)
	}
	found := false
	for _, d := range ov.directives {
		if d == "max-ttft-ms=750" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("directives = %v; want max-ttft-ms=750 recorded", ov.directives)
	}
	if got := dslDirectiveFamily("max-ttft-ms=750"); got != "max-ttft-ms" {
		t.Fatalf("dslDirectiveFamily = %q; want max-ttft-ms", got)
	}
}

// TestParseDSLMaxTTFTMsZeroIsValid: 0 is meaningful (disable for this
// request), the set flag must still flip so it overrides a non-zero
// class default.
func TestParseDSLMaxTTFTMsZeroIsValid(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: "hi :dsl max-ttft-ms=0"}}, nil)
	if !ov.maxTTFTMsSet {
		t.Fatalf("maxTTFTMsSet = false; want true (0 is meaningful)")
	}
	if ov.maxTTFTMsOverride != 0 {
		t.Fatalf("maxTTFTMsOverride = %d; want 0", ov.maxTTFTMsOverride)
	}
}

// TestParseDSLMaxTTFTMsRejectsNegative: matches the max-queue-ms
// shape — negative values are dropped silently.
func TestParseDSLMaxTTFTMsRejectsNegative(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{{Role: "user", Content: "hi :dsl max-ttft-ms=-5"}}, nil)
	if ov.maxTTFTMsSet {
		t.Fatalf("maxTTFTMsSet = true; want false (negative rejected)")
	}
}

// TestPredictTTFTmsAddsPrefillAndFirstToken: with prefill simulation
// disabled, the prediction equals ceil(1000 / TPS). With prefill on,
// the deterministic prefill term is added.
func TestPredictTTFTmsAddsPrefillAndFirstToken(t *testing.T) {
	t.Parallel()
	h := NewHandler()
	h.SetRequestProcessingLimits(10000, 10)
	h.SetPrefillSimulation(0, 0, 0, 1) // prefill off

	got := h.predictTTFTms(0, replayOverrides{})
	// Item 16 (0.14.0): default fallback Node paces at
	// defaultPerRequestTPS = 32; 1000/32 ~= 32 ms. The legacy global
	// of 100 tps was retired alongside `--max-tokens-per-second`.
	if got < 30 || got > 34 {
		t.Fatalf("predictTTFTms (no prefill, default tps=32) = %d; want ~32", got)
	}

	// tps override uses the override rate as first-token rate.
	got = h.predictTTFTms(0, replayOverrides{tpsOverride: 50})
	if got < 19 || got > 22 {
		t.Fatalf("predictTTFTms (tps=50) = %d; want ~20", got)
	}

	// Phase A initial-tps drives first-token rate when active.
	got = h.predictTTFTms(0, replayOverrides{
		phase: phaseEnvelope{InitialTokens: 100, InitialTPS: 200, SustainedTPS: 50},
	})
	if got < 4 || got > 7 {
		t.Fatalf("predictTTFTms (phase initial-tps=200) = %d; want ~5", got)
	}
}

// TestPredictTTFTmsIncludesPrefill: with prefill enabled, predicted
// TTFT must include a positive prefill component.
func TestPredictTTFTmsIncludesPrefill(t *testing.T) {
	t.Parallel()
	h := NewHandler()
	h.SetRequestProcessingLimits(10000, 10)
	// rateMultiplier=1, baseOverhead=200ms, jitter=0, max=60000ms
	h.SetPrefillSimulation(1.0, 200, 0, 60000)

	withPrefill := h.predictTTFTms(50, replayOverrides{})
	noPrefill := h.predictTTFTms(50, replayOverrides{noPrefill: true})
	if withPrefill <= noPrefill {
		t.Fatalf("predictTTFTms with prefill (%d) must exceed no-prefill (%d)", withPrefill, noPrefill)
	}
	// Base overhead alone is 200 ms; total must exceed that.
	if withPrefill < 200 {
		t.Fatalf("predictTTFTms = %d; expected at least 200 ms (base overhead)", withPrefill)
	}
}

// ttftHandlerWithCache builds a handler whose cache contains a single
// entry keyed off the cleaned user prompt, so a request sending that
// prompt (optionally with DSL directives) produces a cache hit.
func ttftHandlerWithCache(t *testing.T, prompt string) *Handler {
	t.Helper()
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	// Generous capacity so admission never gates these tests.
	h.SetRequestProcessingLimits(100000, 100)
	key, err := buildChatCompletionCacheKey(chatCompletionRequest{
		Messages: []chatCompletionMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		t.Fatalf("buildChatCompletionCacheKey: %v", err)
	}
	h.cache.Add(key, cachedVLLMResponse{
		statusCode:  http.StatusOK,
		body:        []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`),
		contentType: "application/json",
	})
	return h
}

func ttftPostBody(prompt string) string {
	return `{"messages":[{"role":"user","content":"` + prompt + `"}],"max_tokens":8}`
}

// TestClassTTFTBudgetClassRejects: a class with a tiny MaxTTFTMs trips
// on a cached hit when prefill is on; reason=class_ttft_budget; tenant
// is refunded.
func TestClassTTFTBudgetClassRejects(t *testing.T) {
	const prompt = "ping"
	h := ttftHandlerWithCache(t, prompt)
	h.SetPrefillSimulation(1.0, 500, 0, 60000) // 500 ms base overhead alone trips
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxTTFTMs: 50},
	})
	routes := h.Routes()

	body := `{"messages":[{"role":"user","content":"` + prompt + ` :dsl workload-class=interactive"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%q; want 429", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "class ttft budget") {
		t.Fatalf("body = %q; want substring 'class ttft budget'", rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "interactive", "class_ttft_budget"))
	if got != 1 {
		t.Fatalf("rejections{default,interactive,class_ttft_budget} = %v; want 1", got)
	}
}

// TestClassTTFTBudgetDSLOverrideTrips: default class has no cap; the
// DSL override forces a tight cap and trips.
func TestClassTTFTBudgetDSLOverrideTrips(t *testing.T) {
	const prompt = "ping"
	h := ttftHandlerWithCache(t, prompt)
	h.SetPrefillSimulation(1.0, 500, 0, 60000)
	routes := h.Routes()

	body := `{"messages":[{"role":"user","content":"` + prompt + ` :dsl max-ttft-ms=10"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%q; want 429", rec.Code, rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, defaultClassName, "class_ttft_budget"))
	if got != 1 {
		t.Fatalf("rejections{default,default,class_ttft_budget} = %v; want 1", got)
	}
}

// TestClassTTFTBudgetDSLZeroDisablesClass: a class with a tight cap
// can be bypassed for one request via `:dsl max-ttft-ms=0`.
func TestClassTTFTBudgetDSLZeroDisablesClass(t *testing.T) {
	const prompt = "ping"
	h := ttftHandlerWithCache(t, prompt)
	h.SetPrefillSimulation(1.0, 500, 0, 60000)
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxTTFTMs: 1},
	})
	routes := h.Routes()

	body := `{"messages":[{"role":"user","content":"` + prompt + ` :dsl workload-class=interactive max-ttft-ms=0"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q; want 200 (DSL override disabled the cap)", rec.Code, rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "interactive", "class_ttft_budget"))
	if got != 0 {
		t.Fatalf("rejections{default,interactive,class_ttft_budget} = %v; want 0", got)
	}
}

// TestClassTTFTBudgetCacheMissBypass: the gate fires only on cache
// hits. A miss must always go to upstream.
func TestClassTTFTBudgetCacheMissBypass(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetRequestProcessingLimits(100000, 100)
	h.SetPrefillSimulation(1.0, 500, 0, 60000)
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxTTFTMs: 1},
	})
	routes := h.Routes()

	// No cache prime → cache miss → goes upstream regardless of cap.
	body := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"unprimed :dsl workload-class=interactive"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q; want 200 (cache miss must bypass)", rec.Code, rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "interactive", "class_ttft_budget"))
	if got != 0 {
		t.Fatalf("rejections{interactive,class_ttft_budget} = %v; want 0 (cache miss)", got)
	}
}

// TestClassTTFTBudgetNoCacheBypass: `:dsl no-cache` skips both the
// cache lookup and the gate.
func TestClassTTFTBudgetNoCacheBypass(t *testing.T) {
	const prompt = "ping"
	h := ttftHandlerWithCache(t, prompt)
	h.SetPrefillSimulation(1.0, 500, 0, 60000)
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxTTFTMs: 1},
	})
	routes := h.Routes()

	body := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"` + prompt + ` :dsl workload-class=interactive no-cache"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q; want 200 (no-cache must bypass gate)", rec.Code, rec.Body.String())
	}
	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues(defaultTenantName, "interactive", "class_ttft_budget"))
	if got != 0 {
		t.Fatalf("rejections{interactive,class_ttft_budget} = %v; want 0 (no-cache)", got)
	}
}

// TestClassTTFTBudgetTenantRefund: after a class_ttft_budget rejection,
// the tenant bucket is refunded so the quota is not drained.
func TestClassTTFTBudgetTenantRefund(t *testing.T) {
	const prompt = "ping"
	h := ttftHandlerWithCache(t, prompt)
	h.SetPrefillSimulation(1.0, 500, 0, 60000)
	h.SetTenants(map[string]TenantConfig{
		"tight": {Rate: 100, Burst: 100},
	})
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxTTFTMs: 1},
	})
	routes := h.Routes()

	body := `{"messages":[{"role":"user","content":"` + prompt + ` :dsl workload-class=interactive"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tight")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%q; want 429", rec.Code, rec.Body.String())
	}

	tenant := h.tenants.resolve("tight")
	if tenant == nil || tenant.name != "tight" {
		t.Fatalf("tenant 'tight' not registered (resolved=%+v)", tenant)
	}
	_, _, balance := tenant.bucket.snapshot()
	if balance < 99 {
		t.Fatalf("tenant balance = %v; want ~100 after refund (no quota drain)", balance)
	}
}
