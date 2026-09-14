package testutil

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// GetErrorMetricValue retrieves the value of the wva_errors_total metric
// for the specified component and error type from the given registry.
// Returns 0.0 if the metric is not found.
func GetErrorMetricValue(registry *prometheus.Registry, component, errorType string) float64 {
	metricFamilies, err := registry.Gather()
	if err != nil {
		return 0.0
	}

	for _, mf := range metricFamilies {
		if mf.GetName() != constants.WVAErrorsTotal {
			continue
		}

		for _, metric := range mf.GetMetric() {
			if matchesLabels(metric, component, errorType) {
				return metric.GetCounter().GetValue()
			}
		}
	}

	return 0.0
}

// matchesLabels checks if a metric has the expected component and error_type labels.
// Both labels must match for the function to return true.
func matchesLabels(metric *dto.Metric, component, errorType string) bool {
	componentMatch := false
	errorTypeMatch := false

	for _, label := range metric.GetLabel() {
		switch label.GetName() {
		case constants.LabelComponent:
			componentMatch = label.GetValue() == component
		case constants.LabelErrorType:
			errorTypeMatch = label.GetValue() == errorType
		}
	}

	// Both labels must match
	return componentMatch && errorTypeMatch
}
