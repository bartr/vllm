package httpapi

import (
	"bytes"
	"cllm/internal/buildinfo"
	"cllm/internal/node"
	"cllm/internal/router"
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
	defaultVLLMURL         = runtimeconfig.DefaultDownstreamURL
	defaultCacheSize       = runtimeconfig.DefaultCacheSize
	defaultCacheFilePath   = runtimeconfig.DefaultCacheFilePath
	defaultSystemPrompt    = runtimeconfig.DefaultSystemPrompt
	minMaxTokens           = runtimeconfig.MinMaxTokens
	maxMaxTokens           = runtimeconfig.MaxMaxTokens
	defaultMaxTokens       = runtimeconfig.DefaultMaxTokens
	defaultTemperature     = runtimeconfig.DefaultTemperature
	defaultVLLMHTTPTimeout = 120 * time.Second
	// defaultPerRequestTPS seeds the implicit single-node fallback's
	// Capacity.PerRequestTPS when no nodes.yaml is present, so cached
	// replay still paces at the legacy 32 tok/s/req default.
	defaultPerRequestTPS = runtimeconfig.DefaultMaxTokensPerSecond
	// fallbackMaxTokensInFlight / fallbackMaxWaitingRequests seed the
	// implicit single-node scheduler created by NewHandler() before any
	// nodes.yaml SetNodes() call replaces it. They are intentionally not
	// configurable: production deployments always supply nodes.yaml.
	fallbackMaxTokensInFlight  = 200000
	fallbackMaxWaitingRequests = 1024
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
	ready             atomic.Bool
	cache             *lruCache
	cacheFilePath     string
	metrics           *handlerMetrics
	configMu          sync.RWMutex
	defaults          askOptions
	vllmURL           string
	downstreamToken   string
	downstreamModel   string
	httpClient        *http.Client
	modelsMu          sync.RWMutex
	modelsCache       *cachedModelsResponse
	scheduler         *requestScheduler
	nodes             []*node.Node
	router            router.Router
	nodeRouterPolicy  string
	sleep             func(context.Context, time.Duration) error
	jitterSource      func() float64
	dslProfiles       map[string][]string
	dslDefaultProfile string
	tenants           *tenantRegistry
	classes           *classRegistry
}

func NewHandler() *Handler {
	handler := &Handler{}
	handler.ready.Store(true)
	handler.cache = newLRUCache(defaultCacheSize)
	handler.cacheFilePath = defaultCacheFilePath
	handler.defaults = askOptions{systemPrompt: defaultSystemPrompt, maxTokens: defaultMaxTokens, temperature: defaultTemperature}
	handler.vllmURL = trimTrailingSlash(defaultVLLMURL)
	handler.httpClient = &http.Client{Timeout: defaultVLLMHTTPTimeout}
	handler.scheduler = newRequestScheduler(fallbackMaxTokensInFlight, fallbackMaxWaitingRequests)
	handler.nodes = []*node.Node{handler.scheduler.node}
	handler.router = router.FromPolicy("")
	handler.sleep = sleepWithContext
	handler.jitterSource = defaultJitterSource
	handler.dslProfiles = cloneDSLProfiles(DefaultDSLProfiles)
	handler.tenants = newTenantRegistry(TenantConfig{}, 256, 50)
	handler.classes = newClassRegistry()
	handler.metrics = newHandlerMetrics(handler)
	handler.scheduler.metrics = handler.metrics
	return handler
}

// SetRequestProcessingLimits resizes the implicit single-node fallback's
// token-budget admission semaphore. Per-request pacing rate (legacy
// `--max-tokens-per-second`) and degradation curve (legacy
// `--max-degradation`) were retired in 0.14.0; they live on per-node
// Capacity.PerRequestTPS / Degradation.MaxDegradation now (see
// docs/system-design.md §6.X). The implicit fallback Node continues to seed
// PerRequestTPS at the historical 32 tok/s default so cached replay
// still paces by default.
func (h *Handler) SetRequestProcessingLimits(maxTokensInFlight, maxWaitingRequests int) {
	if maxTokensInFlight < 1 {
		maxTokensInFlight = fallbackMaxTokensInFlight
	}
	if maxWaitingRequests < 0 {
		maxWaitingRequests = fallbackMaxWaitingRequests
	}

	h.configMu.Lock()
	if h.scheduler == nil {
		h.scheduler = newRequestScheduler(maxTokensInFlight, maxWaitingRequests)
		h.scheduler.metrics = h.metrics
		h.nodes = []*node.Node{h.scheduler.node}
	} else {
		h.scheduler.Reconfigure(maxTokensInFlight, maxWaitingRequests)
	}
	h.configMu.Unlock()
}

type ProcessingStats struct {
	MaxTokensInFlight  int64
	TokensInFlight     int64
	MaxWaitingRequests int
	WaitingRequests    int
}

func (h *Handler) RequestProcessingStats() ProcessingStats {
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()

	if scheduler == nil {
		return ProcessingStats{}
	}
	capacity, inFlight, _, _ := scheduler.node.Budget.Stats()
	_, _, maxWaiting, waiting := scheduler.Stats()
	return ProcessingStats{
		MaxTokensInFlight:  capacity,
		TokensInFlight:     inFlight,
		MaxWaitingRequests: maxWaiting,
		WaitingRequests:    waiting,
	}
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
	mux.HandleFunc("GET /nodes", h.nodesEndpoint)
	mux.HandleFunc("GET /nodes/{id}", h.nodeEndpoint)
	mux.HandleFunc("POST /nodes/{id}", h.nodeEndpoint)
	mux.HandleFunc("PUT /nodes/{id}", h.nodeEndpoint)
	mux.HandleFunc("DELETE /nodes/{id}", h.nodeEndpoint)
	mux.HandleFunc("GET /health", h.healthEndpoint)
	mux.Handle("GET /metrics", h.metrics.Handler())
	mux.HandleFunc("GET /ready", h.readyEndpoint)
	mux.HandleFunc("GET /version", h.version)
	mux.HandleFunc("GET /v1/models", h.models)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	return requestLogger(mux, h.metrics)
}

type runtimeConfig struct {
	TokensInFlight     int64   `json:"tokens_in_flight"`
	WaitingRequests    int     `json:"waiting_requests"`
	Version            string  `json:"version"`
	CacheSize          int     `json:"cache_size"`
	CacheEntries       int     `json:"cache_entries"`
	DownstreamURL      string  `json:"downstream_url"`
	DownstreamModel    string  `json:"downstream_model"`
	SystemPrompt       string  `json:"system_prompt"`
	MaxTokens          int     `json:"max_tokens"`
	MaxTokensInFlight  int64   `json:"max_tokens_in_flight"`
	MaxWaitingRequests int     `json:"max_waiting_requests"`
	Temperature        float64 `json:"temperature"`
	DSLDefaultProfile  string  `json:"dsl_default_profile"`
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
				Edit:   true,
				Values: r.URL.Query(),
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

	// Stage 0b: resolve workload class. Phase 14A surfaces the
	// (tenant × class) cross-product on every admission metric and
	// lifecycle event without altering admission behavior. DSL
	// `workload-class=NAME` wins over the X-Workload-Class header;
	// unknown / malformed values resolve to the "default" class.
	class := h.resolveRequestClass(r, replayDSL)

	// Phase 13.1: resolve the phase-aware token-allocation envelope
	// from the class (DSL overrides land in Phase 13.4). Stored on
	// replayDSL so the replay loop can read it without an extra
	// argument; a zero-value envelope keeps the legacy single-rate
	// pacing path active.
	replayDSL.phase = resolvePhaseEnvelope(class, replayDSL)

	// Cost estimate uses tenant p95 first, then global, then max_tokens.
	cost := estimateRequestCostForTenant(requestPayload, tenant, h.globalEstimator())

	// DSL kv-cost / no-kv overrides shape the second admission axis
	// without touching cost.TotalCost. no-kv wins over kv-cost (the
	// directive class "kv-cost" is shared and first-wins). On
	// KV-disabled nodes (n.KV == nil) the override is a no-op per the
	// backward-compat contract.
	//
	// no-kv encodes "skip the KV gate entirely" via the sentinel
	// KVCost == -1; kv-cost=N sets KVCost = N directly.
	if replayDSL.noKV {
		cost.KVCost = -1
	} else if replayDSL.kvCostOverride > 0 {
		cost.KVCost = replayDSL.kvCostOverride
	}

	// Phase 14C: stamp admission-queue priority. Resolved order: per-request
	// `:dsl priority=NAME` (when set) overrides the class default. Names
	// map to small integers (low=-1, medium=0, high=+1); unknown / empty
	// names fall back to medium so legacy traffic stays at the FIFO
	// equivalent. The TokenBudget uses this score (plus aging) when it
	// promotes a waiter.
	cost.Priority = priorityScore(replayDSL.priorityOverride)
	if replayDSL.priorityOverride == "" {
		cost.Priority = priorityScore(class.config.Priority)
	}

	// Phase 2.4: route the request to a node and use that node for
	// admission, per-node metrics, and completion-token observation.
	// In single-node deployments the routed node is h.nodes[0] which
	// equals h.scheduler.node; admission is byte-for-byte identical to
	// the legacy path. In multi-node deployments admission charges the
	// chosen node's TokenBudget and emits per-node series.
	routedNode, routedReason := h.pickRoutedNode(ctx, replayDSL, cost)
	if routedNode != nil {
		h.logRouterDecisionIfMultiNode(ctx, routedNode, routedReason, replayDSL)
	}
	// Stamp the routed node onto replayOverrides so the cache replay
	// pacer can consult per-node capacity (item 15, 0.13.0). Falls
	// back to handler globals when nil.
	replayDSL.routedNode = routedNode
	// Per-node cache bypass: a node configured with
	// `bypass_cache: true` (e.g. the real-GPU `vllm` lane) forces
	// `:dsl no-cache` semantics for every request routed to it. We
	// flip only the noCache flag (not directives), since the bypass
	// is a node-config decision, not an inbound DSL token; per-node
	// metrics already carry the node label.
	if routedNode != nil && routedNode.Capacity.BypassCache {
		replayDSL.noCache = true
	}
	// nodeLabel resolves the routed node's ID for per-node metric
	// labels. It returns "unknown" for the byte-for-byte legacy path
	// where pickRoutedNode falls back to the implicit single-node
	// default (routedNode == nil).
	nodeLabel := func() string {
		if routedNode == nil {
			return "unknown"
		}
		return routedNode.ID
	}

	// Phase 4 (docs/spec-n-memory-pressure.md §4.1bis): refine KVCost
	// using the routed node's KV estimator, but only when the DSL did
	// not already pin the value. The routed node's KVEstimator is nil
	// when KV modeling is disabled on that node — in which case
	// cost.KVCost stays at its TotalCost default and the rest of the
	// admission path behaves byte-for-byte as today.
	if !replayDSL.noKV && replayDSL.kvCostOverride == 0 && routedNode != nil && routedNode.KVEstimator != nil {
		refineMax := requestPayload.MaxTokens
		if refineMax < 1 {
			refineMax = defaultMaxTokens
		}
		refined := node.EstimateCostWithKV(
			cost.PromptTokens,
			refineMax,
			h.globalEstimator(),
			routedNode.KVEstimator,
			routedNode.Capacity.KVCompletionFactor,
		)
		cost.KVCost = refined.KVCost
	}

	// Stage 1: tenant rate limit (token bucket; non-blocking).
	if !tenant.bucket.tryReserve(float64(cost.TotalCost)) {
		h.metrics.observeJob(endpoint, "rejected", "none", "unknown", 0)
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "rejected")
		h.metrics.observeAdmissionRejection(tenant.name, class.name, "tenant_rate")
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "tenant_rate", "request rejected",
			"status", http.StatusTooManyRequests,
			"mode", mode,
			"max_tokens", requestedMaxTokens,
			"tenant", tenant.name,
			"class", class.name,
			"cost", cost.TotalCost,
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, "tenant rate exceeded\n")
		return
	}

	// Stage 2: global cost budget (FIFO queue).
	//
	// Phase 14B: per-class admission queue cap. When the resolved class
	// (or a `:dsl max-queue-ms=N` override) sets a positive
	// MaxQueueMs, wrap the admit context with that timeout. If the
	// budget queue makes the request wait longer than the cap, the
	// nested context fires DeadlineExceeded and Acquire returns ok=false
	// — we then reclassify the rejection as "class_queue_timeout"
	// rather than "over_capacity".
	effectiveMaxQueueMs := class.config.MaxQueueMs
	if replayDSL.maxQueueMsOverride > 0 {
		effectiveMaxQueueMs = replayDSL.maxQueueMsOverride
	}
	admitCtx := ctx
	var cancelAdmit context.CancelFunc
	if effectiveMaxQueueMs > 0 {
		admitCtx, cancelAdmit = context.WithTimeout(ctx, time.Duration(effectiveMaxQueueMs)*time.Millisecond)
		defer cancelAdmit()
	}
	release, queueWait, ok, rejectReason := h.acquireRequestSlotOnNode(admitCtx, cost, r.URL.RequestURI(), routedNode)
	if !ok {
		// Reclassify deadline-driven rejection as class_queue_timeout
		// so dashboards can split it from generic over-capacity. The
		// parent ctx must NOT be done — that would mean the client
		// disconnected, which is a different failure mode.
		if effectiveMaxQueueMs > 0 && admitCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			rejectReason = "class_queue_timeout"
		}
		// Global gate refused; refund tenant tokens so a rejection here
		// doesn't permanently drain rate quota.
		tenant.bucket.refund(float64(cost.TotalCost))
		if rejectReason == "" {
			rejectReason = "over_capacity"
		}
		// HTTP body and lifecycle message vary by reason; the metric
		// label and lifecycle outcome carry the precise reason.
		var rejectBody string
		switch rejectReason {
		case "kv_pressure":
			rejectBody = "kv pressure\n"
		case "kv_oversize":
			rejectBody = "kv oversize\n"
		case "class_queue_timeout":
			rejectBody = "class queue timeout\n"
		case "node_concurrency":
			rejectBody = "node concurrency\n"
		default:
			rejectBody = "over capacity\n"
		}
		h.metrics.observeJob(endpoint, "rejected", "none", nodeLabel(), 0)
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "rejected")
		h.metrics.observeAdmissionRejection(tenant.name, class.name, rejectReason)
		h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", rejectReason, "request rejected",
			"status", http.StatusTooManyRequests,
			"mode", mode,
			"max_tokens", requestedMaxTokens,
			"tenant", tenant.name,
			"class", class.name,
			"cost", cost.TotalCost,
			"max_queue_ms", effectiveMaxQueueMs,
			"queue_wait_ms", float64(queueWait)/float64(time.Millisecond),
			"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
		)
		markCacheHit(w, false)
		writePlainText(w, http.StatusTooManyRequests, rejectBody)
		return
	}
	defer release()
	h.metrics.observeJob(endpoint, "accepted", "none", nodeLabel(), 0)
	h.metrics.observeAdmissionAccept(tenant.name, class.name, mode)
	h.metrics.observeQueueWait(endpoint, nodeLabel(), queueWait)
	processingStartedAt := time.Now()
	h.emitLifecycleEvent(ctx, slog.LevelInfo, "started", "", "request started",
		"mode", mode,
		"max_tokens", requestedMaxTokens,
		"tenant", tenant.name,
		"class", class.name,
		"cost", cost.TotalCost,
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
			if routedNode != nil && routedNode.Estimator != nil && routedNode != h.scheduler.node {
				routedNode.Estimator.Observe(completionTokens)
			}
			// Phase 4: feed the per-node KV estimator on the same
			// successful-downstream signal. Only nodes with KV
			// modeling enabled have a non-nil KVEstimator, so this
			// is a no-op on the legacy single-node default fleet.
			if routedNode != nil && routedNode.KVEstimator != nil {
				routedNode.KVEstimator.Observe(completionTokens)
			}
			if tenant != nil && tenant.estimator != nil {
				tenant.estimator.Observe(completionTokens)
			}
		}
		h.emitLifecycleEvent(ctx, level, "completed", outcome, "request completed",
			"source", source,
			"mode", mode,
			"status", status,
			"tenant", tenant.name,
			"class", class.name,
			"queue_wait_ms", float64(queueWait)/float64(time.Millisecond),
			"duration_ms", float64(time.Since(processingStartedAt))/float64(time.Millisecond),
			"prompt_tokens", promptTokens,
			"completion_tokens", completionTokens,
			"max_tokens", requestPayload.MaxTokens,
		)
	}

	requestPayload, err := h.populateChatCompletionDefaults(ctx, requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", nodeLabel(), time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
		emitCompleted(slog.LevelInfo, "failed", "downstream", http.StatusBadGateway, 0, 0)
		markCacheHit(w, false)
		http.Error(w, fmt.Sprintf("prepare chat completion: %v", err), http.StatusBadGateway)
		return
	}

	cacheKey, err := buildChatCompletionCacheKey(requestPayload)
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "none", nodeLabel(), time.Since(processingStartedAt))
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
			completionTokens := cachedResponseCompletionTokens(cachedResponse)
			promptTokens := cachedResponsePromptTokens(cachedResponse)
			if promptTokens <= 0 {
				promptTokens = estimatePromptTokensFromRequest(requestPayload)
			}
			cachedSource := "cache"
			cachedMode := mode

			// TTFT-budget gate (system-design §14, item 13 follow-on,
			// 0.11.x). Predicted TTFT = simulated prefill (without
			// jitter) + 1/first-token-tps. Cached-replay path only;
			// no-cache / re-cache / cache-miss bypass. DSL override
			// (`:dsl max-ttft-ms=N`) wins over class config; 0 disables.
			effectiveMaxTTFTMs := class.config.MaxTTFTMs
			if replayDSL.maxTTFTMsSet {
				effectiveMaxTTFTMs = replayDSL.maxTTFTMsOverride
			}
			if effectiveMaxTTFTMs > 0 {
				predictedMs := h.predictTTFTms(promptTokens, replayDSL)
				if predictedMs > effectiveMaxTTFTMs {
					tenant.bucket.refund(float64(cost.TotalCost))
					h.metrics.observeJob(endpoint, "rejected", "none", nodeLabel(), 0)
					h.metrics.observeDSLRequestResult(endpoint, dslFamily, "rejected")
					h.metrics.observeAdmissionRejection(tenant.name, class.name, "class_ttft_budget")
					h.emitLifecycleEvent(ctx, slog.LevelWarn, "rejected", "class_ttft_budget", "request rejected",
						"status", http.StatusTooManyRequests,
						"mode", mode,
						"max_tokens", requestedMaxTokens,
						"tenant", tenant.name,
						"class", class.name,
						"cost", cost.TotalCost,
						"max_ttft_ms", effectiveMaxTTFTMs,
						"predicted_ttft_ms", predictedMs,
						"prompt_tokens", promptTokens,
						"duration_ms", float64(time.Since(receivedAt))/float64(time.Millisecond),
					)
					markCacheHit(w, false)
					writePlainText(w, http.StatusTooManyRequests, "class ttft budget\n")
					return
				}
			}
			markCacheHit(w, true)

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
				h.metrics.observeJob(endpoint, "failed", "cache", nodeLabel(), time.Since(processingStartedAt))
				h.metrics.observeDSLJobDuration(endpoint, "cache", "failed", dslFamily, time.Since(processingStartedAt))
				h.metrics.observeDSLRequestResult(endpoint, dslFamily, "failed")
				emitCompleted(slog.LevelInfo, "failed", "cache", http.StatusServiceUnavailable, promptTokens, 0)
				return
			}

			timedWriter := newFirstByteMetricsWriter(w, func() {
				ttfb := time.Since(processingStartedAt)
				h.metrics.observeTimeToFirstByte(endpoint, cachedSource, nodeLabel(), ttfb)
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
				class:        class.name,
				node:         routedNode,
			}
			if requestPayload.Stream {
				h.replayCachedStream(ctx, timedWriter, cachedResponse, replay)
				deliveredTokens := completionTokens
				if replay.maxTokens > 0 && deliveredTokens > replay.maxTokens {
					deliveredTokens = replay.maxTokens
				}
				h.metrics.observeCompletionTokens(endpoint, "cache", nodeLabel(), deliveredTokens)
				h.metrics.observeJob(endpoint, "completed", "cache", nodeLabel(), time.Since(processingStartedAt))
				h.metrics.observeDSLJobDuration(endpoint, "cache", "completed", dslFamily, time.Since(processingStartedAt))
				h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
				emitCompleted(slog.LevelInfo, "completed", "cache", cachedResponse.statusCode, promptTokens, deliveredTokens)
				return
			}

			h.replayCachedResponse(ctx, timedWriter, cachedResponse, replay)
			h.metrics.observeCompletionTokens(endpoint, "cache", nodeLabel(), completionTokens)
			h.metrics.observeJob(endpoint, "completed", "cache", nodeLabel(), time.Since(processingStartedAt))
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
		h.metrics.observeTimeToFirstByte(endpoint, "downstream", nodeLabel(), ttfb)
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
			h.metrics.observeJob(endpoint, "failed", "downstream", nodeLabel(), time.Since(processingStartedAt))
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
		h.metrics.observeCompletionTokens(endpoint, "downstream", nodeLabel(), completionTokens)
		h.metrics.observeJob(endpoint, "completed", "downstream", nodeLabel(), time.Since(processingStartedAt))
		h.metrics.observeDSLJobDuration(endpoint, "downstream", "completed", dslFamily, time.Since(processingStartedAt))
		h.metrics.observeDSLRequestResult(endpoint, dslFamily, "completed")
		emitCompleted(slog.LevelInfo, "completed", "downstream", cachedResponse.statusCode, promptTokens, completionTokens)
		return
	}

	downstreamStartedAt := time.Now()
	responseBody, statusCode, contentType, err := h.createChatCompletion(ctx, requestPayload)
	h.metrics.observeDownstreamRequest(endpoint, mode, downstreamResultLabel(err), time.Since(downstreamStartedAt))
	if err != nil {
		h.metrics.observeJob(endpoint, "failed", "downstream", nodeLabel(), time.Since(processingStartedAt))
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
	h.metrics.observeCompletionTokens(endpoint, "downstream", nodeLabel(), completionTokens)
	h.metrics.observeJob(endpoint, "completed", "downstream", nodeLabel(), time.Since(processingStartedAt))
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
	dslDefaultProfile := h.dslDefaultProfile
	h.configMu.RUnlock()

	return runtimeConfig{
		TokensInFlight:     processingStats.TokensInFlight,
		WaitingRequests:    processingStats.WaitingRequests,
		Version:            buildinfo.Version,
		CacheSize:          cacheSize,
		CacheEntries:       cacheEntries,
		DownstreamURL:      downstreamURL,
		DownstreamModel:    downstreamModel,
		SystemPrompt:       defaults.systemPrompt,
		MaxTokens:          defaults.maxTokens,
		MaxTokensInFlight:  processingStats.MaxTokensInFlight,
		MaxWaitingRequests: processingStats.MaxWaitingRequests,
		Temperature:        defaults.temperature,
		DSLDefaultProfile:  dslDefaultProfile,
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

	if temperatureRaw := configQueryValue(queryValues, "temperature"); temperatureRaw != "" {
		temperature, err := strconv.ParseFloat(temperatureRaw, 64)
		if err != nil {
			return false, fmt.Errorf("invalid temperature %q", temperatureRaw)
		}
		defaults.temperature = temperature
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
	if dslProfileChanged {
		// Validation already passed above; ignore the error path.
		_ = h.SetDSLDefaultProfile(dslProfileNew)
	}

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
	"me":   {}, "my": {},
	"no": {}, "not": {},
	"of": {}, "on": {}, "or": {}, "our": {}, "ours": {},
	"please": {},
	"she":    {}, "should": {}, "so": {},
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
	// class is the resolved workload-class name; used as the Prometheus
	// label on phase-transition metrics. Empty defaults to
	// defaultClassName at the call site.
	class string
	// node is the routed node, used for per-node vLLM-shaped pacing
	// (item 15, 0.13.0). nil falls back to the legacy fleet-divided
	// rate. Carries no metric labels (those use nodeLabel() at call
	// sites); used purely for pacer rate selection.
	node *node.Node
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
	// Phase 13.2: track whether this request has already crossed its
	// phase boundary so we emit the transition exactly once. The
	// boundary is checked against contentEmitted *before* each
	// segment's throttle delay, since that delay paces the gap to
	// the next token.
	phaseTransitionEmitted := false
	phaseStartedAt := time.Now()
	// Phase 13.3: emit per-stream phase summary metrics on every
	// exit path (including early return on context cancel). Captured
	// closures read contentEmitted via a pointer-style read at defer
	// time so the latest value is observed.
	defer func() {
		h.recordPhaseSummary(opts, contentEmitted)
	}()
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
		// Phase 13.2: if the resolved envelope is two-phase and we
		// just crossed (or straddled) the boundary, emit a single
		// phase_transition lifecycle event + counter increment. We
		// fire after the flush so the transition timestamp aligns
		// with the wire-observable end of phase A. Streams that
		// finish before InitialTokens never emit \u2014 absence is the
		// diagnostic signal for \"interactive request was a short ack.\"
		if env := opts.overrides.phase; env.active() && !phaseTransitionEmitted && contentEmitted >= env.InitialTokens {
			phaseTransitionEmitted = true
			h.emitPhaseTransition(ctx, opts.class, env, contentEmitted, time.Since(phaseStartedAt))
		}
		// Throttle AFTER emitting the chunk, not before. This matches the
		// timing of a real LLM stream: prefill delay covers the time to the
		// first token (handled separately by simulatePrefillDelay before the
		// replay loop starts), and the per-segment decode delay paces the
		// gap between consecutive chunks. Throttling before the first
		// content write would double-count prefill and inflate TTFT
		// proportional to however many tokens happened to be packed into
		// the first cached SSE chunk.
		//
		// tokensSoFar = contentEmitted - segment.tokenCount picks the
		// rate appropriate for the *position* of this segment's first
		// token. A segment that straddles the phase boundary is paced
		// at the higher (phase-A) rate; sub-segment fractional accounting
		// is intentionally out of scope (see design \u00a75.2).
		tokensSoFar := contentEmitted - segment.tokenCount
		if tokensSoFar < 0 {
			tokensSoFar = 0
		}
		if err := h.throttleStreamSegment(ctx, segment.tokenCount, tokensSoFar, opts.overrides); err != nil {
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

func (h *Handler) acquireRequestSlot(ctx context.Context, cost node.RequestCost, path string) (func(), time.Duration, bool) {
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
	return func() {
		release()
	}, queueWait, true
}

// acquireRequestSlotOnNode is the multi-node admission path: it charges
// cost against the supplied node's TokenBudget instead of the scheduler's
// own. When n is nil it falls back to acquireRequestSlot, so callers can
// pass through a router decision without nil-checking. Phase 2.4 of
// multi-node-design.md.
//
// reason is "" on success. On failure it is one of "over_capacity",
// "kv_pressure", or "kv_oversize"; see scheduler.AcquireOnNode for
// details. Callers translate the reason into a metric label and an HTTP
// rejection message.
func (h *Handler) acquireRequestSlotOnNode(ctx context.Context, cost node.RequestCost, path string, n *node.Node) (func(), time.Duration, bool, string) {
	if n == nil {
		release, waited, ok := h.acquireRequestSlot(ctx, cost, path)
		reason := ""
		if !ok {
			reason = "over_capacity"
		}
		return release, waited, ok, reason
	}
	h.configMu.RLock()
	scheduler := h.scheduler
	h.configMu.RUnlock()
	if scheduler == nil {
		return func() {}, 0, true, ""
	}
	release, queueWait, ok, reason := scheduler.AcquireOnNode(ctx, cost, path, n)
	if !ok {
		return nil, 0, false, reason
	}
	return func() {
		release()
	}, queueWait, true, ""
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
func (h *Handler) globalEstimator() *node.CompletionEstimator {
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
		return &tenantState{name: defaultTenantName, bucket: newTenantBucket(0, 0), estimator: node.NewCompletionEstimator(256, 50)}
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

// resolveRequestClass returns the classState for a request, applying
// first-wins precedence: a `:dsl workload-class=NAME` directive in the
// prompt beats the X-Workload-Class header beats "default". Always
// returns non-nil and a name that is safe to use as a Prometheus label.
func (h *Handler) resolveRequestClass(r *http.Request, dsl replayOverrides) *classState {
	h.configMu.RLock()
	classes := h.classes
	h.configMu.RUnlock()
	if classes == nil {
		return &classState{name: defaultClassName, config: ClassConfig{Priority: "medium"}}
	}
	if dsl.workloadClass != "" {
		return classes.resolve(dsl.workloadClass)
	}
	return classes.resolve(r.Header.Get(classHeader))
}

// SetClasses installs a new workload-class configuration set. The
// "default" class is always preserved; existing classes matching new
// names are updated in place; missing classes are removed. Pass
// nil/empty to reset to default-only.
func (h *Handler) SetClasses(classes map[string]ClassConfig) {
	h.configMu.RLock()
	registry := h.classes
	h.configMu.RUnlock()
	if registry == nil {
		return
	}
	registry.configure(classes)
}

// ClassNames returns a snapshot of registered workload-class names
// with "default" first.
func (h *Handler) ClassNames() []string {
	h.configMu.RLock()
	registry := h.classes
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
	delay := h.cachedReplayDelay(tokenCount, 0, overrides)
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

// throttleStreamSegment paces one cached SSE segment. tokensSoFar is
// the count of output tokens emitted *before* this segment, used by
// phase-aware token allocation (item 13) to pick the responsiveness
// vs. sustained rate. Pass 0 for callers without phase tracking; an
// inactive envelope yields legacy single-rate behavior.
func (h *Handler) throttleStreamSegment(ctx context.Context, tokenCount, tokensSoFar int, overrides replayOverrides) error {
	if tokenCount < 1 {
		return nil
	}
	delay, stall := h.computeStreamSegmentDelay(tokenCount, tokensSoFar, overrides)
	if delay <= 0 {
		return nil
	}
	if stall > 0 && h.metrics != nil {
		h.metrics.observeStreamStall(chatCompletionsRouteLabel, "cache", stall)
	}
	return h.sleep(ctx, delay)
}

// cachedReplayDelay returns the simulated decode duration for one
// segment of `tokenCount` tokens.
//
// tokensSoFar is the count of tokens already emitted by the request
// before this segment. It selects the active rate when the resolved
// phaseEnvelope is two-phase (item 13): tokens whose index is below
// `phase.InitialTokens` are paced at `phase.InitialTPS`; later tokens
// are paced at `phase.SustainedTPS`. Both rates pass through the same
// degradation curve as the legacy single rate, so phase-aware pacing
// composes with cost-based slowdown rather than overriding it.
//
// Precedence: a per-request `:dsl tps=N` (overrides.tpsOverride > 0)
// always wins — it explicitly says "single rate." The phase envelope
// only fires when no DSL tps override is in effect.
// cachedReplayDelay returns the simulated decode duration for one
// segment of a cached replay stream.
//
// tokensSoFar is the count of tokens already emitted by the request
// before this segment. It selects the active rate when the resolved
// phaseEnvelope is two-phase (item 13): tokens whose index is below
// `phase.InitialTokens` are paced at `phase.InitialTPS`; later tokens
// are paced at `phase.SustainedTPS`.
//
// Pacing rate (precedence, item 16, 0.14.0):
//
//  1. `:dsl no-tps` → 0 delay (no pacing).
//  2. `:dsl tps=N` → flat per-request rate N.
//  3. phase envelope active → InitialTPS / SustainedTPS by tokensSoFar.
//  4. routed node `Capacity.PerRequestTPS > 0` →
//     `routedNode.PerRequestRate(ConcurrentRequests())` (three-regime curve).
//  5. otherwise → 0 (no pacing). Legacy global `--max-tokens-per-second`
//     and `--max-degradation` were retired; pacing is per-node only now.
func (h *Handler) cachedReplayDelay(tokenCount, tokensSoFar int, overrides replayOverrides) time.Duration {
	if tokenCount < 1 {
		return 0
	}
	if overrides.noTPS {
		return 0
	}

	var base float64
	switch {
	case overrides.tpsOverride > 0:
		base = float64(overrides.tpsOverride)
	case overrides.phase.active():
		rate := overrides.phase.SustainedTPS
		if tokensSoFar < overrides.phase.InitialTokens {
			rate = overrides.phase.InitialTPS
		}
		if rate > 0 {
			base = float64(rate)
		}
	}
	if base <= 0 {
		if n := overrides.routedNode; n != nil && n.Capacity.PerRequestTPS > 0 {
			base = n.PerRequestRate(n.ConcurrentRequests())
		}
	}
	if base <= 0 {
		// Item 16 (0.14.0): when no routed node is supplied (e.g.
		// direct replay paths and unit tests that don't pass through
		// the router), fall back to the implicit fallback Node owned
		// by the scheduler. That Node is seeded with
		// `Capacity.PerRequestTPS = defaultPerRequestTPS` so default
		// pacing is preserved without any global flag.
		h.configMu.RLock()
		sched := h.scheduler
		h.configMu.RUnlock()
		if sched != nil && sched.node != nil && sched.node.Capacity.PerRequestTPS > 0 {
			base = sched.node.PerRequestRate(sched.node.ConcurrentRequests())
		}
	}
	if base <= 0 {
		return 0
	}
	return time.Duration(float64(tokenCount) * float64(time.Second) / base)
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

// SetPrefillSimulation configures the implicit fallback Node's prefill
// realism knobs. Used in single-node fallback mode (no nodes.yaml) and
// by tests; production deployments configure per-node Realism via
// nodes.yaml. A rateMultiplier of 0 disables prefill simulation.
func (h *Handler) SetPrefillSimulation(rateMultiplier float64, baseOverheadMs, jitterPercent, maxMs int) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.scheduler == nil || h.scheduler.node == nil {
		return
	}
	h.scheduler.node.Realism.PrefillRateMultiplier = rateMultiplier
	h.scheduler.node.Realism.PrefillBaseOverheadMs = baseOverheadMs
	h.scheduler.node.Realism.PrefillJitterPercent = jitterPercent
	h.scheduler.node.Realism.PrefillMaxMs = maxMs
}

// SetStreamRealism configures the implicit fallback Node's stream
// realism knobs. Used in single-node fallback mode (no nodes.yaml) and
// by tests; production deployments configure per-node Realism via
// nodes.yaml.
func (h *Handler) SetStreamRealism(variabilityPercent, jitterPercent, stallProbabilityPercent, stallMinMs, stallMaxMs int) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.scheduler == nil || h.scheduler.node == nil {
		return
	}
	h.scheduler.node.Realism.StreamVariabilityPct = variabilityPercent
	h.scheduler.node.Realism.StreamJitterPct = jitterPercent
	h.scheduler.node.Realism.StreamStallProbPct = stallProbabilityPercent
	h.scheduler.node.Realism.StreamStallMinMs = stallMinMs
	h.scheduler.node.Realism.StreamStallMaxMs = stallMaxMs
}

// computePrefillDelay returns the simulated prefill latency for the given
// prompt token count. A return of 0 means prefill simulation is disabled.
//
// Per-node prefill (item 16, 0.14.0): when the routed node carries
// `Capacity.PerRequestTPS > 0`, both the decode-rate basis and the
// prefill realism knobs (`Realism.Prefill*`) come from the node. The
// effective decode rate is `routedNode.PerRequestRate(ConcurrentRequests())`,
// mirroring `cachedReplayDelay`'s 0.13.0 path so prefill TTFT tracks
// the same per-node concurrency curve as decode pacing. Per-node
// realism knobs that are zero fall back to the handler globals so
// operators can leave a class default in nodes.yaml and still tune
// jitter/cap centrally.
//
// Legacy single-node fallback path (routedNode == nil OR
// PerRequestTPS == 0): handler globals (`h.maxTokensPerSecond`,
// `h.maxDegradation`, scheduler degradation curve) drive the rate.
// Passthrough nodes that opt out of per-request pacing therefore
// continue to follow the global pacing rate today; row 2 of 0.14.0
// retires that fallback in a follow-on commit.
func (h *Handler) computePrefillDelay(promptTokens int, overrides replayOverrides) time.Duration {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if overrides.noPrefill {
		return 0
	}

	rateMultiplier, base, jitterPercent, maxDuration, effectiveDecodeRate, ok := h.resolvePrefillParams(overrides)
	if !ok {
		return 0
	}
	prefillRate := effectiveDecodeRate * rateMultiplier
	if prefillRate <= 0 {
		return 0
	}

	h.configMu.RLock()
	jitterSource := h.jitterSource
	h.configMu.RUnlock()

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

// resolvePrefillParams resolves the prefill rate, base overhead, jitter
// percent, max-duration cap, and effective decode rate for one prefill
// computation. Returns ok=false when prefill simulation is effectively
// disabled (zero rate multiplier or zero decode rate). Shared by
// computePrefillDelay, computePrefillDelayDeterministic, and predictTTFTms.
//
// Per-node Realism (from nodes.yaml) is authoritative; when no node is
// routed, the implicit single-node scheduler fallback Node provides the
// realism source so single-node mode and tests still simulate prefill.
func (h *Handler) resolvePrefillParams(overrides replayOverrides) (rateMultiplier float64, base time.Duration, jitterPercent int, maxDuration time.Duration, effectiveDecodeRate float64, ok bool) {
	n := overrides.routedNode
	if n == nil {
		h.configMu.RLock()
		if h.scheduler != nil {
			n = h.scheduler.node
		}
		h.configMu.RUnlock()
	}
	if n != nil {
		rateMultiplier = n.Realism.PrefillRateMultiplier
		base = time.Duration(n.Realism.PrefillBaseOverheadMs) * time.Millisecond
		jitterPercent = n.Realism.PrefillJitterPercent
		maxDuration = time.Duration(n.Realism.PrefillMaxMs) * time.Millisecond
	}
	if rateMultiplier <= 0 {
		return
	}

	// Decode-rate basis (item 16, 0.14.0): per-node PerRequestRate
	// curve only. Legacy global scheduler degradation curve was retired
	// alongside `--max-tokens-per-second` / `--max-degradation`. DSL
	// `tps=N` always wins (per-request explicit pin). Passthrough nodes
	// (PerRequestTPS == 0) intentionally produce zero prefill so a
	// real-GPU baseline lane has no simulated prefill on top of vLLM.
	switch {
	case overrides.tpsOverride > 0:
		effectiveDecodeRate = float64(overrides.tpsOverride)
	case overrides.routedNode != nil && overrides.routedNode.Capacity.PerRequestTPS > 0:
		effectiveDecodeRate = overrides.routedNode.PerRequestRate(overrides.routedNode.ConcurrentRequests())
	default:
		// Item 16 (0.14.0): when no routed node is supplied, fall
		// back to the implicit fallback Node owned by the
		// scheduler so prefill simulation still has a decode-rate
		// basis in single-node default deployments.
		h.configMu.RLock()
		sched := h.scheduler
		h.configMu.RUnlock()
		if sched != nil && sched.node != nil && sched.node.Capacity.PerRequestTPS > 0 {
			effectiveDecodeRate = sched.node.PerRequestRate(sched.node.ConcurrentRequests())
		}
	}
	if effectiveDecodeRate <= 0 {
		return
	}
	ok = true
	return
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

// predictTTFTms returns a stable, jitter-free admission-time estimate
// of the time-to-first-token a cached-replay request would experience.
// Used by the per-class `max_ttft_ms` admission gate (system-design
// §14, item 13 follow-on, 0.11.x). Two components:
//
//	prefill_ms     — same shape as computePrefillDelay but without the
//	                 random ±jitterPercent term, so successive calls
//	                 with identical inputs return the same number.
//	first_token_ms — ceil(1000 / first_token_tps). Per-node since 0.14.0
//	                 (item 16): rate is `:dsl tps=N`, else phase
//	                 envelope's InitialTPS when active, else the routed
//	                 node's `PerRequestRate(ConcurrentRequests())`.
//	                 Passthrough nodes (PerRequestTPS == 0) report a
//	                 zero first-token component, matching pure proxy
//	                 semantics.
//
// Queue-wait is intentionally NOT included; that axis is owned by
// `class_queue_timeout` (Phase 14B). Returns total milliseconds.
func (h *Handler) predictTTFTms(promptTokens int, overrides replayOverrides) int {
	prefillMs := h.computePrefillDelayDeterministic(promptTokens, overrides)

	if overrides.noTPS {
		// No pacing — first token emits as fast as the writer can
		// flush. Use 0 ms for the first-token component.
		return int(prefillMs / time.Millisecond)
	}

	var effective float64
	switch {
	case overrides.tpsOverride > 0:
		effective = float64(overrides.tpsOverride)
	case overrides.phase.active() && overrides.phase.InitialTPS > 0:
		effective = float64(overrides.phase.InitialTPS)
	case overrides.routedNode != nil && overrides.routedNode.Capacity.PerRequestTPS > 0:
		effective = overrides.routedNode.PerRequestRate(overrides.routedNode.ConcurrentRequests())
	default:
		// Item 16 (0.14.0): fall back to the scheduler's implicit
		// fallback Node when no routed node is supplied.
		h.configMu.RLock()
		sched := h.scheduler
		h.configMu.RUnlock()
		if sched != nil && sched.node != nil && sched.node.Capacity.PerRequestTPS > 0 {
			effective = sched.node.PerRequestRate(sched.node.ConcurrentRequests())
		}
	}

	if effective <= 0 {
		return int(prefillMs / time.Millisecond)
	}
	firstTokenMs := int(1000.0/effective + 0.999) // ceil
	return int(prefillMs/time.Millisecond) + firstTokenMs
}

// computePrefillDelayDeterministic mirrors computePrefillDelay but
// omits the random jitter draw, so repeated calls with the same inputs
// return the same value. Used by predictTTFTms; the actual streaming
// path continues to call computePrefillDelay (with jitter) so observed
// TTFT still varies as configured. Resolves rate + caps via the same
// resolvePrefillParams helper so per-node prefill (item 16, 0.14.0)
// also drives the admission-time TTFT prediction.
func (h *Handler) computePrefillDelayDeterministic(promptTokens int, overrides replayOverrides) time.Duration {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if overrides.noPrefill {
		return 0
	}

	rateMultiplier, base, _, maxDuration, effectiveDecodeRate, ok := h.resolvePrefillParams(overrides)
	if !ok {
		return 0
	}
	prefillRate := effectiveDecodeRate * rateMultiplier
	if prefillRate <= 0 {
		return 0
	}

	delay := base + time.Duration(float64(promptTokens)/prefillRate*float64(time.Second))
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

// computeStreamSegmentDelay returns the simulated delay for one streamed
// content segment, plus any stall added on top. The stall component is
// returned separately so callers can record metrics distinct from the base
// pacing delay. Returns (0, 0) when pacing is disabled (rate=0) or when the
// segment carries no tokens.
func (h *Handler) computeStreamSegmentDelay(tokenCount, tokensSoFar int, overrides replayOverrides) (total, stall time.Duration) {
	if tokenCount < 1 {
		return 0, 0
	}
	if overrides.noTPS {
		return 0, 0
	}
	base := h.cachedReplayDelay(tokenCount, tokensSoFar, overrides)
	if base <= 0 {
		return 0, 0
	}

	// Per-node Realism (from nodes.yaml) is the sole source for stream
	// realism knobs; DSL overrides may add deltas on top. When no node
	// is routed, fall back to the implicit single-node scheduler Node
	// so single-node mode and tests still see realism.
	n := overrides.routedNode
	if n == nil {
		h.configMu.RLock()
		if h.scheduler != nil {
			n = h.scheduler.node
		}
		h.configMu.RUnlock()
	}
	var nodeVar, nodeJitter, nodeStall int
	var nodeStallMin, nodeStallMax time.Duration
	if n != nil {
		nodeVar = n.Realism.StreamVariabilityPct
		nodeJitter = n.Realism.StreamJitterPct
		nodeStall = n.Realism.StreamStallProbPct
		nodeStallMin = time.Duration(n.Realism.StreamStallMinMs) * time.Millisecond
		nodeStallMax = time.Duration(n.Realism.StreamStallMaxMs) * time.Millisecond
	}
	variabilityPercent := overrides.resolveVariabilityPercent(nodeVar)
	jitterPercent := overrides.resolveJitterPercent(nodeJitter)
	stallProbPercent := overrides.resolveStallPercent(nodeStall)
	stallMin := nodeStallMin
	stallMax := nodeStallMax
	h.configMu.RLock()
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
// cost is released. It composes a node.Node's primitives (TokenBudget +
// CompletionEstimator) with logging and Prometheus metrics integration.
//
// Phase 1.5 of the multi-node refactor: the scheduler holds a *node.Node
// rather than separate budget/estimator pointers, so Phase 2 can iterate
// over Handler.nodes without further moves.
type requestScheduler struct {
	mu                sync.Mutex
	node              *node.Node
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
	n := &node.Node{
		ID:        "default",
		Class:     "default",
		Budget:    node.NewTokenBudget(int64(maxTokensInFlight), maxWaitingRequests),
		Estimator: node.NewCompletionEstimator(256, 50),
		Capacity: node.Capacity{
			MaxTokensInFlight:  int64(maxTokensInFlight),
			MaxWaitingRequests: maxWaitingRequests,
			// Item 16 (0.14.0): seed the implicit fallback Node with
			// the historical default per-request decode rate so cached
			// replay still paces by default when no nodes.yaml is
			// loaded. Per-node `PerRequestRate(c)` curves now own all
			// pacing; the legacy global `--max-tokens-per-second` /
			// `--max-degradation` flags were retired.
			PerRequestTPS: defaultPerRequestTPS,
		},
	}
	return &requestScheduler{
		node:              n,
		maxTokensInFlight: int64(maxTokensInFlight),
		maxWaiting:        maxWaitingRequests,
	}
}

// Acquire charges cost.TotalCost against the budget. It returns a release
// closure (which refunds the same cost when called), the time spent waiting,
// and ok=false when over capacity (queue full or oversized request).
func (s *requestScheduler) Acquire(ctx context.Context, cost node.RequestCost, path string) (func(), time.Duration, bool) {
	release, waited, ok, _ := s.acquireOn(ctx, cost, path, s.node)
	return release, waited, ok
}

// AcquireOnNode is the multi-node variant of Acquire: it charges the cost
// against the supplied node's TokenBudget instead of the scheduler's own
// (Phase 2.4 of multi-node-design.md). When n is nil it falls back to
// s.node so callers can pass through a router decision without
// nil-checking. Per-node Prometheus metrics are emitted only on this
// path; legacy callers using Acquire continue to share the global series.
//
// reason is "" on success. On failure it is "over_capacity" (TokenBudget
// rejected: queue full or oversize compute), "kv_pressure" (node KV
// budget is currently full), or "kv_oversize" (kv_cost alone exceeds
// MaxKVTokens). See docs/spec-n-memory-pressure.md \u00a75.2.
func (s *requestScheduler) AcquireOnNode(ctx context.Context, cost node.RequestCost, path string, n *node.Node) (func(), time.Duration, bool, string) {
	if n == nil {
		release, waited, ok, reason := s.acquireOn(ctx, cost, path, s.node)
		return release, waited, ok, reason
	}
	return s.acquireOn(ctx, cost, path, n)
}

func (s *requestScheduler) acquireOn(ctx context.Context, cost node.RequestCost, path string, n *node.Node) (func(), time.Duration, bool, string) {
	logger := loggerFromContext(ctx)
	chargedCost := int64(cost.TotalCost)
	if chargedCost < 1 {
		chargedCost = 1
	}
	// kvCost semantics:
	//   cost.KVCost  > 0  -> charge that many KV tokens
	//   cost.KVCost == 0  -> cold-start fallback: mirror chargedCost
	//   cost.KVCost <  0  -> sentinel from `:dsl no-kv`, skip KV gate
	kvCost := int64(cost.KVCost)
	skipKV := kvCost < 0
	if kvCost == 0 {
		kvCost = chargedCost
	}

	emitNodeMetric := n != s.node // only emit per-node metrics for routed nodes

	// Peek to determine whether we will be queued. The result is advisory
	// (used only for logging) and races with concurrent acquirers are
	// acceptable.
	capacity, inFlight, _, _ := n.Budget.Stats()
	willQueue := inFlight+chargedCost > capacity

	if willQueue {
		// Pre-log the queue event before blocking on acquire. We log here
		// because once acquire returns, we cannot distinguish "blocked
		// briefly" from "rejected after blocking" in the queued log.
		_, _, waiting, maxWaiting := n.Budget.Stats()
		logger.Info("queued", "path", path, "node", n.ID, "cost", chargedCost, "tokens_in_flight", inFlight, "max_tokens_in_flight", capacity, "waiting_requests", waiting, "max_waiting_requests", maxWaiting)
		s.metrics.observeLifecycleEvent(chatCompletionEndpoint, "queued", "")
	}

	queueWait, skipped, ok := n.Budget.AcquireWithPriority(ctx, chargedCost, cost.Priority)
	if !ok {
		if emitNodeMetric {
			s.metrics.observeNodeAdmission(n.ID, n.Class, "rejected")
		}
		return nil, queueWait, false, "over_capacity"
	}
	if skipped && emitNodeMetric {
		s.metrics.observePrioritySkip(n.ID, n.Class)
	}

	// Per-request concurrency gate (item 15, 0.13.0). Layered AFTER
	// the token-cost gate because the token-cost gate is the
	// historical primary admission control and any KV/concurrency
	// rejection must refund the compute slot it just acquired. cost=1
	// per request (this gate counts request slots, not tokens).
	// Models a real GPU's batch-slot limit: at MaxConcurrency the
	// per-request rate has fully degraded, and any further request
	// queues here.
	concAcquired := false
	var concWait time.Duration
	if n.Concurrency != nil {
		var concSkipped bool
		concWait, concSkipped, ok = n.Concurrency.AcquireWithPriority(ctx, 1, cost.Priority)
		if !ok {
			n.Budget.Release(chargedCost)
			if emitNodeMetric {
				s.metrics.observeNodeAdmission(n.ID, n.Class, "rejected")
			}
			return nil, queueWait + concWait, false, "node_concurrency"
		}
		if concSkipped && emitNodeMetric {
			s.metrics.observePrioritySkip(n.ID, n.Class)
		}
		concAcquired = true
		queueWait += concWait
	}

	// KV admission gate. Layered on top of the compute gate per
	// docs/spec-n-memory-pressure.md \u00a75.2: if the node has KV modeling
	// enabled and the request's KV cost cannot be charged, release the
	// just-acquired compute slot and reject. The released compute slot
	// wakes the next FIFO waiter so the system stays work-conserving.
	if n.KV != nil {
		if skipKV {
			// :dsl no-kv: charge compute only, leave KV alone.
			kvCost = 0
		} else if kvOK, kvReason := n.KV.TryCharge(kvCost); !kvOK {
			n.Budget.Release(chargedCost)
			if concAcquired {
				n.Concurrency.Release(1)
			}
			if emitNodeMetric {
				s.metrics.observeNodeAdmission(n.ID, n.Class, "rejected")
			}
			logger.Warn("rejected_kv", "path", path, "node", n.ID, "kv_cost", kvCost, "reason", kvReason)
			return nil, queueWait, false, kvReason
		}
	}

	source := "direct"
	if willQueue {
		source = "waiting_to_concurrent"
	}
	_, postInFlight, waiting, maxWaiting := n.Budget.Stats()
	logger.Info("admitted", "path", path, "node", n.ID, "source", source, "cost", chargedCost, "queue_wait_ms", float64(queueWait)/float64(time.Millisecond), "tokens_in_flight", postInFlight, "max_tokens_in_flight", capacity, "waiting_requests", waiting, "max_waiting_requests", maxWaiting)
	s.metrics.observeLifecycleEvent(chatCompletionEndpoint, "admitted", "")
	if emitNodeMetric {
		s.metrics.observeNodeAdmission(n.ID, n.Class, "admitted")
		s.metrics.observeNodeQueueWait(n.ID, n.Class, queueWait)
	}
	return s.releaseFuncWithKV(n, chargedCost, kvCost, concAcquired), queueWait, true, ""
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
	s.node.Capacity.MaxTokensInFlight = int64(maxTokensInFlight)
	s.node.Capacity.MaxWaitingRequests = maxWaitingRequests
	s.mu.Unlock()
	s.node.Budget.Reconfigure(int64(maxTokensInFlight), maxWaitingRequests)
}

// Observe records actual completion tokens for the global p95 estimator,
// which improves cost estimates for subsequent requests.
func (s *requestScheduler) Observe(completionTokens int) {
	if s == nil || s.node.Estimator == nil {
		return
	}
	s.node.Estimator.Observe(completionTokens)
}

// Estimator returns the global completion-token p95 estimator.
func (s *requestScheduler) Estimator() *node.CompletionEstimator {
	if s == nil {
		return nil
	}
	return s.node.Estimator
}

func (s *requestScheduler) Stats() (int, int64, int, int) {
	capacity, inFlight, waiting, maxWaiting := s.node.Budget.Stats()
	return int(capacity), inFlight, maxWaiting, waiting
}

func (s *requestScheduler) releaseFunc(cost int64) func() {
	return s.releaseFuncOn(s.node.Budget, cost)
}

func (s *requestScheduler) releaseFuncOn(b *node.TokenBudget, cost int64) func() {
	return func() {
		b.Release(cost)
	}
}

// releaseFuncWithKV returns a release closure that refunds the compute
// slot, the KV slot (when modeled), and the concurrency slot (when
// modeled). It is the dual of the admission path in acquireOn that
// charges all three budgets.
func (s *requestScheduler) releaseFuncWithKV(n *node.Node, cost, kvCost int64, concAcquired bool) func() {
	return func() {
		n.Budget.Release(cost)
		if n.KV != nil {
			n.KV.Release(kvCost)
		}
		if concAcquired && n.Concurrency != nil {
			n.Concurrency.Release(1)
		}
	}
}

// combinedLoadOf returns max(cost_load, kv_load * kv_weight) for the
// given node. Used by the per-node `cllm_node_combined_load` metric to
// expose the same load fraction the (now per-node) admission path
// reasons about.
func combinedLoadOf(n *node.Node) float64 {
	if n == nil || n.Budget == nil {
		return 0
	}
	capacity, inFlight, _, _ := n.Budget.Stats()
	cost := costLoad(capacity, inFlight)
	if n.KV == nil {
		return cost
	}
	kvCap, kvInFlight := n.KV.Stats()
	kv := costLoad(kvCap, kvInFlight)
	weight := n.Capacity.KVWeight
	if weight <= 0 {
		weight = 1.0
	}
	weighted := kv * weight
	if weighted > cost {
		return weighted
	}
	return cost
}

func costLoad(capacity, inFlight int64) float64 {
	if capacity <= 0 {
		return 0
	}
	return float64(inFlight) / float64(capacity)
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
