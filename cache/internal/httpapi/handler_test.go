package httpapi

import (
	"bytes"
	"cache/internal/config"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	tests := []struct {
		name       string
		method     string
		path       string
		statusCode int
		body       string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz", statusCode: http.StatusOK, body: "ok\n"},
		{name: "readyz", method: http.MethodGet, path: "/readyz", statusCode: http.StatusOK, body: "ready\n"},
		{name: "models", method: http.MethodGet, path: "/v1/models", statusCode: http.StatusOK, body: `{"data":[{"id":"test-model"}]}`},
		{name: "ask", method: http.MethodGet, path: "/ask?q=success", statusCode: http.StatusOK, body: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"success"}}]}`},
		{name: "ask stream", method: http.MethodGet, path: "/ask?q=success&stream=true", statusCode: http.StatusOK, body: "data: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"success\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-test-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"test-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\ndata: [DONE]\n\n"},
		{name: "ask example query", method: http.MethodGet, path: "/ask?q=what%20is%20the%20capital%20of%20Texas%3F", statusCode: http.StatusOK, body: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"what is the capital of Texas?"}}]}`},
		{name: "ask custom options", method: http.MethodGet, path: "/ask?q=success&system-prompt=Be%20precise&max-tokens=700&temperature=0.7", statusCode: http.StatusOK, body: `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"success"}}]}`},
		{name: "ask missing q", method: http.MethodGet, path: "/ask", statusCode: http.StatusBadRequest, body: "missing q\n"},
		{name: "ask invalid max-tokens", method: http.MethodGet, path: "/ask?q=success&max-tokens=99", statusCode: http.StatusBadRequest, body: "max-tokens must be between 100 and 4000\n"},
		{name: "ask invalid max-tokens format", method: http.MethodGet, path: "/ask?q=success&max-tokens=nope", statusCode: http.StatusBadRequest, body: "invalid max-tokens \"nope\"\n"},
		{name: "ask invalid temperature", method: http.MethodGet, path: "/ask?q=success&temperature=nope", statusCode: http.StatusBadRequest, body: "invalid temperature \"nope\"\n"},
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
	if !strings.Contains(secondRecorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("cached stream %q does not contain assistant content", secondRecorder.Body.String())
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

func TestAskCachedStreamReplayDelay(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})

	var delays []time.Duration
	handler.sleep = func(delay time.Duration) {
		delays = append(delays, delay)
	}
	routes := handler.Routes()

	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask?q=hello&stream=true", nil))

	secondRecorder := httptest.NewRecorder()
	routes.ServeHTTP(secondRecorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello&stream=true&replay-delay-ms=5", nil))

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if got := counters.chat.Load(); got != 1 {
		t.Fatalf("chat requests = %d, want 1", got)
	}
	if len(delays) == 0 {
		t.Fatal("expected replay delay hook to be invoked for cached stream")
	}
	for _, delay := range delays {
		if delay != 5*time.Millisecond {
			t.Fatalf("delay = %s, want %s", delay, 5*time.Millisecond)
		}
	}
}

func TestAskCachedNonStreamReplayDelayUsesCompletionTokens(t *testing.T) {
	handler := NewHandler()
	handler.sleep = func(delay time.Duration) {
		if delay != 15*time.Millisecond {
			t.Fatalf("delay = %s, want %s", delay, 15*time.Millisecond)
		}
	}

	cacheKey := buildCacheKey("hello", askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	handler.cache.Add(cacheKey, cachedVLLMResponse{
		statusCode:  http.StatusOK,
		contentType: "application/json",
		body:        []byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`),
	})

	recorder := httptest.NewRecorder()
	handler.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ask?q=hello&replay-delay-ms=5", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() == "" {
		t.Fatal("expected cached response body")
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

func TestParseReplayDelay(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    time.Duration
		wantErr string
	}{
		{name: "missing", path: "/ask?q=hello", want: 0},
		{name: "valid", path: "/ask?q=hello&replay-delay-ms=5", want: 5 * time.Millisecond},
		{name: "negative", path: "/ask?q=hello&replay-delay-ms=-1", wantErr: "replay-delay-ms must be non-negative"},
		{name: "invalid", path: "/ask?q=hello&replay-delay-ms=nope", wantErr: "invalid replay-delay-ms \"nope\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			got, err := parseReplayDelay(req)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("parseReplayDelay() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReplayDelay() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseReplayDelay() = %s, want %s", got, test.want)
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

func TestLoadConfigCacheSize(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cache"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 123 {
		t.Fatalf("CacheSize = %d, want 123", cfg.CacheSize)
	}
}

func TestLoadConfigModelsCacheTTL(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cache"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_MODELS_CACHE_TTL", "30m")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.ModelsCacheTTL != 30*time.Minute {
		t.Fatalf("ModelsCacheTTL = %s, want %s", cfg.ModelsCacheTTL, 30*time.Minute)
	}
}

func TestLoadConfigModelsCacheTTLFlagPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cache", "--models-cache-ttl", "15m"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_MODELS_CACHE_TTL", "30m")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.ModelsCacheTTL != 15*time.Minute {
		t.Fatalf("ModelsCacheTTL = %s, want %s", cfg.ModelsCacheTTL, 15*time.Minute)
	}
}

func TestLoadConfigCacheSizeFlagPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cache", "--cache-size", "7"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
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
	os.Args = []string{"cache", "-c", "0"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
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
		{name: "negative flag", args: []string{"cache", "--cache-size", "-1"}, wantErr: "invalid cache size -1"},
		{name: "non integer flag", args: []string{"cache", "--cache-size", "abc"}, wantErr: "invalid runtime flag"},
		{name: "negative env", args: []string{"cache"}, env: "-1", wantErr: "invalid CACHE_CACHE_SIZE \"-1\""},
		{name: "non integer env", args: []string{"cache"}, env: "abc", wantErr: "invalid CACHE_CACHE_SIZE \"abc\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalArgs := os.Args
			defer func() { os.Args = originalArgs }()
			os.Args = test.args

			t.Setenv("PORT", "8080")
			t.Setenv("SHUTDOWN_TIMEOUT", "10s")
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

func TestLoadConfigInvalidModelsCacheTTL(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{name: "invalid flag", args: []string{"cache", "--models-cache-ttl", "nope"}, wantErr: "invalid runtime flag"},
		{name: "invalid env", args: []string{"cache"}, env: "nope", wantErr: "invalid CACHE_MODELS_CACHE_TTL \"nope\""},
		{name: "negative env", args: []string{"cache"}, env: "-1s", wantErr: "CACHE_MODELS_CACHE_TTL must be non-negative"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalArgs := os.Args
			defer func() { os.Args = originalArgs }()
			os.Args = test.args

			t.Setenv("PORT", "8080")
			t.Setenv("SHUTDOWN_TIMEOUT", "10s")
			if test.env != "" {
				t.Setenv("CACHE_MODELS_CACHE_TTL", test.env)
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

func TestModelsEndpointRefreshesAfterTTL(t *testing.T) {
	vllmServer, counters := newCountingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	currentTime := time.Unix(1_700_000_000, 0)
	handler.now = func() time.Time { return currentTime }
	handler.SetModelsCacheTTL(time.Hour)
	routes := handler.Routes()

	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	currentTime = currentTime.Add(30 * time.Minute)
	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	currentTime = currentTime.Add(31 * time.Minute)
	routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := counters.models.Load(); got != 2 {
		t.Fatalf("models requests = %d, want 2", got)
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
	os.Args = []string{"cache"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_TEMPERATURE", "0.7")

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
	if cfg.Temperature != 0.7 {
		t.Fatalf("Temperature = %v, want 0.7", cfg.Temperature)
	}
}

func TestLoadConfigAskDefaultFlagsPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cache", "--system-prompt", "Be precise", "--max-tokens", "900", "--temperature", "0.9"}

	t.Setenv("PORT", "8080")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_TEMPERATURE", "0.7")

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
	if cfg.Temperature != 0.9 {
		t.Fatalf("Temperature = %v, want 0.9", cfg.Temperature)
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
		{name: "invalid env max tokens", args: []string{"cache"}, envKey: "CACHE_MAX_TOKENS", envVal: "nope", wantErr: `invalid CACHE_MAX_TOKENS "nope"`},
		{name: "env max tokens out of range", args: []string{"cache"}, envKey: "CACHE_MAX_TOKENS", envVal: "99", wantErr: "CACHE_MAX_TOKENS must be between 100 and 10000"},
		{name: "invalid env temperature", args: []string{"cache"}, envKey: "CACHE_TEMPERATURE", envVal: "nope", wantErr: `invalid CACHE_TEMPERATURE "nope"`},
		{name: "flag max tokens out of range", args: []string{"cache", "--max-tokens", "10001"}, wantErr: "max-tokens must be between 100 and 10000"},
		{name: "invalid flag temperature", args: []string{"cache", "--temperature", "nope"}, wantErr: "invalid runtime flag"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalArgs := os.Args
			defer func() { os.Args = originalArgs }()
			os.Args = test.args

			t.Setenv("PORT", "8080")
			t.Setenv("SHUTDOWN_TIMEOUT", "10s")
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
