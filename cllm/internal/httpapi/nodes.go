package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"cllm/internal/node"
	"cllm/internal/router"
)

type nodesResponse struct {
	RouterPolicy string         `json:"router_policy"`
	Count        int            `json:"count"`
	Nodes        []nodeResponse `json:"nodes"`
}

type nodeResponse struct {
	ID          string           `json:"id"`
	Class       string           `json:"class"`
	Capacity    node.Capacity    `json:"capacity"`
	Degradation node.Degradation `json:"degradation"`
	Realism     node.Realism     `json:"realism"`
	Upstream    *node.Upstream   `json:"upstream,omitempty"`
	Stats       nodeRuntimeStats `json:"stats"`
}

type nodeRuntimeStats struct {
	TokensInFlight     int64 `json:"tokens_in_flight"`
	WaitingRequests    int   `json:"waiting_requests"`
	KVTokensInFlight   int64 `json:"kv_tokens_in_flight,omitempty"`
	KVWaitingRequests  int   `json:"kv_waiting_requests,omitempty"`
	ConcurrentRequests int   `json:"concurrent_requests,omitempty"`
	ConcurrencyWaiters int   `json:"concurrency_waiting_requests,omitempty"`
}

type nodeUpsertPayload struct {
	Class string `json:"class"`

	MaxTokensInFlight  *int64 `json:"max_tokens_in_flight"`
	MaxWaitingRequests *int   `json:"max_waiting_requests"`

	PerRequestTPS        *int     `json:"per_request_tokens_per_second"`
	DegradationThreshold *int     `json:"degradation_threshold"`
	MaxConcurrency       *int     `json:"max_concurrency"`
	BypassCache          *bool    `json:"bypass_cache"`
	MaxKVTokens          *int64   `json:"max_kv_tokens"`
	KVWeight             *float64 `json:"kv_weight"`
	KVCompletionFactor   *float64 `json:"kv_completion_factor"`

	FLoadShape     string `json:"f_load_shape"`
	MaxDegradation *int   `json:"max_degradation"`

	PrefillRateMultiplier *float64 `json:"prefill_rate_multiplier"`
	PrefillBaseOverheadMs *int     `json:"prefill_base_overhead_ms"`
	PrefillJitterPercent  *int     `json:"prefill_jitter_percent"`
	PrefillMaxMs          *int     `json:"prefill_max_ms"`
	StreamVariabilityPct  *int     `json:"stream_variability_percent"`
	StreamJitterPct       *int     `json:"stream_jitter_percent"`
	StreamStallProbPct    *int     `json:"stream_stall_probability_percent"`
	StreamStallMinMs      *int     `json:"stream_stall_min_ms"`
	StreamStallMaxMs      *int     `json:"stream_stall_max_ms"`

	UpstreamURL   *string `json:"upstream"`
	UpstreamToken *string `json:"token"`
	UpstreamModel *string `json:"model"`
}

// SetNodes installs a multi-node fleet on the handler. The first call
// replaces the default single-node slice; admission still flows through
// h.scheduler in this phase, but the router decision is logged so the
// routing surface is observable end to end.
//
// The supplied policy maps to a router.Router via router.FromPolicy. An
// empty policy yields the default Chained{ClassPinned, LeastLoaded}.
//
// Phase 2.4 will switch admission and per-node metrics over to use these
// nodes; Phase 2.3 only plumbs them in.
func (h *Handler) SetNodes(nodes []*node.Node, policy string) {
	if h == nil {
		return
	}
	h.configMu.Lock()
	if len(nodes) == 0 {
		// Preserve the default single node so the rest of the handler
		// can keep referencing h.nodes[0].
		if h.scheduler != nil {
			h.nodes = []*node.Node{h.scheduler.node}
		} else {
			h.nodes = nil
		}
	} else {
		h.nodes = nodes
	}
	h.router = router.FromPolicy(policy)
	h.nodeRouterPolicy = strings.TrimSpace(policy)
	h.configMu.Unlock()
}

// NodeIDs returns the IDs of the configured nodes in slice order. Used by
// the /config endpoint and tests; safe for concurrent use.
func (h *Handler) NodeIDs() []string {
	if h == nil {
		return nil
	}
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	out := make([]string, 0, len(h.nodes))
	for _, n := range h.nodes {
		if n == nil {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

func (h *Handler) nodesEndpoint(w http.ResponseWriter, r *http.Request) {
	markCacheHit(w, false)
	switch r.Method {
	case http.MethodGet:
		if preferHTML(r.Header.Get("Accept")) {
			if r.URL.Query().Get("new") == "1" {
				state := nodeFormState{NewNode: true, Status: http.StatusOK}
				// Pre-fill the form with the values of the "cllm" node
				// (or the first node, when no node named "cllm" exists)
				// so operators get a sensible starting point instead of
				// blank fields.
				if tmpl, ok := h.getNodeSnapshot("cllm"); ok {
					state.Node = nodeResponseFromNode(tmpl)
					state.Node.Upstream = nil // don't copy upstream URL/model into a new synthetic node
				} else if ids := h.NodeIDs(); len(ids) > 0 {
					if tmpl, ok := h.getNodeSnapshot(ids[0]); ok {
						state.Node = nodeResponseFromNode(tmpl)
						state.Node.Upstream = nil
					}
				}
				h.renderNodeEditHTML(w, r, state)
				return
			}
			h.renderNodesHTML(w, r)
			return
		}
		writeJSON(w, http.StatusOK, h.currentNodes())
	default:
		writePlainText(w, http.StatusMethodNotAllowed, "method not allowed\n")
	}
}

func (h *Handler) nodeEndpoint(w http.ResponseWriter, r *http.Request) {
	markCacheHit(w, false)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writePlainText(w, http.StatusBadRequest, "missing node id\n")
		return
	}

	wantsHTML := preferHTML(r.Header.Get("Accept"))
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	isFormPost := strings.Contains(contentType, "application/x-www-form-urlencoded")

	switch r.Method {
	case http.MethodGet:
		n, ok := h.getNodeSnapshot(id)
		if !ok {
			if wantsHTML {
				writePlainText(w, http.StatusNotFound, "node not found\n")
				return
			}
			writePlainText(w, http.StatusNotFound, "node not found\n")
			return
		}
		if wantsHTML {
			h.renderNodeEditHTML(w, r, nodeFormState{ID: id, Node: nodeResponseFromNode(n), Status: http.StatusOK})
			return
		}
		writeJSON(w, http.StatusOK, nodeResponseFromNode(n))
	case http.MethodPost, http.MethodPut:
		payload, err := parseNodeUpsertPayload(r)
		if err != nil {
			if isFormPost || wantsHTML {
				values, _ := formValues(r)
				h.renderNodeEditHTML(w, r, nodeFormState{ID: id, NewNode: !nodeExists(h, id), Values: values, Error: err.Error(), Status: http.StatusBadRequest})
				return
			}
			writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
			return
		}
		n, created, err := h.upsertNode(id, payload)
		if err != nil {
			if isFormPost || wantsHTML {
				values, _ := formValues(r)
				h.renderNodeEditHTML(w, r, nodeFormState{ID: id, NewNode: !nodeExists(h, id), Values: values, Error: err.Error(), Status: http.StatusBadRequest})
				return
			}
			writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
			return
		}
		if isFormPost || wantsHTML {
			http.Redirect(w, r, "/nodes", http.StatusSeeOther)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, nodeResponseFromNode(n))
	case http.MethodDelete:
		deleted, err := h.deleteNode(id)
		if err != nil {
			writePlainText(w, http.StatusBadRequest, err.Error()+"\n")
			return
		}
		if !deleted {
			writePlainText(w, http.StatusNotFound, "node not found\n")
			return
		}
		writeJSON(w, http.StatusOK, h.currentNodes())
	default:
		writePlainText(w, http.StatusMethodNotAllowed, "method not allowed\n")
	}
}

func (h *Handler) currentNodes() nodesResponse {
	h.configMu.RLock()
	nodes := append([]*node.Node(nil), h.nodes...)
	policy := h.nodeRouterPolicy
	h.configMu.RUnlock()

	items := make([]nodeResponse, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		items = append(items, nodeResponseFromNode(n))
	}
	return nodesResponse{RouterPolicy: policy, Count: len(items), Nodes: items}
}

func (h *Handler) getNodeSnapshot(id string) (*node.Node, bool) {
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	for _, n := range h.nodes {
		if n != nil && n.ID == id {
			return n, true
		}
	}
	return nil, false
}

func (h *Handler) upsertNode(id string, payload nodeUpsertPayload) (*node.Node, bool, error) {
	if err := validateNodeID(id); err != nil {
		return nil, false, err
	}

	h.configMu.Lock()
	defer h.configMu.Unlock()

	var existing *node.Node
	for _, n := range h.nodes {
		if n != nil && n.ID == id {
			existing = n
			break
		}
	}
	created := existing == nil
	n, err := materializeRuntimeNode(id, payload, existing)
	if err != nil {
		return nil, false, err
	}
	replacement := append([]*node.Node(nil), h.nodes...)
	if created {
		replacement = append(replacement, n)
		sort.SliceStable(replacement, func(i, j int) bool {
			if replacement[i] == nil {
				return false
			}
			if replacement[j] == nil {
				return true
			}
			return replacement[i].ID < replacement[j].ID
		})
		h.nodes = replacement
	} else {
		applyRuntimeNodeConfig(existing, n)
		n = existing
	}
	return n, created, nil
}

func (h *Handler) deleteNode(id string) (bool, error) {
	if err := validateNodeID(id); err != nil {
		return false, err
	}

	h.configMu.Lock()
	defer h.configMu.Unlock()
	activeCount := 0
	replacement := make([]*node.Node, 0, len(h.nodes)-1)
	found := false
	for _, n := range h.nodes {
		if n != nil {
			activeCount++
		}
		if n != nil && n.ID == id {
			found = true
			continue
		}
		replacement = append(replacement, n)
	}
	if !found {
		return false, nil
	}
	if activeCount <= 1 {
		return false, fmt.Errorf("cannot delete the last node")
	}
	h.nodes = replacement
	return true, nil
}

func validateNodeID(id string) error {
	if id == "" {
		return fmt.Errorf("node id must be non-empty")
	}
	if strings.ContainsAny(id, " \t\r\n/") {
		return fmt.Errorf("node id %q must not contain whitespace or slash", id)
	}
	return nil
}

func nodeResponseFromNode(n *node.Node) nodeResponse {
	if n == nil {
		return nodeResponse{}
	}
	return nodeResponse{
		ID:          n.ID,
		Class:       n.Class,
		Capacity:    n.Capacity,
		Degradation: n.Degradation,
		Realism:     n.Realism,
		Upstream:    cloneUpstream(n.Upstream),
		Stats:       runtimeStatsFromNode(n),
	}
}

func runtimeStatsFromNode(n *node.Node) nodeRuntimeStats {
	var stats nodeRuntimeStats
	if n == nil {
		return stats
	}
	if n.Budget != nil {
		_, inFlight, waiting, _ := n.Budget.Stats()
		stats.TokensInFlight = inFlight
		stats.WaitingRequests = waiting
	}
	if n.KV != nil {
		_, inFlight := n.KV.Stats()
		stats.KVTokensInFlight = inFlight
	}
	if n.Concurrency != nil {
		_, inFlight, waiting, _ := n.Concurrency.Stats()
		stats.ConcurrentRequests = int(inFlight)
		stats.ConcurrencyWaiters = waiting
	}
	return stats
}

func cloneUpstream(upstream *node.Upstream) *node.Upstream {
	if upstream == nil {
		return nil
	}
	return &node.Upstream{URL: upstream.URL, Token: upstream.Token, Model: upstream.Model}
}

func materializeRuntimeNode(id string, payload nodeUpsertPayload, existing *node.Node) (*node.Node, error) {
	capacity := node.Capacity{}
	degradation := node.Degradation{Shape: "piecewise_linear"}
	realism := node.Realism{}
	class := ""
	var upstream *node.Upstream
	if existing != nil {
		class = existing.Class
		capacity = existing.Capacity
		degradation = existing.Degradation
		realism = existing.Realism
		upstream = cloneUpstream(existing.Upstream)
	}
	if payload.Class != "" {
		class = strings.TrimSpace(payload.Class)
	}
	if payload.MaxTokensInFlight != nil {
		capacity.MaxTokensInFlight = *payload.MaxTokensInFlight
	}
	if payload.MaxWaitingRequests != nil {
		capacity.MaxWaitingRequests = *payload.MaxWaitingRequests
	}
	if payload.PerRequestTPS != nil {
		capacity.PerRequestTPS = *payload.PerRequestTPS
	}
	if payload.DegradationThreshold != nil {
		capacity.DegradationThreshold = *payload.DegradationThreshold
	}
	if payload.MaxConcurrency != nil {
		capacity.MaxConcurrency = *payload.MaxConcurrency
	}
	if payload.BypassCache != nil {
		capacity.BypassCache = *payload.BypassCache
	}
	if payload.MaxKVTokens != nil {
		capacity.MaxKVTokens = *payload.MaxKVTokens
	}
	if payload.KVWeight != nil {
		capacity.KVWeight = *payload.KVWeight
	}
	if payload.KVCompletionFactor != nil {
		capacity.KVCompletionFactor = *payload.KVCompletionFactor
	}
	if payload.FLoadShape != "" {
		degradation.Shape = strings.TrimSpace(payload.FLoadShape)
	}
	if degradation.Shape == "" {
		degradation.Shape = "piecewise_linear"
	}
	if payload.MaxDegradation != nil {
		degradation.MaxDegradation = *payload.MaxDegradation
	}
	if payload.PrefillRateMultiplier != nil {
		realism.PrefillRateMultiplier = *payload.PrefillRateMultiplier
	}
	if payload.PrefillBaseOverheadMs != nil {
		realism.PrefillBaseOverheadMs = *payload.PrefillBaseOverheadMs
	}
	if payload.PrefillJitterPercent != nil {
		realism.PrefillJitterPercent = *payload.PrefillJitterPercent
	}
	if payload.PrefillMaxMs != nil {
		realism.PrefillMaxMs = *payload.PrefillMaxMs
	}
	if payload.StreamVariabilityPct != nil {
		realism.StreamVariabilityPct = *payload.StreamVariabilityPct
	}
	if payload.StreamJitterPct != nil {
		realism.StreamJitterPct = *payload.StreamJitterPct
	}
	if payload.StreamStallProbPct != nil {
		realism.StreamStallProbPct = *payload.StreamStallProbPct
	}
	if payload.StreamStallMinMs != nil {
		realism.StreamStallMinMs = *payload.StreamStallMinMs
	}
	if payload.StreamStallMaxMs != nil {
		realism.StreamStallMaxMs = *payload.StreamStallMaxMs
	}
	if payload.UpstreamURL != nil || payload.UpstreamToken != nil || payload.UpstreamModel != nil {
		if upstream == nil {
			upstream = &node.Upstream{}
		}
		if payload.UpstreamURL != nil {
			upstream.URL = strings.TrimSpace(*payload.UpstreamURL)
		}
		if payload.UpstreamToken != nil {
			upstream.Token = *payload.UpstreamToken
		}
		if payload.UpstreamModel != nil {
			upstream.Model = strings.TrimSpace(*payload.UpstreamModel)
		}
		if upstream.URL == "" && upstream.Token == "" && upstream.Model == "" {
			upstream = nil
		}
	}
	if class == "" {
		class = "default"
	}
	if capacity.MaxTokensInFlight < 0 {
		return nil, fmt.Errorf("max_tokens_in_flight must be >= 0")
	}
	if capacity.MaxWaitingRequests < 0 {
		return nil, fmt.Errorf("max_waiting_requests must be >= 0")
	}
	if capacity.PerRequestTPS < 0 {
		return nil, fmt.Errorf("per_request_tokens_per_second must be >= 0")
	}
	if capacity.DegradationThreshold < 0 {
		return nil, fmt.Errorf("degradation_threshold must be >= 0")
	}
	if capacity.MaxConcurrency < 0 {
		return nil, fmt.Errorf("max_concurrency must be >= 0")
	}
	if capacity.MaxKVTokens < 0 {
		return nil, fmt.Errorf("max_kv_tokens must be >= 0")
	}
	if capacity.MaxKVTokens > 0 && capacity.KVWeight <= 0 {
		capacity.KVWeight = 1.0
	}
	if degradation.MaxDegradation < 0 || degradation.MaxDegradation > 95 {
		return nil, fmt.Errorf("max_degradation must be between 0 and 95")
	}
	return node.New(id, class, capacity, degradation, realism, upstream), nil
}

func applyRuntimeNodeConfig(dst, src *node.Node) {
	if dst == nil || src == nil {
		return
	}
	dst.Class = src.Class
	dst.Capacity = src.Capacity
	dst.Degradation = src.Degradation
	dst.Realism = src.Realism
	dst.Upstream = cloneUpstream(src.Upstream)

	if dst.Budget == nil {
		dst.Budget = node.NewTokenBudget(src.Capacity.MaxTokensInFlight, src.Capacity.MaxWaitingRequests)
		dst.Budget.SetAgingStepMs(node.DefaultPriorityAgingStepMs)
	} else {
		dst.Budget.Reconfigure(src.Capacity.MaxTokensInFlight, src.Capacity.MaxWaitingRequests)
	}
	if dst.Estimator == nil {
		dst.Estimator = node.NewCompletionEstimator(256, 50)
	}

	if src.Capacity.MaxKVTokens > 0 {
		if dst.KV == nil {
			dst.KV = node.NewKVBudget(src.Capacity.MaxKVTokens)
		} else {
			dst.KV.Reconfigure(src.Capacity.MaxKVTokens)
		}
		if dst.KVEstimator == nil {
			dst.KVEstimator = node.NewCompletionEstimator(256, 50)
		}
	} else {
		dst.KV = nil
		dst.KVEstimator = nil
	}

	if src.Capacity.MaxConcurrency > 0 {
		if dst.Concurrency == nil {
			dst.Concurrency = node.NewTokenBudget(int64(src.Capacity.MaxConcurrency), src.Capacity.MaxWaitingRequests)
			dst.Concurrency.SetAgingStepMs(node.DefaultPriorityAgingStepMs)
		} else {
			dst.Concurrency.Reconfigure(int64(src.Capacity.MaxConcurrency), src.Capacity.MaxWaitingRequests)
		}
	} else {
		dst.Concurrency = nil
	}
}

func parseNodeUpsertPayload(r *http.Request) (nodeUpsertPayload, error) {
	payload := nodeUpsertPayload{}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return payload, fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	values := r.URL.Query()
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return payload, fmt.Errorf("invalid form: %w", err)
		}
		values = r.Form
	}
	if len(values) > 0 {
		if err := overlayNodeQueryValues(&payload, values); err != nil {
			return payload, err
		}
	}
	return payload, nil
}

func overlayNodeQueryValues(payload *nodeUpsertPayload, values url.Values) error {
	if v := configQueryValue(values, "class"); v != "" {
		payload.Class = v
	}
	if err := setInt64(values, &payload.MaxTokensInFlight, "max-tokens-in-flight", "max_tokens_in_flight"); err != nil {
		return err
	}
	if err := setInt(values, &payload.MaxWaitingRequests, "max-waiting-requests", "max_waiting_requests"); err != nil {
		return err
	}
	if err := setInt(values, &payload.PerRequestTPS, "per-request-tokens-per-second", "per_request_tokens_per_second", "tps"); err != nil {
		return err
	}
	if err := setInt(values, &payload.DegradationThreshold, "degradation-threshold", "degradation_threshold"); err != nil {
		return err
	}
	if err := setInt(values, &payload.MaxConcurrency, "max-concurrency", "max_concurrency"); err != nil {
		return err
	}
	if err := setBool(values, &payload.BypassCache, "bypass-cache", "bypass_cache"); err != nil {
		return err
	}
	if err := setInt64(values, &payload.MaxKVTokens, "max-kv-tokens", "max_kv_tokens"); err != nil {
		return err
	}
	if err := setFloat64(values, &payload.KVWeight, "kv-weight", "kv_weight"); err != nil {
		return err
	}
	if err := setFloat64(values, &payload.KVCompletionFactor, "kv-completion-factor", "kv_completion_factor"); err != nil {
		return err
	}
	if v := configQueryValue(values, "f-load-shape", "f_load_shape"); v != "" {
		payload.FLoadShape = v
	}
	if err := setInt(values, &payload.MaxDegradation, "max-degradation", "max_degradation"); err != nil {
		return err
	}
	if err := setFloat64(values, &payload.PrefillRateMultiplier, "prefill-rate-multiplier", "prefill_rate_multiplier"); err != nil {
		return err
	}
	if err := setInt(values, &payload.PrefillBaseOverheadMs, "prefill-base-overhead-ms", "prefill_base_overhead_ms"); err != nil {
		return err
	}
	if err := setInt(values, &payload.PrefillJitterPercent, "prefill-jitter-percent", "prefill_jitter_percent"); err != nil {
		return err
	}
	if err := setInt(values, &payload.PrefillMaxMs, "prefill-max-ms", "prefill_max_ms"); err != nil {
		return err
	}
	if err := setInt(values, &payload.StreamVariabilityPct, "stream-variability-percent", "stream_variability_percent"); err != nil {
		return err
	}
	if err := setInt(values, &payload.StreamJitterPct, "stream-jitter-percent", "stream_jitter_percent"); err != nil {
		return err
	}
	if err := setInt(values, &payload.StreamStallProbPct, "stream-stall-probability-percent", "stream_stall_probability_percent"); err != nil {
		return err
	}
	if err := setInt(values, &payload.StreamStallMinMs, "stream-stall-min-ms", "stream_stall_min_ms"); err != nil {
		return err
	}
	if err := setInt(values, &payload.StreamStallMaxMs, "stream-stall-max-ms", "stream_stall_max_ms"); err != nil {
		return err
	}
	setString(values, &payload.UpstreamURL, "upstream", "upstream-url", "upstream_url")
	setString(values, &payload.UpstreamToken, "token", "upstream-token", "upstream_token")
	setString(values, &payload.UpstreamModel, "model", "upstream-model", "upstream_model")
	return nil
}

func setString(values url.Values, dest **string, keys ...string) {
	if !hasAny(values, keys...) {
		return
	}
	v := configQueryValue(values, keys...)
	*dest = &v
}

func setBool(values url.Values, dest **bool, keys ...string) error {
	if !hasAny(values, keys...) {
		return nil
	}
	raw := configQueryValue(values, keys...)
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q", keys[0], raw)
	}
	*dest = &v
	return nil
}

func setInt(values url.Values, dest **int, keys ...string) error {
	if !hasAny(values, keys...) {
		return nil
	}
	raw := configQueryValue(values, keys...)
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q", keys[0], raw)
	}
	*dest = &v
	return nil
}

func setInt64(values url.Values, dest **int64, keys ...string) error {
	if !hasAny(values, keys...) {
		return nil
	}
	raw := configQueryValue(values, keys...)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid %s %q", keys[0], raw)
	}
	*dest = &v
	return nil
}

func setFloat64(values url.Values, dest **float64, keys ...string) error {
	if !hasAny(values, keys...) {
		return nil
	}
	raw := configQueryValue(values, keys...)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("invalid %s %q", keys[0], raw)
	}
	*dest = &v
	return nil
}

func hasAny(values url.Values, keys ...string) bool {
	for _, key := range keys {
		if values.Has(key) {
			return true
		}
	}
	return false
}

// pickRoutedNode runs the configured router and returns the chosen node
// (if any) and a short reason tag. Phase 2.3: advisory only. Returns nil
// when the router can't match (e.g. ClassPinned-only with no pin in the
// request).
func (h *Handler) pickRoutedNode(ctx context.Context, overrides replayOverrides, cost node.RequestCost) (*node.Node, string) {
	h.configMu.RLock()
	nodes := h.nodes
	r := h.router
	h.configMu.RUnlock()
	if r == nil || len(nodes) == 0 {
		return nil, ""
	}
	req := &router.Request{
		Node:  strings.TrimSpace(overrides.nodeID),
		Class: strings.TrimSpace(overrides.nodeClass),
		Cost:  int64(cost.TotalCost),
	}
	d, err := r.Pick(ctx, req, nodes)
	if err != nil {
		return nil, ""
	}
	return d.Node, d.Reason
}

// logRouterDecisionIfMultiNode emits a structured log line describing the
// router's choice when the fleet is larger than one node OR when the
// request carried an explicit pin. Single-node fleets without pins are
// silent to avoid noise. Used in the chat/completions hot path.
func (h *Handler) logRouterDecisionIfMultiNode(ctx context.Context, n *node.Node, reason string, overrides replayOverrides) {
	if n == nil {
		return
	}
	h.configMu.RLock()
	multi := len(h.nodes) > 1
	h.configMu.RUnlock()
	if !multi && overrides.nodeID == "" && overrides.nodeClass == "" {
		return
	}
	logger := loggerFromContext(ctx)
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("routed",
		"node_id", n.ID,
		"node_class", n.Class,
		"reason", reason,
		"pin_node", overrides.nodeID,
		"pin_class", overrides.nodeClass,
	)
}
