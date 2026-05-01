package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConfigEndpointJSONByDefault(t *testing.T) {
	router := NewHandler().Routes()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}
}

func TestConfigEndpointHTMLWhenAcceptHTML(t *testing.T) {
	router := NewHandler().Routes()
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"cllm runtime configuration",
		"Status (read-only)",
		"<h2>Cache</h2>",
		"<h2>DSL</h2>",
		`href="/config?edit=1"`,
		`readonly`, // edit=0 view
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Vestigial sections (managed per-node in nodes.yaml) must NOT be rendered.
	for _, gone := range []string{
		"<h2>Throughput Limits</h2>",
		"<h2>Prefill Simulation</h2>",
		"<h2>Stream Realism</h2>",
	} {
		if strings.Contains(body, gone) {
			t.Fatalf("body still contains vestigial section %q", gone)
		}
	}
	// Save button must NOT be present in read-only view.
	if strings.Contains(body, `id="save-btn"`) {
		t.Fatalf("read-only view should not contain a Save button")
	}
}

func TestConfigEndpointHTMLEditMode(t *testing.T) {
	router := NewHandler().Routes()
	req := httptest.NewRequest(http.MethodGet, "/config?edit=1", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<form method="POST" action="/config"`,
		`id="save-btn"`,
		`<a href="/config">Cancel</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit body missing %q", want)
		}
	}
}

func TestConfigPostFormUpdatesAndRedirects(t *testing.T) {
	handler := NewHandler()
	router := handler.Routes()
	form := url.Values{}
	form.Set("max_tokens", "1234")
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/config" {
		t.Fatalf("location = %q, want /config", loc)
	}
	if got := handler.currentConfig().MaxTokens; got != 1234 {
		t.Fatalf("max_tokens = %d, want 1234", got)
	}
}

func TestConfigPostInvalidReRendersFormWithError(t *testing.T) {
	router := NewHandler().Routes()
	form := url.Values{}
	form.Set("max_tokens", "99999")
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Validation error") {
		t.Fatalf("body missing 'Validation error': %s", body)
	}
	if !strings.Contains(body, `value="99999"`) {
		t.Fatalf("body should echo back submitted value 99999: %s", body)
	}
}

func TestPreferHTML(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"*/*", false},
		{"application/json", false},
		{"text/html", true},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"application/json,text/html", false}, // json listed first
	}
	for _, c := range cases {
		if got := preferHTML(c.accept); got != c.want {
			t.Fatalf("preferHTML(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}
