package httpapi

import (
	"bytes"
	"cllm/internal/config"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"strings"
	"testing"
	"time"
)

func TestRoutes(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: "You are a detailed assistant.", maxTokens: 2500, temperature: defaultTemperature}).Routes()

	tests := []struct {
		name       string
		method     string
		path       string
		statusCode int
		body       string
		bodyContains []string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz", statusCode: http.StatusOK, body: "ok\n"},
		{name: "readyz", method: http.MethodGet, path: "/readyz", statusCode: http.StatusOK, body: "ready\n"},
		{name: "config", method: http.MethodGet, path: "/config", statusCode: http.StatusOK, bodyContains: []string{`"cache_size":100`, `"cache_entries":0`, `"downstream_url":"` + vllmServer.URL + `"`, `"downstream_model":""`, `"system_prompt":"You are a detailed assistant."`, `"max_tokens":2500`, `"max_tokens_per_second":32`, `"effective_tokens_per_second":32`, `"max_concurrent_requests":512`, `"max_waiting_requests":1023`, `"waiting_requests":0`, `"max_degradation":10`, `"computed_degradation_percentage":0`, `"temperature":0.2`, `"stream":false`}},
		{name: "models", method: http.MethodGet, path: "/v1/models", statusCode: http.StatusOK, body: `{"data":[{"id":"test-model"}]}`},
		{name: "ask", method: http.MethodGet, path: "/ask?q=success", statusCode: http.StatusOK, body: `{"cache":false,"choices":[{"message":{"content":"success","role":"assistant"}}],"id":"chatcmpl-test","object":"chat.completion"}`},
		{name: "ask stream", method: http.MethodGet, path: "/ask?q=success&stream=true", statusCode: http.StatusOK, body: "data: {\"cache\":false,\"choices\":[{\"delta\":{\"content\":\"success\"},\"finish_reason\":null,\"index\":0}],\"created\":123,\"id\":\"chatcmpl-test-stream\",\"model\":\"test-model\",\"object\":\"chat.completion.chunk\"}\n\ndata: {\"cache\":false,\"choices\":[],\"created\":123,\"id\":\"chatcmpl-test-stream\",\"model\":\"test-model\",\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":1,\"prompt_tokens\":5,\"total_tokens\":6}}\n\ndata: [DONE]\n\n"},
		{name: "ask example query", method: http.MethodGet, path: "/ask?q=what%20is%20the%20capital%20of%20Texas%3F", statusCode: http.StatusOK, body: `{"cache":false,"choices":[{"message":{"content":"what is the capital of Texas?","role":"assistant"}}],"id":"chatcmpl-test","object":"chat.completion"}`},
		{name: "ask custom options", method: http.MethodGet, path: "/ask?q=success&system-prompt=Be%20precise&max-tokens=700&temperature=0.7", statusCode: http.StatusOK, body: `{"cache":false,"choices":[{"message":{"content":"success","role":"assistant"}}],"id":"chatcmpl-test","object":"chat.completion"}`},
		{name: "ask missing q", method: http.MethodGet, path: "/ask", statusCode: http.StatusBadRequest, body: "missing q\n"},
		{name: "ask invalid max-tokens", method: http.MethodGet, path: "/ask?q=success&max-tokens=99", statusCode: http.StatusBadRequest, body: "max-tokens must be between 100 and 4000\n"},
		{name: "ask invalid max-tokens format", method: http.MethodGet, path: "/ask?q=success&max-tokens=nope", statusCode: http.StatusBadRequest, body: "invalid max-tokens \"nope\"\n"},
		{name: "ask invalid temperature", method: http.MethodGet, path: "/ask?q=success&temperature=nope", statusCode: http.StatusBadRequest, body: "invalid temperature \"nope\"\n"},
		{name: "ask method not allowed", method: http.MethodPost, path: "/ask", statusCode: http.StatusMethodNotAllowed, body: "Method Not Allowed\n"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			if recorder.Code != test.statusCode {
				t.Fatalf("status code = %d, want %d", recorder.Code, test.statusCode)
			}

			if test.body != "" && recorder.Body.String() != test.body {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.body)
			}
			for _, want := range test.bodyContains {
				if !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("body = %q, want substring %q", recorder.Body.String(), want)
				}
			}
		})
	}
}

func TestChatCompletionsDefaultsMissingModelFromVLLM(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	if (*captured)[0].Model != "test-model" {
		t.Fatalf("model = %q, want %q", (*captured)[0].Model, "test-model")
	}
}

func TestChatCompletionsUsesConfiguredDownstreamModelAndToken(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	handler.SetDownstreamModel("gpt-4.1")
	handler.SetDownstreamToken("secret-token")
	router := handler.Routes()

	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	if (*captured)[0].Model != "gpt-4.1" {
		t.Fatalf("model = %q, want %q", (*captured)[0].Model, "gpt-4.1")
	}
	if (*captured)[0].Authorization != "Bearer secret-token" {
		t.Fatalf("authorization = %q, want %q", (*captured)[0].Authorization, "Bearer secret-token")
	}
}

func TestModelsEndpointCachesUpstreamModelsResponse(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
}

func TestChatCompletionsMissingModelUsesCachedModelsResponse(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	firstBody := `{"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"hello"}],"temperature":0.2,"max_tokens":200}`
	secondBody := `{"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"world"}],"temperature":0.2,"max_tokens":200}`

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(firstBody))
	firstRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), firstRequest)

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(secondBody))
	secondRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), secondRequest)

	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
	if got := counters.chat.Load(); got != 2 {
		t.Fatalf("chat requests = %d, want 2", got)
	}
}

func TestChatCompletionsStreamCachesReplay(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700,"stream":true,"stream_options":{"include_usage":true}}`

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	firstRequest.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK || secondRecorder.Code != http.StatusOK {
		t.Fatalf("status codes = %d, %d, want both 200", firstRecorder.Code, secondRecorder.Code)
	}
	if got := counters.chat.Load(); got != 1 {
		t.Fatalf("chat requests = %d, want 1", got)
	}
	if !strings.Contains(secondRecorder.Body.String(), "data: [DONE]") {
		t.Fatalf("cached stream %q does not contain done marker", secondRecorder.Body.String())
	}
	if !strings.Contains(firstRecorder.Body.String(), `"cache":false`) {
		t.Fatalf("live stream %q does not contain cache=false", firstRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"cache":true`) {
		t.Fatalf("cached stream %q does not contain cache=true", secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("cached stream %q does not contain assistant content", secondRecorder.Body.String())
	}
}

func TestAskCachedReplayThrottlesNonStreamResponses(t *testing.T) {
	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"one two three four"}}],"usage":{"completion_tokens":4}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer vllmServer.Close()

	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	handler.SetRequestProcessingLimits(2, 10, 10, 0)
	var sleeps []time.Duration
	handler.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	}
	router := handler.Routes()

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil))
	secondRecorder := httptest.NewRecorder()
	router.ServeHTTP(secondRecorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil))

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if !strings.Contains(secondRecorder.Body.String(), `"cache":true`) {
		t.Fatalf("cached body %q does not contain cache=true", secondRecorder.Body.String())
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleep calls = %d, want 1", len(sleeps))
	}
	if sleeps[0] != 2*time.Second {
		t.Fatalf("sleep duration = %s, want %s", sleeps[0], 2*time.Second)
	}
}

func TestChatCompletionsCachedReplayThrottlesStreamResponses(t *testing.T) {
	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one two three four\"},\"finish_reason\":null}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":4,\"total_tokens\":9}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer vllmServer.Close()

	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	handler.SetRequestProcessingLimits(2, 10, 10, 0)
	var sleeps []time.Duration
	handler.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	}
	router := handler.Routes()
	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700,"stream":true,"stream_options":{"include_usage":true}}`

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	firstRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), firstRequest)

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	router.ServeHTTP(secondRecorder, secondRequest)

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if !strings.Contains(secondRecorder.Body.String(), `"cache":true`) {
		t.Fatalf("cached stream %q does not contain cache=true", secondRecorder.Body.String())
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleep calls = %d, want 1", len(sleeps))
	}
	if sleeps[0] != 2*time.Second {
		t.Fatalf("sleep duration = %s, want %s", sleeps[0], 2*time.Second)
	}
}

func TestCachedReplayDelayDegradesAfterConcurrencyThreshold(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(20, 10, 10, 50)

	releaseOne, ok := handler.acquireRequestSlot(context.Background(), "/one")
	if !ok {
		t.Fatal("failed to acquire first request slot")
	}
	defer releaseOne()

	if got := handler.cachedReplayDelay(2); got != 100*time.Millisecond {
		t.Fatalf("delay below threshold = %s, want %s", got, 100*time.Millisecond)
	}

	releaseTwo, ok := handler.acquireRequestSlot(context.Background(), "/two")
	if !ok {
		t.Fatal("failed to acquire second request slot")
	}
	defer releaseTwo()

	expectedTokensPerSecond := 20 * (1 - ((50.0 / 100.0) * ((0.2 - degradationThreshold) / (1 - degradationThreshold))))
	expected := time.Duration(float64(2) * float64(time.Second) / expectedTokensPerSecond)
	if got := handler.cachedReplayDelay(2); got != expected {
		t.Fatalf("delay above threshold = %s, want %s", got, expected)
	}
}

func TestCachedReplayDelayUsesWholeRequestThreshold(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(100, 512, 128, 10)

	releases := make([]func(), 0, 52)
	for i := 0; i < 51; i++ {
		release, ok := handler.acquireRequestSlot(context.Background(), "/threshold")
		if !ok {
			t.Fatalf("failed to acquire slot %d", i+1)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	if got := handler.cachedReplayDelay(10); got != 100*time.Millisecond {
		t.Fatalf("delay at threshold = %s, want %s", got, 100*time.Millisecond)
	}

	release, ok := handler.acquireRequestSlot(context.Background(), "/threshold-plus-one")
	if !ok {
		t.Fatal("failed to acquire threshold+1 slot")
	}
	releases = append(releases, release)

	expectedTokensPerSecond := 100 * (1 - ((10.0 / 100.0) * (1.0 / float64(512-51))))
	expected := time.Duration(float64(10) * float64(time.Second) / expectedTokensPerSecond)
	if got := handler.cachedReplayDelay(10); got != expected {
		t.Fatalf("delay above whole-request threshold = %s, want %s", got, expected)
	}
}

func TestAskReturnsOverCapacityWhenConcurrencyAndQueueAreFull(t *testing.T) {
	releaseDownstream := make(chan struct{})
	downstreamStarted := make(chan struct{}, 1)
	vllmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		downstreamStarted <- struct{}{}
		<-releaseDownstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer vllmServer.Close()

	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	handler.SetDownstreamModel("test-model")
	handler.SetRequestProcessingLimits(32, 1, 0, 0)
	router := handler.Routes()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstRecorder := httptest.NewRecorder()
		router.ServeHTTP(firstRecorder, httptest.NewRequest(http.MethodGet, "/ask?q=first", nil))
		firstDone <- firstRecorder
	}()

	<-downstreamStarted

	secondRecorder := httptest.NewRecorder()
	router.ServeHTTP(secondRecorder, httptest.NewRequest(http.MethodGet, "/ask?q=second", nil))

	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
	if secondRecorder.Body.String() != "over capacity\n" {
		t.Fatalf("body = %q, want %q", secondRecorder.Body.String(), "over capacity\n")
	}

	close(releaseDownstream)
	firstRecorder := <-firstDone
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first request status code = %d, want %d", firstRecorder.Code, http.StatusOK)
	}
}

func TestRoutesRequestLogging(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=success", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=%20%20Success%20%20", nil))

	logLine := logBuffer.String()
	for _, want := range []string{
		`msg="request completed"`,
		"method=GET",
		`path="/ask?q=success"`,
		"status=200",
		"bytes=128",
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

func TestRoutesNotFoundLogging(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/foo", nil))

	logLine := logBuffer.String()
	for _, want := range []string{
		"level=WARN",
		`msg="not found"`,
		"method=GET",
		`path=/foo`,
		"status=404",
		"cache=false",
	} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log line %q does not contain %q", logLine, want)
		}
	}
}

func TestAskCachesNormalizedQueries(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	firstRequest := httptest.NewRequest(http.MethodGet, "/ask?q=What%20is%20the%20capital%20of%20Texas%3F", nil)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodGet, "/ask?q=%20%20what%20is%20%20%20the%20capital%20of%20texas!!!%20%20", nil)
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusOK)
	}
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if !strings.Contains(firstRecorder.Body.String(), `"cache":false`) {
		t.Fatalf("live body %q does not contain cache=false", firstRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"cache":true`) {
		t.Fatalf("cached body %q does not contain cache=true", secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `capital of Texas`) {
		t.Fatalf("cached body %q does not contain assistant content", secondRecorder.Body.String())
	}
	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
	if got := counters.chat.Load(); got != 1 {
		t.Fatalf("chat requests = %d, want 1", got)
	}
}

func TestAskStreamsAndReplaysCachedStreamWithFreshMetadata(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	firstRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello&stream=true", nil)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello&stream=true", nil)
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK || secondRecorder.Code != http.StatusOK {
		t.Fatalf("status codes = %d, %d, want both 200", firstRecorder.Code, secondRecorder.Code)
	}
	if got := counters.chat.Load(); got != 1 {
		t.Fatalf("chat requests = %d, want 1", got)
	}
	if contentType := secondRecorder.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q, want %q", contentType, "text/event-stream")
	}
	if !strings.Contains(secondRecorder.Body.String(), "data: [DONE]") {
		t.Fatalf("cached stream %q does not contain done marker", secondRecorder.Body.String())
	}
	if !strings.Contains(firstRecorder.Body.String(), `"cache":false`) {
		t.Fatalf("live stream %q does not contain cache=false", firstRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"cache":true`) {
		t.Fatalf("cached stream %q does not contain cache=true", secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("cached stream %q does not contain assistant content", secondRecorder.Body.String())
	}

	firstID, firstCreated := extractFirstStreamMetadata(t, firstRecorder.Body.String())
	secondID, secondCreated := extractFirstStreamMetadata(t, secondRecorder.Body.String())
	if firstID == secondID {
		t.Fatalf("cached replay id = %q, want a fresh value distinct from %q", secondID, firstID)
	}
	if firstCreated == secondCreated {
		t.Fatalf("cached replay created = %d, want a fresh value distinct from %d", secondCreated, firstCreated)
	}
	if !strings.HasPrefix(secondID, "chatcmpl-") {
		t.Fatalf("cached replay id = %q, want chatcmpl- prefix", secondID)
	}
}

func TestAskDoesNotReuseCacheAcrossStreamMode(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=hello&stream=true", nil))

	if got := counters.chat.Load(); got != 2 {
		t.Fatalf("chat requests = %d, want 2", got)
	}
}

func TestAskDoesNotReuseCacheAcrossOptions(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	firstRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello&temperature=0.2", nil)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello&temperature=0.7", nil)
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK || secondRecorder.Code != http.StatusOK {
		t.Fatalf("status codes = %d, %d, want both 200", firstRecorder.Code, secondRecorder.Code)
	}
	if got := counters.chat.Load(); got != 2 {
		t.Fatalf("chat requests = %d, want 2", got)
	}
}

func TestStandardizeCacheKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "whitespace and punctuation", input: " What, is the capital of Texas?! ", want: "whatisthecapitaloftexas"},
		{name: "numbers preserved", input: "Model 3.1 Turbo", want: "model31turbo"},
		{name: "only punctuation", input: "?! ,.-", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := standardizeCacheKey(test.input)
			if got != test.want {
				t.Fatalf("standardizeCacheKey(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestBuildCacheKeyIncludesAskOptions(t *testing.T) {
	defaultOptions := askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	customOptions := askOptions{systemPrompt: "Be precise", maxTokens: 700, temperature: 0.7}

	defaultKey := buildCacheKey("hello", defaultOptions)
	customKey := buildCacheKey("hello", customOptions)

	if defaultKey == customKey {
		t.Fatalf("buildCacheKey() produced identical keys for different options: %q", defaultKey)
	}
}

func TestParseAskOptions(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		want        askOptions
		wantErr     string
	}{
		{
			name: "defaults",
			path: "/ask?q=hello",
			want: askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature, stream: false},
		},
		{
			name: "custom values",
			path: "/ask?q=hello&system-prompt=Be%20precise&max-tokens=700&temperature=0.7&stream=true",
			want: askOptions{systemPrompt: "Be precise", maxTokens: 700, temperature: 0.7, stream: true},
		},
		{name: "invalid max tokens low", path: "/ask?q=hello&max-tokens=99", wantErr: "max-tokens must be between 100 and 4000"},
		{name: "invalid max tokens high", path: "/ask?q=hello&max-tokens=10001", wantErr: "max-tokens must be between 100 and 4000"},
		{name: "invalid max tokens format", path: "/ask?q=hello&max-tokens=abc", wantErr: "invalid max-tokens \"abc\""},
		{name: "invalid temperature format", path: "/ask?q=hello&temperature=abc", wantErr: "invalid temperature \"abc\""},
		{name: "invalid stream format", path: "/ask?q=hello&stream=maybe", wantErr: "invalid stream \"maybe\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			got, err := parseAskOptions(req, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("parseAskOptions() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAskOptions() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseAskOptions() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestAskUsesDefaultVLLMOptions(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	got := (*captured)[0]
	if got.SystemPrompt != defaultSystemPrompt {
		t.Fatalf("system prompt = %q, want %q", got.SystemPrompt, defaultSystemPrompt)
	}
	if got.MaxTokens != defaultMaxTokens {
		t.Fatalf("max tokens = %d, want %d", got.MaxTokens, defaultMaxTokens)
	}
	if got.Temperature != defaultTemperature {
		t.Fatalf("temperature = %v, want %v", got.Temperature, defaultTemperature)
	}
	if got.UserContent != "hello" {
		t.Fatalf("user content = %q, want %q", got.UserContent, "hello")
	}
}

func TestAskUsesCustomVLLMOptions(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello&system-prompt=Be%20precise&max-tokens=700&temperature=0.7", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	got := (*captured)[0]
	if got.SystemPrompt != "Be precise" {
		t.Fatalf("system prompt = %q, want %q", got.SystemPrompt, "Be precise")
	}
	if got.MaxTokens != 700 {
		t.Fatalf("max tokens = %d, want %d", got.MaxTokens, 700)
	}
	if got.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want %v", got.Temperature, 0.7)
	}
	if got.UserContent != "hello" {
		t.Fatalf("user content = %q, want %q", got.UserContent, "hello")
	}
}

func TestConfigEndpointUpdatesAndReturnsCurrentConfig(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	configRecorder := httptest.NewRecorder()
	handler.ServeHTTP(configRecorder, httptest.NewRequest(http.MethodGet, "/config?cache-size=7&downstream-url="+url.QueryEscape(vllmServer.URL)+"&downstream-model=gpt-4.1&system-prompt=Be%20precise&max-tokens=700&max-tokens-per-second=48&max-concurrent-requests=64&max-waiting-requests=96&max-degradation=25&temperature=0.7&stream=true", nil))

	if configRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", configRecorder.Code, http.StatusOK)
	}

	var got runtimeConfig
	if err := json.Unmarshal(configRecorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config response: %v", err)
	}
	if got.SystemPrompt != "Be precise" {
		t.Fatalf("system prompt = %q, want %q", got.SystemPrompt, "Be precise")
	}
	if got.DownstreamURL != vllmServer.URL {
		t.Fatalf("downstream url = %q, want %q", got.DownstreamURL, vllmServer.URL)
	}
	if got.DownstreamModel != "gpt-4.1" {
		t.Fatalf("downstream model = %q, want %q", got.DownstreamModel, "gpt-4.1")
	}
	if got.CacheSize != 7 {
		t.Fatalf("cache size = %d, want %d", got.CacheSize, 7)
	}
	if got.CacheEntries != 0 {
		t.Fatalf("cache entries = %d, want %d", got.CacheEntries, 0)
	}
	if got.MaxTokens != 700 {
		t.Fatalf("max tokens = %d, want %d", got.MaxTokens, 700)
	}
	if got.MaxTokensPerSecond != 48 {
		t.Fatalf("max tokens per second = %d, want %d", got.MaxTokensPerSecond, 48)
	}
	if got.EffectiveTokensPerSecond != 48 {
		t.Fatalf("effective tokens per second = %v, want %v", got.EffectiveTokensPerSecond, 48)
	}
	if got.MaxConcurrentRequests != 64 {
		t.Fatalf("max concurrent requests = %d, want %d", got.MaxConcurrentRequests, 64)
	}
	if got.ConcurrentRequests != 0 {
		t.Fatalf("concurrent requests = %d, want %d", got.ConcurrentRequests, 0)
	}
	if got.MaxWaitingRequests != 96 {
		t.Fatalf("max waiting requests = %d, want %d", got.MaxWaitingRequests, 96)
	}
	if got.WaitingRequests != 0 {
		t.Fatalf("waiting requests = %d, want %d", got.WaitingRequests, 0)
	}
	if got.MaxDegradation != 25 {
		t.Fatalf("max degradation = %d, want %d", got.MaxDegradation, 25)
	}
	if got.ComputedDegradationPercentage != 0 {
		t.Fatalf("computed degradation percentage = %v, want 0", got.ComputedDegradationPercentage)
	}
	if got.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want %v", got.Temperature, 0.7)
	}
	if !got.Stream {
		t.Fatal("stream = false, want true")
	}
	askRecorder := httptest.NewRecorder()
	handler.ServeHTTP(askRecorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil))

	if askRecorder.Code != http.StatusOK {
		t.Fatalf("ask status code = %d, want %d", askRecorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	request := (*captured)[0]
	if request.Model != "gpt-4.1" {
		t.Fatalf("captured model = %q, want %q", request.Model, "gpt-4.1")
	}
	if request.SystemPrompt != "Be precise" {
		t.Fatalf("captured system prompt = %q, want %q", request.SystemPrompt, "Be precise")
	}
	if request.MaxTokens != 700 {
		t.Fatalf("captured max tokens = %d, want %d", request.MaxTokens, 700)
	}
	if request.Temperature != 0.7 {
		t.Fatalf("captured temperature = %v, want %v", request.Temperature, 0.7)
	}
	if !request.Stream {
		t.Fatal("captured stream = false, want true")
	}

	configRecorder = httptest.NewRecorder()
	handler.ServeHTTP(configRecorder, httptest.NewRequest(http.MethodGet, "/config", nil))
	if configRecorder.Code != http.StatusOK {
		t.Fatalf("config status code = %d, want %d", configRecorder.Code, http.StatusOK)
	}
	if err := json.Unmarshal(configRecorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config response after ask: %v", err)
	}
	if got.CacheEntries != 1 {
		t.Fatalf("cache entries after ask = %d, want %d", got.CacheEntries, 1)
	}
}

func TestConfigEndpointAcceptsSnakeCaseQueryNames(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config?cache_size=7&downstream_url="+url.QueryEscape(vllmServer.URL)+"&downstream_model=gpt-4.1&system_prompt=Be%20precise&max_tokens=700&max_tokens_per_second=48&max_concurrent_requests=64&max_waiting_requests=96&max_degradation=25", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}

	var got runtimeConfig
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config response: %v", err)
	}
	if got.SystemPrompt != "Be precise" {
		t.Fatalf("system prompt = %q, want %q", got.SystemPrompt, "Be precise")
	}
	if got.DownstreamURL != vllmServer.URL {
		t.Fatalf("downstream url = %q, want %q", got.DownstreamURL, vllmServer.URL)
	}
	if got.DownstreamModel != "gpt-4.1" {
		t.Fatalf("downstream model = %q, want %q", got.DownstreamModel, "gpt-4.1")
	}
	if got.CacheSize != 7 {
		t.Fatalf("cache size = %d, want %d", got.CacheSize, 7)
	}
	if got.CacheEntries != 0 {
		t.Fatalf("cache entries = %d, want %d", got.CacheEntries, 0)
	}
	if got.MaxTokens != 700 {
		t.Fatalf("max tokens = %d, want %d", got.MaxTokens, 700)
	}
	if got.MaxTokensPerSecond != 48 {
		t.Fatalf("max tokens per second = %d, want %d", got.MaxTokensPerSecond, 48)
	}
	if got.MaxConcurrentRequests != 64 {
		t.Fatalf("max concurrent requests = %d, want %d", got.MaxConcurrentRequests, 64)
	}
	if got.MaxWaitingRequests != 96 {
		t.Fatalf("max waiting requests = %d, want %d", got.MaxWaitingRequests, 96)
	}
	if got.MaxDegradation != 25 {
		t.Fatalf("max degradation = %d, want %d", got.MaxDegradation, 25)
	}
}

func TestConfigEndpointRejectsInvalidQueueValues(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "concurrent below min", path: "/config?max-concurrent-requests=0", want: `max-concurrent-requests must be between 1 and 512`},
		{name: "concurrent above max", path: "/config?max-concurrent-requests=513", want: `max-concurrent-requests must be between 1 and 512`},
		{name: "waiting below min", path: "/config?max-waiting-requests=-1", want: `max-waiting-requests must be between 0 and 1024`},
		{name: "waiting above max", path: "/config?max-waiting-requests=1025", want: `max-waiting-requests must be between 0 and 1024`},
		{name: "waiting too large for concurrent", path: "/config?max-concurrent-requests=64&max-waiting-requests=128", want: `max-waiting-requests must be less than 128`},
		{name: "tokens per second above max", path: "/config?max-tokens-per-second=1001", want: `max-tokens-per-second must be between 0 and 1000`},
		{name: "degradation above max", path: "/config?max-concurrent-requests=64&max-waiting-requests=96&max-degradation=96", want: `max-degradation must be between 0 and 95`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler().Routes()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.want)
			}
		})
	}
}

func TestSchedulerReconfigureBelowCurrentLengthsPreservesQueuedRequests(t *testing.T) {
	scheduler := newRequestScheduler(2, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	releaseOne, ok := scheduler.AcquirePath(ctx, "/one")
	if !ok {
		t.Fatal("failed to acquire first direct slot")
	}
	releaseTwo, ok := scheduler.AcquirePath(ctx, "/two")
	if !ok {
		t.Fatal("failed to acquire second direct slot")
	}

	type acquiredRelease struct {
		path string
		release func()
	}
	acquired := make(chan acquiredRelease, 2)
	go func() {
		release, ok := scheduler.AcquirePath(ctx, "/three")
		if ok {
			acquired <- acquiredRelease{path: "/three", release: release}
		}
	}()
	go func() {
		release, ok := scheduler.AcquirePath(ctx, "/four")
		if ok {
			acquired <- acquiredRelease{path: "/four", release: release}
		}
	}()

	for {
		_, _, _, waiting := scheduler.Stats()
		if waiting == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	scheduler.Reconfigure(1, 1)
	if maxConcurrent, inFlight, maxWaiting, waiting := scheduler.Stats(); maxConcurrent != 1 || inFlight != 2 || maxWaiting != 1 || waiting != 2 {
		t.Fatalf("stats after reconfigure = (%d,%d,%d,%d), want (1,2,1,2)", maxConcurrent, inFlight, maxWaiting, waiting)
	}

	if _, ok := scheduler.AcquirePath(ctx, "/five"); ok {
		t.Fatal("expected new admission to be rejected while queue remains above max waiting")
	}

	releaseOne()
	select {
	case promoted := <-acquired:
		promoted.release()
		t.Fatal("queued request promoted before in-flight dropped below reconfigured max")
	case <-time.After(20 * time.Millisecond):
	}

	releaseTwo()
	firstPromoted := <-acquired
	if maxConcurrent, inFlight, maxWaiting, waiting := scheduler.Stats(); maxConcurrent != 1 || inFlight != 1 || maxWaiting != 1 || waiting != 1 {
		t.Fatalf("stats after first promotion = (%d,%d,%d,%d), want (1,1,1,1)", maxConcurrent, inFlight, maxWaiting, waiting)
	}
	if _, ok := scheduler.AcquirePath(ctx, "/six"); ok {
		t.Fatal("expected new admission to be rejected while waiting queue is still full")
	}

	firstPromoted.release()
	secondPromoted := <-acquired
	if maxConcurrent, inFlight, maxWaiting, waiting := scheduler.Stats(); maxConcurrent != 1 || inFlight != 1 || maxWaiting != 1 || waiting != 0 {
		t.Fatalf("stats after second promotion = (%d,%d,%d,%d), want (1,1,1,0)", maxConcurrent, inFlight, maxWaiting, waiting)
	}
	secondPromoted.release()
	if maxConcurrent, inFlight, maxWaiting, waiting := scheduler.Stats(); maxConcurrent != 1 || inFlight != 0 || maxWaiting != 1 || waiting != 0 {
		t.Fatalf("final stats = (%d,%d,%d,%d), want (1,0,1,0)", maxConcurrent, inFlight, maxWaiting, waiting)
	}
	if release, ok := scheduler.AcquirePath(ctx, "/seven"); !ok {
		t.Fatal("expected direct admission once both queues had space again")
	} else {
		release()
	}
}

func TestSchedulerLogsAdmissionAndCompletionLifecycle(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	scheduler := newRequestScheduler(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseOne, ok := scheduler.AcquirePath(ctx, "/one")
	if !ok {
		t.Fatal("failed to acquire direct slot")
	}

	queuedAcquired := make(chan func(), 1)
	go func() {
		release, ok := scheduler.AcquirePath(ctx, "/two")
		if ok {
			queuedAcquired <- release
		}
	}()

	for {
		_, _, _, waiting := scheduler.Stats()
		if waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	releaseOne()
	queuedRelease := <-queuedAcquired
	queuedRelease()

	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="request admitted" path=/one source=direct`,
		`msg="request admitted" path=/two source=waiting_queue`,
		`msg="request admitted" path=/two source=waiting_to_concurrent`,
		`msg="request completed" path=/one`,
		`msg="request completed" path=/two`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestConfigEndpointLogsQueueLimitChanges(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	handler := NewHandler().Routes()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config?max-concurrent-requests=64&max-waiting-requests=96", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="request queue limits updated"`,
		`previous_max_concurrent_requests=512`,
		`max_concurrent_requests=64`,
		`previous_max_waiting_requests=1023`,
		`max_waiting_requests=96`,
		`concurrent_requests=0`,
		`waiting_requests=0`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestRequestAdmissionLogsComputedDegradationChanges(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	handler := NewHandler()
	handler.SetRequestProcessingLimits(20, 10, 10, 50)

	releaseOne, ok := handler.acquireRequestSlot(context.Background(), "/one")
	if !ok {
		t.Fatal("failed to acquire first request slot")
	}
	releaseTwo, ok := handler.acquireRequestSlot(context.Background(), "/two")
	if !ok {
		t.Fatal("failed to acquire second request slot")
	}
	releaseTwo()
	releaseOne()

	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="computed degradation updated" source=limits_updated computed_degradation_percentage=0`,
		`msg="computed degradation updated" source=request_admitted computed_degradation_percentage=5.556`,
		`effective_tokens_per_second=18.889`,
		`msg="computed degradation updated" source=request_completed computed_degradation_percentage=0`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestCurrentConfigIncludesComputedDegradation(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(100, 512, 128, 10)

	releases := make([]func(), 0, 52)
	for i := 0; i < 52; i++ {
		release, ok := handler.acquireRequestSlot(context.Background(), "/load")
		if !ok {
			t.Fatalf("failed to acquire slot %d", i+1)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	got := handler.currentConfig()
	if got.ComputedDegradationPercentage != 0.022 {
		t.Fatalf("computed degradation percentage = %v, want 0.022", got.ComputedDegradationPercentage)
	}
	if got.EffectiveTokensPerSecond != 99.978 {
		t.Fatalf("effective tokens per second = %v, want 99.978", got.EffectiveTokensPerSecond)
	}
}

func TestCachedReplayDelayDisabledWhenTokensPerSecondZero(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(0, 10, 10, 10)
	if got := handler.cachedReplayDelay(20); got != 0 {
		t.Fatalf("cached replay delay = %s, want 0", got)
	}
}

func TestConfigEndpointResizesCacheAndEvictsOldestEntries(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 2, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=second", nil))

	configRecorder := httptest.NewRecorder()
	handler.ServeHTTP(configRecorder, httptest.NewRequest(http.MethodGet, "/config?cache-size=1", nil))

	if configRecorder.Code != http.StatusOK {
		t.Fatalf("config status code = %d, want %d", configRecorder.Code, http.StatusOK)
	}

	var got runtimeConfig
	if err := json.Unmarshal(configRecorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal config response: %v", err)
	}
	if got.CacheSize != 1 {
		t.Fatalf("cache size = %d, want %d", got.CacheSize, 1)
	}
	if got.CacheEntries != 1 {
		t.Fatalf("cache entries = %d, want %d", got.CacheEntries, 1)
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=second", nil))
	if got := counters.chat.Load(); got != 2 {
		t.Fatalf("chat requests after cached second = %d, want %d", got, 2)
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=first", nil))
	if got := counters.chat.Load(); got != 3 {
		t.Fatalf("chat requests after evicted first = %d, want %d", got, 3)
	}
}

func TestLoadConfigCacheSize(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 123 {
		t.Fatalf("CacheSize = %d, want 123", cfg.CacheSize)
	}
}

func TestLoadConfigCacheSizeFlagPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm", "--cache-size", "7"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 7 {
		t.Fatalf("CacheSize = %d, want 7", cfg.CacheSize)
	}
}

func TestLoadConfigShortCacheSizeFlag(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm", "-c", "0"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 0 {
		t.Fatalf("CacheSize = %d, want 0", cfg.CacheSize)
	}
}

func TestLoadConfigInvalidCacheSize(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{name: "negative flag", args: []string{"cllm", "--cache-size", "-1"}, wantErr: "invalid cache size -1"},
		{name: "non integer flag", args: []string{"cllm", "--cache-size", "abc"}, wantErr: "invalid runtime flag"},
		{name: "negative env", args: []string{"cllm"}, env: "-1", wantErr: "invalid CACHE_CACHE_SIZE \"-1\""},
		{name: "non integer env", args: []string{"cllm"}, env: "abc", wantErr: "invalid CACHE_CACHE_SIZE \"abc\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalArgs := os.Args
			defer func() { os.Args = originalArgs }()
			os.Args = test.args

			t.Setenv("CACHE_PORT", "8080")
			t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
			t.Setenv("CACHE_CACHE_SIZE", test.env)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("config.Load() error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("config.Load() error = %q, want substring %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestModelsEndpointDoesNotRefreshWithoutRestart(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	routes := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
}

func TestAskWithCacheDisabled(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 0, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	firstRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)

	secondRequest := httptest.NewRequest(http.MethodGet, "/ask?q=hello", nil)
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)

	if firstRecorder.Code != http.StatusOK || secondRecorder.Code != http.StatusOK {
		t.Fatalf("status codes = %d, %d, want both 200", firstRecorder.Code, secondRecorder.Code)
	}
	if got := counters.models.Load(); got != 1 {
		t.Fatalf("models requests = %d, want 1", got)
	}
	if got := counters.chat.Load(); got != 2 {
		t.Fatalf("chat requests = %d, want 2", got)
	}
}

func TestLoadConfigAskDefaultsFromEnv(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_MAX_TOKENS_PER_SECOND", "0")
	t.Setenv("CACHE_MAX_CONCURRENT_REQUESTS", "64")
	t.Setenv("CACHE_MAX_WAITING_REQUESTS", "95")
	t.Setenv("CACHE_MAX_DEGRADATION", "25")
	t.Setenv("CACHE_TEMPERATURE", "0.7")
	t.Setenv("CACHE_DOWNSTREAM_URL", "https://api.openai.com")
	t.Setenv("CACHE_DOWNSTREAM_TOKEN", "secret-token")
	t.Setenv("CACHE_DOWNSTREAM_MODEL", "gpt-4.1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.SystemPrompt != "Be concise" {
		t.Fatalf("SystemPrompt = %q, want %q", cfg.SystemPrompt, "Be concise")
	}
	if cfg.MaxTokens != 700 {
		t.Fatalf("MaxTokens = %d, want 700", cfg.MaxTokens)
	}
	if cfg.MaxTokensPerSecond != 0 {
		t.Fatalf("MaxTokensPerSecond = %d, want 0", cfg.MaxTokensPerSecond)
	}
	if cfg.MaxConcurrentRequests != 64 {
		t.Fatalf("MaxConcurrentRequests = %d, want 64", cfg.MaxConcurrentRequests)
	}
	if cfg.MaxWaitingRequests != 95 {
		t.Fatalf("MaxWaitingRequests = %d, want 95", cfg.MaxWaitingRequests)
	}
	if cfg.MaxDegradation != 25 {
		t.Fatalf("MaxDegradation = %d, want 25", cfg.MaxDegradation)
	}
	if cfg.Temperature != 0.7 {
		t.Fatalf("Temperature = %v, want 0.7", cfg.Temperature)
	}
	if cfg.DownstreamURL != "https://api.openai.com" {
		t.Fatalf("DownstreamURL = %q, want %q", cfg.DownstreamURL, "https://api.openai.com")
	}
	if cfg.DownstreamToken != "secret-token" {
		t.Fatalf("DownstreamToken = %q, want %q", cfg.DownstreamToken, "secret-token")
	}
	if cfg.DownstreamModel != "gpt-4.1" {
		t.Fatalf("DownstreamModel = %q, want %q", cfg.DownstreamModel, "gpt-4.1")
	}
}

func TestLoadConfigAskDefaultFlagsPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm", "--downstream-url", "https://example.test", "--downstream-token", "flag-token", "--downstream-model", "flag-model", "--system-prompt", "Be precise", "--max-tokens", "900", "--max-tokens-per-second", "64", "--max-concurrent-requests", "16", "--max-waiting-requests", "31", "--max-degradation", "15", "--temperature", "0.9"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_MAX_TOKENS_PER_SECOND", "0")
	t.Setenv("CACHE_MAX_CONCURRENT_REQUESTS", "64")
	t.Setenv("CACHE_MAX_WAITING_REQUESTS", "95")
	t.Setenv("CACHE_MAX_DEGRADATION", "25")
	t.Setenv("CACHE_TEMPERATURE", "0.7")
	t.Setenv("CACHE_DOWNSTREAM_URL", "https://api.openai.com")
	t.Setenv("CACHE_DOWNSTREAM_TOKEN", "env-token")
	t.Setenv("CACHE_DOWNSTREAM_MODEL", "env-model")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.SystemPrompt != "Be precise" {
		t.Fatalf("SystemPrompt = %q, want %q", cfg.SystemPrompt, "Be precise")
	}
	if cfg.MaxTokens != 900 {
		t.Fatalf("MaxTokens = %d, want 900", cfg.MaxTokens)
	}
	if cfg.MaxTokensPerSecond != 64 {
		t.Fatalf("MaxTokensPerSecond = %d, want 64", cfg.MaxTokensPerSecond)
	}
	if cfg.MaxConcurrentRequests != 16 {
		t.Fatalf("MaxConcurrentRequests = %d, want 16", cfg.MaxConcurrentRequests)
	}
	if cfg.MaxWaitingRequests != 31 {
		t.Fatalf("MaxWaitingRequests = %d, want 31", cfg.MaxWaitingRequests)
	}
	if cfg.MaxDegradation != 15 {
		t.Fatalf("MaxDegradation = %d, want 15", cfg.MaxDegradation)
	}
	if cfg.Temperature != 0.9 {
		t.Fatalf("Temperature = %v, want 0.9", cfg.Temperature)
	}
	if cfg.DownstreamURL != "https://example.test" {
		t.Fatalf("DownstreamURL = %q, want %q", cfg.DownstreamURL, "https://example.test")
	}
	if cfg.DownstreamToken != "flag-token" {
		t.Fatalf("DownstreamToken = %q, want %q", cfg.DownstreamToken, "flag-token")
	}
	if cfg.DownstreamModel != "flag-model" {
		t.Fatalf("DownstreamModel = %q, want %q", cfg.DownstreamModel, "flag-model")
	}
}

func TestLoadConfigInvalidAskDefaults(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		envKey  string
		envVal  string
		wantErr string
	}{
		{name: "invalid env max tokens", args: []string{"cllm"}, envKey: "CACHE_MAX_TOKENS", envVal: "nope", wantErr: `invalid CACHE_MAX_TOKENS "nope"`},
		{name: "env max tokens out of range", args: []string{"cllm"}, envKey: "CACHE_MAX_TOKENS", envVal: "99", wantErr: "CACHE_MAX_TOKENS must be between 100 and 4000"},
		{name: "invalid env max tokens per second", args: []string{"cllm"}, envKey: "CACHE_MAX_TOKENS_PER_SECOND", envVal: "nope", wantErr: `invalid CACHE_MAX_TOKENS_PER_SECOND "nope"`},
		{name: "flag max tokens per second above max", args: []string{"cllm", "--max-tokens-per-second", "1001"}, wantErr: "max-tokens-per-second must be between 0 and 1000"},
		{name: "flag max concurrent requests too low", args: []string{"cllm", "--max-concurrent-requests", "0"}, wantErr: "max-concurrent-requests must be between 1 and 512"},
		{name: "flag max concurrent requests too high", args: []string{"cllm", "--max-concurrent-requests", "513"}, wantErr: "max-concurrent-requests must be between 1 and 512"},
		{name: "flag max waiting requests negative", args: []string{"cllm", "--max-waiting-requests", "-1"}, wantErr: "max-waiting-requests must be between 0 and 1024"},
		{name: "flag max waiting requests too high", args: []string{"cllm", "--max-waiting-requests", "1025"}, wantErr: "max-waiting-requests must be between 0 and 1024"},
		{name: "flag max waiting requests too large for concurrent", args: []string{"cllm", "--max-concurrent-requests", "64", "--max-waiting-requests", "128"}, wantErr: "max-waiting-requests must be less than 128"},
		{name: "flag max degradation too high", args: []string{"cllm", "--max-degradation", "96"}, wantErr: "max-degradation must be between 0 and 95"},
		{name: "invalid env temperature", args: []string{"cllm"}, envKey: "CACHE_TEMPERATURE", envVal: "nope", wantErr: `invalid CACHE_TEMPERATURE "nope"`},
		{name: "flag max tokens out of range", args: []string{"cllm", "--max-tokens", "10001"}, wantErr: "max-tokens must be between 100 and 4000"},
		{name: "invalid flag temperature", args: []string{"cllm", "--temperature", "nope"}, wantErr: "invalid runtime flag"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalArgs := os.Args
			defer func() { os.Args = originalArgs }()
			os.Args = test.args

			t.Setenv("CACHE_PORT", "8080")
			t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
			if test.envKey != "" {
				t.Setenv(test.envKey, test.envVal)
			}

			_, err := config.Load()
			if err == nil {
				t.Fatalf("config.Load() error = nil, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("config.Load() error = %q, want substring %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestLRUCacheLifecycleLogging(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	cache := newLRUCache(1)
	cache.Add("firstkey", cachedVLLMResponse{statusCode: http.StatusOK, body: []byte(`{"ok":true}`)})
	cache.Add("secondkey", cachedVLLMResponse{statusCode: http.StatusOK, body: []byte(`{"ok":true}`)})

	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="cache insert"`,
		`msg="cache evict"`,
		"size=1",
		"capacity=1",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}

	if strings.Contains(logOutput, `msg="cache insert" size=2 capacity=1`) {
		t.Fatalf("log output %q unexpectedly contains over-capacity insert log", logOutput)
	}
	if strings.Contains(logOutput, `msg="cache evict" size=2 capacity=1`) {
		t.Fatalf("log output %q unexpectedly contains pre-eviction delete log", logOutput)
	}

	evictIndex := strings.LastIndex(logOutput, `msg="cache evict"`)
	lastInsertIndex := strings.LastIndex(logOutput, `msg="cache insert"`)
	if evictIndex == -1 || lastInsertIndex == -1 || evictIndex > lastInsertIndex {
		t.Fatalf("log output %q does not show eviction before the final insert log", logOutput)
	}

	for _, unwanted := range []string{"firstkey", "secondkey"} {
		if strings.Contains(logOutput, unwanted) {
			t.Fatalf("log output %q unexpectedly contains %q", logOutput, unwanted)
		}
	}
}

type vllmCounters struct {
	chat   atomic.Int64
	models atomic.Int64
}

type capturedChatRequest struct {
	MaxTokens    int
	Model        string
	SystemPrompt string
	Temperature  float64
	UserContent  string
	Stream       bool
	Authorization string
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
			var requestBody chatCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			if len(requestBody.Messages) < 2 {
				t.Fatalf("messages = %#v, want at least two messages", requestBody.Messages)
			}
			content := requestBody.Messages[1].Content
			mu.Lock()
			defer mu.Unlock()
			if requestBody.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + content + "\"},\"finish_reason\":null}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))

	return server, counters
}

func newTestVLLMServer(t *testing.T) *httptest.Server {
	t.Helper()

	server, _ := newCapturingTestVLLMServer(t)
	return server
}

func newCapturingTestVLLMServer(t *testing.T) (*httptest.Server, *[]capturedChatRequest) {
	t.Helper()

	var captured []capturedChatRequest
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var requestBody chatCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if len(requestBody.Messages) < 2 {
				t.Fatalf("messages = %#v, want at least two messages", requestBody.Messages)
			}

			mu.Lock()
			captured = append(captured, capturedChatRequest{
				Model:        requestBody.Model,
				SystemPrompt: requestBody.Messages[0].Content,
				UserContent:  requestBody.Messages[1].Content,
				Temperature:  requestBody.Temperature,
				MaxTokens:    requestBody.MaxTokens,
				Stream:       requestBody.Stream,
				Authorization: r.Header.Get("Authorization"),
			})
			mu.Unlock()

			if requestBody.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + requestBody.Messages[1].Content + "\"},\"finish_reason\":null}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + requestBody.Messages[1].Content + `"}}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))

	return server, &captured
}

func extractFirstStreamMetadata(t *testing.T, body string) (string, int64) {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}

		var chunk struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode stream chunk %q: %v", line, err)
		}
		return chunk.ID, chunk.Created
	}

	t.Fatalf("stream body %q did not contain a data chunk", body)
	return "", 0
}
