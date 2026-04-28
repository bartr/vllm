package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// classChatRequest issues a non-streaming chat completion against the
// given handler, optionally setting an X-Workload-Class header and a
// `:dsl workload-class=` directive in the user message.
func classChatRequest(t *testing.T, h http.Handler, header, dslClass string) *httptest.ResponseRecorder {
	t.Helper()
	user := "hello"
	if dslClass != "" {
		user = "hello :dsl workload-class=" + dslClass
	}
	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"` + user + `"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("X-Workload-Class", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestClassHeaderLabelsAdmissionMetric: a request with X-Workload-Class
// must increment cllm_tenant_admissions_total with the matching class
// label.
func TestClassHeaderLabelsAdmissionMetric(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxQueueMs: 500},
	})
	routes := h.Routes()

	if rec := classChatRequest(t, routes, "interactive", ""); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	got := testutil.ToFloat64(h.metrics.tenantAdmissionTotal.WithLabelValues(defaultTenantName, "interactive"))
	if got != 1 {
		t.Fatalf("tenantAdmissionTotal{default,interactive} = %v; want 1", got)
	}
	// Default-class counter must NOT have fired for this request.
	bleed := testutil.ToFloat64(h.metrics.tenantAdmissionTotal.WithLabelValues(defaultTenantName, defaultClassName))
	if bleed != 0 {
		t.Fatalf("tenantAdmissionTotal{default,default} = %v; want 0 (no bleed)", bleed)
	}
}

// TestClassDSLWinsOverHeader: `:dsl workload-class=batch` must beat the
// X-Workload-Class header.
func TestClassDSLWinsOverHeader(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high", MaxQueueMs: 500},
		"batch":       {Priority: "low", MaxQueueMs: 10000},
	})
	routes := h.Routes()

	if rec := classChatRequest(t, routes, "interactive", "batch"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	gotBatch := testutil.ToFloat64(h.metrics.tenantAdmissionTotal.WithLabelValues(defaultTenantName, "batch"))
	if gotBatch != 1 {
		t.Fatalf("tenantAdmissionTotal{default,batch} = %v; want 1 (DSL must win over header)", gotBatch)
	}
	gotHeader := testutil.ToFloat64(h.metrics.tenantAdmissionTotal.WithLabelValues(defaultTenantName, "interactive"))
	if gotHeader != 0 {
		t.Fatalf("tenantAdmissionTotal{default,interactive} = %v; want 0 (header must lose)", gotHeader)
	}
}

// TestClassUnknownHeaderRoutesToDefault: an unknown class header
// resolves to the "default" class label.
func TestClassUnknownHeaderRoutesToDefault(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high"},
	})
	routes := h.Routes()

	if rec := classChatRequest(t, routes, "stranger", ""); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := testutil.ToFloat64(h.metrics.tenantAdmissionTotal.WithLabelValues(defaultTenantName, defaultClassName))
	if got != 1 {
		t.Fatalf("tenantAdmissionTotal{default,default} = %v; want 1 (unknown header → default class)", got)
	}
}

// TestClassRejectionCarriesLabel: a rejection at the tenant rate gate
// must record class on the rejection counter so dashboards can split
// rejections by (tenant, class, reason).
func TestClassRejectionCarriesLabel(t *testing.T) {
	vllm := newTestVLLMServer(t)
	h := NewHandlerWithDependencies(vllm.URL, vllm.Client(), 10, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	h.SetTenants(map[string]TenantConfig{"tight": {Rate: 1, Burst: 5}})
	h.SetClasses(map[string]ClassConfig{"batch": {Priority: "low"}})
	routes := h.Routes()

	body := `{"messages":[{"role":"user","content":"hi :dsl workload-class=batch"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tight")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%q", rec.Code, rec.Body.String())
	}

	got := testutil.ToFloat64(h.metrics.tenantRejectionsTotal.WithLabelValues("tight", "batch", "tenant_rate"))
	if got != 1 {
		t.Fatalf("tenantRejectionsTotal{tight,batch,tenant_rate} = %v; want 1", got)
	}
}

// TestClassNamesIncludesDefault: ClassNames() always lists "default" first.
func TestClassNamesIncludesDefault(t *testing.T) {
	h := NewHandler()
	h.SetClasses(map[string]ClassConfig{
		"interactive": {Priority: "high"},
		"batch":       {Priority: "low"},
	})
	names := h.ClassNames()
	if len(names) == 0 || names[0] != defaultClassName {
		t.Fatalf("ClassNames()[0] = %v; want default first", names)
	}
	hasInteractive, hasBatch := false, false
	for _, n := range names {
		if n == "interactive" {
			hasInteractive = true
		}
		if n == "batch" {
			hasBatch = true
		}
	}
	if !hasInteractive || !hasBatch {
		t.Fatalf("ClassNames missing entries: %v", names)
	}
}
