package httpapi

import (
	"bytes"
	"context"
	"container/list"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"unicode"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultVLLMURL          = "http://127.0.0.1:32080"
	defaultCacheSize        = 100
	defaultSystemPrompt     = "You are a concise assistant."
	minMaxTokens            = 100
	maxMaxTokens            = 10000
	defaultMaxTokens        = 500
	defaultTemperature      = 0.2
	defaultVLLMHTTPTimeout  = 30 * time.Second
)

type askOptions struct {
	systemPrompt string
	maxTokens    int
	temperature  float64
}

func NewAskOptions(systemPrompt string, maxTokens int, temperature float64) askOptions {
	return askOptions{
		systemPrompt: systemPrompt,
		maxTokens:    maxTokens,
		temperature:  temperature,
	}
}

type Handler struct {
	ready      atomic.Bool
	cache      *lruCache
	defaults   askOptions
	vllmURL    string
	httpClient *http.Client
}

func NewHandler() *Handler {
	handler := &Handler{}
	handler.ready.Store(true)
	handler.cache = newLRUCache(defaultCacheSize)
	handler.defaults = askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	handler.vllmURL = defaultVLLMURL
	handler.httpClient = &http.Client{Timeout: defaultVLLMHTTPTimeout}
	return handler
}

func NewHandlerWithDependencies(vllmURL string, httpClient *http.Client, cacheSize int, defaults askOptions) *Handler {
	handler := NewHandler()
	if vllmURL != "" {
		handler.vllmURL = vllmURL
	}
	if httpClient != nil {
		handler.httpClient = httpClient
	}
	if cacheSize == 0 {
		handler.cache = nil
	}
	if cacheSize > 0 {
		handler.cache = newLRUCache(cacheSize)
	}
	if defaults.systemPrompt != "" {
		handler.defaults = defaults
	}
	return handler
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /ask", h.ask)
	return requestLogger(mux)
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writePlainText(w, http.StatusOK, "ok\n")
}

func (h *Handler) readyz(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		writePlainText(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}

	writePlainText(w, http.StatusOK, "ready\n")
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "missing q\n")
		return
	}

	options, err := parseAskOptions(r, h.defaults)
	if err != nil {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}

	cacheKey := buildCacheKey(query, options)
	if cacheKey == "" {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "missing q\n")
		return
	}

	if h.cache != nil {
		if cachedResponse, ok := h.cache.Get(cacheKey); ok {
			markCacheHit(w, true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cachedResponse.statusCode)
			_, _ = w.Write(cachedResponse.body)
			return
		}
	}

	markCacheHit(w, false)

	model, err := h.fetchModel(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch vllm model: %v", err), http.StatusBadGateway)
		return
	}

	responseBody, statusCode, err := h.createChatCompletion(r.Context(), model, query, options)
	if err != nil {
		http.Error(w, fmt.Sprintf("query vllm: %v", err), http.StatusBadGateway)
		return
	}

	if h.cache != nil {
		h.cache.Add(cacheKey, cachedVLLMResponse{statusCode: statusCode, body: responseBody})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(responseBody)
}

func parseAskOptions(r *http.Request, defaults askOptions) (askOptions, error) {
	options := defaults

	queryValues := r.URL.Query()

	if systemPrompt := queryValues.Get("system-prompt"); systemPrompt != "" {
		options.systemPrompt = systemPrompt
	}

	if maxTokensRaw := queryValues.Get("max-tokens"); maxTokensRaw != "" {
		maxTokens, err := strconv.Atoi(maxTokensRaw)
		if err != nil {
			return askOptions{}, fmt.Errorf("invalid max-tokens %q", maxTokensRaw)
		}
		if maxTokens < minMaxTokens || maxTokens > maxMaxTokens {
			return askOptions{}, fmt.Errorf("max-tokens must be between %d and %d", minMaxTokens, maxMaxTokens)
		}
		options.maxTokens = maxTokens
	}

	if temperatureRaw := queryValues.Get("temperature"); temperatureRaw != "" {
		temperature, err := strconv.ParseFloat(temperatureRaw, 64)
		if err != nil {
			return askOptions{}, fmt.Errorf("invalid temperature %q", temperatureRaw)
		}
		options.temperature = temperature
	}

	return options, nil
}

func buildCacheKey(query string, options askOptions) string {
	queryKey := standardizeCacheKey(query)
	if queryKey == "" {
		return ""
	}

	return fmt.Sprintf("%s|%s|%d|%.6f", queryKey, standardizeCacheKey(options.systemPrompt), options.maxTokens, options.temperature)
}

func standardizeCacheKey(query string) string {
	var builder strings.Builder
	builder.Grow(len(query))

	for _, char := range strings.ToLower(query) {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			builder.WriteRune(char)
		}
	}

	return builder.String()
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type chatCompletionRequest struct {
	Model       string                   `json:"model"`
	Messages    []chatCompletionMessage  `json:"messages"`
	Temperature float64                  `json:"temperature"`
	MaxTokens   int                      `json:"max_tokens"`
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (h *Handler) fetchModel(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.vllmURL+"/v1/models", nil)
	if err != nil {
		return "", fmt.Errorf("build models request: %w", err)
	}

	response, err := h.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("send models request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return "", fmt.Errorf("models request failed with HTTP %d: %s", response.StatusCode, bytes.TrimSpace(body))
	}

	var models modelsResponse
	if err := json.NewDecoder(response.Body).Decode(&models); err != nil {
		return "", fmt.Errorf("decode models response: %w", err)
	}
	if len(models.Data) == 0 || models.Data[0].ID == "" {
		return "", fmt.Errorf("models response did not include a model id")
	}

	return models.Data[0].ID, nil
}

func (h *Handler) createChatCompletion(ctx context.Context, model, query string, options askOptions) ([]byte, int, error) {
	requestPayload := chatCompletionRequest{
		Model: model,
		Messages: []chatCompletionMessage{
			{Role: "system", Content: options.systemPrompt},
			{Role: "user", Content: query},
		},
		Temperature: options.temperature,
		MaxTokens:   options.maxTokens,
	}

	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal chat request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.vllmURL+"/v1/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		return nil, 0, fmt.Errorf("build chat request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := h.httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("send chat request: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read chat response: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("chat request failed with HTTP %d: %s", response.StatusCode, bytes.TrimSpace(responseBody))
	}

	return responseBody, response.StatusCode, nil
}

func writePlainText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

type loggingResponseWriter struct {
	http.ResponseWriter
	cacheHit   bool
	statusCode int
	bytes      int
}

func (w *loggingResponseWriter) SetCacheHit(cacheHit bool) {
	w.cacheHit = cacheHit
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	bytesWritten, err := w.ResponseWriter.Write(body)
	w.bytes += bytesWritten
	return bytesWritten, err
}

func markCacheHit(w http.ResponseWriter, cacheHit bool) {
	cacheAwareWriter, ok := w.(interface{ SetCacheHit(bool) })
	if !ok {
		return
	}

	cacheAwareWriter.SetCacheHit(cacheHit)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		responseWriter := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(responseWriter, r)

		statusCode := responseWriter.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"status", statusCode,
			"bytes", responseWriter.bytes,
			"cache", responseWriter.cacheHit,
			"duration_ms", float64(time.Since(startedAt))/float64(time.Millisecond),
		}

		switch {
		case statusCode == http.StatusNotFound:
			slog.Warn("not found", attrs...)
		case statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError:
			slog.Warn("client error", attrs...)
		case statusCode >= http.StatusInternalServerError:
			slog.Error("server error", attrs...)
		default:
			slog.Info("request completed", attrs...)
		}
	})
}

type cachedVLLMResponse struct {
	statusCode int
	body       []byte
}

type lruCache struct {
	capacity int
	entries  map[string]*list.Element
	items    *list.List
	mu       sync.Mutex
}

type cacheEntry struct {
	key   string
	value cachedVLLMResponse
}

func newLRUCache(capacity int) *lruCache {
	if capacity < 1 {
		capacity = defaultCacheSize
	}

	return &lruCache{
		capacity: capacity,
		entries:  make(map[string]*list.Element, capacity),
		items:    list.New(),
	}
}

func (c *lruCache) Get(key string) (cachedVLLMResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return cachedVLLMResponse{}, false
	}

	c.items.MoveToFront(element)
	entry := element.Value.(*cacheEntry)
	return cachedVLLMResponse{
		statusCode: entry.value.statusCode,
		body:       append([]byte(nil), entry.value.body...),
	}, true
}

func (c *lruCache) Add(key string, value cachedVLLMResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[key]; ok {
		c.items.MoveToFront(element)
		entry := element.Value.(*cacheEntry)
		entry.value = cachedVLLMResponse{statusCode: value.statusCode, body: append([]byte(nil), value.body...)}
		return
	}

	element := c.items.PushFront(&cacheEntry{
		key: key,
		value: cachedVLLMResponse{
			statusCode: value.statusCode,
			body:       append([]byte(nil), value.body...),
		},
	})
	c.entries[key] = element

	if c.items.Len() > c.capacity {
		oldest := c.items.Back()
		if oldest != nil {
			c.items.Remove(oldest)
			entry := oldest.Value.(*cacheEntry)
			delete(c.entries, entry.key)
			slog.Info("cache evict", "size", c.items.Len(), "capacity", c.capacity)
		}
	}

	slog.Info("cache insert", "size", c.items.Len(), "capacity", c.capacity)
}
