package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HttpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests processed",
		},
		[]string{"method", "path", "status"},
	)

	DiagnosisTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "diagnosis_total",
			Help: "Total number of diagnosis runs initiated",
		},
	)

	DiagnosisFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "diagnosis_failed_total",
			Help: "Total number of failed diagnosis runs",
		},
		[]string{"error_type"},
	)

	DiagnosisLatencySeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "diagnosis_latency_seconds",
			Help:    "Diagnosis end-to-end processing latency",
			Buckets: prometheus.DefBuckets,
		},
	)

	WorkerInflight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "worker_inflight",
			Help: "Number of diagnosis tasks currently being executed by workers",
		},
	)

	MQRedeliveryTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mq_redelivery_total",
			Help: "Total number of redelivered transport messages observed",
		},
	)

	ApplicationRetryTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "application_retry_total",
			Help: "Total number of application retries scheduled",
		},
	)

	StaleAttemptRecoveredTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "stale_attempt_recovered_total",
			Help: "Total number of stale attempts recovered from crashed workers",
		},
	)

	RetrievalRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "retrieval_requests_total",
			Help: "Total number of code retrieval requests",
		},
		[]string{"strategy"},
	)

	RetrievalLatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "retrieval_latency_seconds",
			Help:    "Retrieval request latency",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"strategy"},
	)

	ToolCallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tool_calls_total",
			Help: "Total number of agent tool calls executed",
		},
		[]string{"tool_name", "status"},
	)

	TokenUsageTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "token_usage_total",
			Help: "Total tokens consumed by LLM interactions",
		},
		[]string{"type"}, // "prompt", "completion"
	)
)
