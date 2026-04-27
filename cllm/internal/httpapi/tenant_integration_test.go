package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatRequest issues a non-streaming chat completion against the given
// handler, optionally setting an X-Tenant-Id header. Returns response
// recorder for assertions.
func tenantChatRequest(t *testing.T, h http.Handler, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTenantRateLimitRejects429(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	// Tenant "tight" with very small burst (5 tokens) — request cost
	// will be max_tokens=50 since estimators are cold, far above 5.
	h.SetTenants(map[string]TenantConfig{
		"tight": {Rate: 1, Burst: 5},
	})
	routes := h.Routes()

	rec := tenantChatRequest(t, routes, "tight")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from tenant rate limit, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tenant rate exceeded") {
		t.Fatalf("expected tenant rate body, got %q", rec.Body.String())
	}
}

func TestTenantIsolationDoesNotDrainOthers(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	// "tight" cannot pay; "ample" has plenty.
	h.SetTenants(map[string]TenantConfig{
		"tight": {Rate: 1, Burst: 5},
		"ample": {Rate: 10000, Burst: 100000},
	})
	routes := h.Routes()

	// Drain "tight": first request should be rejected (cost > burst).
	if rec := tenantChatRequest(t, routes, "tight"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tight: expected 429, got %d", rec.Code)
	}
	// "ample" must still succeed.
	if rec := tenantChatRequest(t, routes, "ample"); rec.Code != http.StatusOK {
		t.Fatalf("ample: expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	// Default tenant (no header) must still succeed (rate=0 disables bucket).
	if rec := tenantChatRequest(t, routes, ""); rec.Code != http.StatusOK {
		t.Fatalf("default: expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestTenantUnknownHeaderRoutesToDefault(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	// Configure only "ample"; default has rate=0 (disabled).
	h.SetTenants(map[string]TenantConfig{
		"ample": {Rate: 10000, Burst: 100000},
	})
	routes := h.Routes()

	// Unknown header → routed to default tenant (disabled bucket) → admitted.
	rec := tenantChatRequest(t, routes, "stranger")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown tenant: expected 200 via default, got %d body=%q", rec.Code, rec.Body.String())
	}

	// Invalid characters in header → also routed to default.
	rec = tenantChatRequest(t, routes, "Bad Tenant!")
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid header: expected 200 via default, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestTenantNamesIncludesDefault(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetTenants(map[string]TenantConfig{
		"acme": {Rate: 100, Burst: 1000},
	})
	names := h.TenantNames()
	hasDefault, hasAcme := false, false
	for _, n := range names {
		if n == defaultTenantName {
			hasDefault = true
		}
		if n == "acme" {
			hasAcme = true
		}
	}
	if !hasDefault || !hasAcme {
		t.Fatalf("expected default and acme in TenantNames, got %v", names)
	}
}
