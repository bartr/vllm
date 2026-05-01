package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cllm/internal/node"
)

func TestNodesEndpointJSONByDefault(t *testing.T) {
	router := NewHandler().Routes()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}
}

func TestNodesEndpointHTMLWhenAcceptHTML(t *testing.T) {
	h := NewHandler()
	// Install two nodes: one synthetic, one upstream-backed, so both
	// branches of the row template are exercised.
	synthetic := &node.Node{
		ID:    "synth",
		Class: "default",
		Capacity: node.Capacity{
			MaxTokensInFlight:  1000,
			MaxWaitingRequests: 8,
			MaxConcurrency:     4,
		},
	}
	upstream := &node.Node{
		ID:    "vllm",
		Class: "passthrough",
		Capacity: node.Capacity{
			BypassCache: true,
		},
		Upstream: &node.Upstream{URL: "http://vllm:8000", Model: "Qwen/Qwen2.5-1.5B-Instruct"},
	}
	h.SetNodes([]*node.Node{synthetic, upstream}, "chained:class_pinned,least_loaded")

	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"cllm nodes",
		"router_policy:",
		"chained:class_pinned,least_loaded",
		`<td class="id">synth</td>`,
		`<td class="id">vllm `,
		"http://vllm:8000",
		"Qwen/Qwen2.5-1.5B-Instruct",
		"synthetic",         // synthetic node upstream cell
		`title="bypass_cache"`, // tag rendered for bypass_cache
		`href="/nodes/synth?edit=1"`, // Edit link per row
		`class="del-btn"`,            // Delete button per row
		`href="/nodes?new=1"`,        // Add button
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestNodesEndpointHTMLDisablesDeleteForLastNode(t *testing.T) {
	// NewHandler installs the implicit single-node fallback, so the
	// list view must render the Delete button as disabled.
	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	NewHandler().Routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Cannot delete the last node") {
		t.Fatalf("expected disabled-delete tooltip\n%s", body)
	}
	if !strings.Contains(body, `class="del-btn" data-node-id=`) {
		t.Fatalf("expected per-row delete button\n%s", body)
	}
}

func TestNodeEndpointHTMLEditForm(t *testing.T) {
	h := NewHandler()
	h.SetNodes([]*node.Node{
		{ID: "cllm", Class: "default", Capacity: node.Capacity{MaxTokensInFlight: 4242}},
		{ID: "vllm", Class: "passthrough"},
	}, "")
	req := httptest.NewRequest(http.MethodGet, "/nodes/cllm?edit=1", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`edit node <code>cllm</code>`,
		`action="/nodes/cllm"`,
		`name="class"`,
		`value="4242"`, // pre-filled max_tokens_in_flight
		`name="bypass_cache"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n%s", want, body)
		}
	}
}

func TestNodesEndpointHTMLNewForm(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/nodes?new=1", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	NewHandler().Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"add node",
		`<input type="text" id="f-id" name="id"`,
		// no fixed action attribute; JS sets it on submit
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/nodes/`) {
		t.Fatalf("new form should not pre-set form action: %s", body)
	}
}

func TestNodesEndpointHTMLNewFormPrefillsFromCLLM(t *testing.T) {
	h := NewHandler()
	h.SetNodes([]*node.Node{
		{
			ID:    "cllm",
			Class: "default",
			Capacity: node.Capacity{
				MaxTokensInFlight:  9999,
				MaxWaitingRequests: 17,
				MaxConcurrency:     5,
				PerRequestTPS:      33,
			},
		},
		{
			ID:       "vllm",
			Class:    "passthrough",
			Upstream: &node.Upstream{URL: "http://vllm:8000", Model: "Qwen"},
		},
	}, "")

	req := httptest.NewRequest(http.MethodGet, "/nodes?new=1", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="class" value="default"`,         // from cllm
		`name="max_tokens_in_flight" value="9999"`,
		`name="max_waiting_requests" value="17"`,
		`name="max_concurrency" value="5"`,
		`name="per_request_tokens_per_second" value="33"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("new form missing prefilled %q\n%s", want, body)
		}
	}
	// Upstream URL/model from the cllm node are intentionally NOT
	// carried into the new-node form — each upstream must be unique.
	if strings.Contains(body, "http://vllm:8000") {
		t.Fatalf("new form should not copy a different node's upstream URL")
	}
}

func TestNodeEndpointFormPostRedirects(t *testing.T) {
	h := NewHandler()
	h.SetNodes([]*node.Node{
		{ID: "cllm", Class: "default"},
		{ID: "extra", Class: "default"},
	}, "")
	form := url.Values{}
	form.Set("class", "newclass")
	form.Set("max_tokens_in_flight", "12345")
	form.Set("bypass_cache", "true")
	req := httptest.NewRequest(http.MethodPost, "/nodes/cllm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/nodes" {
		t.Fatalf("location = %q, want /nodes", loc)
	}
	// Verify the update actually applied.
	got, ok := h.getNodeSnapshot("cllm")
	if !ok {
		t.Fatal("cllm node missing after form post")
	}
	if got.Class != "newclass" {
		t.Fatalf("class = %q, want newclass", got.Class)
	}
	if got.Capacity.MaxTokensInFlight != 12345 {
		t.Fatalf("max_tokens_in_flight = %d, want 12345", got.Capacity.MaxTokensInFlight)
	}
	if !got.Capacity.BypassCache {
		t.Fatal("bypass_cache should be true")
	}
}

func TestNodeEndpointFormPostInvalidReRenders(t *testing.T) {
	h := NewHandler()
	h.SetNodes([]*node.Node{
		{ID: "cllm", Class: "default"},
		{ID: "extra", Class: "default"},
	}, "")
	form := url.Values{}
	form.Set("max_tokens_in_flight", "not-a-number")
	req := httptest.NewRequest(http.MethodPost, "/nodes/cllm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want html (re-rendered form)", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Validation error") {
		t.Fatalf("expected error banner; body=%s", body)
	}
}

func TestNodesEndpointHTMLEmpty(t *testing.T) {
	// Default handler has the implicit single-node fallback installed by
	// NewHandler(); SetNodes with nil should keep it. Confirm the table
	// renders without crashing and the summary line is present.
	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	NewHandler().Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cllm nodes") {
		t.Fatalf("body missing title\n%s", body)
	}
}
