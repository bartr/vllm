package httpapi

import (
	"bytes"
	"context"
	"container/list"
	"crypto/rand"
	"encoding/json"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	defaultModelsCacheTTL   = time.Hour
	defaultSystemPrompt     = "You are a helpful assistant."
	minMaxTokens            = 100
	maxMaxTokens            = 4000
	defaultMaxTokens        = 4000
	defaultTemperature      = 0.2
	defaultVLLMHTTPTimeout  = 120 * time.Second
)

type askOptions struct {
	systemPrompt string
	maxTokens    int
	temperature  float64
	stream       bool
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
	configMu   sync.RWMutex
	defaults   askOptions
	replayDelay time.Duration
	vllmURL    string
	httpClient *http.Client
	sleep      func(time.Duration)
	now        func() time.Time
	modelsMu   sync.RWMutex
	modelsCache *cachedModelsResponse
	modelsCacheTTL time.Duration
}

func NewHandler() *Handler {
	handler := &Handler{}
	handler.ready.Store(true)
	handler.cache = newLRUCache(defaultCacheSize)
	handler.defaults = askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	handler.vllmURL = defaultVLLMURL
	handler.httpClient = &http.Client{Timeout: defaultVLLMHTTPTimeout}
	handler.sleep = time.Sleep
	handler.now = time.Now
	handler.replayDelay = 0
	handler.modelsCacheTTL = defaultModelsCacheTTL
	return handler
}

func (h *Handler) SetModelsCacheTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	h.modelsMu.Lock()
	h.modelsCacheTTL = ttl
	h.modelsMu.Unlock()
}

func (h *Handler) SetReplayDelay(delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	h.replayDelay = delay
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
	mux.HandleFunc("GET /config", h.config)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /ask", h.ask)
	mux.HandleFunc("GET /v1/models", h.models)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	return requestLogger(mux)
}

type runtimeConfig struct {
	SystemPrompt   string  `json:"system_prompt"`
	MaxTokens      int     `json:"max_tokens"`
	Temperature    float64 `json:"temperature"`
	Stream         bool    `json:"stream"`
	ReplayDelay    string  `json:"replay_delay"`
	ModelsCacheTTL string  `json:"models_cache_ttl"`
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

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	if _, err := h.applyConfigQuery(r); err != nil {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}

	markCacheHit(w, false)
	writeJSON(w, http.StatusOK, h.currentConfig())
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	body, statusCode, contentType, err := h.fetchModels(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch vllm models: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var requestPayload chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&requestPayload); err != nil {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v\n", err))
		return
	}

	if len(requestPayload.Messages) == 0 {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "missing messages\n")
		return
	}

	requestPayload, err := h.populateChatCompletionDefaults(r.Context(), requestPayload)
	if err != nil {
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("prepare chat completion: %v", err), http.StatusBadGateway)
		return
	}

	cacheKey, err := buildChatCompletionCacheKey(requestPayload)
	if err != nil {
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("cache chat completion request: %v", err), http.StatusInternalServerError)
		return
	}

	if h.cache != nil {
		if cachedResponse, ok := h.cache.Get(cacheKey); ok {
			markCacheHit(w, true)
			if cachedResponse.streaming {
				h.replayCachedStream(r.Context(), w, cachedResponse, h.getReplayDelay())
				return
			}

			h.replayCachedResponse(r.Context(), w, cachedResponse, h.getReplayDelay())
			return
		}
	}

	markCacheHit(w, false)

	if requestPayload.Stream {
		cachedResponse, err := h.streamChatCompletion(r.Context(), w, requestPayload)
		if err != nil {
			http.Error(w, fmt.Sprintf("query vllm: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil {
			h.cache.Add(cacheKey, cachedResponse)
		}
		return
	}

	responseBody, statusCode, contentType, err := h.createChatCompletion(r.Context(), requestPayload)
	if err != nil {
		http.Error(w, fmt.Sprintf("query vllm: %v", err), http.StatusBadGateway)
		return
	}

	if h.cache != nil {
		h.cache.Add(cacheKey, cachedVLLMResponse{statusCode: statusCode, body: responseBody, contentType: contentType})
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	_, _ = w.Write(responseBody)
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "missing q\n")
		return
	}

	options, err := parseAskOptions(r, h.getDefaults())
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
			if cachedResponse.streaming {
				h.replayCachedStream(r.Context(), w, cachedResponse, h.getReplayDelay())
				return
			}

			h.replayCachedResponse(r.Context(), w, cachedResponse, h.getReplayDelay())
			return
		}
	}

	markCacheHit(w, false)

	model, err := h.fetchModel(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch vllm model: %v", err), http.StatusBadGateway)
		return
	}

	requestPayload := chatCompletionRequest{
		Model: model,
		Messages: []chatCompletionMessage{
			{Role: "system", Content: options.systemPrompt},
			{Role: "user", Content: query},
		},
		Temperature: options.temperature,
		MaxTokens:   options.maxTokens,
		Stream:      options.stream,
	}
	if options.stream {
		requestPayload.StreamOptions = &chatCompletionStreamOptions{IncludeUsage: true}
	}

	if options.stream {
		cachedResponse, err := h.streamChatCompletion(r.Context(), w, requestPayload)
		if err != nil {
			http.Error(w, fmt.Sprintf("query vllm: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil {
			h.cache.Add(cacheKey, cachedResponse)
		}
		return
	}

	responseBody, statusCode, contentType, err := h.createChatCompletion(r.Context(), requestPayload)
	if err != nil {
		http.Error(w, fmt.Sprintf("query vllm: %v", err), http.StatusBadGateway)
		return
	}

	if h.cache != nil {
		h.cache.Add(cacheKey, cachedVLLMResponse{statusCode: statusCode, body: responseBody, contentType: contentType})
	}

	w.Header().Set("Content-Type", contentType)
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

	if streamRaw := queryValues.Get("stream"); streamRaw != "" {
		stream, err := strconv.ParseBool(streamRaw)
		if err != nil {
			return askOptions{}, fmt.Errorf("invalid stream %q", streamRaw)
		}
		options.stream = stream
	}

	return options, nil
}

func (h *Handler) getDefaults() askOptions {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	return h.defaults
}

func (h *Handler) getReplayDelay() time.Duration {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	return h.replayDelay
}

func (h *Handler) currentConfig() runtimeConfig {
	defaults := h.getDefaults()
	replayDelay := h.getReplayDelay()

	h.modelsMu.RLock()
	modelsCacheTTL := h.modelsCacheTTL
	h.modelsMu.RUnlock()

	return runtimeConfig{
		SystemPrompt:   defaults.systemPrompt,
		MaxTokens:      defaults.maxTokens,
		Temperature:    defaults.temperature,
		Stream:         defaults.stream,
		ReplayDelay:    replayDelay.String(),
		ModelsCacheTTL: modelsCacheTTL.String(),
	}
}

func (h *Handler) applyConfigQuery(r *http.Request) (bool, error) {
	queryValues := r.URL.Query()
	if len(queryValues) == 0 {
		return false, nil
	}

	defaults := h.getDefaults()

	if systemPrompt := configQueryValue(queryValues, "system-prompt", "system_prompt"); systemPrompt != "" {
		defaults.systemPrompt = systemPrompt
	}

	if maxTokensRaw := configQueryValue(queryValues, "max-tokens", "max_tokens"); maxTokensRaw != "" {
		maxTokens, err := strconv.Atoi(maxTokensRaw)
		if err != nil {
			return false, fmt.Errorf("invalid max-tokens %q", maxTokensRaw)
		}
		if maxTokens < minMaxTokens || maxTokens > maxMaxTokens {
			return false, fmt.Errorf("max-tokens must be between %d and %d", minMaxTokens, maxMaxTokens)
		}
		defaults.maxTokens = maxTokens
	}

	if temperatureRaw := configQueryValue(queryValues, "temperature"); temperatureRaw != "" {
		temperature, err := strconv.ParseFloat(temperatureRaw, 64)
		if err != nil {
			return false, fmt.Errorf("invalid temperature %q", temperatureRaw)
		}
		defaults.temperature = temperature
	}

	if streamRaw := configQueryValue(queryValues, "stream"); streamRaw != "" {
		stream, err := strconv.ParseBool(streamRaw)
		if err != nil {
			return false, fmt.Errorf("invalid stream %q", streamRaw)
		}
		defaults.stream = stream
	}

	replayDelay := h.getReplayDelay()
	if replayDelayRaw := configQueryValue(queryValues, "replay-delay", "replay_delay"); replayDelayRaw != "" {
		parsed, err := time.ParseDuration(replayDelayRaw)
		if err != nil {
			return false, fmt.Errorf("invalid replay-delay %q: %w", replayDelayRaw, err)
		}
		if parsed < 0 {
			return false, fmt.Errorf("replay-delay must be non-negative")
		}
		replayDelay = parsed
	}

	h.modelsMu.RLock()
	modelsCacheTTL := h.modelsCacheTTL
	h.modelsMu.RUnlock()
	if modelsCacheTTLRaw := configQueryValue(queryValues, "models-cache-ttl", "models_cache_ttl"); modelsCacheTTLRaw != "" {
		parsed, err := time.ParseDuration(modelsCacheTTLRaw)
		if err != nil {
			return false, fmt.Errorf("invalid models-cache-ttl %q: %w", modelsCacheTTLRaw, err)
		}
		if parsed < 0 {
			return false, fmt.Errorf("models-cache-ttl must be non-negative")
		}
		modelsCacheTTL = parsed
	}

	h.configMu.Lock()
	h.defaults = defaults
	h.replayDelay = replayDelay
	h.configMu.Unlock()
	h.SetModelsCacheTTL(modelsCacheTTL)

	return true, nil
}

func configQueryValue(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}

func buildCacheKey(query string, options askOptions) string {
	queryKey := standardizeCacheKey(query)
	if queryKey == "" {
		return ""
	}

	return fmt.Sprintf("%s|%s|%d|%.6f|%t", queryKey, standardizeCacheKey(options.systemPrompt), options.maxTokens, options.temperature, options.stream)
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

type cachedModelsResponse struct {
	body        []byte
	statusCode  int
	contentType string
	defaultModel string
	cachedAt    time.Time
}

type chatCompletionRequest struct {
	Model         string                      `json:"model"`
	Messages      []chatCompletionMessage     `json:"messages"`
	Temperature   float64                     `json:"temperature"`
	MaxTokens     int                         `json:"max_tokens"`
	Stream        bool                        `json:"stream,omitempty"`
	StreamOptions *chatCompletionStreamOptions `json:"stream_options,omitempty"`
}

type chatCompletionStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (h *Handler) fetchModel(ctx context.Context) (string, error) {
	models, err := h.getOrFetchModels(ctx)
	if err != nil {
		return "", err
	}

	return models.defaultModel, nil
}

func (h *Handler) fetchModels(ctx context.Context) ([]byte, int, string, error) {
	models, err := h.getOrFetchModels(ctx)
	if err != nil {
		return nil, 0, "", err
	}

	return append([]byte(nil), models.body...), models.statusCode, models.contentType, nil
}

func (h *Handler) getOrFetchModels(ctx context.Context) (cachedModelsResponse, error) {
	h.modelsMu.RLock()
	if h.modelsCache != nil && h.modelsCacheFreshLocked() {
		cached := cloneCachedModelsResponse(*h.modelsCache)
		h.modelsMu.RUnlock()
		return cached, nil
	}
	h.modelsMu.RUnlock()

	h.modelsMu.Lock()
	defer h.modelsMu.Unlock()
	if h.modelsCache != nil && h.modelsCacheFreshLocked() {
		return cloneCachedModelsResponse(*h.modelsCache), nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.vllmURL+"/v1/models", nil)
	if err != nil {
		return cachedModelsResponse{}, fmt.Errorf("build models request: %w", err)
	}

	response, err := h.httpClient.Do(request)
	if err != nil {
		return cachedModelsResponse{}, fmt.Errorf("send models request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return cachedModelsResponse{}, fmt.Errorf("read models response: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cachedModelsResponse{}, fmt.Errorf("models request failed with HTTP %d: %s", response.StatusCode, bytes.TrimSpace(body))
	}

	var modelsResponseBody modelsResponse
	if err := json.Unmarshal(body, &modelsResponseBody); err != nil {
		return cachedModelsResponse{}, fmt.Errorf("decode models response: %w", err)
	}
	if len(modelsResponseBody.Data) == 0 || modelsResponseBody.Data[0].ID == "" {
		return cachedModelsResponse{}, fmt.Errorf("models response did not include a model id")
	}

	h.modelsCache = &cachedModelsResponse{
		body:         append([]byte(nil), body...),
		statusCode:   response.StatusCode,
		contentType:  contentTypeOrDefault(response.Header.Get("Content-Type"), "application/json"),
		defaultModel: modelsResponseBody.Data[0].ID,
		cachedAt:     h.now(),
	}

	return cloneCachedModelsResponse(*h.modelsCache), nil
}

func cloneCachedModelsResponse(models cachedModelsResponse) cachedModelsResponse {
	return cachedModelsResponse{
		body:         append([]byte(nil), models.body...),
		statusCode:   models.statusCode,
		contentType:  models.contentType,
		defaultModel: models.defaultModel,
		cachedAt:     models.cachedAt,
	}
}

func (h *Handler) modelsCacheFreshLocked() bool {
	if h.modelsCache == nil {
		return false
	}
	if h.modelsCacheTTL == 0 {
		return false
	}
	return h.now().Sub(h.modelsCache.cachedAt) < h.modelsCacheTTL
}

func (h *Handler) createChatCompletion(ctx context.Context, requestPayload chatCompletionRequest) ([]byte, int, string, error) {
	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, 0, "", fmt.Errorf("marshal chat request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.vllmURL+"/v1/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		return nil, 0, "", fmt.Errorf("build chat request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := h.httpClient.Do(request)
	if err != nil {
		return nil, 0, "", fmt.Errorf("send chat request: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, 0, "", fmt.Errorf("read chat response: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, 0, "", fmt.Errorf("chat request failed with HTTP %d: %s", response.StatusCode, bytes.TrimSpace(responseBody))
	}

	responseBody = rewriteJSONCacheField(responseBody, false)

	return responseBody, response.StatusCode, contentTypeOrDefault(response.Header.Get("Content-Type"), "application/json"), nil
}

func (h *Handler) streamChatCompletion(ctx context.Context, w http.ResponseWriter, requestPayload chatCompletionRequest) (cachedVLLMResponse, error) {
	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return cachedVLLMResponse{}, fmt.Errorf("marshal chat request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.vllmURL+"/v1/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		return cachedVLLMResponse{}, fmt.Errorf("build chat request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := h.httpClient.Do(request)
	if err != nil {
		return cachedVLLMResponse{}, fmt.Errorf("send chat request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			return cachedVLLMResponse{}, fmt.Errorf("read chat response: %w", readErr)
		}
		return cachedVLLMResponse{}, fmt.Errorf("chat request failed with HTTP %d: %s", response.StatusCode, bytes.TrimSpace(responseBody))
	}

	contentType := contentTypeOrDefault(response.Header.Get("Content-Type"), "text/event-stream")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.StatusCode)

	flusher, _ := w.(http.Flusher)
	reader := response.Body
	var buffer bytes.Buffer

	for {
		line, readErr := readSSELine(reader)
		if len(line) > 0 {
			rewritten := rewriteSSECacheField(line, false)
			buffer.Write(rewritten)
			if _, writeErr := w.Write(rewritten); writeErr != nil {
				return cachedVLLMResponse{}, fmt.Errorf("write stream response: %w", writeErr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}

		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			break
		}
		return cachedVLLMResponse{}, fmt.Errorf("read chat stream: %w", readErr)
	}

	return cachedVLLMResponse{
		statusCode:  response.StatusCode,
		body:        buffer.Bytes(),
		contentType: contentType,
		streaming:   true,
	}, nil
}

func (h *Handler) populateChatCompletionDefaults(ctx context.Context, requestPayload chatCompletionRequest) (chatCompletionRequest, error) {
	if requestPayload.Model == "" {
		model, err := h.fetchModel(ctx)
		if err != nil {
			return chatCompletionRequest{}, err
		}
		requestPayload.Model = model
	}

	return requestPayload, nil
}

func buildChatCompletionCacheKey(requestPayload chatCompletionRequest) (string, error) {
	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return "", fmt.Errorf("marshal chat completion cache key: %w", err)
	}

	return string(requestBody), nil
}

func (h *Handler) replayCachedStream(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse, replayDelay time.Duration) {
	w.Header().Set("Content-Type", contentTypeOrDefault(cachedResponse.contentType, "text/event-stream"))
	w.WriteHeader(cachedResponse.statusCode)

	flusher, _ := w.(http.Flusher)
	reader := bytes.NewReader(cachedResponse.body)
	streamID := newChatCompletionID()
	createdAt := time.Now().UnixMilli()
	firstLine := true

	for {
		line, err := readSSELine(reader)
		if len(line) > 0 {
			if !firstLine && replayDelay > 0 {
				if !sleepWithContext(ctx, replayDelay, h.sleep) {
					return
				}
			}
			rewritten := rewriteSSEDataLine(line, true, streamID, createdAt)
			_, _ = w.Write(rewritten)
			if flusher != nil {
				flusher.Flush()
			}
			firstLine = false
		}

		if err == nil {
			continue
		}
		if err == io.EOF {
			return
		}
		return
	}
}

func (h *Handler) replayCachedResponse(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse, replayDelay time.Duration) {
	completionTokens := cachedCompletionTokens(cachedResponse.body)
	if completionTokens > 0 && replayDelay > 0 {
		totalDelay := replayDelay * time.Duration(completionTokens)
		if !sleepWithContext(ctx, totalDelay, h.sleep) {
			return
		}
	}

	body := rewriteJSONCacheField(cachedResponse.body, true)
	w.Header().Set("Content-Type", cachedResponse.contentType)
	w.WriteHeader(cachedResponse.statusCode)
	_, _ = w.Write(body)
}

func cachedCompletionTokens(body []byte) int {
	var response struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return 0
	}
	if response.Usage.CompletionTokens < 0 {
		return 0
	}
	return response.Usage.CompletionTokens
}

func sleepWithContext(ctx context.Context, delay time.Duration, sleepFn func(time.Duration)) bool {
	if delay <= 0 {
		return true
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	if sleepFn == nil {
		time.Sleep(delay)
		return ctx.Err() == nil
	}
	sleepFn(delay)
	return ctx.Err() == nil
}

func readSSELine(reader io.Reader) ([]byte, error) {
	if byteReader, ok := reader.(interface{ ReadBytes(byte) ([]byte, error) }); ok {
		return byteReader.ReadBytes('\n')
	}

	buf := make([]byte, 1)
	var line []byte
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			line = append(line, buf[:n]...)
			if buf[0] == '\n' {
				return line, nil
			}
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, io.EOF
			}
			return line, err
		}
	}
}

func rewriteSSEDataLine(line []byte, cacheHit bool, streamID string, createdAt int64) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return line
	}

	payload := bytes.TrimPrefix(trimmed, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return line
	}

	body["id"] = streamID
	body["created"] = createdAt
	body["cache"] = cacheHit
	rewrittenPayload, err := json.Marshal(body)
	if err != nil {
		return line
	}

	return append(append([]byte("data: "), rewrittenPayload...), lineEnding(line)...)
}

func rewriteSSECacheField(line []byte, cacheHit bool) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return line
	}

	payload := bytes.TrimPrefix(trimmed, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}

	rewrittenPayload, ok := rewriteJSONCacheFieldBytes(payload, cacheHit)
	if !ok {
		return line
	}

	return append(append([]byte("data: "), rewrittenPayload...), lineEnding(line)...)
}

func rewriteJSONCacheField(body []byte, cacheHit bool) []byte {
	rewritten, ok := rewriteJSONCacheFieldBytes(body, cacheHit)
	if !ok {
		return body
	}
	return rewritten
}

func rewriteJSONCacheFieldBytes(body []byte, cacheHit bool) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	payload["cache"] = cacheHit
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}

func lineEnding(line []byte) []byte {
	if bytes.HasSuffix(line, []byte("\r\n")) {
		return []byte("\r\n")
	}
	if bytes.HasSuffix(line, []byte("\n")) {
		return []byte("\n")
	}
	return nil
}

func newChatCompletionID() string {
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(randomBytes)
}

func contentTypeOrDefault(contentType, defaultValue string) string {
	if strings.TrimSpace(contentType) == "" {
		return defaultValue
	}
	return contentType
}

func writePlainText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
	statusCode  int
	body        []byte
	contentType string
	streaming   bool
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
		statusCode:  entry.value.statusCode,
		body:        append([]byte(nil), entry.value.body...),
		contentType: entry.value.contentType,
		streaming:   entry.value.streaming,
	}, true
}

func (c *lruCache) Add(key string, value cachedVLLMResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[key]; ok {
		c.items.MoveToFront(element)
		entry := element.Value.(*cacheEntry)
		entry.value = cachedVLLMResponse{statusCode: value.statusCode, body: append([]byte(nil), value.body...), contentType: value.contentType, streaming: value.streaming}
		return
	}

	element := c.items.PushFront(&cacheEntry{
		key: key,
		value: cachedVLLMResponse{
			statusCode:  value.statusCode,
			body:        append([]byte(nil), value.body...),
			contentType: value.contentType,
			streaming:   value.streaming,
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
