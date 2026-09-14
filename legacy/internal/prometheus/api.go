package prometheus

import (
	"context"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

func QueryPrometheusWithBackoff(ctx context.Context, promAPI promv1.API, query string) (val model.Value, warn promv1.Warnings, err error) {
	var lastErr error

	err = wait.ExponentialBackoffWithContext(ctx, constants.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		val, warn, err = promAPI.Query(ctx, query, time.Now())
		if err != nil {
			// Record the last error so that we can surface it if the backoff is exhausted.
			lastErr = err
			ctrl.Log.Info("Query Prometheus failed, retrying",
				"query", query,
				"error", err.Error())
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			return nil, nil, lastErr
		}
		return nil, nil, err
	}

	return
}

// ValidatePrometheusAPIWithBackoff validates Prometheus API connectivity with retry logic
func ValidatePrometheusAPIWithBackoff(ctx context.Context, promAPI promv1.API, backoff wait.Backoff) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Test with a simple query that should always work
		query := "up"
		_, _, err := promAPI.Query(ctx, query, time.Now())
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Prometheus API validation failed, retrying - ", "query: ", query)
			return false, nil // Retry on transient errors
		}

		ctrl.LoggerFrom(ctx).Info("Prometheus API validation successful with query", "query", query)
		return true, nil
	})
}

// ValidatePrometheusAPI validates Prometheus API connectivity using standard Prometheus backoff
func ValidatePrometheusAPI(ctx context.Context, promAPI promv1.API) error {
	return ValidatePrometheusAPIWithBackoff(ctx, promAPI, constants.PrometheusValidationBackoff)
}
