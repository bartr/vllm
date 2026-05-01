package httpapi

import (
	"bytes"
	"cllm/internal/buildinfo"
	"cllm/internal/config"
	"cllm/internal/node"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoutes(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: "You are a detailed assistant.", maxTokens: 2500, temperature: defaultTemperature}).Routes()

	tests := []struct {
		name         string
		method       string
		path         string
		statusCode   int
		body         string
		bodyContains []string
	}{
		{name: "health", method: http.MethodGet, path: "/health", statusCode: http.StatusOK, body: "ok\n"},
		{name: "ready", method: http.MethodGet, path: "/ready", statusCode: http.StatusOK, body: "ready\n"},
		{name: "version", method: http.MethodGet, path: "/version", statusCode: http.StatusOK, body: "9.9.9"},
		{name: "config", method: http.MethodGet, path: "/config", statusCode: http.StatusOK, bodyContains: []string{`{"tokens_in_flight":0,"waiting_requests":0,"version":"9.9.9"`, `"cache_size":100`, `"cache_entries":0`, `"downstream_url":"` + vllmServer.URL + `"`, `"downstream_model":""`, `"system_prompt":"You are a detailed assistant."`, `"max_tokens":2500`, `"max_tokens_in_flight":200000`, `"max_waiting_requests":1024`, `"temperature":0.2`, `"dsl_default_profile":""`}},
		{name: "cache", method: http.MethodGet, path: "/cache", statusCode: http.StatusOK, bodyContains: []string{`"enabled":true`, `"cache_size":100`, `"cache_entries":0`, `"cache_file_path":"/var/lib/cllm/cache.json"`}},
		{name: "metrics", method: http.MethodGet, path: "/metrics", statusCode: http.StatusOK, bodyContains: []string{"# HELP cllm_http_requests_total", "cllm_http_inflight_requests", "cllm_queue_waiting_requests"}},
		{name: "models", method: http.MethodGet, path: "/v1/models", statusCode: http.StatusOK, body: `{"data":[{"id":"test-model"}]}`},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			originalVersion := buildinfo.Version
			buildinfo.Version = "9.9.9"
			defer func() { buildinfo.Version = originalVersion }()

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

func TestCacheEndpointListsKeysAndItemDetails(t *testing.T) {
	handler := NewHandler()
	handler.cache.Add("abc123", cachedVLLMResponse{
		statusCode:  http.StatusOK,
		body:        []byte(`{"choices":[{"message":{"content":"Explain Azure is a cloud platform."}}],"usage":{"completion_tokens":5}}`),
		contentType: "application/json",
	})
	router := handler.Routes()

	summaryRecorder := httptest.NewRecorder()
	router.ServeHTTP(summaryRecorder, httptest.NewRequest(http.MethodGet, "/cache", nil))

	if summaryRecorder.Code != http.StatusOK {
		t.Fatalf("summary status code = %d, want %d", summaryRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"key":"abc123"`, `"completion_tokens":5`, `"content_preview":"Explain Azure is a cloud platform."`} {
		if !strings.Contains(summaryRecorder.Body.String(), want) {
			t.Fatalf("summary body = %q, want substring %q", summaryRecorder.Body.String(), want)
		}
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/cache/abc123", nil))

	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status code = %d, want %d", detailRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"key":"abc123"`, `"completion_tokens":5`, `"content":"Explain Azure is a cloud platform."`, `"text_tokens":["Explain","Azure","is","a","cloud","platform."]`} {
		if !strings.Contains(detailRecorder.Body.String(), want) {
			t.Fatalf("detail body = %q, want substring %q", detailRecorder.Body.String(), want)
		}
	}
}

func TestCacheEndpointClearAndResize(t *testing.T) {
	handler := NewHandler()
	handler.cache.Add("first", cachedVLLMResponse{statusCode: http.StatusOK, body: []byte(`{"choices":[{"message":{"content":"one"}}],"usage":{"completion_tokens":1}}`), contentType: "application/json"})
	router := handler.Routes()

	clearRecorder := httptest.NewRecorder()
	router.ServeHTTP(clearRecorder, httptest.NewRequest(http.MethodGet, "/cache?action=clear", nil))
	if clearRecorder.Code != http.StatusOK {
		t.Fatalf("clear status code = %d, want %d", clearRecorder.Code, http.StatusOK)
	}
	if !strings.Contains(clearRecorder.Body.String(), `"cache_entries":0`) {
		t.Fatalf("clear body = %q, want cache_entries=0", clearRecorder.Body.String())
	}

	sizeRecorder := httptest.NewRecorder()
	router.ServeHTTP(sizeRecorder, httptest.NewRequest(http.MethodGet, "/cache?size=0", nil))
	if sizeRecorder.Code != http.StatusOK {
		t.Fatalf("size status code = %d, want %d", sizeRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"enabled":false`, `"cache_size":0`, `"cache_entries":0`} {
		if !strings.Contains(sizeRecorder.Body.String(), want) {
			t.Fatalf("size body = %q, want substring %q", sizeRecorder.Body.String(), want)
		}
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/cache/first", nil))
	if detailRecorder.Code != http.StatusNotFound {
		t.Fatalf("detail status code after disable = %d, want %d", detailRecorder.Code, http.StatusNotFound)
	}
}

func TestCacheEndpointSaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	handler := NewHandler()
	handler.SetCacheFilePath(tempDir + "/cache.json")
	handler.cache.Add("first", cachedVLLMResponse{statusCode: http.StatusCreated, body: []byte(`{"choices":[{"message":{"content":"saved value"}}],"usage":{"completion_tokens":2}}`), contentType: "application/json"})
	router := handler.Routes()

	saveRecorder := httptest.NewRecorder()
	router.ServeHTTP(saveRecorder, httptest.NewRequest(http.MethodGet, "/cache?action=save", nil))
	if saveRecorder.Code != http.StatusOK {
		t.Fatalf("save status code = %d, want %d", saveRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"name":"save"`, `"saved_entries":1`, `"cache_file_path":"` + tempDir + `/cache.json"`} {
		if !strings.Contains(saveRecorder.Body.String(), want) {
			t.Fatalf("save body = %q, want substring %q", saveRecorder.Body.String(), want)
		}
	}

	handler.cache.Clear()

	loadRecorder := httptest.NewRecorder()
	router.ServeHTTP(loadRecorder, httptest.NewRequest(http.MethodGet, "/cache?action=load", nil))
	if loadRecorder.Code != http.StatusOK {
		t.Fatalf("load status code = %d, want %d", loadRecorder.Code, http.StatusOK)
	}
	for _, want := range []string{`"name":"load"`, `"loaded_entries":1`, `"cache_entries":1`} {
		if !strings.Contains(loadRecorder.Body.String(), want) {
			t.Fatalf("load body = %q, want substring %q", loadRecorder.Body.String(), want)
		}
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/cache/first", nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status code after load = %d, want %d", detailRecorder.Code, http.StatusOK)
	}
	if !strings.Contains(detailRecorder.Body.String(), `"content":"saved value"`) {
		t.Fatalf("detail body = %q, want restored content", detailRecorder.Body.String())
	}
}

func TestLoadCacheFromDiskReplacesExistingCache(t *testing.T) {
	tempDir := t.TempDir()
	handler := NewHandler()
	handler.SetCacheFilePath(tempDir + "/cache.json")
	handler.cache.Add("persisted", cachedVLLMResponse{statusCode: http.StatusAccepted, body: []byte(`{"choices":[{"message":{"content":"persisted"}}],"usage":{"completion_tokens":3}}`), contentType: "application/json"})

	if savedEntries, _, err := handler.SaveCacheToDisk(); err != nil {
		t.Fatalf("SaveCacheToDisk() error = %v", err)
	} else if savedEntries != 1 {
		t.Fatalf("savedEntries = %d, want 1", savedEntries)
	}

	handler.cache.Add("transient", cachedVLLMResponse{statusCode: http.StatusOK, body: []byte(`{"choices":[{"message":{"content":"transient"}}],"usage":{"completion_tokens":1}}`), contentType: "application/json"})

	loadedEntries, _, err := handler.LoadCacheFromDisk()
	if err != nil {
		t.Fatalf("LoadCacheFromDisk() error = %v", err)
	}
	if loadedEntries != 1 {
		t.Fatalf("loadedEntries = %d, want 1", loadedEntries)
	}

	if _, ok := handler.cache.Peek("transient"); ok {
		t.Fatal("transient cache entry still present after replace load")
	}
	value, ok := handler.cache.Peek("persisted")
	if !ok {
		t.Fatal("persisted cache entry missing after load")
	}
	if value.statusCode != http.StatusAccepted {
		t.Fatalf("statusCode = %d, want %d", value.statusCode, http.StatusAccepted)
	}
}

func TestLoadCacheFromDiskIgnoresPersistedCapacity(t *testing.T) {
	tempDir := t.TempDir()
	handler := NewHandler()
	handler.SetCacheFilePath(tempDir + "/cache.json")

	cacheFileBody := []byte("{\n  \"version\": 1,\n  \"capacity\": 0,\n  \"entries\": [\n    {\n      \"key\": \"persisted\",\n      \"status_code\": 202,\n      \"body\": \"eyJjaG9pY2VzIjpbeyJtZXNzYWdlIjp7ImNvbnRlbnQiOiJ1c2UgcnVudGltZSBzaXplIn19XSwidXNhZ2UiOnsiY29tcGxldGlvbl90b2tlbnMiOjN9fQ==\",\n      \"content_type\": \"application/json\",\n      \"streaming\": false\n    }\n  ]\n}\n")
	if err := os.WriteFile(tempDir+"/cache.json", cacheFileBody, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loadedEntries, _, err := handler.LoadCacheFromDisk()
	if err != nil {
		t.Fatalf("LoadCacheFromDisk() error = %v", err)
	}
	if loadedEntries != 1 {
		t.Fatalf("loadedEntries = %d, want 1", loadedEntries)
	}

	capacity, entries := handler.cache.Stats()
	if capacity != 100 {
		t.Fatalf("capacity = %d, want 100", capacity)
	}
	if entries != 1 {
		t.Fatalf("entries = %d, want 1", entries)
	}

	value, ok := handler.cache.Peek("persisted")
	if !ok {
		t.Fatal("persisted cache entry missing after load")
	}
	if value.statusCode != http.StatusAccepted {
		t.Fatalf("statusCode = %d, want %d", value.statusCode, http.StatusAccepted)
	}
}

func TestSaveCacheToDiskOmitsCapacity(t *testing.T) {
	tempDir := t.TempDir()
	handler := NewHandler()
	handler.SetCacheFilePath(tempDir + "/cache.json")
	handler.cache.Add("persisted", cachedVLLMResponse{statusCode: http.StatusOK, body: []byte(`{"choices":[{"message":{"content":"saved"}}],"usage":{"completion_tokens":1}}`), contentType: "application/json"})

	if _, _, err := handler.SaveCacheToDisk(); err != nil {
		t.Fatalf("SaveCacheToDisk() error = %v", err)
	}

	body, err := os.ReadFile(tempDir + "/cache.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(body), `"capacity"`) {
		t.Fatalf("saved cache file unexpectedly contains capacity: %s", string(body))
	}
}

func TestBuildChatCompletionCacheKeyCanonicalizesEquivalentRequests(t *testing.T) {
	base := chatCompletionRequest{
		Model: "model-a",
		Messages: []chatCompletionMessage{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Explain Azure"},
		},
		Temperature: 0.2,
		MaxTokens:   4000,
		Stream:      true,
		StreamOptions: &chatCompletionStreamOptions{
			IncludeUsage: true,
		},
	}

	equivalent := chatCompletionRequest{
		Model: "model-b",
		Messages: []chatCompletionMessage{
			{Role: "system", Content: "Different system prompt entirely."},
			{Role: "user", Content: "Explain Azure"},
		},
		Temperature: 0.9,
		MaxTokens:   1234,
		Stream:      false,
	}

	different := chatCompletionRequest{
		Messages: []chatCompletionMessage{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Explain Kubernetes"},
		},
		Temperature: 0.2,
		MaxTokens:   4000,
		Stream:      true,
	}

	baseKey, err := buildChatCompletionCacheKey(base)
	if err != nil {
		t.Fatalf("buildChatCompletionCacheKey(base) error = %v", err)
	}

	equivalentKey, err := buildChatCompletionCacheKey(equivalent)
	if err != nil {
		t.Fatalf("buildChatCompletionCacheKey(equivalent) error = %v", err)
	}

	differentKey, err := buildChatCompletionCacheKey(different)
	if err != nil {
		t.Fatalf("buildChatCompletionCacheKey(different) error = %v", err)
	}

	if baseKey != equivalentKey {
		t.Fatalf("equivalent cache keys differ: %q != %q", baseKey, equivalentKey)
	}

	if baseKey == differentKey {
		t.Fatalf("different request produced same cache key: %q", baseKey)
	}
}

func TestMetricsCollapseStreamModeAndKeepChatCompletionsDurationRoute(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	nonStreamingBody := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	nonStreamingRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(nonStreamingBody))
	nonStreamingRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), nonStreamingRequest)

	streamingBody := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"world"}],"temperature":0.7,"max_tokens":700,"stream":true,"stream_options":{"include_usage":true}}`
	streamingRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamingBody))
	streamingRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), streamingRequest)

	metricsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(metricsRecorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if metricsRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", metricsRecorder.Code, http.StatusOK)
	}

	body := metricsRecorder.Body.String()
	for _, forbidden := range []string{
		`mode="stream"`,
		`mode="nonstream"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics body contains unexpected mode split %q", forbidden)
		}
	}

	for _, want := range []string{
		`cllm_http_request_duration_seconds_bucket{method="GET",route="other",status="200"`,
		`cllm_http_request_duration_seconds_bucket{method="POST",route="POST /v1/chat/completions",status="200"`,
		`cllm_time_to_first_byte_seconds_bucket{endpoint="chat_completions",node="default",source="downstream"`,
		`cllm_job_duration_seconds_bucket{endpoint="chat_completions",node="default",result="completed",source="downstream"`,
		`cllm_downstream_request_duration_seconds_bucket{endpoint="chat_completions",result="completed"`,
		`cllm_completion_tokens_total{endpoint="chat_completions",node="default",source="downstream"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing substring %q", want)
		}
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
	// Cost-based admission: each request charges prompt_tokens + max_tokens
	// (~705 here). Use a 2000-token budget so two requests fit.
	handler.SetRequestProcessingLimits(2000, 10)
	handler.SetPrefillSimulation(0, 0, 0, 1)
	handler.SetStreamRealism(0, 0, 0, 0, 0)
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
	// Item 16 (0.14.0): the implicit fallback Node now paces at
	// defaultPerRequestTPS = 32 tok/s/req (was the legacy global
	// --max-tokens-per-second=2 in 0.13).
	expected := time.Duration(float64(4) * float64(time.Second) / calibratedTokensPerSecond(32))
	if sleeps[0] != expected {
		t.Fatalf("sleep duration = %s, want %s", sleeps[0], expected)
	}
}

func TestCachedReplayDelayDegradesAfterConcurrencyThreshold(t *testing.T) {
	t.Skip("item 16 (0.14.0): legacy global degradation curve retired; per-node concurrency degradation is covered by TestCachedReplayDelayPerNodeDegradesAtConcurrency in prefill_per_node_test.go")
}

func TestCachedReplayDelayUsesWholeRequestThreshold(t *testing.T) {
	t.Skip("item 16 (0.14.0): legacy whole-request threshold retired; per-node MaxConcurrency now governs the regime boundary")
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

func TestConfigEndpointAcceptsSnakeCaseQueryNames(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config?cache_size=7&downstream_url="+url.QueryEscape(vllmServer.URL)+"&downstream_model=gpt-4.1&system_prompt=Be%20precise&max_tokens=700&max_tokens_in_flight=64&max_waiting_requests=96&downstream_token=secret-token", nil))

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
	if got.MaxTokensInFlight != 64 {
		t.Fatalf("max tokens in flight = %d, want %d", got.MaxTokensInFlight, 64)
	}
	if got.MaxWaitingRequests != 96 {
		t.Fatalf("max waiting requests = %d, want %d", got.MaxWaitingRequests, 96)
	}
}

func TestConfigEndpointUpdatesDownstreamTokenAndIgnoresStream(t *testing.T) {
	vllmServer, captured := newCapturingTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	router := handler.Routes()

	configRecorder := httptest.NewRecorder()
	router.ServeHTTP(configRecorder, httptest.NewRequest(http.MethodGet, "/config?downstream_token=updated-token&stream=true", nil))
	if configRecorder.Code != http.StatusOK {
		t.Fatalf("config status code = %d, want %d", configRecorder.Code, http.StatusOK)
	}
	if strings.Contains(configRecorder.Body.String(), `"stream"`) {
		t.Fatalf("config body unexpectedly contained stream field: %s", configRecorder.Body.String())
	}

	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(*captured))
	}
	if (*captured)[0].Authorization != "Bearer updated-token" {
		t.Fatalf("authorization = %q, want %q", (*captured)[0].Authorization, "Bearer updated-token")
	}
}

func TestConfigEndpointSetsAndClearsDSLDefaultProfile(t *testing.T) {
	handler := NewHandler()
	handler.SetDSLProfiles(map[string][]string{"fast": {"tps=99"}})
	router := handler.Routes()

	// Set
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config?dsl-profile=fast", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"dsl_default_profile":"fast"`) {
		t.Fatalf("expected dsl_default_profile=fast in body, got %s", rec.Body.String())
	}
	if got := handler.DSLDefaultProfile(); got != "fast" {
		t.Fatalf("DSLDefaultProfile() = %q, want fast", got)
	}

	// snake_case alias also accepted
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config?dsl_profile=FAST", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("snake_case set status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := handler.DSLDefaultProfile(); got != "fast" {
		t.Fatalf("DSLDefaultProfile() after snake_case = %q, want fast", got)
	}

	// Clear via empty value
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config?dsl-profile=", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := handler.DSLDefaultProfile(); got != "" {
		t.Fatalf("DSLDefaultProfile() after clear = %q, want empty", got)
	}
}

func TestConfigEndpointRejectsInvalidQueueValues(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "tokens-in-flight below min", path: "/config?max-tokens-in-flight=0", want: `max-tokens-in-flight must be between 1 and 2000000`},
		{name: "tokens-in-flight above max", path: "/config?max-tokens-in-flight=2000001", want: `max-tokens-in-flight must be between 1 and 2000000`},
		{name: "waiting below min", path: "/config?max-waiting-requests=-1", want: `max-waiting-requests must be between 0 and 1024`},
		{name: "waiting above max", path: "/config?max-waiting-requests=1025", want: `max-waiting-requests must be between 0 and 1024`},
		{name: "unknown dsl-profile", path: "/config?dsl-profile=does-not-exist", want: `unknown dsl-profile "does-not-exist"`},
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

	releaseOne, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/one")
	if !ok {
		t.Fatal("failed to acquire first direct slot")
	}
	releaseTwo, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/two")
	if !ok {
		t.Fatal("failed to acquire second direct slot")
	}

	type acquiredRelease struct {
		path    string
		release func()
	}
	acquired := make(chan acquiredRelease, 2)
	go func() {
		release, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/three")
		if ok {
			acquired <- acquiredRelease{path: "/three", release: release}
		}
	}()
	go func() {
		release, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/four")
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

	if _, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/five"); ok {
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
	if _, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/six"); ok {
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
	if release, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/seven"); !ok {
		t.Fatal("expected direct admission once both queues had space again")
	} else {
		release()
	}
}

func TestSchedulerLogsAdmissionAndCompletionLifecycle(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(originalLogger)

	scheduler := newRequestScheduler(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseOne, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/one")
	if !ok {
		t.Fatal("failed to acquire direct slot")
	}

	queuedAcquired := make(chan func(), 1)
	go func() {
		release, _, ok := scheduler.Acquire(ctx, node.RequestCost{TotalCost: 1}, "/two")
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
		`msg=admitted path=/one node=default source=direct`,
		`msg=queued path=/two node=default`,
		`msg=admitted path=/two node=default source=waiting_to_concurrent`,
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
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config?max-tokens-in-flight=64&max-waiting-requests=96", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusOK)
	}
	logOutput := logBuffer.String()
	for _, want := range []string{
		`msg="request queue limits updated"`,
		`previous_max_tokens_in_flight=200000`,
		`max_tokens_in_flight=64`,
		`previous_max_waiting_requests=1024`,
		`max_waiting_requests=96`,
		`tokens_in_flight=0`,
		`waiting_requests=0`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestRequestAdmissionLogsComputedDegradationChanges(t *testing.T) {
	t.Skip("item 16 (0.14.0): global computed_degradation_percentage log line retired; per-node degradation is observable via cllm_node_per_request_tps_effective")
}

func TestCurrentConfigIncludesComputedDegradation(t *testing.T) {
	t.Skip("item 16 (0.14.0): runtimeConfig.ComputedDegradationPercentage / EffectiveTokensPerSecond fields retired")
}

func TestCachedReplayDelayDisabledWhenTokensPerSecondZero(t *testing.T) {
	t.Skip("item 16 (0.14.0): legacy global --max-tokens-per-second retired; cachedReplayDelay disabling is now governed by per-node Capacity.PerRequestTPS == 0 (covered by passthrough tests)")
}

func TestLoadConfigCacheSize(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")
	t.Setenv("CACHE_CACHE_FILE_PATH", "/tmp/cache-from-env.json")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 123 {
		t.Fatalf("CacheSize = %d, want 123", cfg.CacheSize)
	}
	if cfg.CacheFilePath != "/tmp/cache-from-env.json" {
		t.Fatalf("CacheFilePath = %q, want %q", cfg.CacheFilePath, "/tmp/cache-from-env.json")
	}
}

func TestLoadConfigCacheFlagsPrecedence(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm", "--cache-size", "7", "--cache-file-path", "/tmp/cache-from-flag.json"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_CACHE_SIZE", "123")
	t.Setenv("CACHE_CACHE_FILE_PATH", "/tmp/cache-from-env.json")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if cfg.CacheSize != 7 {
		t.Fatalf("CacheSize = %d, want 7", cfg.CacheSize)
	}
	if cfg.CacheFilePath != "/tmp/cache-from-flag.json" {
		t.Fatalf("CacheFilePath = %q, want %q", cfg.CacheFilePath, "/tmp/cache-from-flag.json")
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

func TestLoadConfigAskDefaultsFromEnv(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cllm"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_MAX_TOKENS_IN_FLIGHT", "64")
	t.Setenv("CACHE_MAX_WAITING_REQUESTS", "95")
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
	if cfg.MaxTokensInFlight != 64 {
		t.Fatalf("MaxTokensInFlight = %d, want 64", cfg.MaxTokensInFlight)
	}
	if cfg.MaxWaitingRequests != 95 {
		t.Fatalf("MaxWaitingRequests = %d, want 95", cfg.MaxWaitingRequests)
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
	os.Args = []string{"cllm", "--downstream-url", "https://example.test", "--downstream-token", "flag-token", "--downstream-model", "flag-model", "--system-prompt", "Be precise", "--max-tokens", "900", "--max-tokens-in-flight", "16", "--max-waiting-requests", "31", "--temperature", "0.9"}

	t.Setenv("CACHE_PORT", "8080")
	t.Setenv("CACHE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("CACHE_SYSTEM_PROMPT", "Be concise")
	t.Setenv("CACHE_MAX_TOKENS", "700")
	t.Setenv("CACHE_MAX_TOKENS_IN_FLIGHT", "64")
	t.Setenv("CACHE_MAX_WAITING_REQUESTS", "95")
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
	if cfg.MaxTokensInFlight != 16 {
		t.Fatalf("MaxTokensInFlight = %d, want 16", cfg.MaxTokensInFlight)
	}
	if cfg.MaxWaitingRequests != 31 {
		t.Fatalf("MaxWaitingRequests = %d, want 31", cfg.MaxWaitingRequests)
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
		{name: "flag max tokens in flight too low", args: []string{"cllm", "--max-tokens-in-flight", "0"}, wantErr: "max-tokens-in-flight must be between 1 and 2000000"},
		{name: "flag max tokens in flight too high", args: []string{"cllm", "--max-tokens-in-flight", "2000001"}, wantErr: "max-tokens-in-flight must be between 1 and 2000000"},
		{name: "flag max waiting requests negative", args: []string{"cllm", "--max-waiting-requests", "-1"}, wantErr: "max-waiting-requests must be between 0 and 1024"},
		{name: "flag max waiting requests too high", args: []string{"cllm", "--max-waiting-requests", "1025"}, wantErr: "max-waiting-requests must be between 0 and 1024"},
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
	MaxTokens     int
	Model         string
	SystemPrompt  string
	Temperature   float64
	UserContent   string
	Stream        bool
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
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + content + `"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
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
				Model:         requestBody.Model,
				SystemPrompt:  requestBody.Messages[0].Content,
				UserContent:   requestBody.Messages[1].Content,
				Temperature:   requestBody.Temperature,
				MaxTokens:     requestBody.MaxTokens,
				Stream:        requestBody.Stream,
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
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + requestBody.Messages[1].Content + `"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
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

func TestRequestIDValidation(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"abc-123_xyz", "abc-123_xyz"},
		{"has space", ""},
		{"has/slash", ""},
		{strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{strings.Repeat("a", 129), ""},
	}
	for _, tc := range tests {
		if got := validateRequestID(tc.input); got != tc.want {
			t.Errorf("validateRequestID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNewULIDFormat(t *testing.T) {
	id := newULID()
	if len(id) != 26 {
		t.Fatalf("len = %d, want 26", len(id))
	}
	for _, c := range id {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			t.Fatalf("char %q not in crockford alphabet", c)
		}
	}
}

func TestRequestLoggerSetsRequestIDResponseHeader(t *testing.T) {
	handler := NewHandler().Routes()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	if got := recorder.Header().Get(requestIDHeader); got == "" || len(got) != 26 {
		t.Fatalf("X-Request-ID header = %q, want generated 26-char ULID", got)
	}
}

func TestRequestLoggerPreservesValidInboundRequestID(t *testing.T) {
	handler := NewHandler().Routes()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "client-supplied-123")
	handler.ServeHTTP(recorder, req)
	if got := recorder.Header().Get(requestIDHeader); got != "client-supplied-123" {
		t.Fatalf("X-Request-ID header = %q, want %q", got, "client-supplied-123")
	}
}

func TestRequestLoggerRegeneratesInvalidInboundRequestID(t *testing.T) {
	handler := NewHandler().Routes()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "has space")
	handler.ServeHTTP(recorder, req)
	got := recorder.Header().Get(requestIDHeader)
	if got == "has space" || len(got) != 26 {
		t.Fatalf("X-Request-ID header = %q, want regenerated 26-char ULID", got)
	}
}

func TestRequestLoggerSkipsRequestIDForNonChatEndpoints(t *testing.T) {
	handler := NewHandler().Routes()
	for _, path := range []string{"/health", "/ready", "/metrics", "/version", "/config", "/cache"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Header().Get(requestIDHeader); got != "" {
			t.Fatalf("path %s: X-Request-ID header = %q, want empty", path, got)
		}
	}
}

func TestChatCompletionsLifecycleEventsLogged(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(originalLogger)

	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}).Routes()
	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "lifecycle-test-id")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	logOutput := logBuffer.String()
	for _, want := range []string{
		`request_id=lifecycle-test-id`,
		`msg=admitted`,
		`msg="request started"`,
		`event=started`,
		`msg="first token emitted"`,
		`event=first_token`,
		`source=downstream`,
		`msg="request completed"`,
		`event=completed outcome=completed`,
		`prompt_tokens=5`,
		`completion_tokens=1`,
		`max_tokens=700`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestChatCompletionsRejectedOverCapacityEmitsLifecycleEvent(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	handler := NewHandler()
	handler.SetRequestProcessingLimits(1, 0)
	release, _, ok := handler.acquireRequestSlot(context.Background(), node.RequestCost{TotalCost: 1}, "/v1/chat/completions")
	if !ok {
		t.Fatal("failed to acquire baseline slot")
	}
	defer release()

	router := handler.Routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "rejected-id")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get(requestIDHeader); got != "rejected-id" {
		t.Fatalf("X-Request-ID = %q, want %q", got, "rejected-id")
	}
	logOutput := logBuffer.String()
	for _, want := range []string{
		`request_id=rejected-id`,
		`msg="request rejected"`,
		`event=rejected outcome=over_capacity`,
		`status=429`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output %q does not contain %q", logOutput, want)
		}
	}
}

func TestChatCompletionsRejectedBadRequestEmitsLifecycleEvent(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	handler := NewHandler().Routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	logOutput := logBuffer.String()
	if !strings.Contains(logOutput, `msg="request rejected"`) || !strings.Contains(logOutput, `event=rejected outcome=bad_request`) {
		t.Fatalf("log output missing rejected event: %q", logOutput)
	}
}

func TestLifecycleEventsCounter(t *testing.T) {
	vllmServer := newTestVLLMServer(t)
	handler := NewHandlerWithDependencies(vllmServer.URL, vllmServer.Client(), 100, askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature})
	router := handler.Routes()
	body := `{"messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"hello"}],"temperature":0.7,"max_tokens":700}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)

	metricsRecorder := httptest.NewRecorder()
	router.ServeHTTP(metricsRecorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricsBody := metricsRecorder.Body.String()
	for _, want := range []string{
		`cllm_request_lifecycle_events_total{endpoint="chat_completions",event="admitted",outcome=""} 1`,
		`cllm_request_lifecycle_events_total{endpoint="chat_completions",event="first_token",outcome=""} 1`,
		`cllm_request_lifecycle_events_total{endpoint="chat_completions",event="completed",outcome="completed"} 1`,
	} {
		if !strings.Contains(metricsBody, want) {
			t.Fatalf("metrics body missing %q\nbody:\n%s", want, metricsBody)
		}
	}
}

func TestCacheKeyIgnoresSystemMessageAndParameters(t *testing.T) {
	a := chatCompletionRequest{
		Model: "x",
		Messages: []chatCompletionMessage{
			{Role: "system", Content: "Be brief"},
			{Role: "user", Content: "hi"},
		},
		Temperature: 0.1, MaxTokens: 100, Stream: false,
	}
	b := chatCompletionRequest{
		Model: "y",
		Messages: []chatCompletionMessage{
			{Role: "system", Content: "Wholly different system prompt"},
			{Role: "user", Content: "hi"},
		},
		Temperature: 0.9, MaxTokens: 5000, Stream: true,
		StreamOptions: &chatCompletionStreamOptions{IncludeUsage: true},
	}
	c := chatCompletionRequest{
		Messages: []chatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	keyA, _ := buildChatCompletionCacheKey(a)
	keyB, _ := buildChatCompletionCacheKey(b)
	keyC, _ := buildChatCompletionCacheKey(c)
	if keyA != keyB || keyA != keyC {
		t.Fatalf("expected same key, got A=%s B=%s C=%s", keyA, keyB, keyC)
	}
}

func TestReplayCachedStreamTruncatesAtMaxTokens(t *testing.T) {
	cached := cachedVLLMResponse{
		statusCode:  200,
		contentType: "text/event-stream",
		streaming:   true,
		body: []byte("" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"c\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"d\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":4,\"total_tokens\":6}}\n\n" +
			"data: [DONE]\n\n"),
	}
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	recorder := httptest.NewRecorder()
	handler.replayCachedStream(context.Background(), recorder, cached, replayOptions{maxTokens: 2, includeUsage: true, stream: true})
	body := recorder.Body.String()
	contentDeltas := strings.Count(body, `"content":`)
	if contentDeltas != 2 {
		t.Fatalf("expected 2 content deltas, got %d in %q", contentDeltas, body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish_reason=stop in %q", body)
	}
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("missing usage chunk in %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("missing [DONE] terminator in %q", body)
	}
}

func TestReplayCachedStreamFromJSONCache(t *testing.T) {
	cached := cachedVLLMResponse{
		statusCode:  200,
		contentType: "application/json",
		streaming:   false,
		body:        []byte(`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`),
	}
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	recorder := httptest.NewRecorder()
	handler.replayCachedStream(context.Background(), recorder, cached, replayOptions{maxTokens: 0, includeUsage: true, stream: true})
	body := recorder.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role chunk in %q", body)
	}
	if strings.Count(body, `"content":`) != 3 {
		t.Fatalf("expected 3 content chunks for completion_tokens=3, got %d in %q", strings.Count(body, `"content":`), body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish chunk in %q", body)
	}
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("missing usage chunk in %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE] in %q", body)
	}
}

func TestReplayCachedResponseFromSSECache(t *testing.T) {
	cached := cachedVLLMResponse{
		statusCode:  200,
		contentType: "text/event-stream",
		streaming:   true,
		body: []byte("" +
			"data: {\"id\":\"x\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"x\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello \"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"x\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"x\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n" +
			"data: [DONE]\n\n"),
	}
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	recorder := httptest.NewRecorder()
	handler.replayCachedResponse(context.Background(), recorder, cached, replayOptions{maxTokens: 0, stream: false})
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, recorder.Body.String())
	}
	choices := parsed["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Fatalf("content = %v, want %q", msg["content"], "hello world")
	}
	usage := parsed["usage"].(map[string]any)
	if usage["completion_tokens"].(float64) != 2 {
		t.Fatalf("completion_tokens = %v, want 2", usage["completion_tokens"])
	}
}

func TestReplayCachedResponseCapsPacingAtMaxTokens(t *testing.T) {
	cached := cachedVLLMResponse{
		statusCode:  200,
		contentType: "application/json",
		streaming:   false,
		body:        []byte(`{"choices":[{"message":{"role":"assistant","content":"abc"}}],"usage":{"completion_tokens":100}}`),
	}
	handler := NewHandlerWithDependencies("", nil, 1, askOptions{})
	handler.SetRequestProcessingLimits(10, 10)
	var sleeps []time.Duration
	handler.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	recorder := httptest.NewRecorder()
	handler.replayCachedResponse(context.Background(), recorder, cached, replayOptions{maxTokens: 5})
	if len(sleeps) != 1 {
		t.Fatalf("expected 1 sleep, got %d", len(sleeps))
	}
	// Item 16 (0.14.0): default fallback paces at 32 tok/s/req.
	expected := time.Duration(float64(5) * float64(time.Second) / calibratedTokensPerSecond(32))
	if sleeps[0] != expected {
		t.Fatalf("sleep = %s, want %s", sleeps[0], expected)
	}
}

func TestCacheKeyFuzzyMatchesEquivalentPrompts(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"case_and_punctuation", "Explain Azure!", "explain azure"},
		{"stop_words", "Please explain Azure to me", "explain azure"},
		{"word_order", "Azure explain", "explain Azure"},
		{"extra_whitespace", "  explain   azure  ", "explain azure"},
		{"verbose_with_stops", "Could you please explain what Azure is?", "explain azure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyA, err := buildChatCompletionCacheKey(chatCompletionRequest{
				Messages: []chatCompletionMessage{{Role: "user", Content: tc.a}},
			})
			if err != nil {
				t.Fatalf("buildChatCompletionCacheKey(a): %v", err)
			}
			keyB, err := buildChatCompletionCacheKey(chatCompletionRequest{
				Messages: []chatCompletionMessage{{Role: "user", Content: tc.b}},
			})
			if err != nil {
				t.Fatalf("buildChatCompletionCacheKey(b): %v", err)
			}
			if keyA != keyB {
				t.Fatalf("expected same key for %q and %q, got %s vs %s", tc.a, tc.b, keyA, keyB)
			}
		})
	}
}

func TestCacheKeyDistinguishesDifferentPrompts(t *testing.T) {
	keyA, _ := buildChatCompletionCacheKey(chatCompletionRequest{
		Messages: []chatCompletionMessage{{Role: "user", Content: "Explain Azure"}},
	})
	keyB, _ := buildChatCompletionCacheKey(chatCompletionRequest{
		Messages: []chatCompletionMessage{{Role: "user", Content: "Explain Kubernetes"}},
	})
	if keyA == keyB {
		t.Fatalf("expected different keys for distinct prompts, got %s", keyA)
	}
}

func TestComputePrefillDelayScalesWithPromptTokens(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(2.0, 50, 0, 60000)
	handler.jitterSource = func() float64 { return 0 }

	d10 := handler.computePrefillDelay(10, replayOverrides{})
	d100 := handler.computePrefillDelay(100, replayOverrides{})
	// Item 16 (0.14.0): the implicit fallback Node paces at
	// defaultPerRequestTPS = 32; prefill rate is that x multiplier.
	rate := calibratedTokensPerSecond(32) * 2.0
	want10 := 50*time.Millisecond + time.Duration(float64(10)/rate*float64(time.Second))
	want100 := 50*time.Millisecond + time.Duration(float64(100)/rate*float64(time.Second))
	if d10 != want10 {
		t.Fatalf("d10 = %s, want %s", d10, want10)
	}
	if d100 != want100 {
		t.Fatalf("d100 = %s, want %s", d100, want100)
	}
}

func TestComputePrefillDelayDisabledWhenMultiplierZero(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(0, 100, 0, 60000)
	if d := handler.computePrefillDelay(500, replayOverrides{}); d != 0 {
		t.Fatalf("delay = %s, want 0 when multiplier=0", d)
	}
}

func TestComputePrefillDelayDisabledWhenDecodeRateZero(t *testing.T) {
	t.Skip("item 16 (0.14.0): legacy global --max-tokens-per-second retired; the implicit fallback Node always has PerRequestTPS=32. Passthrough nodes (PerRequestTPS=0) are exercised in prefill_per_node_test.go.")
}

func TestComputePrefillDelayCappedAtMax(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(0.5, 0, 0, 200)
	handler.jitterSource = func() float64 { return 0 }
	if d := handler.computePrefillDelay(1000000, replayOverrides{}); d != 200*time.Millisecond {
		t.Fatalf("delay = %s, want 200ms", d)
	}
}

func TestSimulatePrefillDelayHonorsContextCancel(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(1.0, 1000, 0, 60000)
	handler.jitterSource = func() float64 { return 0 }
	handler.sleep = func(_ context.Context, _ time.Duration) error {
		return context.Canceled
	}
	delay, err := handler.simulatePrefillDelay(context.Background(), 10, replayOverrides{})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if delay <= 0 {
		t.Fatalf("delay = %s, want > 0", delay)
	}
}

func TestComputePrefillDelayAppliesJitter(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetPrefillSimulation(1.0, 100, 50, 60000)
	handler.jitterSource = func() float64 { return 1.0 } // +50%
	dHigh := handler.computePrefillDelay(0, replayOverrides{})
	handler.jitterSource = func() float64 { return -1.0 } // -50%
	dLow := handler.computePrefillDelay(0, replayOverrides{})
	if dHigh <= dLow {
		t.Fatalf("expected high (%s) > low (%s)", dHigh, dLow)
	}
}

func TestComputeStreamSegmentDelayBaseline(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(0, 0, 0, 0, 0)
	handler.jitterSource = func() float64 { return 0 }
	delay, stall := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})
	if stall != 0 {
		t.Fatalf("stall = %s, want 0", stall)
	}
	want := handler.cachedReplayDelay(10, 0, replayOverrides{})
	if delay != want {
		t.Fatalf("delay = %s, want %s", delay, want)
	}
}

func TestComputeStreamSegmentDelayDisabledWhenRateZero(t *testing.T) {
	t.Skip("item 16 (0.14.0): legacy global rate=0 disable retired; the implicit fallback Node always has PerRequestTPS=32. Passthrough nodes are exercised in prefill_per_node_test.go.")
}

func TestComputeStreamSegmentDelayVariabilitySigns(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(50, 0, 0, 0, 0)
	base := handler.cachedReplayDelay(10, 0, replayOverrides{})

	handler.jitterSource = func() float64 { return 1 }
	high, _ := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})

	handler.jitterSource = func() float64 { return -1 }
	low, _ := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})

	if !(low < base && base < high) {
		t.Fatalf("expected low(%s) < base(%s) < high(%s)", low, base, high)
	}
}

func TestComputeStreamSegmentDelayStallAlwaysFires(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(0, 0, 100, 100, 100)
	// Variability/jitter zero so first two draws are no-ops; stall draws use 3rd & 4th.
	calls := 0
	handler.jitterSource = func() float64 {
		calls++
		return 0 // (0+1)/2 = 0.5 — but with prob=100 we always stall; r=0.5 -> midpoint
	}
	delay, stall := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})
	if stall != 100*time.Millisecond {
		t.Fatalf("stall = %s, want 100ms (min==max)", stall)
	}
	base := handler.cachedReplayDelay(10, 0, replayOverrides{})
	if delay != base+stall {
		t.Fatalf("delay = %s, want %s", delay, base+stall)
	}
	if calls < 2 {
		t.Fatalf("jitter draws = %d, want >= 2", calls)
	}
}

func TestComputeStreamSegmentDelayStallNeverFires(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(0, 0, 0, 100, 200)
	handler.jitterSource = func() float64 { return 1 }
	_, stall := handler.computeStreamSegmentDelay(10, 0, replayOverrides{})
	if stall != 0 {
		t.Fatalf("stall = %s, want 0 when probability=0", stall)
	}
}

func TestThrottleStreamSegmentRecordsStallMetric(t *testing.T) {
	handler := NewHandler()
	handler.SetRequestProcessingLimits(10, 10)
	handler.SetStreamRealism(0, 0, 100, 50, 50)
	handler.jitterSource = func() float64 { return 0 }
	handler.sleep = func(_ context.Context, _ time.Duration) error { return nil }
	if err := handler.throttleStreamSegment(context.Background(), 5, 0, replayOverrides{}); err != nil {
		t.Fatalf("err = %v", err)
	}
}
