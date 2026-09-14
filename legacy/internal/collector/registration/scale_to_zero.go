// Package registration provides query registration functionality for metrics sources.
//
// This file provides scale-to-zero metrics collection using the source
// infrastructure with registered query templates.
package registration

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
)

// Query name constants for scale-to-zero metrics.
const (
	// QueryModelRequestCount is the query name for total model requests over a time window.
	QueryModelRequestCount = "model_request_count"

	// ParamRetentionPeriod is the parameter name for the retention period duration.
	ParamRetentionPeriod = "retentionPeriod"
)

// RegisterScaleToZeroQueries registers queries used for scale-to-zero decisions.
// This should be called during initialization to register query templates with the prometheus source.
func RegisterScaleToZeroQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping scale-to-zero query registration")
		return
	}

	registry := metricsSource.QueryList()

	// Model request count over a retention period
	// Uses sum(increase(...)) to get total requests over the time window
	// The retentionPeriod parameter should be in Prometheus duration format (e.g., "10m", "1h")
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryModelRequestCount,
		Type:        source.QueryTypePromQL,
		Template:    `sum(increase(vllm:request_success_total{namespace="{{.namespace}}",model_name="{{.modelID}}"}[{{.retentionPeriod}}]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID, ParamRetentionPeriod},
		Description: "Total successful requests for a model over the retention period",
	})

	// SGLang variant: sglang:num_requests_total is the received-request counter.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryModelRequestCount,
		Type:        source.QueryTypePromQL,
		Template:    `sum(increase(sglang:num_requests_total{namespace="{{.namespace}}",model_name="{{.modelID}}"}[{{.retentionPeriod}}]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID, ParamRetentionPeriod},
		Description: "Total requests for a model over the retention period (SGLang)",
	})
}

// CollectModelRequestCount collects the total number of successful requests for a model
// over the specified retention period. This is used for scale-to-zero decisions.
//
// The function returns an error when it cannot determine the request count with certainty.
// This is important for scale-to-zero safety: we should only scale to zero when we have
// positive confirmation that no requests were made. If we can't determine the count,
// the enforcer will keep current replicas (preventing premature scale-to-zero).
//
// Parameters:
//   - ctx: Context for the operation
//   - source: The metrics source to query
//   - modelID: The model identifier
//   - namespace: The namespace where the model is deployed
//   - retentionPeriod: How far back to look for requests
//
// Returns:
//   - float64: Total request count over the retention period
//   - error: Error if the request count cannot be determined (query failed, no data, etc.)
func CollectModelRequestCount(
	ctx context.Context,
	metricsSource source.MetricsSource,
	modelID string,
	namespace string,
	retentionPeriod time.Duration,
) (float64, error) {
	// Defaults to vLLM for backward compatibility: this queries
	// vllm:request_success_total regardless of the model's actual engine. Engine-
	// aware scale-to-zero is available via CollectModelRequestCountForEngine but is
	// not yet threaded through the enforcer (see docs/proposals/sglang-backend.md
	// Phase 2). Until it is, callers MUST NOT invoke scale-to-zero for non-vLLM
	// models — the saturation engine gates this via scaleToZeroSupportedForEngines,
	// since for SGLang this function would always return 0 (no vllm:* series) and
	// the enforcer would incorrectly scale the model to zero.
	return CollectModelRequestCountForEngine(ctx, metricsSource, inferenceengine.EngineVLLM, modelID, namespace, retentionPeriod)
}

// CollectModelRequestCountForEngine is the engine-aware variant of
// CollectModelRequestCount. It selects the request-count metric appropriate for
// the given inference engine (vllm:request_success_total vs sglang:num_requests_total).
func CollectModelRequestCountForEngine(
	ctx context.Context,
	metricsSource source.MetricsSource,
	engine inferenceengine.Engine,
	modelID string,
	namespace string,
	retentionPeriod time.Duration,
) (float64, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Convert Go duration to Prometheus duration format
	retentionPeriodStr := prometheus.FormatPrometheusDuration(retentionPeriod)

	params := map[string]string{
		source.ParamModelID:   modelID,
		source.ParamNamespace: namespace,
		ParamRetentionPeriod:  retentionPeriodStr,
	}

	queryName := EngineQuery(engine, QueryModelRequestCount)

	// Execute the query with timing
	startTime := time.Now()
	results, err := metricsSource.Refresh(ctx, source.RefreshSpec{
		Queries: []string{queryName},
		Params:  params,
	})
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeRequestCount)

	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeRequestCount, reason)
		logger.V(logging.VERBOSE).Info("Failed to query model request count",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr,
			"error", err)
		return 0, fmt.Errorf("failed to query request count for model %s: %w", modelID, err)
	}

	// Extract the result
	result := results[queryName]
	if result == nil {
		logger.V(logging.VERBOSE).Info("No result for model request count query",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr)
		return 0, fmt.Errorf("no result for request count query for model %s (metrics may not be available yet)", modelID)
	}

	if result.HasError() {
		logger.V(logging.VERBOSE).Info("Model request count query failed",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr,
			"error", result.Error)
		return 0, fmt.Errorf("request count query failed for model %s: %v", modelID, result.Error)
	}

	// Get the first value (sum query returns a single scalar)
	if len(result.Values) == 0 {
		logger.V(logging.DEBUG).Info("No values in model request count result",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr)
		return 0, fmt.Errorf("no values in request count result for model %s (metrics may not be scraped yet)", modelID)
	}

	count := result.FirstValue().Value

	logger.V(logging.DEBUG).Info("Collected model request count",
		"model", modelID,
		"namespace", namespace,
		"retentionPeriod", retentionPeriodStr,
		"count", count)

	return count, nil
}
