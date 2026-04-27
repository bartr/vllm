package httpapi

import (
	"bytes"
	"cllm/internal/buildinfo"
	"cllm/internal/runtimeconfig"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	defaultVLLMURL                    = runtimeconfig.DefaultDownstreamURL
	defaultCacheSize                  = runtimeconfig.DefaultCacheSize
	defaultCacheFilePath              = runtimeconfig.DefaultCacheFilePath
	defaultSystemPrompt               = runtimeconfig.DefaultSystemPrompt
	minMaxTokens                      = runtimeconfig.MinMaxTokens
	maxMaxTokens                      = runtimeconfig.MaxMaxTokens
	minMaxTokensPerSecond             = runtimeconfig.MinMaxTokensPerSecond
	maxMaxTokensPerSecond             = runtimeconfig.MaxMaxTokensPerSecond
	minMaxTokensInFlight              = runtimeconfig.MinMaxTokensInFlight
	maxMaxTokensInFlight              = runtimeconfig.MaxMaxTokensInFlight
	minMaxWaitingRequests             = runtimeconfig.MinMaxWaitingRequests
	maxMaxWaitingRequests             = runtimeconfig.MaxMaxWaitingRequests
	minMaxDegradation                 = runtimeconfig.MinMaxDegradation
	maxMaxDegradation                 = runtimeconfig.MaxMaxDegradation
	defaultMaxTokens                  = runtimeconfig.DefaultMaxTokens
	defaultTemperature                = runtimeconfig.DefaultTemperature
	defaultVLLMHTTPTimeout            = 120 * time.Second
	defaultMaxTokensPerSecond         = runtimeconfig.DefaultMaxTokensPerSecond
	defaultMaxTokensInFlight          = runtimeconfig.DefaultMaxTokensInFlight
	defaultMaxWaitingRequests         = runtimeconfig.DefaultMaxWaitingRequests
	defaultMaxDegradation             = runtimeconfig.DefaultMaxDegradation
	defaultPrefillRateMultiplier      = runtimeconfig.DefaultPrefillRateMultiplier
	defaultPrefillBaseOverheadMs      = runtimeconfig.DefaultPrefillBaseOverheadMs
	defaultPrefillJitterPercent       = runtimeconfig.DefaultPrefillJitterPercent
	defaultPrefillMaxMs               = runtimeconfig.DefaultPrefillMaxMs
	minPrefillRateMultiplier          = runtimeconfig.MinPrefillRateMultiplier
	maxPrefillRateMultiplier          = runtimeconfig.MaxPrefillRateMultiplier
	minPrefillBaseOverheadMs          = runtimeconfig.MinPrefillBaseOverheadMs
	maxPrefillBaseOverheadMs          = runtimeconfig.MaxPrefillBaseOverheadMs
	minPrefillJitterPercent           = runtimeconfig.MinPrefillJitterPercent
	maxPrefillJitterPercent           = runtimeconfig.MaxPrefillJitterPercent
	defaultStreamVariabilityPercent      = runtimeconfig.DefaultStreamVariabilityPercent
	defaultStreamJitterPercent           = runtimeconfig.DefaultStreamJitterPercent
	defaultStreamStallProbabilityPercent = runtimeconfig.DefaultStreamStallProbabilityPercent
	defaultStreamStallMinMs              = runtimeconfig.DefaultStreamStallMinMs
	defaultStreamStallMaxMs              = runtimeconfig.DefaultStreamStallMaxMs
	minStreamVariabilityPercent          = runtimeconfig.MinStreamVariabilityPercent
	maxStreamVariabilityPercent          = runtimeconfig.MaxStreamVariabilityPercent
	minStreamJitterPercent               = runtimeconfig.MinStreamJitterPercent
	maxStreamJitterPercent               = runtimeconfig.MaxStreamJitterPercent
	minStreamStallProbabilityPercent     = runtimeconfig.MinStreamStallProbabilityPercent
	maxStreamStallProbabilityPercent     = runtimeconfig.MaxStreamStallProbabilityPercent
	minStreamStallMs                     = runtimeconfig.MinStreamStallMs
	maxStreamStallMs                     = runtimeconfig.MaxStreamStallMs
	degradationThreshold              = 0.10
)

type askOptions struct {
	systemPrompt string
	maxTokens    int
	temperature  float64
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
	ready                                     atomic.Bool
	cache                                     *lruCache
	cacheFilePath                             string
	metrics                                   *handlerMetrics
	configMu                                  sync.RWMutex
	defaults                                  askOptions
	vllmURL                                   string
	downstreamToken                           string
	downstreamModel                           string
	httpClient                                *http.Client
	modelsMu                                  sync.RWMutex
	modelsCache                               *cachedModelsResponse
	maxTokensPerSecond                        int
	maxDegradation                            int
	prefillRateMultiplier                     float64
	prefillBaseOverhead                       time.Duration
	prefillJitterPercent                      int
	prefillMaxDuration                        time.Duration
	streamVariabilityPercent                  int
	streamJitterPercent                       int
	streamStallProbabilityPercent             int
	streamStallMin                            time.Duration
	streamStallMax                            time.Duration
	scheduler                                 *requestScheduler
	sleep                                     func(context.Context, time.Duration) error
	jitterSource                              func() float64
	dslProfiles                               map[string][]string
	dslDefaultProfile                         string
	tenants                                   *tenantRegistry
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
	handler.prefillRateMultiplier = defaultPrefillRateMultiplier
	handler.prefillBaseOverhead = time.Duration(defaultPrefillBaseOverheadMs) * time.Millisecond
	handler.prefillJitterPercent = defaultPrefillJitterPercent
	handler.prefillMaxDuration = time.Duration(defaultPrefillMaxMs) * time.Millisecond
	handler.streamVariabilityPercent = defaultStreamVariabilityPercent
	handler.streamJitterPercent = defaultStreamJitterPercent
	handler.streamStallProbabilityPercent = defaultStreamStallProbabilityPercent
	handler.streamStallMin = time.Duration(defaultStreamStallMinMs) * time.Millisecond
	handler.streamStallMax = time.Duration(defaultStreamStallMaxMs) * time.Millisecond
	handler.scheduler = newRequestScheduler(defaultMaxTokensInFlight, defaultMaxWaitingRequests)
	handler.sleep = sleepWithContext
	handler.jitterSource = defaultJitterSource
	handler.dslProfiles = cloneDSLProfiles(DefaultDSLProfiles)
	handler.tenants = newTenantRegistry(TenantConfig{}, 256, 50)
	handler.lastLoggedComputedDegradationMilliPercent.Store(-1)
	handler.metrics = newHandlerMetrics(handler)
	handler.scheduler.metrics = handler.metrics
	return handler
}

func (h *Handler) SetRequestProcessingLimits(maxTokensPerSecond, maxTokensInFlight, maxWaitingRequests, maxDegradation int) {
	if maxTokensPerSecond < minMaxTokensPerSecond || maxTokensPerSecond > maxMaxTokensPerSecond {
		maxTokensPerSecond = defaultMaxTokensPerSecond
	}
	if maxTokensInFlight < minMaxTokensInFlight || maxTokensInFlight > maxMaxTokensInFlight {
		maxTokensInFlight = defaultMaxTokensInFlight
	}
	if maxWaitingRequests < minMaxWaitingRequests || maxWaitingRequests > maxMaxWaitingRequests {
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
		h.scheduler = newRequestScheduler(maxTokensInFlight, maxWaitingRequests)
		h.scheduler.metrics = h.metrics
	} else {
		h.scheduler.Reconfigure(maxTokensInFlight, maxWaitingRequests)
	}
	h.configMu.Unlock()
	h.logComputedDegradationIfChanged("limits_updated")
}

type ProcessingStats struct {
	MaxTokensPerSecond            int
	EffectiveTokensPerSecond      float64
	MaxTokensInFlight             int64
	TokensInFlight                int64
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

	maxTokensInFlight, tokensInFlight, maxWaitingRequests, waitingRequests, computedDegradationPercentage, effectiveTokensPerSecond := scheduler.processingStats(baseTokensPerSecond, maxDegradation)
	stats.MaxTokensInFlight = maxTokensInFlight
	stats.TokensInFlight = tokensInFlight
	stats.MaxWaitingRequests = maxWaitingRequests
	stats.WaitingRequests = waitingRequests
	stats.ComputedDegradationPercentage = roundMetric(computedDegradationPercentage)
	stats.EffectiveTokensPerSecond = roundMetric(effectiveTokensPerSecond)
	return stats
}

func (h *Handler) RequestQueueStats() (int64, int64, int, int) {
	stats := h.RequestProcessingStats()
	return stats.MaxTokensInFlight, stats.TokensInFlight, stats.MaxWaitingRequests, stats.WaitingRequests
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

// SetDSLProfiles replaces the active map of named DSL profile bundles.
// Passing a nil or empty map restores the built-in defaults. Profile names
// are matched case-insensitively. Each value is the slice of directive
// tokens that the profile expands into; tokens unknown to the parser are
// silently ignored at request time.
func (h *Handler) SetDSLProfiles(profiles map[string][]string) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if len(profiles) == 0 {
		h.dslProfiles = cloneDSLProfiles(DefaultDSLProfiles)
		return
	}
	h.dslProfiles = cloneDSLProfiles(profiles)
}

// SetDSLDefaultProfile installs a profile bundle that is implicitly
// expanded for every request that omits the `:dsl` marker. Pass an empty
// string to disable. Returns an error if the named profile is not present
// in the currently loaded profile map. Names are matched
// case-insensitively.
func (h *Handler) SetDSLDefaultProfile(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if name == "" {
		h.dslDefaultProfile = ""
		return nil
	}
	if _, ok := h.dslProfiles[name]; !ok {
		return fmt.Errorf("unknown DSL profile %q", name)
	}
	h.dslDefaultProfile = name
	return nil
}

// DSLDefaultProfile returns the currently configured default DSL profile
// name (empty when none is set).
func (h *Handler) DSLDefaultProfile() string {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	return h.dslDefaultProfile
}

// DSLProfileNames returns the sorted list of currently loaded DSL profile
// names. Used by the HTML config form to populate a dropdown.
func (h *Handler) DSLProfileNames() []string {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	names := make([]string, 0, len(h.dslProfiles))
	for name := range h.dslProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	mux.HandleFunc("POST /config", h.configPost)
	mux.HandleFunc("GET /health", h.healthEndpoint)
	mux.Handle("GET /metrics", h.metrics.Handler())
	mux.HandleFunc("GET /ready", h.readyEndpoint)
	mux.HandleFunc("GET /version", h.version)
	mux.HandleFunc("GET /v1/models", h.models)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	return requestLogger(mux, h.metrics)
}

type runtimeConfig struct {
	TokensInFlight                int64   `json:"tokens_in_flight"`
	WaitingRequests               int     `json:"waiting_requests"`
	Version                       string  `json:"version"`
	CacheSize                     int     `json:"cache_size"`
	CacheEntries                  int     `json:"cache_entries"`
	DownstreamURL                 string  `json:"downstream_url"`
	DownstreamModel               string  `json:"downstream_model"`
	SystemPrompt                  string  `json:"system_prompt"`
	MaxTokens                     int     `json:"max_tokens"`
	MaxTokensPerSecond            int     `json:"max_tokens_per_second"`
	EffectiveTokensPerSecond      float64 `json:"effective_tokens_per_second"`
	MaxTokensInFlight             int64   `json:"max_tokens_in_flight"`
	MaxWaitingRequests            int     `json:"max_waiting_requests"`
	MaxDegradation                int     `json:"max_degradation"`
	ComputedDegradationPercentage float64 `json:"computed_degradation_percentage"`
	Temperature                   float64 `json:"temperature"`
	PrefillRateMultiplier         float64 `json:"prefill_rate_multiplier"`
	PrefillBaseOverheadMs         int     `json:"prefill_base_overhead_ms"`
	PrefillJitterPercent          int     `json:"prefill_jitter_percent"`
	PrefillMaxMs                  int     `json:"prefill_max_ms"`
	StreamVariabilityPercent      int     `json:"stream_variability_percent"`
	StreamJitterPercent           int     `json:"stream_jitter_percent"`
	StreamStallProbabilityPercent int     `json:"stream_stall_probability_percent"`
	StreamStallMinMs              int     `json:"stream_stall_min_ms"`
	StreamStallMaxMs              int     `json:"stream_stall_max_ms"`
	DSLDefaultProfile             string  `json:"dsl_default_profile"`
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
	Enabled       bool                `json:"enabled"`
	CacheSize     int                 `json:"cache_size"`
	CacheEntries  int                 `json:"cache_entries"`
	CacheFilePath string              `json:"cache_file_path"`
	Keys          []cacheEntrySummary `json:"keys"`
	Action        *cacheActionResult  `json:"action,omitempty"`
}

type cacheActionResult struct {
	Name          string `json:"name"`
	CacheFilePath string `json:"cache_file_path"`
	SavedEntries  int    `json:"saved_entries,omitempty"`
	LoadedEntries int    `json:"loaded_entries,omitempty"`
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
	wantsHTML := preferHTML(r.Header.Get("Accept"))

	if _, err := h.applyConfigQuery(r); err != nil {
		markCacheHit(w, false)
		if wantsHTML {
			h.renderConfigHTML(w, r, configFormState{
				Edit:    true,
				Values:  r.URL.Query(),
				Error:   err.Error(),
				Status:  http.StatusBadRequest,
			})
			return
		}
		writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}

	markCacheHit(w, false)
	if wantsHTML {
		edit := r.URL.Query().Get("edit") == "1"
		h.renderConfigHTML(w, r, configFormState{Edit: edit, Status: http.StatusOK})
		return
	}
	writeJSON(w, http.StatusOK, h.currentConfig())
}

// configPost handles the HTML edit form submission. Validation re-uses the
// same applyConfigValues logic that backs the GET query-string API.
func (h *Handler) configPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "invalid form: "+err.Error()+"\n")
		return
	}
	wantsHTML := preferHTML(r.Header.Get("Accept")) || r.Header.Get("Content-Type") == "application/x-www-form-urlencoded"

	if _, err := h.applyConfigValues(r.PostForm); err != nil {
		markCacheHit(w, false)
		if wantsHTML {
			h.renderConfigHTML(w, r, configFormState{
				Edit:   true,
				Values: r.PostForm,
				Error:  err.Error(),
				Status: http.StatusBadRequest,
			})
			return
		}
		writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}

	markCacheHit(w, false)
	if wantsHTML {
		// Post/Redirect/Get: send the browser back to read-only view.
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, h.currentConfig())
}

// preferHTML returns true when the Accept header lists text/html with a
// quality factor strictly higher than application/json (or before it). It
// is intentionally permissive: any browser request gets the form, while
// `curl` (which sends `*/*` or no Accept) keeps the JSON contract.
func preferHTML(accept string) bool {
	if accept == "" {
		return false
	}
	// Cheap parse: if text/html appears before any application/json or */*,
	// prefer HTML. Browsers send `text/html,application/xhtml+xml,...` so
	// the substring check is sufficient.
	lower := strings.ToLower(accept)
	htmlIdx := strings.Index(lower, "text/html")
	if htmlIdx < 0 {
		return false
	}
	jsonIdx := strings.Index(lower, "application/json")
	if jsonIdx >= 0 && jsonIdx < htmlIdx {
		return false
	}
	return true
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
	const endpoint = chatCompletionEndpoint
	ctx := r.Context()
	receivedAt := time.Now()

	var requestPayload chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&requestPayload); err != nil {
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "bad_request", "request rejected",
			"status", http.StatusBadRequest,
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v\n", err))
		return
	}

	if len(requestPayload.Messages) == 0 {
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "missing_messages", "request rejected",
			"status", http.StatusBadRequest,
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusBadRequest, "missing messages\n")
		return
	}

	// Parse DSL before scheduler admission so directive families are
	// available on rejection paths too. Parsing has no side effects beyond
	// stripping the DSL tokens from message contents.
	h.configMu.RLock()
	dslDraw := h.jitterSource
	dslProfiles := h.dslProfiles
	dslDefaultProfile := h.dslDefaultProfile
	h.configMu.RUnlock()
	cleanedMessages, replayDSL := parseDSLWithDefaultProfile(requestPayload.Messages, dslDraw, dslProfiles, dslDefaultProfile)
	requestPayload.Messages = cleanedMessages
	dslFamily := dslFamilies(replayDSL.directives)
	if replayDSL.active() {
		h.emitLifecycleEvent(ctx, slog.LevelInfo, "dsl_applied", "", "replay DSL applied",
			"directives", strings.Join(replayDSL.directives, " "),
		)
		if h.metrics != nil {
			for _, d := range replayDSL.directives {
				h.metrics.observeDSLDirective(endpoint, d)
			}
		}
	}
	if replayDSL.maxTokensOverride > 0 {
		requestPayload.MaxTokens = replayDSL.maxTokensOverride
	}

	mode := modeLabel(requestPayload.Stream)
	requestedMaxTokens := requestPayload.MaxTokens

	// Stage 0: resolve tenant. Unknown / missing tenant → "default".
	tenant := h.resolveRequestTenant(r)

	// Cost estimate uses tenant p95 first, then global, then max_tokens.
	cost := estimateRequestCostForTenant(requestPayload, tenant, h.globalEstimator())

	// Stage 1: tenant rate limit (token bucket; non-blocking).
	if !tenant.bucket.tryReserve(float64(cost.totalCost)) {
		h.metrics.observeJob(endpoint, "rejected", "none", "unknown", 0)
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "rejected")
		h.metrics.observeAdmissionRejection(tenant.name, "tenant_rate")
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "tenant_rate", "request rejected",
			"status", http.StatusTooManyRequests,
			"mode", mode,
			"max_tokens", requestedMaxTokens,
			"tenant", tenant.name,
			"cost", cost.totalCost,
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "tenant rate exceeded\n")
		return
	}

	// Stage 2: global cost budget (FIFO queue).
	release, queueWait, ok := h.acquireRequestSlot(ctx, cost, r.URL.RequestURI())
	if !ok {
		// Global gate refused; refund tenant tokens so a rejection here
		// doesn't permanently drain rate quota.
		tenant.bucket.refund(float64(cost.totalCost))
		h.metrics.observeJob(endpoint, "rejected", "none", "unknown", 0)
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "rejected")
		h.metrics.observeAdmissionRejection(tenant.name, "over_capacity")
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "over_capacity", "request rejected",
			"status", http.StatusTooManyRequests,
			"mode", mode,
			"max_tokens", requestedMaxTokens,
			"tenant", tenant.name,
			"cost", cost.totalCost,
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "over capacity\n")
		return
	}
	defer release()
	h.metrics.observeJob(endpoint, "accepted", "none", mode, 0)
	h.metrics.observeAdmissionAccept(tenant.name, mode)
	if queueWait > 0 {
		h.metrics.observeQueueWait(endpoint, queueWait)
	}
	processingStartedAt := time.Now()
	h.emitLifecycleEvent(ctx, slog.LevelInfo, "started", "", "request started",
		"mode", mode,
		"max_tokens", requestedMaxTokens,
		"tenant", tenant.name,
		"cost", cost.totalCost,
		"queue_wait_ms", float64(queueWait)/float64(time.Millisecond),
	)

	emitCompleted := func(level slog.Level, outcome, source string, status int, promptTokens, completionTokens int) {
		// Feed the actual completion-token count back into the global p95
		// estimator AND the tenant's per-tenant estimator, but only on
		// successful, non-cached completions. Cached replays mirror
		// upstream behavior and rejected/failed requests do not represent
		// real workload.
		if outcome == "completed" && source == "downstream" && status >= 200 && status < 300 {
			h.observeCompletionTokens(completionTokens)
			if tenant != nil && tenant.estimator != nil {
				tenant.estimator.observe(completionTokens)
			}
		}
		h.emitLifecycleEvent(ctx, level, "completed", outcome, "request completed",
			"source", source,
			"mode", mode,
			"status", status,
			"tenant", tenant.name,
			"queue_wait_ms", float64(queueWait)/float64(time.Millisecond),
			"duration_ms", float64(time.Since(processingStartedAt))/float64(time.Millisecond),
			"prompt_tokens", promptTokens,
			"completion_tokens", completionTokens,
			"max_tokens", requestPayload.MaxTokens,
		)
	}

	requestPayload, err := h.populateChatCompletionDefaults(ctx, requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
		emitCompleted(slog.LevelInfo, "failed", "downstream", http.StatusBadGateway, 0, 0)
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("prepare chat completion: %v", err), http.StatusBadGateway)
		return
	}

	cacheKey, err := buildChatCompletionCacheKey(requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "none", mode, time.Since(processingStartedAt))
		h.metrics.observeDSLJobDuration(endpoint, "none", "failed", dslFamily, time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
		emitCompleted(slog.LevelInfo, "failed", "none", http.StatusInternalServerError, 0, 0)
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("cache chat completion request: %v", err), http.StatusInternalServerError)
		return
	}

	// Cache lookup is skipped when either no-cache or re-cache is in
	// effect; the latter still writes the fresh response back below.
	if h.cache != nil && !replayDSL.noCache && !replayDSL.reCache {
		if cachedResponse, ok := h.cache.Get(cacheKey); ok {
			h.metrics.observeCacheLookup(endpoint, "hit")
			markCacheHit(w, true)
			completionTokens := cachedResponseCompletionTokens(cachedResponse)
			promptTokens := cachedResponsePromptTokens(cachedResponse)
			if promptTokens <= 0 {
				promptTokens = estimatePromptTokensFromRequest(requestPayload)
			}
			cachedSource := "cache"
			cachedMode := mode

			prefillDelay, prefillErr := h.simulatePrefillDelay(ctx, promptTokens, replayDSL)
			if prefillDelay > 0 {
				h.metrics.observePrefillDuration(endpoint, cachedSource, prefillDelay)
				h.emitLifecycleEvent(ctx, slog.LevelInfo, "prefill", "", "prefill simulated",
					"source", cachedSource,
					"mode", cachedMode,
					"prompt_tokens", promptTokens,
					"prefill_ms", float64(prefillDelay)/float64(time.Millisecond),
				)
			}
			if prefillErr != nil {
				h.metrics.observeJob(endpoint, "failed", "cache", mode, time.Since(processingStartedAt))
				h.metrics.observeDSLJobDuration(endpoint, "cache", "failed", dslFamily, time.Since(processingStartedAt))
				h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
				emitCompleted(slog.LevelInfo, "failed", "cache", http.StatusServiceUnavailable, promptTokens, 0)
				return
			}

			timedWriter := newFirstByteMetricsWriter(w, func() {
				ttfb := time.Since(processingStartedAt)
				h.metrics.observeTimeToFirstByte(endpoint, cachedSource, cachedMode, ttfb)
				h.metrics.observeDSLTimeToFirstByte(endpoint, cachedSource, dslFamily, ttfb)
				h.emitLifecycleEvent(ctx, slog.LevelInfo, "first_token", "", "first token emitted",
					"source", cachedSource,
					"mode", cachedMode,
					"ttfb_ms", float64(ttfb)/float64(time.Millisecond),
				)
			})
			replay := replayOptions{
				maxTokens:    requestPayload.MaxTokens,
				includeUsage: requestPayload.StreamOptions != nil && requestPayload.StreamOptions.IncludeUsage,
				stream:       requestPayload.Stream,
				overrides:    replayDSL,
			}
			if requestPayload.Stream {
				h.replayCachedStream(ctx, timedWriter, cachedResponse, replay)
				deliveredTokens := completionTokens
				if replay.maxTokens > 0 && deliveredTokens > replay.maxTokens {
					deliveredTokens = replay.maxTokens
				}
				h.metrics.observeCompletionTokens(endpoint, "cache", deliveredTokens)
				h.metrics.observeJob(endpoint, "completed", "cache", modeLabel(true), time.Since(processingStartedAt))
				h.metrics.observeDSLJobDuration(endpoint, "cache", "completed", dslFamily, time.Since(processingStartedAt))
				h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
				emitCompleted(slog.LevelInfo, "completed", "cache", cachedResponse.statusCode, promptTokens, deliveredTokens)
				return
			}

			h.replayCachedResponse(ctx, timedWriter, cachedResponse, replay)
			h.metrics.observeCompletionTokens(endpoint, "cache", completionTokens)
			h.metrics.observeJob(endpoint, "completed", "cache", modeLabel(false), time.Since(processingStartedAt))
			h.metrics.observeDSLJobDuration(endpoint, "cache", "completed", dslFamily, time.Since(processingStartedAt))
			h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
			emitCompleted(slog.LevelInfo, "completed", "cache", cachedResponse.statusCode, promptTokens, completionTokens)
			return
		}
		h.metrics.observeCacheLookup(endpoint, "miss")
	}

	markCacheHit(w, false)
	timedWriter := newFirstByteMetricsWriter(w, func() {
		ttfb := time.Since(processingStartedAt)
		h.metrics.observeTimeToFirstByte(endpoint, "downstream", mode, ttfb)
		h.metrics.observeDSLTimeToFirstByte(endpoint, "downstream", dslFamily, ttfb)
		h.emitLifecycleEvent(ctx, slog.LevelInfo, "first_token", "", "first token emitted",
			"source", "downstream",
			"mode", mode,
			"ttfb_ms", float64(ttfb)/float64(time.Millisecond),
		)
	})

	if requestPayload.Stream {
		downstreamStartedAt := time.Now()
		cachedResponse, err := h.streamChatCompletion(ctx, timedWriter, requestPayload)
		h.metrics.observeDownstreamRequest(endpoint, mode, downstreamResultLabel(err), time.Since(downstreamStartedAt))
		if err != nil {
			h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
			h.metrics.observeDSLJobDuration(endpoint, "downstream", "failed", dslFamily, time.Since(processingStartedAt))
			h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
			emitCompleted(slog.LevelInfo, "failed", "downstream", http.StatusBadGateway, 0, 0)
			http.Error(timedWriter, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
			return
		}

		if h.cache != nil && !replayDSL.noCache {
			h.cache.Add(cacheKey, cachedResponse)
		}
		completionTokens := cachedResponseCompletionTokens(cachedResponse)
		promptTokens := cachedResponsePromptTokens(cachedResponse)
		h.metrics.observeCompletionTokens(endpoint, "downstream", completionTokens)
		h.metrics.observeJob(endpoint, "completed", "downstream", mode, time.Since(processingStartedAt))
		h.metrics.observeDSLJobDuration(endpoint, "downstream", "completed", dslFamily, time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
		emitCompleted(slog.LevelInfo, "completed", "downstream", cachedResponse.statusCode, promptTokens, completionTokens)
		return
	}

	downstreamStartedAt := time.Now()
	responseBody, statusCode, contentType, err := h.createChatCompletion(ctx, requestPayload)
	h.metrics.observeDownstreamRequest(endpoint, mode, downstreamResultLabel(err), time.Since(downstreamStartedAt))
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", mode, time.Since(processingStartedAt))
		h.metrics.observeDSLJobDuration(endpoint, "downstream", "failed", dslFamily, time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
		emitCompleted(slog.LevelInfo, "failed", "downstream", http.StatusBadGateway, 0, 0)
		http.Error(timedWriter, fmt.Sprintf("query downstream: %v", err), http.StatusBadGateway)
		return
	}

	if h.cache != nil && !replayDSL.noCache {
		h.cache.Add(cacheKey, cachedVLLMResponse{statusCode: statusCode, body: responseBody, contentType: contentType})
	}

	timedWriter.Header().Set("Content-Type", contentType)
	timedWriter.WriteHeader(statusCode)
	_, _ = timedWriter.Write(responseBody)
	completionTokens := cachedJSONTokenCount(responseBody)
	promptTokens := cachedJSONPromptTokens(responseBody)
	h.metrics.observeCompletionTokens(endpoint, "downstream", completionTokens)
	h.metrics.observeJob(endpoint, "completed", "downstream", mode, time.Since(processingStartedAt))
	h.metrics.observeDSLJobDuration(endpoint, "downstream", "completed", dslFamily, time.Since(processingStartedAt))
	h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
	emitCompleted(slog.LevelInfo, "completed", "downstream", statusCode, promptTokens, completionTokens)
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
		Enabled:       true,
		CacheSize:     capacity,
		CacheEntries:  len(keys),
		CacheFilePath: cacheFilePath,
		Keys:          keys,
		Action:        actionResult,
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
		if size < runtimeconfig.MinCacheSize || size > runtimeconfig.MaxCacheSize {
			return nil, fmt.Errorf("size must be between %d and %d", runtimeconfig.MinCacheSize, runtimeconfig.MaxCacheSize)
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

	h.configMu.RLock()
	prefillRateMultiplier := h.prefillRateMultiplier
	prefillBaseOverhead := h.prefillBaseOverhead
	prefillJitterPercent := h.prefillJitterPercent
	prefillMaxDuration := h.prefillMaxDuration
	streamVariabilityPercent := h.streamVariabilityPercent
	streamJitterPercent := h.streamJitterPercent
	streamStallProbabilityPercent := h.streamStallProbabilityPercent
	streamStallMin := h.streamStallMin
	streamStallMax := h.streamStallMax
	dslDefaultProfile := h.dslDefaultProfile
	h.configMu.RUnlock()

	return runtimeConfig{
		TokensInFlight:                processingStats.TokensInFlight,
		WaitingRequests:               processingStats.WaitingRequests,
		Version:                       buildinfo.Version,
		CacheSize:                     cacheSize,
		CacheEntries:                  cacheEntries,
		DownstreamURL:                 downstreamURL,
		DownstreamModel:               downstreamModel,
		SystemPrompt:                  defaults.systemPrompt,
		MaxTokens:                     defaults.maxTokens,
		MaxTokensPerSecond:            processingStats.MaxTokensPerSecond,
		EffectiveTokensPerSecond:      processingStats.EffectiveTokensPerSecond,
		MaxTokensInFlight:             processingStats.MaxTokensInFlight,
		MaxWaitingRequests:            processingStats.MaxWaitingRequests,
		MaxDegradation:                processingStats.MaxDegradation,
		ComputedDegradationPercentage: processingStats.ComputedDegradationPercentage,
		Temperature:                   defaults.temperature,
		PrefillRateMultiplier:         prefillRateMultiplier,
		PrefillBaseOverheadMs:         int(prefillBaseOverhead / time.Millisecond),
		PrefillJitterPercent:          prefillJitterPercent,
		PrefillMaxMs:                  int(prefillMaxDuration / time.Millisecond),
		StreamVariabilityPercent:      streamVariabilityPercent,
		StreamJitterPercent:           streamJitterPercent,
		StreamStallProbabilityPercent: streamStallProbabilityPercent,
		StreamStallMinMs:              int(streamStallMin / time.Millisecond),
		StreamStallMaxMs:              int(streamStallMax / time.Millisecond),
		DSLDefaultProfile:             dslDefaultProfile,
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
	return h.applyConfigValues(r.URL.Query())
}

func (h *Handler) applyConfigValues(queryValues url.Values) (bool, error) {
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
	var maxTokensInFlight int64
	var maxWaitingRequests int
	if scheduler != nil {
		cap, _, mw, _ := scheduler.Stats()
		maxTokensInFlight = int64(cap)
		maxWaitingRequests = mw
	}
	previousMaxTokensInFlight := maxTokensInFlight
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

	if maxTokensInFlightRaw := configQueryValue(queryValues, "max-tokens-in-flight", "max_tokens_in_flight"); maxTokensInFlightRaw != "" {
		parsed, err := strconv.ParseInt(maxTokensInFlightRaw, 10, 64)
		if err != nil {
			return false, fmt.Errorf("invalid max-tokens-in-flight %q", maxTokensInFlightRaw)
		}
		if parsed < int64(minMaxTokensInFlight) || parsed > int64(maxMaxTokensInFlight) {
			return false, fmt.Errorf("max-tokens-in-flight must be between %d and %d", minMaxTokensInFlight, maxMaxTokensInFlight)
		}
		maxTokensInFlight = parsed
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

	h.configMu.RLock()
	prefillRateMultiplier := h.prefillRateMultiplier
	prefillBaseOverheadMs := int(h.prefillBaseOverhead / time.Millisecond)
	prefillJitterPercent := h.prefillJitterPercent
	prefillMaxMs := int(h.prefillMaxDuration / time.Millisecond)
	h.configMu.RUnlock()
	prefillChanged := false

	if raw := configQueryValue(queryValues, "prefill-rate-multiplier", "prefill_rate_multiplier"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return false, fmt.Errorf("invalid prefill-rate-multiplier %q", raw)
		}
		if parsed < minPrefillRateMultiplier || parsed > maxPrefillRateMultiplier {
			return false, fmt.Errorf("prefill-rate-multiplier must be between %g and %g", minPrefillRateMultiplier, maxPrefillRateMultiplier)
		}
		prefillRateMultiplier = parsed
		prefillChanged = true
	}
	if raw := configQueryValue(queryValues, "prefill-base-overhead-ms", "prefill_base_overhead_ms"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid prefill-base-overhead-ms %q", raw)
		}
		if parsed < minPrefillBaseOverheadMs || parsed > maxPrefillBaseOverheadMs {
			return false, fmt.Errorf("prefill-base-overhead-ms must be between %d and %d", minPrefillBaseOverheadMs, maxPrefillBaseOverheadMs)
		}
		prefillBaseOverheadMs = parsed
		prefillChanged = true
	}
	if raw := configQueryValue(queryValues, "prefill-jitter-percent", "prefill_jitter_percent"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid prefill-jitter-percent %q", raw)
		}
		if parsed < minPrefillJitterPercent || parsed > maxPrefillJitterPercent {
			return false, fmt.Errorf("prefill-jitter-percent must be between %d and %d", minPrefillJitterPercent, maxPrefillJitterPercent)
		}
		prefillJitterPercent = parsed
		prefillChanged = true
	}
	if raw := configQueryValue(queryValues, "prefill-max-ms", "prefill_max_ms"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid prefill-max-ms %q", raw)
		}
		if parsed < 1 {
			return false, fmt.Errorf("prefill-max-ms must be positive")
		}
		prefillMaxMs = parsed
		prefillChanged = true
	}

	h.configMu.RLock()
	streamVariabilityPercent := h.streamVariabilityPercent
	streamJitterPercent := h.streamJitterPercent
	streamStallProbabilityPercent := h.streamStallProbabilityPercent
	streamStallMinMs := int(h.streamStallMin / time.Millisecond)
	streamStallMaxMs := int(h.streamStallMax / time.Millisecond)
	h.configMu.RUnlock()
	streamChanged := false

	if raw := configQueryValue(queryValues, "stream-variability-percent", "stream_variability_percent"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid stream-variability-percent %q", raw)
		}
		if parsed < minStreamVariabilityPercent || parsed > maxStreamVariabilityPercent {
			return false, fmt.Errorf("stream-variability-percent must be between %d and %d", minStreamVariabilityPercent, maxStreamVariabilityPercent)
		}
		streamVariabilityPercent = parsed
		streamChanged = true
	}
	if raw := configQueryValue(queryValues, "stream-jitter-percent", "stream_jitter_percent"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid stream-jitter-percent %q", raw)
		}
		if parsed < minStreamJitterPercent || parsed > maxStreamJitterPercent {
			return false, fmt.Errorf("stream-jitter-percent must be between %d and %d", minStreamJitterPercent, maxStreamJitterPercent)
		}
		streamJitterPercent = parsed
		streamChanged = true
	}
	if raw := configQueryValue(queryValues, "stream-stall-probability-percent", "stream_stall_probability_percent"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid stream-stall-probability-percent %q", raw)
		}
		if parsed < minStreamStallProbabilityPercent || parsed > maxStreamStallProbabilityPercent {
			return false, fmt.Errorf("stream-stall-probability-percent must be between %d and %d", minStreamStallProbabilityPercent, maxStreamStallProbabilityPercent)
		}
		streamStallProbabilityPercent = parsed
		streamChanged = true
	}
	if raw := configQueryValue(queryValues, "stream-stall-min-ms", "stream_stall_min_ms"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid stream-stall-min-ms %q", raw)
		}
		if parsed < minStreamStallMs || parsed > maxStreamStallMs {
			return false, fmt.Errorf("stream-stall-min-ms must be between %d and %d", minStreamStallMs, maxStreamStallMs)
		}
		streamStallMinMs = parsed
		streamChanged = true
	}
	if raw := configQueryValue(queryValues, "stream-stall-max-ms", "stream_stall_max_ms"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return false, fmt.Errorf("invalid stream-stall-max-ms %q", raw)
		}
		if parsed < minStreamStallMs || parsed > maxStreamStallMs {
			return false, fmt.Errorf("stream-stall-max-ms must be between %d and %d", minStreamStallMs, maxStreamStallMs)
		}
		streamStallMaxMs = parsed
		streamChanged = true
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
	downstreamToken := h.downstreamToken
	downstreamModel := h.downstreamModel
	h.modelsMu.RUnlock()
	if downstreamURLRaw := configQueryValue(queryValues, "downstream-url", "downstream_url"); downstreamURLRaw != "" {
		downstreamURL = downstreamURLRaw
	}
	if downstreamTokenRaw := configQueryValue(queryValues, "downstream-token", "downstream_token"); downstreamTokenRaw != "" {
		downstreamToken = downstreamTokenRaw
	}
	if downstreamModelRaw := configQueryValue(queryValues, "downstream-model", "downstream_model"); downstreamModelRaw != "" {
		downstreamModel = downstreamModelRaw
	}

	// dsl-profile uses Has(...) rather than the value-presence helper so an
	// explicit `?dsl-profile=` (empty value) clears the default. Validate
	// against the currently loaded profile map before committing.
	dslProfileChanged := false
	dslProfileNew := ""
	if queryValues.Has("dsl-profile") || queryValues.Has("dsl_profile") {
		dslProfileChanged = true
		dslProfileNew = configQueryValue(queryValues, "dsl-profile", "dsl_profile")
		if dslProfileNew != "" {
			h.configMu.RLock()
			_, ok := h.dslProfiles[strings.ToLower(strings.TrimSpace(dslProfileNew))]
			h.configMu.RUnlock()
			if !ok {
				return false, fmt.Errorf("unknown dsl-profile %q", dslProfileNew)
			}
		}
	}

	h.configMu.Lock()
	h.defaults = defaults
	h.configMu.Unlock()
	h.SetCacheSize(cacheSize)
	h.SetDownstreamURL(downstreamURL)
	h.SetDownstreamToken(downstreamToken)
	h.SetDownstreamModel(downstreamModel)
	h.SetRequestProcessingLimits(maxTokensPerSecond, int(maxTokensInFlight), maxWaitingRequests, maxDegradation)
	if prefillChanged {
		h.SetPrefillSimulation(prefillRateMultiplier, prefillBaseOverheadMs, prefillJitterPercent, prefillMaxMs)
	}
	if streamChanged {
		h.SetStreamRealism(streamVariabilityPercent, streamJitterPercent, streamStallProbabilityPercent, streamStallMinMs, streamStallMaxMs)
	}
	if dslProfileChanged {
		// Validation already passed above; ignore the error path.
		_ = h.SetDSLDefaultProfile(dslProfileNew)
	}
	updatedMaxTokensInFlight, updatedTokensInFlight, updatedMaxWaitingRequests, updatedWaitingRequests := h.RequestQueueStats()
	if updatedMaxTokensInFlight != previousMaxTokensInFlight || updatedMaxWaitingRequests != previousMaxWaitingRequests {
		slog.Info(
			"request queue limits updated",
			"max_tokens_in_flight", updatedMaxTokensInFlight,
			"previous_max_tokens_in_flight", previousMaxTokensInFlight,
			"tokens_in_flight", updatedTokensInFlight,
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
	body         []byte
	statusCode   int
	contentType  string
	defaultModel string
}

type chatCompletionRequest struct {
	Model         string                       `json:"model"`
	Messages      []chatCompletionMessage      `json:"messages"`
	Temperature   float64                      `json:"temperature"`
	MaxTokens     int                          `json:"max_tokens"`
	Stream        bool                         `json:"stream,omitempty"`
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
	Messages []chatCompletionMessage `json:"messages"`
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
	// Disable transparent gzip on the upstream SSE stream. Go's default
	// transport advertises Accept-Encoding: gzip and decodes inline, but
	// gzip block boundaries buffer multiple SSE chunks together, which
	// destroys per-token TTFT. Asking for identity guarantees vLLM emits
	// the same wire bytes our reader hands to the client.
	request.Header.Set("Accept-Encoding", "identity")
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
	// SSE anti-buffering hints: disable client/intermediary caches and
	// disable nginx response buffering so each chunk reaches the client
	// as it is written.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
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
	filtered := make([]chatCompletionMessage, 0, len(requestPayload.Messages))
	for _, message := range requestPayload.Messages {
		if message.Role == "system" {
			continue
		}
		filtered = append(filtered, chatCompletionMessage{
			Role:    message.Role,
			Content: normalizeCacheKeyContent(message.Content),
		})
	}
	requestBody, err := json.Marshal(chatCompletionCacheKey{Messages: filtered})
	if err != nil {
		return "", fmt.Errorf("marshal chat completion cache key: %w", err)
	}

	sum := sha256.Sum256(requestBody)
	return hex.EncodeToString(sum[:]), nil
}

// normalizeCacheKeyContent reduces a message's content to a fuzzy canonical
// form so semantically-equivalent prompts collapse to the same cache key:
//   - lowercased
//   - punctuation and symbols stripped (Unicode-aware)
//   - tokens split on whitespace
//   - English stop words removed
//   - remaining tokens sorted and space-joined
func normalizeCacheKeyContent(content string) string {
	lowered := strings.ToLower(content)
	var b strings.Builder
	b.Grow(len(lowered))
	for _, r := range lowered {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			// drop punctuation, symbols, control characters
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	tokens := fields[:0]
	for _, f := range fields {
		if _, isStop := cacheKeyStopWords[f]; isStop {
			continue
		}
		tokens = append(tokens, f)
	}
	sort.Strings(tokens)
	return strings.Join(tokens, " ")
}

// cacheKeyStopWords is a compact English stop-word set used only for
// computing fuzzy cache keys. Keep it short and stable; changing this set
// invalidates existing cache entries.
var cacheKeyStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "but": {}, "by": {},
	"can": {}, "could": {},
	"did": {}, "do": {}, "does": {},
	"for": {}, "from": {},
	"had": {}, "has": {}, "have": {}, "he": {}, "her": {}, "hers": {}, "him": {}, "his": {}, "how": {},
	"i": {}, "if": {}, "in": {}, "into": {}, "is": {}, "it": {}, "its": {},
	"just": {},
	"me": {}, "my": {},
	"no": {}, "not": {},
	"of": {}, "on": {}, "or": {}, "our": {}, "ours": {},
	"please": {},
	"she": {}, "should": {}, "so": {},
	"than": {}, "that": {}, "the": {}, "their": {}, "them": {}, "then": {}, "there": {}, "these": {}, "they": {}, "this": {}, "those": {}, "to": {},
	"us":  {},
	"was": {}, "we": {}, "were": {}, "what": {}, "when": {}, "where": {}, "which": {}, "who": {}, "why": {}, "will": {}, "with": {}, "would": {},
	"you": {}, "your": {}, "yours": {},
}

type replayOptions struct {
	maxTokens    int
	includeUsage bool
	stream       bool
	overrides    replayOverrides
}

func (h *Handler) replayCachedStream(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse, opts replayOptions) {
	w.Header().Set("Content-Type", contentTypeOrDefault("text/event-stream", "text/event-stream"))
	// Match the live-stream path's anti-buffering hints so cache replays
	// flush per-token to the client too.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(cachedResponse.statusCode)

	flusher, _ := w.(http.Flusher)
	streamID := newChatCompletionID()
	createdAt := time.Now().UnixMilli()

	var segments []replayStreamSegment
	if cachedResponse.streaming {
		segments = parseCachedStreamReplaySegments(cachedResponse.body)
	} else {
		segments = synthesizeStreamSegmentsFromJSON(cachedResponse.body)
	}

	contentEmitted := 0
	for _, segment := range segments {
		kind := classifyStreamSegment(segment.line)
		switch kind {
		case streamSegmentKindContent:
			if opts.maxTokens > 0 && contentEmitted > 0 && contentEmitted+segment.tokenCount > opts.maxTokens {
				continue
			}
			contentEmitted += segment.tokenCount
		case streamSegmentKindFinish:
			// always emit finish chunk
		case streamSegmentKindUsage:
			// Skip cached usage chunks; we synthesize one below so completion_tokens
			// reflects what was actually delivered (truncation may have occurred).
			continue
		case streamSegmentKindDone:
			// Defer [DONE] until after the synthesized usage chunk.
			continue
		}

		if len(segment.line) == 0 {
			continue
		}
		rewritten := rewriteSSEDataLine(segment.line, true, streamID, createdAt)
		_, _ = w.Write(rewritten)
		if flusher != nil {
			flusher.Flush()
		}
		// Throttle AFTER emitting the chunk, not before. This matches the
		// timing of a real LLM stream: prefill delay covers the time to the
		// first token (handled separately by simulatePrefillDelay before the
		// replay loop starts), and the per-segment decode delay paces the
		// gap between consecutive chunks. Throttling before the first
		// content write would double-count prefill and inflate TTFT
		// proportional to however many tokens happened to be packed into
		// the first cached SSE chunk.
		if err := h.throttleStreamSegment(ctx, segment.tokenCount, opts.overrides); err != nil {
			return
		}
	}

	// Emit a usage segment when the client asked for it, with completion_tokens
	// reflecting the actual number of tokens delivered after any truncation.
	if opts.includeUsage {
		promptTokens := cachedResponsePromptTokens(cachedResponse)
		usagePayload := map[string]any{
			"id":      streamID,
			"object":  "chat.completion.chunk",
			"created": createdAt,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": contentEmitted,
				"total_tokens":      promptTokens + contentEmitted,
			},
			"cache": true,
		}
		if line := encodeSSELine(usagePayload); line != nil {
			_, _ = w.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func (h *Handler) replayCachedResponse(ctx context.Context, w http.ResponseWriter, cachedResponse cachedVLLMResponse, opts replayOptions) {
	body, contentType := canonicalJSONResponse(cachedResponse)
	body = rewriteJSONCacheField(body, true)

	pacingTokens := cachedJSONTokenCount(body)
	if opts.maxTokens > 0 && pacingTokens > opts.maxTokens {
		pacingTokens = opts.maxTokens
	}
	if err := h.throttleCachedReplay(ctx, pacingTokens, opts.overrides); err != nil {
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(cachedResponse.statusCode)
	_, _ = w.Write(body)
}

type streamSegmentKind int

const (
	streamSegmentKindOther streamSegmentKind = iota
	streamSegmentKindContent
	streamSegmentKindFinish
	streamSegmentKindUsage
	streamSegmentKindDone
)

func classifyStreamSegment(line []byte) streamSegmentKind {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return streamSegmentKindOther
	}
	payload := bytes.TrimPrefix(trimmed, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return streamSegmentKindDone
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return streamSegmentKindOther
	}
	if chunk.Usage != nil && len(chunk.Choices) == 0 {
		return streamSegmentKindUsage
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			return streamSegmentKindContent
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			return streamSegmentKindFinish
		}
	}
	return streamSegmentKindOther
}

func synthesizeStreamSegmentsFromJSON(body []byte) []replayStreamSegment {
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) == 0 {
		return nil
	}
	role := response.Choices[0].Message.Role
	if role == "" {
		role = "assistant"
	}
	finishReason := response.Choices[0].FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	completionTokens := response.Usage.CompletionTokens
	if completionTokens <= 0 {
		completionTokens = estimateTextTokens(response.Choices[0].Message.Content)
	}
	if completionTokens <= 0 {
		completionTokens = 1
	}

	segments := make([]replayStreamSegment, 0, completionTokens+4)
	roleLine := encodeSSELine(map[string]any{
		"object": "chat.completion.chunk",
		"model":  response.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": role},
			"finish_reason": nil,
		}},
	})
	if roleLine != nil {
		segments = append(segments, replayStreamSegment{line: roleLine, tokenCount: 0})
	}

	contentChunks := splitContentByCount(response.Choices[0].Message.Content, completionTokens)
	for _, chunk := range contentChunks {
		line := encodeSSELine(map[string]any{
			"object": "chat.completion.chunk",
			"model":  response.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": chunk},
				"finish_reason": nil,
			}},
		})
		if line == nil {
			continue
		}
		segments = append(segments, replayStreamSegment{line: line, tokenCount: 1})
	}

	finishLine := encodeSSELine(map[string]any{
		"object": "chat.completion.chunk",
		"model":  response.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	})
	if finishLine != nil {
		segments = append(segments, replayStreamSegment{line: finishLine, tokenCount: 0})
	}

	usageLine := encodeSSELine(map[string]any{
		"object":  "chat.completion.chunk",
		"model":   response.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     response.Usage.PromptTokens,
			"completion_tokens": response.Usage.CompletionTokens,
			"total_tokens":      response.Usage.TotalTokens,
		},
	})
	if usageLine != nil {
		segments = append(segments, replayStreamSegment{line: usageLine, tokenCount: 0})
	}

	segments = append(segments, replayStreamSegment{line: []byte("data: [DONE]\n\n"), tokenCount: 0})
	return segments
}

func splitContentByCount(content string, count int) []string {
	if count <= 0 {
		return nil
	}
	runes := []rune(content)
	if len(runes) == 0 {
		out := make([]string, count)
		for i := range out {
			out[i] = ""
		}
		return out
	}
	if count >= len(runes) {
		out := make([]string, len(runes))
		for i, r := range runes {
			out[i] = string(r)
		}
		return out
	}
	out := make([]string, count)
	base := len(runes) / count
	extra := len(runes) % count
	pos := 0
	for i := 0; i < count; i++ {
		size := base
		if i < extra {
			size++
		}
		out[i] = string(runes[pos : pos+size])
		pos += size
	}
	return out
}

func encodeSSELine(payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(encoded)+10)
	out = append(out, []byte("data: ")...)
	out = append(out, encoded...)
	out = append(out, []byte("\n\n")...)
	return out
}

func canonicalJSONResponse(cachedResponse cachedVLLMResponse) ([]byte, string) {
	if !cachedResponse.streaming {
		return cachedResponse.body, contentTypeOrDefault(cachedResponse.contentType, "application/json")
	}
	body := convertSSEToChatCompletionJSON(cachedResponse.body)
	return body, "application/json"
}

func convertSSEToChatCompletionJSON(body []byte) []byte {
	reader := bytes.NewReader(body)
	contentBuilder := strings.Builder{}
	model := ""
	id := ""
	created := int64(0)
	finishReason := "stop"
	role := "assistant"
	var promptTokens, completionTokens, totalTokens int
	for {
		line, err := readSSELine(reader)
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data: ")) {
				payload := bytes.TrimPrefix(trimmed, []byte("data: "))
				if !bytes.Equal(payload, []byte("[DONE]")) {
					var chunk struct {
						ID      string `json:"id"`
						Created int64  `json:"created"`
						Model   string `json:"model"`
						Choices []struct {
							Delta struct {
								Role    string `json:"role"`
								Content string `json:"content"`
							} `json:"delta"`
							FinishReason *string `json:"finish_reason"`
						} `json:"choices"`
						Usage *struct {
							PromptTokens     int `json:"prompt_tokens"`
							CompletionTokens int `json:"completion_tokens"`
							TotalTokens      int `json:"total_tokens"`
						} `json:"usage"`
					}
					if jsonErr := json.Unmarshal(payload, &chunk); jsonErr == nil {
						if chunk.ID != "" {
							id = chunk.ID
						}
						if chunk.Created != 0 {
							created = chunk.Created
						}
						if chunk.Model != "" {
							model = chunk.Model
						}
						for _, choice := range chunk.Choices {
							if choice.Delta.Role != "" {
								role = choice.Delta.Role
							}
							if choice.Delta.Content != "" {
								contentBuilder.WriteString(choice.Delta.Content)
							}
							if choice.FinishReason != nil && *choice.FinishReason != "" {
								finishReason = *choice.FinishReason
							}
						}
						if chunk.Usage != nil {
							promptTokens = chunk.Usage.PromptTokens
							completionTokens = chunk.Usage.CompletionTokens
							totalTokens = chunk.Usage.TotalTokens
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if id == "" {
		id = newChatCompletionID()
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    role,
				"content": contentBuilder.String(),
			},
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      totalTokens,
		},
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return encoded
}

func (h *Handler) acquireRequestSlot(ctx context.Context, cost requestCost, path string) (func(), time.Duration, bool) {
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return func() {}, 0, true
	}
	release, queueWait, ok := scheduler.Acquire(ctx, cost, path)
	if !ok {
		return nil, 0, false
	}
	h.logComputedDegradationIfChanged("request_admitted")
	return func() {
		release()
		h.logComputedDegradationIfChanged("request_completed")
	}, queueWait, true
}

// observeCompletionTokens feeds an actual completion-token count into the
// global p95 estimator, improving cost estimates for subsequent requests.
func (h *Handler) observeCompletionTokens(completionTokens int) {
	if completionTokens <= 0 {
		return
	}
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return
	}
	scheduler.Observe(completionTokens)
}

// globalEstimator returns the scheduler's completion-token p95 estimator,
// used to compute cost before admission. Returns nil if the scheduler is
// not yet initialized.
func (h *Handler) globalEstimator() *completionEstimator {
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return nil
	}
	return scheduler.Estimator()
}

// resolveRequestTenant returns the tenantState for a request's
// X-Tenant-Id header, falling back to the default tenant for missing,
// invalid, or unregistered values. Always returns non-nil.
func (h *Handler) resolveRequestTenant(r *http.Request) *tenantState {
	h.configMu.RLock()
	tenants := h.tenants
	h.configMu.RUnlock()
	if tenants == nil {
		// Should not happen in production (NewHandler initializes the
		// registry) but guard for tests that build Handler{} directly.
		return &tenantState{name: defaultTenantName, bucket: newTenantBucket(0, 0), estimator: newCompletionEstimator(256, 50)}
	}
	return tenants.resolve(r.Header.Get(tenantHeader))
}

// SetTenants installs a new tenant configuration set. The "default"
// tenant is always preserved; existing tenants matching new names are
// updated in place; missing tenants are removed. Pass nil/empty to
// reset to default-only.
func (h *Handler) SetTenants(tenants map[string]TenantConfig) {
	h.configMu.RLock()
	registry := h.tenants
	h.configMu.RUnlock()
	if registry == nil {
		return
	}
	registry.configure(tenants)
}

// SetDefaultTenantConfig updates the configuration applied to the
// always-present "default" tenant.
func (h *Handler) SetDefaultTenantConfig(cfg TenantConfig) {
	h.configMu.RLock()
	registry := h.tenants
	h.configMu.RUnlock()
	if registry == nil {
		return
	}
	registry.mu.Lock()
	registry.defaultConfig = cfg
	def, ok := registry.tenants[defaultTenantName]
	registry.mu.Unlock()
	if ok {
		def.bucket.reconfigure(cfg.Rate, cfg.Burst)
	}
}

// TenantNames returns a snapshot of registered tenant names. Order is
// not guaranteed; callers needing deterministic order should sort.
func (h *Handler) TenantNames() []string {
	h.configMu.RLock()
	registry := h.tenants
	h.configMu.RUnlock()
	if registry == nil {
		return nil
	}
	return registry.names()
}

func (h *Handler) throttleCachedReplay(ctx context.Context, tokenCount int, overrides replayOverrides) error {
	if tokenCount < 1 {
		return nil
	}
	delay := h.cachedReplayDelay(tokenCount, overrides)
	if delay <= 0 {
		return nil
	}
	if overrides.delayScaleFn != nil {
		delay = time.Duration(float64(delay) * overrides.delayScaleFn())
		if delay < 0 {
			delay = 0
		}
	}
	if delay <= 0 {
		return nil
	}
	return h.sleep(ctx, delay)
}

func (h *Handler) throttleStreamSegment(ctx context.Context, tokenCount int, overrides replayOverrides) error {
	if tokenCount < 1 {
		return nil
	}
	delay, stall := h.computeStreamSegmentDelay(tokenCount, overrides)
	if delay <= 0 {
		return nil
	}
	if stall > 0 && h.metrics != nil {
		h.metrics.observeStreamStall(chatCompletionsRouteLabel, "cache", stall)
	}
	return h.sleep(ctx, delay)
}

func (h *Handler) cachedReplayDelay(tokenCount int, overrides replayOverrides) time.Duration {
	if tokenCount < 1 {
		return 0
	}
	if overrides.noTPS {
		return 0
	}

	h.configMu.RLock()
	baseTokensPerSecond := h.maxTokensPerSecond
	maxDegradation := h.maxDegradation
	scheduler := h.scheduler
	h.configMu.RUnlock()

	if overrides.tpsOverride > 0 {
		baseTokensPerSecond = overrides.tpsOverride
	}

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

// defaultJitterSource returns a uniform float64 in [-1.0, 1.0).
func defaultJitterSource() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	// Convert 8 random bytes to a uniform [0, 1) float, then map to [-1, 1).
	u := binary.BigEndian.Uint64(b[:]) >> 11 // 53 bits of mantissa precision
	f := float64(u) / float64(uint64(1)<<53)
	return f*2 - 1
}

// SetPrefillSimulation configures the simulated prefill latency model used on
// cache hits. The effective prefill rate is rateMultiplier * max-tokens-per-second.
// A rateMultiplier of 0 disables prefill simulation entirely.
func (h *Handler) SetPrefillSimulation(rateMultiplier float64, baseOverheadMs, jitterPercent, maxMs int) {
	if rateMultiplier < minPrefillRateMultiplier || rateMultiplier > maxPrefillRateMultiplier {
		rateMultiplier = defaultPrefillRateMultiplier
	}
	if baseOverheadMs < minPrefillBaseOverheadMs || baseOverheadMs > maxPrefillBaseOverheadMs {
		baseOverheadMs = defaultPrefillBaseOverheadMs
	}
	if jitterPercent < minPrefillJitterPercent || jitterPercent > maxPrefillJitterPercent {
		jitterPercent = defaultPrefillJitterPercent
	}
	if maxMs < 1 {
		maxMs = defaultPrefillMaxMs
	}

	h.configMu.Lock()
	h.prefillRateMultiplier = rateMultiplier
	h.prefillBaseOverhead = time.Duration(baseOverheadMs) * time.Millisecond
	h.prefillJitterPercent = jitterPercent
	h.prefillMaxDuration = time.Duration(maxMs) * time.Millisecond
	h.configMu.Unlock()
}

// computePrefillDelay returns the simulated prefill latency for the given
// prompt token count. A return of 0 means prefill simulation is disabled.
func (h *Handler) computePrefillDelay(promptTokens int, overrides replayOverrides) time.Duration {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if overrides.noPrefill {
		return 0
	}

	h.configMu.RLock()
	rateMultiplier := h.prefillRateMultiplier
	base := h.prefillBaseOverhead
	jitterPercent := h.prefillJitterPercent
	maxDuration := h.prefillMaxDuration
	baseTokensPerSecond := h.maxTokensPerSecond
	maxDegradation := h.maxDegradation
	scheduler := h.scheduler
	jitterSource := h.jitterSource
	h.configMu.RUnlock()

	if overrides.tpsOverride > 0 {
		baseTokensPerSecond = overrides.tpsOverride
	}

	if rateMultiplier <= 0 || baseTokensPerSecond <= 0 {
		return 0
	}

	effectiveDecodeRate := calibratedTokensPerSecond(baseTokensPerSecond)
	if scheduler != nil {
		effectiveDecodeRate = scheduler.effectiveTokensPerSecond(baseTokensPerSecond, maxDegradation)
	}
	prefillRate := effectiveDecodeRate * rateMultiplier
	if prefillRate <= 0 {
		return 0
	}

	delay := base + time.Duration(float64(promptTokens)/prefillRate*float64(time.Second))
	if jitterPercent > 0 && jitterSource != nil {
		j := jitterSource() // [-1, 1)
		delay = time.Duration(float64(delay) * (1 + j*float64(jitterPercent)/100))
	}
	if overrides.prefillDurationScale > 0 && overrides.prefillDurationScale != 1 {
		delay = time.Duration(float64(delay) * overrides.prefillDurationScale)
	}
	if delay < 0 {
		delay = 0
	}
	if maxDuration > 0 && delay > maxDuration {
		delay = maxDuration
	}
	return delay
}

func (h *Handler) simulatePrefillDelay(ctx context.Context, promptTokens int, overrides replayOverrides) (time.Duration, error) {
	delay := h.computePrefillDelay(promptTokens, overrides)
	if delay <= 0 {
		return 0, nil
	}
	if err := h.sleep(ctx, delay); err != nil {
		return delay, err
	}
	return delay, nil
}

// SetStreamRealism configures variability, jitter, and partial-stall behavior
// applied per content segment during cached stream replay. Setting all knobs
// to zero (or rate-multiplier-equivalent: max-tokens-per-second to 0) disables
// stream realism beyond the base pacing model.
func (h *Handler) SetStreamRealism(variabilityPercent, jitterPercent, stallProbabilityPercent, stallMinMs, stallMaxMs int) {
	if variabilityPercent < minStreamVariabilityPercent || variabilityPercent > maxStreamVariabilityPercent {
		variabilityPercent = defaultStreamVariabilityPercent
	}
	if jitterPercent < minStreamJitterPercent || jitterPercent > maxStreamJitterPercent {
		jitterPercent = defaultStreamJitterPercent
	}
	if stallProbabilityPercent < minStreamStallProbabilityPercent || stallProbabilityPercent > maxStreamStallProbabilityPercent {
		stallProbabilityPercent = defaultStreamStallProbabilityPercent
	}
	if stallMinMs < minStreamStallMs || stallMinMs > maxStreamStallMs {
		stallMinMs = defaultStreamStallMinMs
	}
	if stallMaxMs < minStreamStallMs || stallMaxMs > maxStreamStallMs {
		stallMaxMs = defaultStreamStallMaxMs
	}
	if stallMaxMs < stallMinMs {
		stallMaxMs = stallMinMs
	}

	h.configMu.Lock()
	h.streamVariabilityPercent = variabilityPercent
	h.streamJitterPercent = jitterPercent
	h.streamStallProbabilityPercent = stallProbabilityPercent
	h.streamStallMin = time.Duration(stallMinMs) * time.Millisecond
	h.streamStallMax = time.Duration(stallMaxMs) * time.Millisecond
	h.configMu.Unlock()
}

// computeStreamSegmentDelay returns the simulated delay for one streamed
// content segment, plus any stall added on top. The stall component is
// returned separately so callers can record metrics distinct from the base
// pacing delay. Returns (0, 0) when pacing is disabled (rate=0) or when the
// segment carries no tokens.
func (h *Handler) computeStreamSegmentDelay(tokenCount int, overrides replayOverrides) (total, stall time.Duration) {
	if tokenCount < 1 {
		return 0, 0
	}
	if overrides.noTPS {
		return 0, 0
	}
	base := h.cachedReplayDelay(tokenCount, overrides)
	if base <= 0 {
		return 0, 0
	}

	h.configMu.RLock()
	variabilityPercent := overrides.resolveVariabilityPercent(h.streamVariabilityPercent)
	jitterPercent := overrides.resolveJitterPercent(h.streamJitterPercent)
	stallProbPercent := overrides.resolveStallPercent(h.streamStallProbabilityPercent)
	stallMin := h.streamStallMin
	stallMax := h.streamStallMax
	jitterSource := h.jitterSource
	h.configMu.RUnlock()

	delay := base
	if variabilityPercent > 0 && jitterSource != nil {
		v := jitterSource() // [-1, 1)
		delay = time.Duration(float64(delay) * (1 + v*float64(variabilityPercent)/100))
	}
	if jitterPercent > 0 && jitterSource != nil {
		j := jitterSource() // [-1, 1)
		delay = time.Duration(float64(delay) * (1 + j*float64(jitterPercent)/100))
	}
	if delay < 0 {
		delay = 0
	}

	if stallProbPercent > 0 && jitterSource != nil {
		// jitterSource returns [-1, 1); shift to [0, 1) for the probability draw.
		p := (jitterSource() + 1) / 2
		if p < float64(stallProbPercent)/100 {
			// Map a second draw into [stallMin, stallMax].
			r := (jitterSource() + 1) / 2
			if stallMax < stallMin {
				stallMax = stallMin
			}
			stall = stallMin + time.Duration(r*float64(stallMax-stallMin))
			delay += stall
		}
	}
	if overrides.delayScaleFn != nil {
		delay = time.Duration(float64(delay) * overrides.delayScaleFn())
		if delay < 0 {
			delay = 0
		}
	}
	return delay, stall
}

// estimatePromptTokensFromRequest is used as a fallback when the cached
// response does not record prompt_tokens.
func estimatePromptTokensFromRequest(payload chatCompletionRequest) int {
	parts := make([]string, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		if m.Content == "" {
			continue
		}
		parts = append(parts, m.Content)
	}
	return estimateTextTokens(strings.Join(parts, " "))
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
		"tokens_in_flight", stats.TokensInFlight,
		"max_tokens_in_flight", stats.MaxTokensInFlight,
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
	return float64(configuredTokensPerSecond)
}

// requestScheduler is a cost-based admission gate. It charges each request
// a token cost (prompt_tokens + min(max_tokens, p95_completion_tokens)) to a
// token budget; when the budget is full, requests block FIFO until enough
// cost is released. It composes a tokenBudget primitive with logging and
// Prometheus metrics integration.
type requestScheduler struct {
	mu                sync.Mutex
	budget            *tokenBudget
	estimator         *completionEstimator
	metrics           *handlerMetrics
	maxTokensInFlight int64
	maxWaiting        int
}

func newRequestScheduler(maxTokensInFlight, maxWaitingRequests int) *requestScheduler {
	if maxTokensInFlight < 1 {
		maxTokensInFlight = 1
	}
	if maxWaitingRequests < 0 {
		maxWaitingRequests = 0
	}
	return &requestScheduler{
		budget:            newTokenBudget(int64(maxTokensInFlight), maxWaitingRequests),
		estimator:         newCompletionEstimator(256, 50),
		maxTokensInFlight: int64(maxTokensInFlight),
		maxWaiting:        maxWaitingRequests,
	}
}

// Acquire charges cost.totalCost against the budget. It returns a release
// closure (which refunds the same cost when called), the time spent waiting,
// and ok=false when over capacity (queue full or oversized request).
func (s *requestScheduler) Acquire(ctx context.Context, cost requestCost, path string) (func(), time.Duration, bool) {
	logger := loggerFromContext(ctx)
	chargedCost := int64(cost.totalCost)
	if chargedCost < 1 {
		chargedCost = 1
	}

	// Peek to determine whether we will be queued. The result is advisory
	// (used only for logging) and races with concurrent acquirers are
	// acceptable.
	capacity, inFlight, _, _ := s.budget.stats()
	willQueue := inFlight+chargedCost > capacity

	if willQueue {
		// Pre-log the queue event before blocking on acquire. We log here
		// because once acquire returns, we cannot distinguish "blocked
		// briefly" from "rejected after blocking" in the queued log.
		_, _, waiting, maxWaiting := s.budget.stats()
		logger.Info("queued", "path", path, "cost", chargedCost, "tokens_in_flight", inFlight, "max_tokens_in_flight", capacity, "waiting_requests", waiting, "max_waiting_requests", maxWaiting)
		s.metrics.observeLifecycleEvent(chatCompletionEndpoint, "queued", "")
	}

	queueWait, ok := s.budget.acquire(ctx, chargedCost)
	if !ok {
		return nil, queueWait, false
	}

	source := "direct"
	if willQueue {
		source = "waiting_to_concurrent"
	}
	_, postInFlight, waiting, maxWaiting := s.budget.stats()
	logger.Info("admitted", "path", path, "source", source, "cost", chargedCost, "queue_wait_ms", float64(queueWait)/float64(time.Millisecond), "tokens_in_flight", postInFlight, "max_tokens_in_flight", capacity, "waiting_requests", waiting, "max_waiting_requests", maxWaiting)
	s.metrics.observeLifecycleEvent(chatCompletionEndpoint, "admitted", "")
	return s.releaseFunc(chargedCost), queueWait, true
}

func (s *requestScheduler) Reconfigure(maxTokensInFlight, maxWaitingRequests int) {
	if maxTokensInFlight < 1 {
		maxTokensInFlight = 1
	}
	if maxWaitingRequests < 0 {
		maxWaitingRequests = 0
	}
	s.mu.Lock()
	s.maxTokensInFlight = int64(maxTokensInFlight)
	s.maxWaiting = maxWaitingRequests
	s.mu.Unlock()
	s.budget.reconfigure(int64(maxTokensInFlight), maxWaitingRequests)
}

// Observe records actual completion tokens for the global p95 estimator,
// which improves cost estimates for subsequent requests.
func (s *requestScheduler) Observe(completionTokens int) {
	if s == nil || s.estimator == nil {
		return
	}
	s.estimator.observe(completionTokens)
}

// Estimator returns the global completion-token p95 estimator.
func (s *requestScheduler) Estimator() *completionEstimator {
	if s == nil {
		return nil
	}
	return s.estimator
}

func (s *requestScheduler) Stats() (int, int64, int, int) {
	capacity, inFlight, waiting, maxWaiting := s.budget.stats()
	return int(capacity), inFlight, maxWaiting, waiting
}

func (s *requestScheduler) releaseFunc(cost int64) func() {
	return func() {
		s.budget.release(cost)
	}
}

func (s *requestScheduler) effectiveTokensPerSecond(baseTokensPerSecond, maxDegradation int) float64 {
	_, effectiveTokensPerSecond := s.degradationMetrics(baseTokensPerSecond, maxDegradation)
	return effectiveTokensPerSecond
}

// processingStats returns the snapshot used for metrics and the /config
// endpoint: (maxTokensInFlight, tokensInFlight, maxWaiting, waiting,
// computedDegradationPercentage, effectiveTokensPerSecond).
func (s *requestScheduler) processingStats(baseTokensPerSecond, maxDegradation int) (int64, int64, int, int, float64, float64) {
	capacity, inFlight, waiting, maxWaiting := s.budget.stats()
	computedDegradationPercentage, effectiveTokensPerSecond := s.degradationMetricsFor(capacity, inFlight, baseTokensPerSecond, maxDegradation)
	return capacity, inFlight, maxWaiting, waiting, computedDegradationPercentage, effectiveTokensPerSecond
}

func (s *requestScheduler) degradationMetrics(baseTokensPerSecond, maxDegradation int) (float64, float64) {
	capacity, inFlight, _, _ := s.budget.stats()
	return s.degradationMetricsFor(capacity, inFlight, baseTokensPerSecond, maxDegradation)
}

// degradationMetricsFor computes (computedDegradationPercentage,
// effectiveTokensPerSecond) using the cost-based fill ratio
// inFlight/capacity. Below the 10% threshold there is no degradation; above
// it, degradation scales linearly to maxDegradation at full capacity.
func (s *requestScheduler) degradationMetricsFor(capacity, inFlight int64, baseTokensPerSecond, maxDegradation int) (float64, float64) {
	if baseTokensPerSecond < 1 {
		return 0, 0
	}
	calibratedBaseTokensPerSecond := calibratedTokensPerSecond(baseTokensPerSecond)
	if maxDegradation == 0 || capacity <= 0 {
		return 0, calibratedBaseTokensPerSecond
	}
	thresholdCost := int64(math.Floor(float64(capacity) * degradationThreshold))
	if inFlight <= thresholdCost {
		return 0, calibratedBaseTokensPerSecond
	}
	degradationWindow := capacity - thresholdCost
	if degradationWindow <= 0 {
		return 0, calibratedBaseTokensPerSecond
	}
	progress := float64(inFlight-thresholdCost) / float64(degradationWindow)
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
	line       []byte
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

func cachedJSONPromptTokens(body []byte) int {
	var response struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0
	}
	return response.Usage.PromptTokens
}

func cachedStreamPromptTokens(body []byte) int {
	reader := bytes.NewReader(body)
	prompt := 0
	for {
		line, err := readSSELine(reader)
		if len(line) > 0 {
			if value := inspectSSEPromptTokens(line); value > 0 {
				prompt = value
			}
		}
		if err != nil {
			break
		}
	}
	return prompt
}

func inspectSSEPromptTokens(line []byte) int {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return 0
	}
	payload := bytes.TrimPrefix(trimmed, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0
	}
	var chunk struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return 0
	}
	return chunk.Usage.PromptTokens
}

func cachedResponsePromptTokens(cachedResponse cachedVLLMResponse) int {
	if cachedResponse.streaming {
		return cachedStreamPromptTokens(cachedResponse.body)
	}
	return cachedJSONPromptTokens(cachedResponse.body)
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

// Flush forwards to the underlying ResponseWriter when it supports
// http.Flusher. Without this explicit method, type-assertions through
// the wrapper chain (loggingResponseWriter -> firstByteMetricsWriter)
// fail and SSE chunks accumulate in the server's 32 KiB write buffer
// instead of being pushed to the client per-token.
func (w *loggingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
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

		var requestID string
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			requestID = resolveRequestID(r)
			w.Header().Set(requestIDHeader, requestID)
			r = r.WithContext(withRequestID(r.Context(), requestID))
		}

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

		attrs := make([]any, 0, 14)
		if requestID != "" {
			attrs = append(attrs, requestIDLogKey, requestID)
		}
		attrs = append(attrs,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"status", statusCode,
			"bytes", responseWriter.bytes,
			"cache", responseWriter.cacheHit,
			"duration_ms", float64(time.Since(startedAt))/float64(time.Millisecond),
		)

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
