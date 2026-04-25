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
	jobDuration               *prometheus.HistogramVec
	downstreamRequestDuration *prometheus.HistogramVec
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
		metrics.jobDuration,
		metrics.downstreamRequestDuration,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_active_requests",
			Help: "Current number of active admitted requests.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.ConcurrentRequests)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_waiting_requests",
			Help: "Current number of requests waiting in the queue.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.WaitingRequests)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cllm_queue_max_active_requests",
			Help: "Configured maximum number of active concurrent requests.",
		}, func() float64 {
			stats := handler.RequestProcessingStats()
			return float64(stats.MaxConcurrentRequests)
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
