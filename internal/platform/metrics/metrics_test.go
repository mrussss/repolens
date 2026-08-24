package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"repolens/internal/platform/metrics"
)

func getCounterValue(counter prometheus.Counter) float64 {
	var m dto.Metric
	_ = counter.Write(&m)
	return m.GetCounter().GetValue()
}

func getGaugeValue(gauge prometheus.Gauge) float64 {
	var m dto.Metric
	_ = gauge.Write(&m)
	return m.GetGauge().GetValue()
}

func TestPrometheusMetricsWired(t *testing.T) {
	// 1. Test HttpRequestsTotal
	beforeHTTP := getCounterValue(metrics.HttpRequestsTotal.WithLabelValues("POST", "/diagnoses", "202"))
	metrics.HttpRequestsTotal.WithLabelValues("POST", "/diagnoses", "202").Inc()
	afterHTTP := getCounterValue(metrics.HttpRequestsTotal.WithLabelValues("POST", "/diagnoses", "202"))
	if afterHTTP != beforeHTTP+1 {
		t.Errorf("expected HttpRequestsTotal incremented by 1, got %f -> %f", beforeHTTP, afterHTTP)
	}

	// 2. Test DiagnosisTotal
	beforeDiag := getCounterValue(metrics.DiagnosisTotal)
	metrics.DiagnosisTotal.Inc()
	afterDiag := getCounterValue(metrics.DiagnosisTotal)
	if afterDiag != beforeDiag+1 {
		t.Errorf("expected DiagnosisTotal incremented by 1, got %f -> %f", beforeDiag, afterDiag)
	}

	// 3. Test WorkerInflight Gauge
	beforeInflight := getGaugeValue(metrics.WorkerInflight)
	metrics.WorkerInflight.Inc()
	if getGaugeValue(metrics.WorkerInflight) != beforeInflight+1 {
		t.Errorf("expected WorkerInflight +1")
	}
	metrics.WorkerInflight.Dec()
	if getGaugeValue(metrics.WorkerInflight) != beforeInflight {
		t.Errorf("expected WorkerInflight decremented back")
	}

	// 4. Test TokenUsageTotal & ToolCallsTotal
	beforeTokens := getCounterValue(metrics.TokenUsageTotal.WithLabelValues("prompt"))
	metrics.TokenUsageTotal.WithLabelValues("prompt").Add(150)
	afterTokens := getCounterValue(metrics.TokenUsageTotal.WithLabelValues("prompt"))
	if afterTokens != beforeTokens+150 {
		t.Errorf("expected TokenUsageTotal +150, got %f -> %f", beforeTokens, afterTokens)
	}

	beforeTools := getCounterValue(metrics.ToolCallsTotal.WithLabelValues("search_code", "success"))
	metrics.ToolCallsTotal.WithLabelValues("search_code", "success").Inc()
	afterTools := getCounterValue(metrics.ToolCallsTotal.WithLabelValues("search_code", "success"))
	if afterTools != beforeTools+1 {
		t.Errorf("expected ToolCallsTotal +1")
	}

	// 5. Test Recovery, MQ, and Retry counters
	metrics.StaleAttemptRecoveredTotal.Inc()
	metrics.MQRedeliveryTotal.Inc()
	metrics.ApplicationRetryTotal.Inc()
	metrics.DiagnosisFailedTotal.WithLabelValues("TERMINAL_ERROR").Inc()
	metrics.RetrievalRequestsTotal.WithLabelValues("bm25").Inc()
	metrics.RetrievalLatencySeconds.WithLabelValues("bm25").Observe(0.045)
	metrics.DiagnosisLatencySeconds.Observe(1.25)
}
