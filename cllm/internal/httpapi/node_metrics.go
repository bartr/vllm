package httpapi

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// nodeFleetCollector exposes per-node tokens_in_flight and capacity
// gauges by walking the handler's node list at scrape time. Using a
// custom collector (rather than a GaugeVec we mutate on every admission)
// keeps the metric value in lock-step with the underlying TokenBudget,
// matches the existing pattern used for cllm_tokens_in_flight, and
// avoids a separate update path.
type nodeFleetCollector struct {
	handler *Handler
}

var (
	nodeTokensInFlightDesc = prometheus.NewDesc(
		"cllm_node_tokens_in_flight",
		"Per-node admitted token cost in flight. Emitted only when nodes.yaml is loaded.",
		[]string{"node", "class"}, nil,
	)
	nodeMaxTokensInFlightDesc = prometheus.NewDesc(
		"cllm_node_max_tokens_in_flight",
		"Per-node configured maximum admitted token cost in flight. Emitted only when nodes.yaml is loaded.",
		[]string{"node", "class"}, nil,
	)
	nodeWaitingRequestsDesc = prometheus.NewDesc(
		"cllm_node_waiting_requests",
		"Per-node FIFO queue depth (requests awaiting admission). Emitted only when nodes.yaml is loaded.",
		[]string{"node", "class"}, nil,
	)
	nodeKVTokensInFlightDesc = prometheus.NewDesc(
		"cllm_node_kv_tokens_in_flight",
		"Per-node KV-cache occupancy in KV tokens. Emitted only when the node has KV modeling enabled (max_kv_tokens > 0) and nodes.yaml defines more than one node.",
		[]string{"node", "class"}, nil,
	)
	nodeMaxKVTokensDesc = prometheus.NewDesc(
		"cllm_node_max_kv_tokens",
		"Per-node configured KV-cache occupancy ceiling in KV tokens. Emitted only when the node has KV modeling enabled.",
		[]string{"node", "class"}, nil,
	)
	nodeCombinedLoadDesc = prometheus.NewDesc(
		"cllm_node_combined_load",
		"Per-node combined load max(cost_load, kv_load * kv_weight) feeding f(load). Emitted only when the node has KV modeling enabled.",
		[]string{"node", "class"}, nil,
	)
	nodeKVEstimatorP95Desc = prometheus.NewDesc(
		"cllm_node_kv_estimator_p95",
		"Per-node KV estimator p95 of recent completion tokens (Phase 4 of design-memory-pressure.md). Emitted only when the node has KV modeling enabled and the estimator has reached its warm-up sample count; otherwise the series is omitted.",
		[]string{"node", "class"}, nil,
	)
	nodeConcurrentRequestsDesc = prometheus.NewDesc(
		"cllm_node_concurrent_requests",
		"Per-node concurrent admitted requests (item 15, 0.13.0). Emitted only when the node has the concurrency gate enabled (max_concurrency > 0).",
		[]string{"node", "class"}, nil,
	)
	nodeMaxConcurrencyDesc = prometheus.NewDesc(
		"cllm_node_max_concurrency",
		"Per-node configured maximum concurrent admitted requests (item 15, 0.13.0). Emitted only when the concurrency gate is enabled.",
		[]string{"node", "class"}, nil,
	)
	nodePerRequestTPSEffectiveDesc = prometheus.NewDesc(
		"cllm_node_per_request_tps_effective",
		"Per-node effective per-request decode rate (tokens/second) at current concurrency, after three-regime degradation. Emitted only when per_request_tokens_per_second > 0 on the node.",
		[]string{"node", "class"}, nil,
	)
)

func (c nodeFleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nodeTokensInFlightDesc
	ch <- nodeMaxTokensInFlightDesc
	ch <- nodeWaitingRequestsDesc
	ch <- nodeKVTokensInFlightDesc
	ch <- nodeMaxKVTokensDesc
	ch <- nodeCombinedLoadDesc
	ch <- nodeKVEstimatorP95Desc
	ch <- nodeConcurrentRequestsDesc
	ch <- nodeMaxConcurrencyDesc
	ch <- nodePerRequestTPSEffectiveDesc
}

func (c nodeFleetCollector) Collect(ch chan<- prometheus.Metric) {
	if c.handler == nil {
		return
	}
	c.handler.configMu.RLock()
	nodes := c.handler.nodes
	c.handler.configMu.RUnlock()
	// Single-node default fleets are uninteresting at the per-node
	// granularity (the global cllm_tokens_in_flight already covers
	// them) and would otherwise show up as a redundant series.
	if len(nodes) <= 1 {
		return
	}
	for _, n := range nodes {
		if n == nil || n.Budget == nil {
			continue
		}
		capacity, inFlight, _, _ := n.Budget.Stats()
		ch <- prometheus.MustNewConstMetric(nodeTokensInFlightDesc, prometheus.GaugeValue, float64(inFlight), n.ID, n.Class)
		ch <- prometheus.MustNewConstMetric(nodeMaxTokensInFlightDesc, prometheus.GaugeValue, float64(capacity), n.ID, n.Class)
		// KV-axis series are emitted only for nodes that have it
		// enabled, so deployments mixing KV-modeled and KV-disabled
		// nodes don't produce zero-valued KV series for the disabled
		// ones.
		if n.KV != nil {
			kvCap, kvInFlight := n.KV.Stats()
			ch <- prometheus.MustNewConstMetric(nodeKVTokensInFlightDesc, prometheus.GaugeValue, float64(kvInFlight), n.ID, n.Class)
			ch <- prometheus.MustNewConstMetric(nodeMaxKVTokensDesc, prometheus.GaugeValue, float64(kvCap), n.ID, n.Class)
			ch <- prometheus.MustNewConstMetric(nodeCombinedLoadDesc, prometheus.GaugeValue, combinedLoadOf(n), n.ID, n.Class)
			// Phase 4: emit kv_estimator_p95 only when the
			// estimator has crossed its warm-up threshold so cold
			// nodes don't stamp 0 into a gauge that scrapers
			// would otherwise plot as a real reading.
			if n.KVEstimator != nil {
				if p95, ok := n.KVEstimator.P95(); ok {
					ch <- prometheus.MustNewConstMetric(nodeKVEstimatorP95Desc, prometheus.GaugeValue, float64(p95), n.ID, n.Class)
				}
			}
		}
		// vLLM-style per-request pacing series (item 15, 0.13.0).
		// Emitted only when the node has the concurrency gate
		// enabled OR per-request pacing enabled, so passthrough-style
		// nodes don't stamp zeros into series scrapers would plot.
		if n.Concurrency != nil {
			_, conc, concWaiting, _ := n.Concurrency.Stats()
			ch <- prometheus.MustNewConstMetric(nodeConcurrentRequestsDesc, prometheus.GaugeValue, float64(conc), n.ID, n.Class)
			ch <- prometheus.MustNewConstMetric(nodeMaxConcurrencyDesc, prometheus.GaugeValue, float64(n.Capacity.MaxConcurrency), n.ID, n.Class)
			ch <- prometheus.MustNewConstMetric(nodeWaitingRequestsDesc, prometheus.GaugeValue, float64(concWaiting), n.ID, n.Class)
		}
		if n.Capacity.PerRequestTPS > 0 {
			ch <- prometheus.MustNewConstMetric(nodePerRequestTPSEffectiveDesc, prometheus.GaugeValue, n.PerRequestRate(n.ConcurrentRequests()), n.ID, n.Class)
		}
	}
}

func (m *handlerMetrics) observeNodeAdmission(nodeID, class, result string) {
	if m == nil || m.nodeAdmissionsTotal == nil {
		return
	}
	m.nodeAdmissionsTotal.WithLabelValues(nodeID, class, result).Inc()
}

func (m *handlerMetrics) observeNodeQueueWait(nodeID, class string, d time.Duration) {
	if m == nil || m.nodeQueueWait == nil || d <= 0 {
		return
	}
	m.nodeQueueWait.WithLabelValues(nodeID, class).Observe(d.Seconds())
}

// observePrioritySkip increments the per-node priority-skip counter when
// the admission queue promoted a waiter past the FIFO head (Phase 14C).
// Class is the routed node's class, not the resolved workload class \u2014
// kept consistent with the rest of the per-node metric family.
func (m *handlerMetrics) observePrioritySkip(nodeID, class string) {
	if m == nil || m.prioritySkipsTotal == nil {
		return
	}
	m.prioritySkipsTotal.WithLabelValues(nodeID, class).Inc()
}
