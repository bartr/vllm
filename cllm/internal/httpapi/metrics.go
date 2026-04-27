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
			Help: "Total number of cllm jobs by endpoint and result.",
		}, []string{"endpoint", "result", "source"}),
		completionTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_completion_tokens_total",
			Help: "Total number of completion tokens returned by cllm responses.",
		}, []string{"endpoint", "source"}),
		cacheLookupsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_cache_lookups_total",
			Help: "Total number of cache lookups by endpoint and result.",
		}, []string{"endpoint", "result"}),
		queueWaitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_queue_wait_duration_seconds",
			Help:    "Time jobs spend waiting in the request queue before admission.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint"}),
		timeToFirstByteDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_time_to_first_byte_seconds",
			Help:    "Time from job admission to the first response byte written to the client.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint", "source"}),
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
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"endpoint", "family", "source", "result"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cllm_job_duration_seconds",
			Help:    "Time from job admission to completion.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"endpoint", "source", "result"}),
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
			Help: "Total chat completion requests admitted past both tenant rate limit and global cost budget, by tenant.",
		}, []string{"tenant"}),
		tenantRejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cllm_tenant_rejections_total",
			Help: "Total chat completion requests rejected at admission, by tenant and reason (tenant_rate, over_capacity).",
		}, []string{"tenant", "reason"}),
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

func (m *handlerMetrics) observeCompletionTokens(endpoint, source string, tokenCount int) {
	if tokenCount < 1 {
		return
	}
	m.completionTokensTotal.WithLabelValues(endpoint, source).Add(float64(tokenCount))
}

func (m *handlerMetrics) observeQueueWait(endpoint string, duration time.Duration) {
	m.queueWaitDuration.WithLabelValues(endpoint).Observe(duration.Seconds())
}

func (m *handlerMetrics) observeTimeToFirstByte(endpoint, source, _ string, duration time.Duration) {
	m.timeToFirstByteDuration.WithLabelValues(endpoint, source).Observe(duration.Seconds())
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

func (m *handlerMetrics) observeAdmissionAccept(tenant, _ string) {
	if m == nil || m.tenantAdmissionTotal == nil {
		return
	}
	m.tenantAdmissionTotal.WithLabelValues(tenant).Inc()
}

func (m *handlerMetrics) observeAdmissionRejection(tenant, reason string) {
	if m == nil || m.tenantRejectionsTotal == nil {
		return
	}
	m.tenantRejectionsTotal.WithLabelValues(tenant, reason).Inc()
}

func (m *handlerMetrics) observeJob(endpoint, result, source, _ string, duration time.Duration) {
	m.jobsTotal.WithLabelValues(endpoint, result, source).Inc()
	if duration > 0 {
		m.jobDuration.WithLabelValues(endpoint, source, result).Observe(duration.Seconds())
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
