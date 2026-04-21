package httpapi

import (
	"bytes"
	"cplane/internal/config"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100).Routes()

	tests := []struct {
		name       string
		method     string
		path       string
		statusCode int
		body       string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz", statusCode: http.StatusOK, body: "ok\n"},
		{name: "readyz", method: http.MethodGet, path: "/readyz", statusCode: http.StatusOK, body: "ready\n"},
		{name: "ask", method: http.MethodGet, path: "/ask?q=success", statusCode: http.StatusOK, body: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"success"}}]}`},
		{name: "ask example query", method: http.MethodGet, path: "/ask?q=what%20is%20the%20capital%20of%20Texas%3F", statusCode: http.StatusOK, body: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"what is the capital of Texas?"}}]}`},
		{name: "ask missing q", method: http.MethodGet, path: "/ask", statusCode: http.StatusBadRequest, body: "missing q\n"},
		{name: "ask method not allowed", method: http.MethodPost, path: "/ask", statusCode: http.StatusMethodNotAllowed, body: "Method Not Allowed\n"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			if recorder.Code != test.statusCode {
				t.Fatalf("status code = %d, want %d", recorder.Code, test.statusCode)
			}

			if recorder.Body.String() != test.body {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.body)
			}
		})
	}
}

func TestRoutesRequestLogging(t *testing.T) {
	var logBuffer bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logBuffer)
	defer log.SetOutput(originalOutput)

	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=success", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=%20%20Success%20%20", nil))

	logLine := logBuffer.String()
	for _, want := range []string{
		"method=GET",
		"path=/ask?q=success",
		"status=200",
		"bytes=114",
		"cache=false",
		"cache=true",
	} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log line %q does not contain %q", logLine, want)
		}
	}

	matched, err := regexp.MatchString(`duration_ms=\d+\.\d{2}`, logLine)
	if err != nil {
		t.Fatalf("match duration regex: %v", err)
	}
	if !matched {
		t.Fatalf("log line %q does not contain duration_ms with two decimals", logLine)
	}
}

func TestAskCachesNormalizedQueries(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100).Routes()

	firstRequest := httptest.NewRequest(http.MethodGet, "/ask?q=What%20is%20the%20capital%20of%20Texas%3F", nil)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodGet, "/ask?q=%20%20what%20is%20%20%20the%20capital%20of%20texas?%20%20", nil)
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusOK)
	}
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if firstRecorder.Body.String() != secondRecorder.Body.String() {
		t.Fatalf("cached body = %q, want %q", secondRecorder.Body.String(), firstRecorder.Body.String())
	}
	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
	if got := counters.chat.Load(); got != 1 {
		t.Fatalf("chat requests = %d, want 1", got)
	}
}

func TestLoadConfigCacheSize(t *testing.T) {
	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SIZE", "123")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 123 {
		t.Fatalf("CacheSize = %d, want 123", cfg.CacheSize)
	}
}

func newTestVLLMServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var requestBody map[string]any
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			messages, ok := requestBody["messages"].([]any)
			if !ok || len(messages) < 2 {
				t.Fatalf("messages = %#v, want at least two messages", requestBody["messages"])
			}

			userMessage, ok := messages[1].(map[string]any)
			if !ok {
				t.Fatalf("user message = %#v, want map", messages[1])
			}

			content, _ := userMessage["content"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

type vllmCounters struct {
	chat   atomic.Int64
	models atomic.Int64
}

func newCountingTestVLLMServer(t *testing.T) (*httptest.Server, *vllmCounters) {
	t.Helper()

	counters := &vllmCounters{}
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			counters.models.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			counters.chat.Add(1)
			var requestBody map[string]any
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			messages, ok := requestBody["messages"].([]any)
			if !ok || len(messages) < 2 {
				t.Fatalf("messages = %#v, want at least two messages", requestBody["messages"])
			}

			userMessage, ok := messages[1].(map[string]any)
			if !ok {
				t.Fatalf("user message = %#v, want map", messages[1])
			}

			content, _ := userMessage["content"].(string)
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))

	return server, counters
}
