package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type handlerMetrics struct {
	registry *prometheus.Registry

	httpInflightRequests      prometheus.Gauge
	httpRequestsTotal         *prometheus.CounterVec
	httpRequestDuration       *prometheus.HistogramVec
	httpResponseSizeBytes     *prometheus.HistogramVec
	jobsTotal                 *prometheus.CounterVec
	completionTokensTotal     *prometheus.CounterVec
	cacheLookupsTotal         *prometheus.CounterVec
	queueWaitDuration         *prometheus.HistogramVec
	timeToFirstByteDuration   *prometheus.HistogramVec
	prefillDuration           *prometheus.HistogramVec
	streamStallDuration       *prometheus.HistogramVec
	streamStallsTotal         *prometheus.CounterVec
	dslDirectivesTotal        *prometheus.CounterVec
	dslRequestsTotal          *prometheus.CounterVec
	dslTimeToFirstByte        *prometheus.HistogramVec
	dslJobDuration            *prometheus.HistogramVec
	jobDuration               *prometheus.HistogramVec
	downstreamRequestDuration *prometheus.HistogramVec
	requestLifecycleEvents    *prometheus.CounterVec
	tenantAdmissionTotal      *prometheus.CounterVec
	tenantRejectionsTotal     *prometheus.CounterVec

	// Per-node admission metrics. Populated only when nodes.yaml is
	// loaded and routing assigns each request to a specific node.
	// Phase 2.4 of the multi-node design.
	nodeAdmissionsTotal *prometheus.CounterVec
	nodeQueueWait       *prometheus.HistogramVec

	// Phase-aware token allocation (item 13). Counts streams that
	// crossed a phase boundary, labelled by class and the (from, to)
	// pair. Today only (phase_a, phase_b) fires; future expansions
	// (e.g. an explicit cool-down phase C) reuse the same counter.
	phaseTransitionsTotal *prometheus.CounterVec
	// Phase-A and phase-B tokens emitted, per class. The split lets
	// the dashboard render the responsiveness vs. sustained mix as a
	// stacked rate.
	phaseATokensTotal *prometheus.CounterVec
	phaseBTokensTotal *prometheus.CounterVec
	// Most recently observed effective phase-A and phase-B TPS, per
	// class. Each cached-replay completion sets the value to its own
	// computed rate; this is a sample, not a moving average. Useful
	// as a sanity check that the configured rate is actually in force.
	phaseInitialTPSEffective   *prometheus.GaugeVec
	phaseSustainedTPSEffective *prometheus.GaugeVec
	// Reclaim counter: (initial_tps - sustained_tps) * tokens_in_phase_b.
	// Always >= 0 because rates with sustained >= initial are degenerate
	// (legacy single-rate, no reclaim). The headline efficiency metric
	// for the design.
	phaseReclaimTokenSecondsTotal *prometheus.CounterVec

	// Priority-skips counter: how often the admission queue promoted a
	// non-head waiter, by node + class (Phase 14C). A zero series after
	// load means priority did not affect ordering (everything in the
	// queue had matching priority). Emitted only on the multi-node
	// admission path.
	prioritySkipsTotal *prometheus.CounterVec
}

const chatCompletionsRouteLabel = "POST /v1/chat/completions"

func newHandlerMetrics(handler *Handler) *handlerMetrics {
	registry := prometheus.NewRegistry()
	metrics := &handlerMetrics{
		registry: registry,
		httpInflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cllm_http_inflight_requests",
			Help: "Current number of in-flight HTTP requests.",
		}),
		httpRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_http_requests_total",
			Help: "Total number of HTTP requests handled by cllm.",
		}, []string{"route", "method", "status"}),
		httpRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "status"}),
		httpResponseSizeBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_http_response_size_bytes",
			Help:    "HTTP response size in bytes.",
			Buckets: prometheus.ExponentialBuckets(128, 2, 10),
		}, []string{"route", "method", "status"}),
		jobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_jobs_total",
			Help: "Total number of cllm jobs by endpoint, result, source, and routed node. The node label is 'unknown' for jobs rejected before routing or in single-node deployments.",
		}, []string{"endpoint", "result", "source", "node"}),
		completionTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_completion_tokens_total",
			Help: "Total number of completion tokens returned by cllm responses, broken down by routed node.",
		}, []string{"endpoint", "source", "node"}),
		cacheLookupsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_cache_lookups_total",
			Help: "Total number of cache lookups by endpoint and result.",
		}, []string{"endpoint", "result"}),
		queueWaitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_queue_wait_duration_seconds",
			Help:    "Time jobs spend waiting in the request queue before admission, broken down by routed node.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint", "node"}),
		timeToFirstByteDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_time_to_first_byte_seconds",
			Help:    "Time from job admission to the first response byte written to the client, broken down by routed node.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 10, 30},
		}, []string{"endpoint", "source", "node"}),
		prefillDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_prefill_duration_seconds",
			Help:    "Simulated prefill latency before the first response byte on cache replay.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint", "source"}),
		streamStallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_stream_stall_duration_seconds",
			Help:    "Simulated mid-stream stall durations on cache replay.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}, []string{"endpoint", "source"}),
		streamStallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_stream_stalls_total",
			Help: "Total simulated mid-stream stall events on cache replay.",
		}, []string{"endpoint", "source"}),
		dslDirectivesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_dsl_directives_total",
			Help: "Total replay-DSL directives parsed from prompts.",
		}, []string{"endpoint", "directive"}),
		dslRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_dsl_requests_total",
			Help: "Total chat completion requests by DSL directive family and terminal result. The family label is 'none' for requests without DSL directives.",
		}, []string{"endpoint", "family", "result"}),
		dslTimeToFirstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_dsl_time_to_first_byte_seconds",
			Help:    "Time to first byte by DSL directive family. The family label is 'none' for requests without DSL directives.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint", "family", "source"}),
		dslJobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_dsl_job_duration_seconds",
			Help:    "Job duration by DSL directive family. The family label is 'none' for requests without DSL directives.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 10, 30, 60},
		}, []string{"endpoint", "family", "source", "result"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_job_duration_seconds",
			Help:    "Time from job admission to completion, broken down by routed node.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 10, 30, 60},
		}, []string{"endpoint", "source", "result", "node"}),
		downstreamRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_downstream_request_duration_seconds",
			Help:    "Time spent waiting on downstream chat completion requests.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"endpoint", "result"}),
		requestLifecycleEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_request_lifecycle_events_total",
			Help: "Total per-request lifecycle events (admitted, queued, started, completed, rejected). The outcome label is empty for events that do not carry a result or reason.",
		}, []string{"endpoint", "event", "outcome"}),
		tenantAdmissionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_tenant_admissions_total",
			Help: "Total chat completion requests admitted past both tenant rate limit and global cost budget, by tenant and workload class.",
		}, []string{"tenant", "class"}),
		tenantRejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_tenant_rejections_total",
			Help: "Total chat completion requests rejected at admission, by tenant, workload class, and reason (tenant_rate, over_capacity, kv_pressure, kv_oversize, class_queue_timeout, class_ttft_budget, node_concurrency).",
		}, []string{"tenant", "class", "reason"}),
		nodeAdmissionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_node_admissions_total",
			Help: "Total chat completion requests by per-node admission result (admitted, rejected). Emitted only when nodes.yaml is loaded.",
		}, []string{"node", "class", "result"}),
		nodeQueueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_node_queue_wait_seconds",
			Help:    "Per-node queue wait duration before admission. Emitted only when nodes.yaml is loaded.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"node", "class"}),
		phaseTransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_phase_transitions_total",
			Help: "Total cache-replay streams that crossed a phase-aware token-allocation boundary, by class and (from, to) phase pair.",
		}, []string{"class", "from", "to"}),
		phaseATokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_phase_a_tokens_total",
			Help: "Total tokens emitted under phase-A (responsiveness) pacing, by class. Phase-aware token allocation (item 13).",
		}, []string{"class"}),
		phaseBTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_phase_b_tokens_total",
			Help: "Total tokens emitted under phase-B (sustained) pacing, by class. Phase-aware token allocation (item 13).",
		}, []string{"class"}),
		phaseInitialTPSEffective: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cllm_class_initial_tps_effective",
			Help: "Most recently observed effective phase-A tokens-per-second for the class, after degradation. Phase-aware token allocation (item 13).",
		}, []string{"class"}),
		phaseSustainedTPSEffective: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cllm_class_sustained_tps_effective",
			Help: "Most recently observed effective phase-B tokens-per-second for the class, after degradation. Phase-aware token allocation (item 13).",
		}, []string{"class"}),
		phaseReclaimTokenSecondsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_class_reclaim_token_seconds_total",
			Help: "Throughput reclaimed by phase-aware token allocation, per class: sum over completed streams of (initial_tps - sustained_tps) * tokens_in_phase_b. Always >= 0.",
		}, []string{"class"}),
		prioritySkipsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_admission_priority_skips_total",
			Help: "Total times the admission queue promoted a waiter that was not at the FIFO head, attributed by routed node and workload class. Phase 14C priority-weighted dequeue (or priority-aging) caused the reorder.",
		}, []string{"node", "class"}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.httpInflightRequests,
		metrics.httpRequestsTotal,
		metrics.httpRequestDuration,
		metrics.httpResponseSizeBytes,
		metrics.jobsTotal,
		metrics.completionTokensTotal,
		metrics.cacheLookupsTotal,
		metrics.queueWaitDuration,
		metrics.timeToFirstByteDuration,
		metrics.prefillDuration,
		metrics.streamStallDuration,
		metrics.streamStallsTotal,
		metrics.dslDirectivesTotal,
		metrics.dslRequestsTotal,
		metrics.dslTimeToFirstByte,
		metrics.dslJobDuration,
		metrics.jobDuration,
		metrics.downstreamRequestDuration,
		metrics.requestLifecycleEvents,
		metrics.tenantAdmissionTotal,
		metrics.tenantRejectionsTotal,
		metrics.nodeAdmissionsTotal,
		metrics.nodeQueueWait,
		metrics.phaseTransitionsTotal,
		metrics.phaseATokensTotal,
		metrics.phaseBTokensTotal,
		metrics.phaseInitialTPSEffective,
		metrics.phaseSustainedTPSEffective,
		metrics.phaseReclaimTokenSecondsTotal,
		metrics.prioritySkipsTotal,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_tokens_in_flight",
			Help: "Current admitted token cost in flight.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.TokensInFlight)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_waiting_requests",
			Help: "Current number of requests waiting in the queue.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.WaitingRequests)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_max_tokens_in_flight",
			Help: "Configured maximum admitted token cost in flight.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.MaxTokensInFlight)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_max_waiting_requests",
			Help: "Configured maximum number of waiting requests.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.MaxWaitingRequests)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_tokens_per_second_configured",
			Help: "Configured cached replay tokens per second.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.MaxTokensPerSecond)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_tokens_per_second_effective",
			Help: "Effective cached replay tokens per second after degradation.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return stats.EffectiveTokensPerSecond
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_computed_degradation_percentage",
			Help: "Current computed replay degradation percentage.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return stats.ComputedDegradationPercentage
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_cache_entries",
			Help: "Current number of cache entries.",
		}, func() float64 {
			_, entries := handler.cacheStats()
			return float64(entries)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_cache_capacity",
			Help: "Configured cache capacity.",
		}, func() float64 {
			capacity, _ := handler.cacheStats()
			return float64(capacity)
		}),
		nodeFleetCollector{handler: handler},
	)

	return metrics
}

func (m *handlerMetrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *handlerMetrics) observeHTTPRequest(route, method string, statusCode int, duration time.Duration, bytes int) {
	status := strconv.Itoa(statusCode)
	durationRoute := requestDurationRouteLabel(route)
	m.httpRequestsTotal.WithLabelValues(route, method, status).Inc()
	m.httpRequestDuration.WithLabelValues(durationRoute, method, status).Observe(duration.Seconds())
	m.httpResponseSizeBytes.WithLabelValues(route, method, status).Observe(float64(bytes))
}

func (m *handlerMetrics) observeCacheLookup(endpoint, result string) {
	m.cacheLookupsTotal.WithLabelValues(endpoint, result).Inc()
}

func (m *handlerMetrics) observeCompletionTokens(endpoint, source, node string, tokenCount int) {
	if tokenCount < 1 {
		return
	}
	if node == "" {
		node = "unknown"
	}
	m.completionTokensTotal.WithLabelValues(endpoint, source, node).Add(float64(tokenCount))
}

func (m *handlerMetrics) observeQueueWait(endpoint, node string, duration time.Duration) {
	m.queueWaitDuration.WithLabelValues(endpoint, node).Observe(duration.Seconds())
}

func (m *handlerMetrics) observeTimeToFirstByte(endpoint, source, node string, duration time.Duration) {
	if node == "" {
		node = "unknown"
	}
	m.timeToFirstByteDuration.WithLabelValues(endpoint, source, node).Observe(duration.Seconds())
}

func (m *handlerMetrics) observePrefillDuration(endpoint, source string, duration time.Duration) {
	if m == nil || m.prefillDuration == nil || duration <= 0 {
		return
	}
	m.prefillDuration.WithLabelValues(endpoint, source).Observe(duration.Seconds())
}

func (m *handlerMetrics) observeStreamStall(endpoint, source string, duration time.Duration) {
	if m == nil || duration <= 0 {
		return
	}
	if m.streamStallDuration != nil {
		m.streamStallDuration.WithLabelValues(endpoint, source).Observe(duration.Seconds())
	}
	if m.streamStallsTotal != nil {
		m.streamStallsTotal.WithLabelValues(endpoint, source).Inc()
	}
}

func (m *handlerMetrics) observeDSLDirective(endpoint, directive string) {
	if m == nil || m.dslDirectivesTotal == nil || directive == "" {
		return
	}
	m.dslDirectivesTotal.WithLabelValues(endpoint, directive).Inc()
}

// observeDSLRequestResult increments the per-family request counter for a
// terminal request outcome. families is typically the result of dslFamilies
// and always contains at least one entry ("none" baseline when DSL is absent).
func (m *handlerMetrics) observeDSLRequestResult(endpoint string, families []string, result string) {
	if m == nil || m.dslRequestsTotal == nil || result == "" {
		return
	}
	if len(families) == 0 {
		families = []string{"none"}
	}
	for _, fam := range families {
		m.dslRequestsTotal.WithLabelValues(endpoint, fam, result).Inc()
	}
}

// observeDSLTimeToFirstByte records TTFT into the per-family histogram.
func (m *handlerMetrics) observeDSLTimeToFirstByte(endpoint, source string, families []string, duration time.Duration) {
	if m == nil || m.dslTimeToFirstByte == nil || duration <= 0 {
		return
	}
	if len(families) == 0 {
		families = []string{"none"}
	}
	for _, fam := range families {
		m.dslTimeToFirstByte.WithLabelValues(endpoint, fam, source).Observe(duration.Seconds())
	}
}

// observeDSLJobDuration records terminal job duration into the per-family
// histogram.
func (m *handlerMetrics) observeDSLJobDuration(endpoint, source, result string, families []string, duration time.Duration) {
	if m == nil || m.dslJobDuration == nil || duration <= 0 {
		return
	}
	if len(families) == 0 {
		families = []string{"none"}
	}
	for _, fam := range families {
		m.dslJobDuration.WithLabelValues(endpoint, fam, source, result).Observe(duration.Seconds())
	}
}

func (m *handlerMetrics) observeLifecycleEvent(endpoint, event, outcome string) {
	if m == nil || m.requestLifecycleEvents == nil {
		return
	}
	m.requestLifecycleEvents.WithLabelValues(endpoint, event, outcome).Inc()
}

func (m *handlerMetrics) observeAdmissionAccept(tenant, class, _ string) {
	if m == nil || m.tenantAdmissionTotal == nil {
		return
	}
	m.tenantAdmissionTotal.WithLabelValues(tenant, class).Inc()
}

func (m *handlerMetrics) observeAdmissionRejection(tenant, class, reason string) {
	if m == nil || m.tenantRejectionsTotal == nil {
		return
	}
	m.tenantRejectionsTotal.WithLabelValues(tenant, class, reason).Inc()
}

// observePhaseTransition increments the phase-transition counter. The
// `from` and `to` labels are constants today ("phase_a", "phase_b") but
// the counter is shaped to accept future phases without a metric
// migration.
func (m *handlerMetrics) observePhaseTransition(class, from, to string) {
	if m == nil || m.phaseTransitionsTotal == nil {
		return
	}
	m.phaseTransitionsTotal.WithLabelValues(class, from, to).Inc()
}

// observePhaseTokens adds tokens emitted in phase A and phase B.
// Either count may be 0 (e.g. a request that never crossed the
// boundary contributes phaseB == 0).
func (m *handlerMetrics) observePhaseTokens(class string, phaseA, phaseB int) {
	if m == nil {
		return
	}
	if phaseA > 0 && m.phaseATokensTotal != nil {
		m.phaseATokensTotal.WithLabelValues(class).Add(float64(phaseA))
	}
	if phaseB > 0 && m.phaseBTokensTotal != nil {
		m.phaseBTokensTotal.WithLabelValues(class).Add(float64(phaseB))
	}
}

// observePhaseEffectiveTPS records the effective phase-A and phase-B
// rates seen on the most recent completed stream for the class. Pass
// 0 to skip an axis (e.g. a request that never reached phase B).
func (m *handlerMetrics) observePhaseEffectiveTPS(class string, initialTPS, sustainedTPS float64) {
	if m == nil {
		return
	}
	if initialTPS > 0 && m.phaseInitialTPSEffective != nil {
		m.phaseInitialTPSEffective.WithLabelValues(class).Set(initialTPS)
	}
	if sustainedTPS > 0 && m.phaseSustainedTPSEffective != nil {
		m.phaseSustainedTPSEffective.WithLabelValues(class).Set(sustainedTPS)
	}
}

// observePhaseReclaim adds the reclaimed token-seconds for one
// completed stream. Caller is responsible for clamping non-positive
// reclaim (initial_tps <= sustained_tps) to zero so the counter never
// goes backwards.
func (m *handlerMetrics) observePhaseReclaim(class string, tokenSeconds float64) {
	if m == nil || m.phaseReclaimTokenSecondsTotal == nil {
		return
	}
	if tokenSeconds <= 0 {
		return
	}
	m.phaseReclaimTokenSecondsTotal.WithLabelValues(class).Add(tokenSeconds)
}

func (m *handlerMetrics) observeJob(endpoint, result, source, node string, duration time.Duration) {
	if node == "" {
		node = "unknown"
	}
	m.jobsTotal.WithLabelValues(endpoint, result, source, node).Inc()
	if duration > 0 {
		m.jobDuration.WithLabelValues(endpoint, source, result, node).Observe(duration.Seconds())
	}
}

func (m *handlerMetrics) observeDownstreamRequest(endpoint, _ string, result string, duration time.Duration) {
	m.downstreamRequestDuration.WithLabelValues(endpoint, result).Observe(duration.Seconds())
}

func requestDurationRouteLabel(route string) string {
	if route == chatCompletionsRouteLabel {
		return route
	}
	return "other"
}

func routeLabel(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	if r.URL.Path != "" {
		return r.URL.Path
	}
	return "unmatched"
}

type firstByteMetricsWriter struct {
	http.ResponseWriter
	once        bool
	onFirstByte func()
}

func newFirstByteMetricsWriter(w http.ResponseWriter, onFirstByte func()) *firstByteMetricsWriter {
	return &firstByteMetricsWriter{ResponseWriter: w, onFirstByte: onFirstByte}
}

func (w *firstByteMetricsWriter) Write(body []byte) (int, error) {
	if !w.once && len(body) > 0 {
		w.once = true
		if w.onFirstByte != nil {
			w.onFirstByte()
		}
	}
	return w.ResponseWriter.Write(body)
}

func (w *firstByteMetricsWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
