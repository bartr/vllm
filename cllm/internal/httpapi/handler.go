package httpapi

import (
	"bytes"
	"cllm/internal/buildinfo"
	"context"
	"container/list"
	"crypto/sha256"
	"crypto/rand"
	"encoding/json"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultVLLMURL          = "http://localhost:8000"
	defaultCacheSize        = 100
	defaultCacheFilePath    = "/var/lib/cllm/cache.json"
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
	replayTokensPerSecondCompensation = 1.025
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
	cacheFilePath string
	metrics    *handlerMetrics
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
	handler.cacheFilePath = defaultCacheFilePath
	handler.defaults = askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	handler.vllmURL = trimTrailingSlash(defaultVLLMURL)
	handler.httpClient = &http.Client{Timeout: defaultVLLMHTTPTimeout}
	handler.maxTokensPerSecond = defaultMaxTokensPerSecond
	handler.maxDegradation = defaultMaxDegradation
	handler.scheduler = newRequestScheduler(defaultMaxConcurrentRequests, defaultMaxWaitingRequests)
	handler.sleep = sleepWithContext
	handler.lastLoggedComputedDegradationMilliPercent.Store(-1)
	handler.metrics = newHandlerMetrics(handler)
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
		EffectiveTokensPerSecond: roundMetric(calibratedTokensPerSecond(baseTokensPerSecond)),
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

func (h *Handler) SetCacheFilePath(path string) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if strings.TrimSpace(path) == "" {
		h.cacheFilePath = defaultCacheFilePath
		return
	}
	h.cacheFilePath = path
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
	mux.HandleFunc("GET /cache", h.cacheEndpoint)
	mux.HandleFunc("GET /cache/{key}", h.cacheItemEndpoint)
	mux.HandleFunc("GET /config", h.config)
	mux.HandleFunc("GET /health", h.healthEndpoint)
	mux.Handle("GET /metrics", h.metrics.Handler())
	mux.HandleFunc("GET /ready", h.readyEndpoint)
	mux.HandleFunc("GET /version", h.version)
	mux.HandleFunc("GET /v1/models", h.models)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	return requestLogger(mux, h.metrics)
}

type runtimeConfig struct {
	CacheSize       int     `json:"cache_size"`
	CacheEntries    int     `json:"cache_entries"`
	CacheFilePath   string  `json:"cache_file_path"`
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

type cacheEntrySummary struct {
	Key              string `json:"key"`
	StatusCode       int    `json:"status_code"`
	ContentType      string `json:"content_type"`
	Streaming        bool   `json:"streaming"`
	BodyBytes        int    `json:"body_bytes"`
	CompletionTokens int    `json:"completion_tokens"`
	ContentPreview   string `json:"content_preview"`
}

type cacheResponse struct {
	Enabled     bool                `json:"enabled"`
	CacheSize   int                 `json:"cache_size"`
	CacheEntries int                `json:"cache_entries"`
	CacheFilePath string            `json:"cache_file_path"`
	Keys        []cacheEntrySummary `json:"keys"`
	Action      *cacheActionResult  `json:"action,omitempty"`
}

type cacheActionResult struct {
	Name         string `json:"name"`
	CacheFilePath string `json:"cache_file_path"`
	SavedEntries int    `json:"saved_entries,omitempty"`
	LoadedEntries int   `json:"loaded_entries,omitempty"`
}

type cacheItemResponse struct {
	Key              string   `json:"key"`
	StatusCode       int      `json:"status_code"`
	ContentType      string   `json:"content_type"`
	Streaming        bool     `json:"streaming"`
	BodyBytes        int      `json:"body_bytes"`
	CompletionTokens int      `json:"completion_tokens"`
	Content          string   `json:"content"`
	TextTokens       []string `json:"text_tokens"`
	Body             string   `json:"body"`
}

type cacheSnapshotEntry struct {
	key   string
	value cachedVLLMResponse
}

type persistedCache struct {
	Version int                   `json:"version"`
	Entries []persistedCacheEntry `json:"entries"`
}

type persistedCacheEntry struct {
	Key         string `json:"key"`
	StatusCode  int    `json:"status_code"`
	Body        []byte `json:"body"`
	ContentType string `json:"content_type"`
	Streaming   bool   `json:"streaming"`
}

func (h *Handler) healthEndpoint(w http.ResponseWriter, _ *http.Request) {
	writePlainText(w, http.StatusOK, "ok\n")
}

func (h *Handler) readyEndpoint(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		writePlainText(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}

	writePlainText(w, http.StatusOK, "ready\n")
}

func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	markCacheHit(w, false)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, buildinfo.Version)
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

func (h *Handler) cacheEndpoint(w http.ResponseWriter, r *http.Request) {
	actionResult, err := h.applyCacheQuery(r)
	if err != nil {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}

	markCacheHit(w, false)
	writeJSON(w, http.StatusOK, h.currentCache(actionResult))
}

func (h *Handler) cacheItemEndpoint(w http.ResponseWriter, r *http.Request) {
	markCacheHit(w, false)
	key := r.PathValue("key")
	if key == "" {
		writePlainText(w, http.StatusBadRequest, "missing cache key\n")
		return
	}

	h.configMu.RLock()
	cache := h.cache
	h.configMu.RUnlock()
	if cache == nil {
		writePlainText(w, http.StatusNotFound, "cache disabled\n")
		return
	}

	value, ok := cache.Peek(key)
	if !ok {
		writePlainText(w, http.StatusNotFound, "cache key not found\n")
		return
	}

	content := cachedResponseContent(value)
	writeJSON(w, http.StatusOK, cacheItemResponse{
		Key:              key,
		StatusCode:       value.statusCode,
		ContentType:      value.contentType,
		Streaming:        value.streaming,
		BodyBytes:        len(value.body),
		CompletionTokens: cachedResponseCompletionTokens(value),
		Content:          content,
		TextTokens:       strings.Fields(content),
		Body:             string(value.body),
	})
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
	const endpoint = "chat_completions"

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

	release, queueWait, ok := h.acquireRequestSlot(r.Context(), r.URL.RequestURI())
	if !ok {
		h.metrics.observeJob(endpoint, "rejected", "none", "unknown", 0)
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "over capacity\n")
		return
	}
	defer release()
	mode := modeLabel(requestPayload.Stream)
	h.metrics.observeJob(endpoint, "accepted", "none", mode, 0)
	if queueWait > 0 {
		h.metrics.observeQueueWait(endpoint, queueWait)
	}
	processingStartedAt := time.Now()

	requestPayload, err := h.populateChatCompletionDefaults(r.Context(), requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("prepare chat completion: %v", err), http.StatusBadGateway)
		return
	}

	cacheKey, err := buildChatCompletionCacheKey(requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "none", mode, time.Since(processingStartedAt))
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("cache chat completion request: %v", err), http.StatusInternalServerError)
		return
	}

	if h.cache != nil {
		if cachedResponse, ok := h.cache.Get(cacheKey); ok {
			h.metrics.observeCacheLookup(endpoint, "hit")
			markCacheHit(w, true)
			completionTokens := cachedResponseCompletionTokens(cachedResponse)
			timedWriter := newFirstByteMetricsWriter(w, func() {
				h.metrics.observeTimeToFirstByte(endpoint, "cache", modeLabel(cachedResponse.streaming), time.Since(processingStartedAt))
			})
			if cachedResponse.streaming {
				h.replayCachedStream(r.Context(), timedWriter, cachedResponse)
				h.metrics.observeCompletionTokens(endpoint, "cache", completionTokens)
				h.metrics.observeJob(endpoint, "completed", "cache", modeLabel(true), time.Since(processingStartedAt))
				return
			}

			h.replayCachedResponse(r.Context(), timedWriter, cachedResponse)
			h.metrics.observeCompletionTokens(endpoint, "cache", completionTokens)
			h.metrics.observeJob(endpoint, "completed", "cache", modeLabel(false), time.Since(processingStartedAt))
			return
		}
		h.metrics.observeCacheLookup(endpoint, "miss")
	}

	markCacheHit(w, false)
	timedWriter := newFirstByteMetricsWriter(w, func() {
		h.metrics.observeTimeToFirstByte(endpoint, "downstream", mode, time.Since(processingStartedAt))
	})

	if requestPayload.Stream {
		downstreamStartedAt := time.Now()
		cachedResponse, err := h.streamChatCompletion(r.Context(), timedWriter, requestPayload)
		h.metrics.observeDownstreamRequest(endpoint, mode, downstreamResultLabel(err), time.Since(downstreamStartedAt))
		if err != nil {
			h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
			http.Error(timedWriter, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil {
			h.cache.Add(cacheKey, cachedResponse)
		}
		h.metrics.observeCompletionTokens(endpoint, "downstream", cachedResponseCompletionTokens(cachedResponse))
		h.metrics.observeJob(endpoint, "completed", "downstream", mode, time.Since(processingStartedAt))
		return
	}

	downstreamStartedAt := time.Now()
	responseBody, statusCode, contentType, err := h.createChatCompletion(r.Context(), requestPayload)
	h.metrics.observeDownstreamRequest(endpoint, mode, downstreamResultLabel(err), time.Since(downstreamStartedAt))
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
		http.Error(timedWriter, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
		return
	}

	if h.cache != nil {
		h.cache.Add(cacheKey, cachedVLLMResponse{statusCode: statusCode, body: responseBody, contentType: contentType})
	}

	timedWriter.Header().Set("Content-Type", contentType)
	timedWriter.WriteHeader(statusCode)
	_, _ = timedWriter.Write(responseBody)
	h.metrics.observeCompletionTokens(endpoint, "downstream", cachedJSONTokenCount(responseBody))
	h.metrics.observeJob(endpoint, "completed", "downstream", mode, time.Since(processingStartedAt))
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

func (h *Handler) currentCache(actionResult *cacheActionResult) cacheResponse {
	h.configMu.RLock()
	cache := h.cache
	cacheFilePath := h.cacheFilePath
	h.configMu.RUnlock()
	if cache == nil {
		return cacheResponse{Enabled: false, CacheSize: 0, CacheEntries: 0, CacheFilePath: cacheFilePath, Keys: []cacheEntrySummary{}, Action: actionResult}
	}

	capacity, snapshots := cache.Snapshot()
	keys := make([]cacheEntrySummary, 0, len(snapshots))
	for _, snapshot := range snapshots {
		content := cachedResponseContent(snapshot.value)
		keys = append(keys, cacheEntrySummary{
			Key:              snapshot.key,
			StatusCode:       snapshot.value.statusCode,
			ContentType:      snapshot.value.contentType,
			Streaming:        snapshot.value.streaming,
			BodyBytes:        len(snapshot.value.body),
			CompletionTokens: cachedResponseCompletionTokens(snapshot.value),
			ContentPreview:   previewText(content, 120),
		})
	}

	return cacheResponse{
		Enabled:      true,
		CacheSize:    capacity,
		CacheEntries: len(keys),
		CacheFilePath: cacheFilePath,
		Keys:         keys,
		Action:       actionResult,
	}
}

func (h *Handler) applyCacheQuery(r *http.Request) (*cacheActionResult, error) {
	queryValues := r.URL.Query()
	var actionResult *cacheActionResult
	if action := queryValues.Get("action"); action != "" {
		switch action {
		case "clear":
			h.configMu.RLock()
			cache := h.cache
			cacheFilePath := h.cacheFilePath
			h.configMu.RUnlock()
			if cache != nil {
				cache.Clear()
			}
			actionResult = &cacheActionResult{Name: action, CacheFilePath: cacheFilePath}
		case "save":
			savedEntries, cacheFilePath, err := h.SaveCacheToDisk()
			if err != nil {
				return nil, fmt.Errorf("save cache: %w", err)
			}
			actionResult = &cacheActionResult{Name: action, CacheFilePath: cacheFilePath, SavedEntries: savedEntries}
		case "load":
			loadedEntries, cacheFilePath, err := h.LoadCacheFromDisk()
			if err != nil {
				return nil, fmt.Errorf("load cache: %w", err)
			}
			actionResult = &cacheActionResult{Name: action, CacheFilePath: cacheFilePath, LoadedEntries: loadedEntries}
		default:
			return nil, fmt.Errorf("invalid action %q", action)
		}
	}

	if sizeRaw := queryValues.Get("size"); sizeRaw != "" {
		size, err := strconv.Atoi(sizeRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid size %q", sizeRaw)
		}
		if size < 0 || size > 10000 {
			return nil, fmt.Errorf("size must be between %d and %d", 0, 10000)
		}
		h.SetCacheSize(size)
	}

	return actionResult, nil
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
		CacheFilePath: h.getCacheFilePath(),
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

func (h *Handler) getCacheFilePath() string {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	return h.cacheFilePath
}

func (h *Handler) SaveCacheToDisk() (int, string, error) {
	h.configMu.RLock()
	cache := h.cache
	cacheFilePath := h.cacheFilePath
	h.configMu.RUnlock()

	persisted := persistedCache{Version: 1}
	if cache == nil {
		persisted.Entries = []persistedCacheEntry{}
	} else {
		_, snapshots := cache.Snapshot()
		persisted.Entries = make([]persistedCacheEntry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			persisted.Entries = append(persisted.Entries, persistedCacheEntry{
				Key:         snapshot.key,
				StatusCode:  snapshot.value.statusCode,
				Body:        append([]byte(nil), snapshot.value.body...),
				ContentType: snapshot.value.contentType,
				Streaming:   snapshot.value.streaming,
			})
		}
	}

	body, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return 0, cacheFilePath, fmt.Errorf("marshal cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cacheFilePath), 0o755); err != nil {
		return 0, cacheFilePath, fmt.Errorf("create cache directory: %w", err)
	}
	if err := os.WriteFile(cacheFilePath, append(body, '\n'), 0o644); err != nil {
		return 0, cacheFilePath, fmt.Errorf("write cache file: %w", err)
	}

	return len(persisted.Entries), cacheFilePath, nil
}

func (h *Handler) LoadCacheFromDisk() (int, string, error) {
	cacheFilePath := h.getCacheFilePath()
	body, err := os.ReadFile(cacheFilePath)
	if err != nil {
		return 0, cacheFilePath, err
	}

	var persisted persistedCache
	if err := json.Unmarshal(body, &persisted); err != nil {
		return 0, cacheFilePath, fmt.Errorf("decode cache file: %w", err)
	}
	if persisted.Version != 1 {
		return 0, cacheFilePath, fmt.Errorf("unsupported cache file version %d", persisted.Version)
	}

	entries := make([]cacheSnapshotEntry, 0, len(persisted.Entries))
	for _, entry := range persisted.Entries {
		if entry.Key == "" {
			return 0, cacheFilePath, fmt.Errorf("cache entry key must not be empty")
		}
		entries = append(entries, cacheSnapshotEntry{
			key: entry.Key,
			value: cachedVLLMResponse{
				statusCode:  entry.StatusCode,
				body:        append([]byte(nil), entry.Body...),
				contentType: entry.ContentType,
				streaming:   entry.Streaming,
			},
		})
	}

	h.configMu.RLock()
	cache := h.cache
	h.configMu.RUnlock()
	if cache == nil {
		return 0, cacheFilePath, nil
	}

	capacity, _ := cache.Stats()
	loadedEntries := h.replaceCache(capacity, entries)
	return loadedEntries, cacheFilePath, nil
}

func (h *Handler) replaceCache(capacity int, entries []cacheSnapshotEntry) int {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if capacity <= 0 {
		if h.cache == nil {
			return 0
		}
		h.cache = nil
		return 0
	}

	replacement := newLRUCache(capacity)
	for index := len(entries) - 1; index >= 0; index-- {
		replacement.Add(entries[index].key, entries[index].value)
	}
	h.cache = replacement
	_, loadedEntries := replacement.Stats()
	return loadedEntries
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

type chatCompletionCacheKey struct {
	Messages    []chatCompletionMessage `json:"messages"`
	Temperature float64                 `json:"temperature"`
	MaxTokens   int                     `json:"max_tokens"`
	Stream      bool                    `json:"stream"`
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
	requestBody, err := json.Marshal(chatCompletionCacheKey{
		Messages:    requestPayload.Messages,
		Temperature: requestPayload.Temperature,
		MaxTokens:   requestPayload.MaxTokens,
		Stream:      requestPayload.Stream,
	})
	if err != nil {
		return "", fmt.Errorf("marshal chat completion cache key: %w", err)
	}

	sum := sha256.Sum256(requestBody)
	return hex.EncodeToString(sum[:]), nil
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

func (h *Handler) acquireRequestSlot(ctx context.Context, path string) (func(), time.Duration, bool) {
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return func() {}, 0, true
	}
	release, queueWait, ok := scheduler.AcquirePath(ctx, path)
	if !ok {
		return nil, 0, false
	}
	h.logComputedDegradationIfChanged("request_admitted")
	return func() {
		release()
		h.logComputedDegradationIfChanged("request_completed")
	}, queueWait, true
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

	effectiveTokensPerSecond := calibratedTokensPerSecond(baseTokensPerSecond)
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

func modeLabel(stream bool) string {
	if stream {
		return "stream"
	}
	return "nonstream"
}

func downstreamResultLabel(err error) string {
	if err != nil {
		return "failed"
	}
	return "completed"
}

func calibratedTokensPerSecond(configuredTokensPerSecond int) float64 {
	if configuredTokensPerSecond < 1 {
		return 0
	}
	calibrated := float64(configuredTokensPerSecond) * replayTokensPerSecondCompensation
	if calibrated < 1 {
		return 1
	}
	return calibrated
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
	enqueuedAt time.Time
	queueWait time.Duration
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

func (s *requestScheduler) Acquire(ctx context.Context) (func(), time.Duration, bool) {
	return s.AcquirePath(ctx, "")
}

func (s *requestScheduler) AcquirePath(ctx context.Context, path string) (func(), time.Duration, bool) {
	s.mu.Lock()
	if s.inFlight < s.maxConcurrent {
		s.inFlight++
		slog.Info("request admitted", "path", path, "source", "direct", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()
		return s.releaseFunc(path), 0, true
	}
	if s.waiting >= s.maxWaiting {
		slog.Warn("request admission rejected", "path", path, "reason", "over_capacity", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()
		return nil, 0, false
	}

	request := &waitingRequest{path: path, ready: make(chan struct{}), enqueuedAt: time.Now()}
	request.element = s.queue.PushBack(request)
	s.waiting++
	slog.Info("request admitted", "path", path, "source", "waiting_queue", "concurrent_requests", s.inFlight, "max_concurrent_requests", s.maxConcurrent, "waiting_requests", s.waiting, "max_waiting_requests", s.maxWaiting)
		s.mu.Unlock()

	select {
	case <-request.ready:
		return s.releaseFunc(path), request.queueWait, true
	case <-ctx.Done():
		s.mu.Lock()
		if request.admitted {
			s.mu.Unlock()
			return s.releaseFunc(path), request.queueWait, true
		}
		if request.element != nil {
			s.queue.Remove(request.element)
			request.element = nil
			s.waiting--
		}
		s.mu.Unlock()
		return nil, 0, false
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
		request.queueWait = time.Since(request.enqueuedAt)
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
	calibratedBaseTokensPerSecond := calibratedTokensPerSecond(baseTokensPerSecond)
	if maxDegradation == 0 {
		return 0, calibratedBaseTokensPerSecond
	}
	capacity := s.maxConcurrent
	inFlight := s.inFlight
	if capacity == 0 {
		return 0, calibratedBaseTokensPerSecond
	}
	thresholdRequests := int(math.Floor(float64(capacity) * degradationThreshold))
	if inFlight <= thresholdRequests {
		return 0, calibratedBaseTokensPerSecond
	}
	degradationWindow := capacity - thresholdRequests
	if degradationWindow <= 0 {
		return 0, calibratedBaseTokensPerSecond
	}
	progress := float64(inFlight-thresholdRequests) / float64(degradationWindow)
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	computedDegradationPercentage := float64(maxDegradation) * progress
	effectiveTokensPerSecond := calibratedBaseTokensPerSecond * (1 - computedDegradationPercentage/100)
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

func cachedStreamTokenCount(body []byte) int {
	segments := parseCachedStreamReplaySegments(body)
	tokenCount := 0
	for _, segment := range segments {
		tokenCount += segment.tokenCount
	}
	return tokenCount
}

func cachedResponseCompletionTokens(cachedResponse cachedVLLMResponse) int {
	if cachedResponse.streaming {
		return cachedStreamTokenCount(cachedResponse.body)
	}
	return cachedJSONTokenCount(cachedResponse.body)
}

func cachedResponseContent(cachedResponse cachedVLLMResponse) string {
	if cachedResponse.streaming {
		segments := parseCachedStreamReplaySegments(cachedResponse.body)
		parts := make([]string, 0, len(segments))
		for _, segment := range segments {
			content, _ := inspectSSEChunk(segment.line)
			if content != "" {
				parts = append(parts, content)
			}
		}
		return strings.Join(parts, "")
	}

	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(cachedResponse.body, &response); err != nil {
		return ""
	}
	parts := make([]string, 0, len(response.Choices))
	for _, choice := range response.Choices {
		if choice.Message.Content != "" {
			parts = append(parts, choice.Message.Content)
		}
	}
	return strings.Join(parts, " ")
}

func previewText(text string, maxLen int) string {
	if maxLen < 1 || len(text) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return text[:maxLen]
	}
	return text[:maxLen-3] + "..."
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

func requestLogger(next http.Handler, metrics *handlerMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		if metrics != nil {
			metrics.httpInflightRequests.Inc()
			defer metrics.httpInflightRequests.Dec()
		}
		responseWriter := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(responseWriter, r)

		statusCode := responseWriter.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		if metrics != nil {
			metrics.observeHTTPRequest(routeLabel(r), r.Method, statusCode, time.Since(startedAt), responseWriter.bytes)
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

func (c *lruCache) Peek(key string) (cachedVLLMResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return cachedVLLMResponse{}, false
	}

	entry := element.Value.(*cacheEntry)
	return cachedVLLMResponse{
		statusCode:  entry.value.statusCode,
		body:        append([]byte(nil), entry.value.body...),
		contentType: entry.value.contentType,
		streaming:   entry.value.streaming,
	}, true
}

func (c *lruCache) Snapshot() (int, []cacheSnapshotEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := make([]cacheSnapshotEntry, 0, c.items.Len())
	for element := c.items.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		entries = append(entries, cacheSnapshotEntry{
			key: entry.key,
			value: cachedVLLMResponse{
				statusCode:  entry.value.statusCode,
				body:        append([]byte(nil), entry.value.body...),
				contentType: entry.value.contentType,
				streaming:   entry.value.streaming,
			},
		})
	}

	return c.capacity, entries
}

func (c *lruCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element, c.capacity)
	c.items.Init()
}
