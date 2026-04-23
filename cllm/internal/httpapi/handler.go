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
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	defaultVLLMURL          = "http://127.0.0.1:32080"
	defaultCacheSize        = 100
	defaultSystemPrompt     = "You are a helpful assistant."
	minMaxTokens            = 100
	maxMaxTokens            = 4000
	minMaxTokensPerSecond   = 0
	maxMaxTokensPerSecond   = 1000
	minMaxConcurrentRequests = 1
	maxMaxConcurrentRequests = 512
	minMaxWaitingRequests   = 0
	maxMaxWaitingRequests   = 1024
	minMaxDegradation       = 0
	maxMaxDegradation       = 95
	defaultMaxTokens        = 4000
	defaultTemperature      = 0.2
	defaultVLLMHTTPTimeout  = 120 * time.Second
	defaultMaxTokensPerSecond = 32
	defaultMaxConcurrentRequests = 512
	defaultMaxWaitingRequests = 1023
	defaultMaxDegradation = 10
	degradationThreshold = 0.10
)

type askOptions struct {
	systemPrompt string
	maxTokens    int
	temperature  float64
	stream       bool
}

func trimTrailingSlash(rawURL string) string {
	return strings.TrimRight(rawURL, "/")
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
	vllmURL    string
	downstreamToken string
	downstreamModel string
	httpClient *http.Client
	modelsMu   sync.RWMutex
	modelsCache *cachedModelsResponse
	maxTokensPerSecond int
	maxDegradation int
	scheduler *requestScheduler
	sleep func(context.Context, time.Duration) error
	lastLoggedComputedDegradationMilliPercent atomic.Int64
}

func NewHandler() *Handler {
	handler := &Handler{}
	handler.ready.Store(true)
	handler.cache = newLRUCache(defaultCacheSize)
	handler.defaults = askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	handler.vllmURL = trimTrailingSlash(defaultVLLMURL)
	handler.httpClient = &http.Client{Timeout: defaultVLLMHTTPTimeout}
	handler.maxTokensPerSecond = defaultMaxTokensPerSecond
	handler.maxDegradation = defaultMaxDegradation
	handler.scheduler = newRequestScheduler(defaultMaxConcurrentRequests, defaultMaxWaitingRequests)
	handler.sleep = sleepWithContext
	handler.lastLoggedComputedDegradationMilliPercent.Store(-1)
	return handler
}

func (h *Handler) SetRequestProcessingLimits(maxTokensPerSecond, maxConcurrentRequests, maxWaitingRequests, maxDegradation int) {
	if maxTokensPerSecond < minMaxTokensPerSecond || maxTokensPerSecond > maxMaxTokensPerSecond {
		maxTokensPerSecond = defaultMaxTokensPerSecond
	}
	if maxConcurrentRequests < minMaxConcurrentRequests || maxConcurrentRequests > maxMaxConcurrentRequests {
		maxConcurrentRequests = defaultMaxConcurrentRequests
	}
	if maxWaitingRequests < minMaxWaitingRequests || maxWaitingRequests > maxMaxWaitingRequests || maxWaitingRequests >= 2*maxConcurrentRequests {
		maxWaitingRequests = defaultMaxWaitingRequests
	}
	if maxDegradation < minMaxDegradation {
		maxDegradation = 0
	}
	if maxDegradation > maxMaxDegradation {
		maxDegradation = maxMaxDegradation
	}

	h.configMu.Lock()
	h.maxTokensPerSecond = maxTokensPerSecond
	h.maxDegradation = maxDegradation
	if h.scheduler == nil {
		h.scheduler = newRequestScheduler(maxConcurrentRequests, maxWaitingRequests)
	} else {
		h.scheduler.Reconfigure(maxConcurrentRequests, maxWaitingRequests)
	}
	h.configMu.Unlock()
	h.logComputedDegradationIfChanged("limits_updated")
}

type ProcessingStats struct {
	MaxTokensPerSecond            int
	EffectiveTokensPerSecond      float64
	MaxConcurrentRequests         int
	ConcurrentRequests            int
	MaxWaitingRequests            int
	WaitingRequests               int
	MaxDegradation                int
	ComputedDegradationPercentage float64
}

func (h *Handler) RequestProcessingStats() ProcessingStats {
	h.configMu.RLock()
	baseTokensPerSecond := h.maxTokensPerSecond
	maxDegradation := h.maxDegradation
	scheduler := h.scheduler
	h.configMu.RUnlock()

	stats := ProcessingStats{
		MaxTokensPerSecond:       baseTokensPerSecond,
		EffectiveTokensPerSecond: roundMetric(float64(baseTokensPerSecond)),
		MaxDegradation:           maxDegradation,
	}
	if baseTokensPerSecond == 0 {
		stats.EffectiveTokensPerSecond = 0
	}
	if scheduler == nil {
		return stats
	}

	maxConcurrentRequests, concurrentRequests, maxWaitingRequests, waitingRequests, computedDegradationPercentage, effectiveTokensPerSecond := scheduler.processingStats(baseTokensPerSecond, maxDegradation)
	stats.MaxConcurrentRequests = maxConcurrentRequests
	stats.ConcurrentRequests = concurrentRequests
	stats.MaxWaitingRequests = maxWaitingRequests
	stats.WaitingRequests = waitingRequests
	stats.ComputedDegradationPercentage = roundMetric(computedDegradationPercentage)
	stats.EffectiveTokensPerSecond = roundMetric(effectiveTokensPerSecond)
	return stats
}

func (h *Handler) RequestQueueStats() (int, int, int, int) {
	stats := h.RequestProcessingStats()
	return stats.MaxConcurrentRequests, stats.ConcurrentRequests, stats.MaxWaitingRequests, stats.WaitingRequests
}

func (h *Handler) SetDownstreamToken(token string) {
	h.modelsMu.Lock()
	h.downstreamToken = token
	h.modelsCache = nil
	h.modelsMu.Unlock()
}

func (h *Handler) SetDownstreamURL(rawURL string) {
	h.modelsMu.Lock()
	h.vllmURL = trimTrailingSlash(rawURL)
	h.modelsCache = nil
	h.modelsMu.Unlock()
}

func (h *Handler) SetDownstreamModel(model string) {
	h.modelsMu.Lock()
	h.downstreamModel = model
	h.modelsCache = nil
	h.modelsMu.Unlock()
}

func (h *Handler) SetCacheSize(size int) {
	if size < 0 {
		return
	}
	if size == 0 {
		h.configMu.Lock()
		h.cache = nil
		h.configMu.Unlock()
		return
	}

	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.cache == nil {
		h.cache = newLRUCache(size)
		return
	}
	h.cache.Resize(size)
}

func NewHandlerWithDependencies(vllmURL string, httpClient *http.Client, cacheSize int, defaults askOptions) *Handler {
	handler := NewHandler()
	if vllmURL != "" {
		handler.vllmURL = trimTrailingSlash(vllmURL)
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

func (h *Handler) applyDownstreamAuth(request *http.Request) {
	if h.downstreamToken == "" {
		return
	}
	request.Header.Set("Authorization", "Bearer "+h.downstreamToken)
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
	CacheSize       int     `json:"cache_size"`
	CacheEntries    int     `json:"cache_entries"`
	DownstreamURL string  `json:"downstream_url"`
	DownstreamModel string `json:"downstream_model"`
	SystemPrompt   string  `json:"system_prompt"`
	MaxTokens      int     `json:"max_tokens"`
	MaxTokensPerSecond int `json:"max_tokens_per_second"`
	EffectiveTokensPerSecond float64 `json:"effective_tokens_per_second"`
	MaxConcurrentRequests int `json:"max_concurrent_requests"`
	ConcurrentRequests int `json:"concurrent_requests"`
	MaxWaitingRequests int `json:"max_waiting_requests"`
	WaitingRequests int `json:"waiting_requests"`
	MaxDegradation int `json:"max_degradation"`
	ComputedDegradationPercentage float64 `json:"computed_degradation_percentage"`
	Temperature    float64 `json:"temperature"`
	Stream         bool    `json:"stream"`
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
		http.Error(w, fmt.Sprintf("fetch downstream models: %v", err), http.StatusBadGateway)
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

	release, ok := h.acquireRequestSlot(r.Context(), r.URL.RequestURI())
	if !ok {
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "over capacity\n")
		return
	}
	defer release()

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
				h.replayCachedStream(r.Context(), w, cachedResponse)
				return
			}

			h.replayCachedResponse(r.Context(), w, cachedResponse)
			return
		}
	}

	markCacheHit(w, false)

	if requestPayload.Stream {
		cachedResponse, err := h.streamChatCompletion(r.Context(), w, requestPayload)
		if err != nil {
			http.Error(w, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil {
			h.cache.Add(cacheKey, cachedResponse)
		}
		return
	}

	responseBody, statusCode, contentType, err := h.createChatCompletion(r.Context(), requestPayload)
	if err != nil {
		http.Error(w, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
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

	release, ok := h.acquireRequestSlot(r.Context(), r.URL.RequestURI())
	if !ok {
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "over capacity\n")
		return
	}
	defer release()

	if h.cache != nil {
		if cachedResponse, ok := h.cache.Get(cacheKey); ok {
			markCacheHit(w, true)
			if cachedResponse.streaming {
				h.replayCachedStream(r.Context(), w, cachedResponse)
				return
			}

			h.replayCachedResponse(r.Context(), w, cachedResponse)
			return
		}
	}

	markCacheHit(w, false)

	model, err := h.fetchModel(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch downstream model: %v", err), http.StatusBadGateway)
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
			http.Error(w, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil {
			h.cache.Add(cacheKey, cachedResponse)
		}
		return
	}

	responseBody, statusCode, contentType, err := h.createChatCompletion(r.Context(), requestPayload)
	if err != nil {
		http.Error(w, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
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

func (h *Handler) cacheStats() (int, int) {
	h.configMu.RLock()
	cache := h.cache
	h.configMu.RUnlock()
	if cache == nil {
		return 0, 0
	}
	return cache.Stats()
}

func (h *Handler) currentConfig() runtimeConfig {
	defaults := h.getDefaults()
	cacheSize, cacheEntries := h.cacheStats()
	processingStats := h.RequestProcessingStats()

	h.modelsMu.RLock()
	downstreamURL := h.vllmURL
	downstreamModel := h.downstreamModel
	h.modelsMu.RUnlock()

	return runtimeConfig{
		CacheSize: cacheSize,
		CacheEntries: cacheEntries,
		DownstreamURL: downstreamURL,
		DownstreamModel: downstreamModel,
		SystemPrompt:   defaults.systemPrompt,
		MaxTokens:      defaults.maxTokens,
		MaxTokensPerSecond: processingStats.MaxTokensPerSecond,
		EffectiveTokensPerSecond: processingStats.EffectiveTokensPerSecond,
		MaxConcurrentRequests: processingStats.MaxConcurrentRequests,
		ConcurrentRequests: processingStats.ConcurrentRequests,
		MaxWaitingRequests: processingStats.MaxWaitingRequests,
		WaitingRequests: processingStats.WaitingRequests,
		MaxDegradation: processingStats.MaxDegradation,
		ComputedDegradationPercentage: processingStats.ComputedDegradationPercentage,
		Temperature:    defaults.temperature,
		Stream:         defaults.stream,
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

	h.configMu.RLock()
	maxTokensPerSecond := h.maxTokensPerSecond
	maxDegradation := h.maxDegradation
	scheduler := h.scheduler
	h.configMu.RUnlock()
	maxConcurrentRequests, _, maxWaitingRequests, _ := 0, 0, 0, 0
	if scheduler != nil {
		maxConcurrentRequests, _, maxWaitingRequests, _ = scheduler.Stats()
	}
	previousMaxConcurrentRequests := maxConcurrentRequests
	previousMaxWaitingRequests := maxWaitingRequests

	if maxTokensPerSecondRaw := configQueryValue(queryValues, "max-tokens-per-second", "max_tokens_per_second"); maxTokensPerSecondRaw != "" {
		parsed, err := strconv.Atoi(maxTokensPerSecondRaw)
		if err != nil {
			return false, fmt.Errorf("invalid max-tokens-per-second %q", maxTokensPerSecondRaw)
		}
		if parsed < minMaxTokensPerSecond || parsed > maxMaxTokensPerSecond {
			return false, fmt.Errorf("max-tokens-per-second must be between %d and %d", minMaxTokensPerSecond, maxMaxTokensPerSecond)
		}
		maxTokensPerSecond = parsed
	}

	if maxConcurrentRequestsRaw := configQueryValue(queryValues, "max-concurrent-requests", "max_concurrent_requests"); maxConcurrentRequestsRaw != "" {
		parsed, err := strconv.Atoi(maxConcurrentRequestsRaw)
		if err != nil {
			return false, fmt.Errorf("invalid max-concurrent-requests %q", maxConcurrentRequestsRaw)
		}
		if parsed < minMaxConcurrentRequests || parsed > maxMaxConcurrentRequests {
			return false, fmt.Errorf("max-concurrent-requests must be between %d and %d", minMaxConcurrentRequests, maxMaxConcurrentRequests)
		}
		maxConcurrentRequests = parsed
	}

	if maxWaitingRequestsRaw := configQueryValue(queryValues, "max-waiting-requests", "max_waiting_requests"); maxWaitingRequestsRaw != "" {
		parsed, err := strconv.Atoi(maxWaitingRequestsRaw)
		if err != nil {
			return false, fmt.Errorf("invalid max-waiting-requests %q", maxWaitingRequestsRaw)
		}
		if parsed < minMaxWaitingRequests || parsed > maxMaxWaitingRequests {
			return false, fmt.Errorf("max-waiting-requests must be between %d and %d", minMaxWaitingRequests, maxMaxWaitingRequests)
		}
		maxWaitingRequests = parsed
	}
	if maxWaitingRequests >= 2*maxConcurrentRequests {
		return false, fmt.Errorf("max-waiting-requests must be less than %d", 2*maxConcurrentRequests)
	}

	if maxDegradationRaw := configQueryValue(queryValues, "max-degradation", "max_degradation"); maxDegradationRaw != "" {
		parsed, err := strconv.Atoi(maxDegradationRaw)
		if err != nil {
			return false, fmt.Errorf("invalid max-degradation %q", maxDegradationRaw)
		}
		if parsed < minMaxDegradation || parsed > maxMaxDegradation {
			return false, fmt.Errorf("max-degradation must be between %d and %d", minMaxDegradation, maxMaxDegradation)
		}
		maxDegradation = parsed
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

	cacheSize, _ := h.cacheStats()
	if cacheSizeRaw := configQueryValue(queryValues, "cache-size", "cache_size"); cacheSizeRaw != "" {
		parsed, err := strconv.Atoi(cacheSizeRaw)
		if err != nil {
			return false, fmt.Errorf("invalid cache-size %q", cacheSizeRaw)
		}
		if parsed < 0 {
			return false, fmt.Errorf("cache-size must be non-negative")
		}
		cacheSize = parsed
	}

	h.modelsMu.RLock()
	downstreamURL := h.vllmURL
	downstreamModel := h.downstreamModel
	h.modelsMu.RUnlock()
	if downstreamURLRaw := configQueryValue(queryValues, "downstream-url", "downstream_url"); downstreamURLRaw != "" {
		downstreamURL = downstreamURLRaw
	}
	if downstreamModelRaw := configQueryValue(queryValues, "downstream-model", "downstream_model"); downstreamModelRaw != "" {
		downstreamModel = downstreamModelRaw
	}

	h.configMu.Lock()
	h.defaults = defaults
	h.configMu.Unlock()
	h.SetCacheSize(cacheSize)
	h.SetDownstreamURL(downstreamURL)
	h.SetDownstreamModel(downstreamModel)
	h.SetRequestProcessingLimits(maxTokensPerSecond, maxConcurrentRequests, maxWaitingRequests, maxDegradation)
	updatedMaxConcurrentRequests, updatedConcurrentRequests, updatedMaxWaitingRequests, updatedWaitingRequests := h.RequestQueueStats()
	if updatedMaxConcurrentRequests != previousMaxConcurrentRequests || updatedMaxWaitingRequests != previousMaxWaitingRequests {
		slog.Info(
			"request queue limits updated",
			"max_concurrent_requests", updatedMaxConcurrentRequests,
			"previous_max_concurrent_requests", previousMaxConcurrentRequests,
			"concurrent_requests", updatedConcurrentRequests,
			"max_waiting_requests", updatedMaxWaitingRequests,
			"previous_max_waiting_requests", previousMaxWaitingRequests,
			"waiting_requests", updatedWaitingRequests,
		)
	}
	h.logComputedDegradationIfChanged("config_updated")

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
	if h.downstreamModel != "" {
		return h.downstreamModel, nil
	}

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
	h.applyDownstreamAuth(request)

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
	}

	return cloneCachedModelsResponse(*h.modelsCache), nil
}

func cloneCachedModelsResponse(models cachedModelsResponse) cachedModelsResponse {
	return cachedModelsResponse{
		body:         append([]byte(nil), models.body...),
		statusCode:   models.statusCode,
		contentType:  models.contentType,
		defaultModel: models.defaultModel,
	}
}

func (h *Handler) modelsCacheFreshLocked() bool {
	return h.modelsCache != nil
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
	h.applyDownstreamAuth(request)

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
	h.applyDownstreamAuth(request)

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
	if requestPayload.Model == "" && h.downstreamModel != "" {
		requestPayload.Model = h.downstreamModel
		return requestPayload, nil
	}

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

func (h *Handler) replayCachedStream(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse) {
	w.Header().Set("Content-Type", contentTypeOrDefault(cachedResponse.contentType, "text/event-stream"))
	w.WriteHeader(cachedResponse.statusCode)

	flusher, _ := w.(http.Flusher)
	streamID := newChatCompletionID()
	createdAt := time.Now().UnixMilli()
	segments := parseCachedStreamReplaySegments(cachedResponse.body)

	for _, segment := range segments {
		if err := h.throttleCachedReplay(ctx, segment.tokenCount); err != nil {
			return
		}
		if len(segment.line) > 0 {
			rewritten := rewriteSSEDataLine(segment.line, true, streamID, createdAt)
			_, _ = w.Write(rewritten)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (h *Handler) replayCachedResponse(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse) {
	body := rewriteJSONCacheField(cachedResponse.body, true)
	if err := h.throttleCachedReplay(ctx, cachedJSONTokenCount(cachedResponse.body)); err != nil {
		return
	}
	w.Header().Set("Content-Type", cachedResponse.contentType)
	w.WriteHeader(cachedResponse.statusCode)
	_, _ = w.Write(body)
}

func (h *Handler) acquireRequestSlot(ctx context.Context, path string) (func(), bool) {
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return func() {}, true
	}
	release, ok := scheduler.AcquirePath(ctx, path)
	if !ok {
		return nil, false
	}
	h.logComputedDegradationIfChanged("request_admitted")
	return func() {
		release()
		h.logComputedDegradationIfChanged("request_completed")
	}, true
}

func (h *Handler) throttleCachedReplay(ctx context.Context, tokenCount int) error {
	if tokenCount < 1 {
		return nil
	}
	if delay := h.cachedReplayDelay(tokenCount); delay > 0 {
		return h.sleep(ctx, delay)
	}
	return nil
}

func (h *Handler) cachedReplayDelay(tokenCount int) time.Duration {
	if tokenCount < 1 {
		return 0
	}

	h.configMu.RLock()
	baseTokensPerSecond := h.maxTokensPerSecond
	maxDegradation := h.maxDegradation
	scheduler := h.scheduler
	h.configMu.RUnlock()

	effectiveTokensPerSecond := float64(baseTokensPerSecond)
	if baseTokensPerSecond == 0 {
		return 0
	}
	if scheduler != nil {
		effectiveTokensPerSecond = scheduler.effectiveTokensPerSecond(baseTokensPerSecond, maxDegradation)
	}
	if effectiveTokensPerSecond <= 0 {
		effectiveTokensPerSecond = 1
	}

	return time.Duration(float64(tokenCount) * float64(time.Second) / effectiveTokensPerSecond)
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) logComputedDegradationIfChanged(source string) {
	stats := h.RequestProcessingStats()
	currentMilliPercent := int64(math.Round(stats.ComputedDegradationPercentage * 1000))
	if h.lastLoggedComputedDegradationMilliPercent.Load() == currentMilliPercent {
		return
	}
	h.lastLoggedComputedDegradationMilliPercent.Store(currentMilliPercent)
	slog.Info(
		"computed degradation updated",
		"source", source,
		"computed_degradation_percentage", stats.ComputedDegradationPercentage,
		"effective_tokens_per_second", stats.EffectiveTokensPerSecond,
		"max_tokens_per_second", stats.MaxTokensPerSecond,
		"concurrent_requests", stats.ConcurrentRequests,
		"max_concurrent_requests", stats.MaxConcurrentRequests,
		"waiting_requests", stats.WaitingRequests,
		"max_waiting_requests", stats.MaxWaitingRequests,
	)
}

func roundMetric(value float64) float64 {
	return math.Round(value*1000) / 1000
}

type requestScheduler struct {
	mu sync.Mutex
	queue *list.List
	maxConcurrent int
	maxWaiting int
	inFlight int
	waiting int
}

type waitingRequest struct {
	path string
	ready chan struct{}
	element *list.Element
	admitted bool
}

func newRequestScheduler(maxConcurrentRequests, maxWaitingRequests int) *requestScheduler {
	if maxConcurrentRequests < 1 {
		maxConcurrentRequests = 1
	}
	if maxWaitingRequests < 0 {
		maxWaitingRequests = 0
	}
	return &requestScheduler{
		queue: list.New(),
		maxConcurrent: maxConcurrentRequests,
		maxWaiting: maxWaitingRequests,
	}
}

func (s *requestScheduler) Acquire(ctx context.Context) (func(), bool) {
	return s.AcquirePath(ctx, "")
}

func (s *requestScheduler) AcquirePath(ctx context.Context, path string) (func(), bool) {
	s.mu.Lock()
	if s.inFlight < s.maxConcurrent {
		s.inFlight++
		slog.Info("request admitted", "path", path, "source", "direct", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()
		return s.releaseFunc(path), true
	}
	if s.waiting >= s.maxWaiting {
		slog.Warn("request admission rejected", "path", path, "reason", "over_capacity", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()
		return nil, false
	}

	request := &waitingRequest{path: path, ready: make(chan struct{})}
	request.element = s.queue.PushBack(request)
	s.waiting++
	slog.Info("request admitted", "path", path, "source", "waiting_queue", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()

	select {
	case <-request.ready:
		return s.releaseFunc(path), true
	case <-ctx.Done():
		s.mu.Lock()
		if request.admitted {
			s.mu.Unlock()
			return s.releaseFunc(path), true
		}
		if request.element != nil {
			s.queue.Remove(request.element)
			request.element = nil
			s.waiting--
		}
		s.mu.Unlock()
		return nil, false
	}
}

func (s *requestScheduler) Reconfigure(maxConcurrentRequests, maxWaitingRequests int) {
	if maxConcurrentRequests < 1 {
		maxConcurrentRequests = 1
	}
	if maxWaitingRequests < 0 {
		maxWaitingRequests = 0
	}

	s.mu.Lock()
	s.maxConcurrent = maxConcurrentRequests
	s.maxWaiting = maxWaitingRequests
	s.promoteWaitingLocked()
	s.mu.Unlock()
}

func (s *requestScheduler) Stats() (int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrent, s.inFlight, s.maxWaiting, s.waiting
}

func (s *requestScheduler) releaseFunc(path string) func() {
	return func() {
		s.mu.Lock()
		if s.inFlight > 0 {
			s.inFlight--
		}
		slog.Info("request completed", "path", path, "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.promoteWaitingLocked()
		s.mu.Unlock()
	}
}

func (s *requestScheduler) promoteWaitingLocked() {
	for s.inFlight < s.maxConcurrent && s.queue.Len() > 0 {
		front := s.queue.Front()
		request := front.Value.(*waitingRequest)
		s.queue.Remove(front)
		request.element = nil
		request.admitted = true
		s.waiting--
		s.inFlight++
		slog.Info("request admitted", "path", request.path, "source", "waiting_to_concurrent", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		close(request.ready)
	}
}

func (s *requestScheduler) effectiveTokensPerSecond(baseTokensPerSecond, maxDegradation int) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, effectiveTokensPerSecond := s.degradationMetricsLocked(baseTokensPerSecond, maxDegradation)
	return effectiveTokensPerSecond
}

func (s *requestScheduler) processingStats(baseTokensPerSecond, maxDegradation int) (int, int, int, int, float64, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	computedDegradationPercentage, effectiveTokensPerSecond := s.degradationMetricsLocked(baseTokensPerSecond, maxDegradation)
	return s.maxConcurrent, s.inFlight, s.maxWaiting, s.waiting, computedDegradationPercentage, effectiveTokensPerSecond
}

func (s *requestScheduler) degradationMetricsLocked(baseTokensPerSecond, maxDegradation int) (float64, float64) {
	if baseTokensPerSecond < 1 {
		return 0, 0
	}
	if maxDegradation == 0 {
		return 0, float64(baseTokensPerSecond)
	}
	capacity := s.maxConcurrent
	inFlight := s.inFlight
	if capacity == 0 {
		return 0, float64(baseTokensPerSecond)
	}
	thresholdRequests := int(math.Floor(float64(capacity) * degradationThreshold))
	if inFlight <= thresholdRequests {
		return 0, float64(baseTokensPerSecond)
	}
	degradationWindow := capacity - thresholdRequests
	if degradationWindow <= 0 {
		return 0, float64(baseTokensPerSecond)
	}
	progress := float64(inFlight-thresholdRequests) / float64(degradationWindow)
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	computedDegradationPercentage := float64(maxDegradation) * progress
	effectiveTokensPerSecond := float64(baseTokensPerSecond) * (1 - computedDegradationPercentage/100)
	if effectiveTokensPerSecond < 1 {
		effectiveTokensPerSecond = 1
	}
	return computedDegradationPercentage, effectiveTokensPerSecond
}

type replayStreamSegment struct {
	line []byte
	tokenCount int
}

func parseCachedStreamReplaySegments(body []byte) []replayStreamSegment {
	reader := bytes.NewReader(body)
	segments := make([]replayStreamSegment, 0)
	contentIndexes := make([]int, 0)
	contentWeights := make([]int, 0)
	usageCompletionTokens := 0

	for {
		line, err := readSSELine(reader)
		if len(line) > 0 {
			segment := replayStreamSegment{line: append([]byte(nil), line...)}
			content, completionTokens := inspectSSEChunk(line)
			if completionTokens > 0 {
				usageCompletionTokens = completionTokens
			}
			if content != "" {
				segment.tokenCount = estimateTextTokens(content)
				contentIndexes = append(contentIndexes, len(segments))
				contentWeights = append(contentWeights, segment.tokenCount)
			}
			segments = append(segments, segment)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}

	if usageCompletionTokens > 0 && len(contentIndexes) > 0 {
		distributed := distributeTokenBudget(usageCompletionTokens, contentWeights)
		for index, segmentIndex := range contentIndexes {
			segments[segmentIndex].tokenCount = distributed[index]
		}
	}

	return segments
}

func cachedJSONTokenCount(body []byte) int {
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0
	}
	if response.Usage.CompletionTokens > 0 {
		return response.Usage.CompletionTokens
	}
	parts := make([]string, 0, len(response.Choices))
	for _, choice := range response.Choices {
		if choice.Message.Content != "" {
			parts = append(parts, choice.Message.Content)
		}
	}
	return estimateTextTokens(strings.Join(parts, " "))
}

func inspectSSEChunk(line []byte) (string, int) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return "", 0
	}
	payload := bytes.TrimPrefix(trimmed, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return "", 0
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return "", 0
	}
	contentParts := make([]string, 0, len(chunk.Choices))
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			contentParts = append(contentParts, choice.Delta.Content)
		}
	}
	return strings.Join(contentParts, " "), chunk.Usage.CompletionTokens
}

func distributeTokenBudget(total int, weights []int) []int {
	distributed := make([]int, len(weights))
	if total <= 0 || len(weights) == 0 {
		return distributed
	}
	weightSum := 0
	for _, weight := range weights {
		if weight > 0 {
			weightSum += weight
		}
	}
	if weightSum == 0 {
		for index := 0; index < len(distributed) && total > 0; index++ {
			distributed[index] = 1
			total--
		}
		return distributed
	}

	allocated := 0
	for index, weight := range weights {
		if weight < 1 {
			weight = 1
		}
		share := int(math.Floor(float64(total) * float64(weight) / float64(weightSum)))
		if share < 1 && allocated < total {
			share = 1
		}
		distributed[index] = share
		allocated += share
	}
	for allocated > total {
		for index := len(distributed) - 1; index >= 0 && allocated > total; index-- {
			if distributed[index] > 0 {
				distributed[index]--
				allocated--
			}
		}
	}
	for allocated < total {
		for index := range distributed {
			distributed[index]++
			allocated++
			if allocated == total {
				break
			}
		}
	}
	return distributed
}

func estimateTextTokens(text string) int {
	fields := strings.Fields(text)
	if len(fields) > 0 {
		return len(fields)
	}
	if strings.TrimSpace(text) != "" {
		return 1
	}
	return 0
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

func (c *lruCache) Resize(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if capacity < 1 {
		capacity = 1
	}
	c.capacity = capacity
	for c.items.Len() > c.capacity {
		oldest := c.items.Back()
		if oldest == nil {
			break
		}
		c.items.Remove(oldest)
		entry := oldest.Value.(*cacheEntry)
		delete(c.entries, entry.key)
		slog.Info("cache evict", "size", c.items.Len(), "capacity", c.capacity)
	}
}

func (c *lruCache) Stats() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity, c.items.Len()
}
