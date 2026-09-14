/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
)

func TestObserveMetricsCollectionDuration(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Observe durations for different query types
	ObserveMetricsCollectionDuration(0.05, "replica_metrics")
	ObserveMetricsCollectionDuration(0.02, "scheduler_queue")
	ObserveMetricsCollectionDuration(0.01, constants.QueryTypeRequestCount)

	// Verify the histogram was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAMetricsCollectionDurationSeconds {
			found = true
			// Should have 3 metrics (one per query_type label)
			if len(mf.GetMetric()) != 3 {
				t.Errorf("Expected 3 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				h := m.GetHistogram()
				if h == nil {
					t.Error("Expected histogram metric")
					continue
				}
				if h.GetSampleCount() != 1 {
					t.Errorf("Expected 1 sample per query_type, got %d", h.GetSampleCount())
				}
				// Check query_type label
				queryType := getLabelValue(m, constants.LabelQueryType)
				switch queryType {
				case "replica_metrics":
					if h.GetSampleSum() < 0.04 || h.GetSampleSum() > 0.06 {
						t.Errorf("Expected replica_metrics duration ~0.05, got %f", h.GetSampleSum())
					}
				case "scheduler_queue":
					if h.GetSampleSum() < 0.01 || h.GetSampleSum() > 0.03 {
						t.Errorf("Expected scheduler_queue duration ~0.02, got %f", h.GetSampleSum())
					}
				case constants.QueryTypeRequestCount:
					if h.GetSampleSum() < 0.005 || h.GetSampleSum() > 0.015 {
						t.Errorf("Expected request_count duration ~0.01, got %f", h.GetSampleSum())
					}
				default:
					t.Errorf("Unexpected query_type label: %s", queryType)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAMetricsCollectionDurationSeconds)
	}
}

func TestIncMetricsCollectionErrors(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Increment errors for different query types and reasons
	IncMetricsCollectionErrors("replica_metrics", "timeout")
	IncMetricsCollectionErrors("replica_metrics", "timeout")
	IncMetricsCollectionErrors(constants.QueryTypeRequestCount, "connection_refused")
	IncMetricsCollectionErrors("scheduler_queue", "parse_error")

	// Verify the counter was incremented
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAMetricsCollectionErrorsTotal {
			found = true
			// Should have 3 metrics (one per unique query_type + reason combination)
			if len(mf.GetMetric()) != 3 {
				t.Errorf("Expected 3 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				queryType := getLabelValue(m, constants.LabelQueryType)
				reason := getLabelValue(m, constants.LabelReason)

				// Verify counts
				switch {
				case queryType == "replica_metrics" && reason == "timeout":
					if c.GetValue() != 2 {
						t.Errorf("Expected count 2 for replica_metrics/timeout, got %f", c.GetValue())
					}
				case queryType == constants.QueryTypeRequestCount && reason == "connection_refused":
					if c.GetValue() != 1 {
						t.Errorf("Expected count 1 for request_count/connection_refused, got %f", c.GetValue())
					}
				case queryType == "scheduler_queue" && reason == "parse_error":
					if c.GetValue() != 1 {
						t.Errorf("Expected count 1 for scheduler_queue/parse_error, got %f", c.GetValue())
					}
				default:
					t.Errorf("Unexpected query_type/reason combination: %s/%s", queryType, reason)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAMetricsCollectionErrorsTotal)
	}
}

func TestSetMetricsPodsDiscovered(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set pods discovered for different namespaces
	SetMetricsPodsDiscovered("default", 5)
	SetMetricsPodsDiscovered("kube-system", 10)
	SetMetricsPodsDiscovered("default", 7) // Update value

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAMetricsPodsDiscovered {
			found = true
			// Should have 2 metrics (one per namespace)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				g := m.GetGauge()
				if g == nil {
					t.Error("Expected gauge metric")
					continue
				}
				namespace := getLabelValue(m, constants.LabelNamespace)
				switch namespace {
				case "default":
					if g.GetValue() != 7 {
						t.Errorf("Expected default namespace pods 7 (last set), got %f", g.GetValue())
					}
				case "kube-system":
					if g.GetValue() != 10 {
						t.Errorf("Expected kube-system namespace pods 10, got %f", g.GetValue())
					}
				default:
					t.Errorf("Unexpected namespace label: %s", namespace)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAMetricsPodsDiscovered)
	}
}

func TestSetMetricsFreshnessStatus(t *testing.T) {
	const testVariantLlama = "llama-3-8b"

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set freshness status for different variants and statuses
	SetMetricsFreshnessStatus(testVariantLlama, "fresh", 5)
	SetMetricsFreshnessStatus(testVariantLlama, "stale", 2)
	SetMetricsFreshnessStatus(testVariantLlama, "missing", 1)
	SetMetricsFreshnessStatus("mistral-7b", "fresh", 8)
	SetMetricsFreshnessStatus("mistral-7b", "unavailable", 1)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAMetricsFreshnessStatus {
			found = true
			// Should have 5 metrics (3 for llama + 2 for mistral)
			if len(mf.GetMetric()) != 5 {
				t.Errorf("Expected 5 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				g := m.GetGauge()
				if g == nil {
					t.Error("Expected gauge metric")
					continue
				}
				variantName := getLabelValue(m, constants.LabelVariantName)
				status := getLabelValue(m, constants.LabelStatus)

				// Verify counts
				switch {
				case variantName == testVariantLlama && status == "fresh":
					if g.GetValue() != 5 {
						t.Errorf("Expected count 5 for llama-3-8b/fresh, got %f", g.GetValue())
					}
				case variantName == testVariantLlama && status == "stale":
					if g.GetValue() != 2 {
						t.Errorf("Expected count 2 for llama-3-8b/stale, got %f", g.GetValue())
					}
				case variantName == testVariantLlama && status == "missing":
					if g.GetValue() != 1 {
						t.Errorf("Expected count 1 for llama-3-8b/missing, got %f", g.GetValue())
					}
				case variantName == "mistral-7b" && status == "fresh":
					if g.GetValue() != 8 {
						t.Errorf("Expected count 8 for mistral-7b/fresh, got %f", g.GetValue())
					}
				case variantName == "mistral-7b" && status == "unavailable":
					if g.GetValue() != 1 {
						t.Errorf("Expected count 1 for mistral-7b/unavailable, got %f", g.GetValue())
					}
				default:
					t.Errorf("Unexpected variant_name/status combination: %s/%s", variantName, status)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAMetricsFreshnessStatus)
	}
}

func TestCollectionMetrics_NilSafety(t *testing.T) {
	const testVariantLlama = "llama-3-8b"

	// Reset the package-level vars to nil to simulate uninitialized state
	savedDuration := metricsCollectionDuration
	savedErrors := metricsCollectionErrors
	savedPodsDiscovered := metricsPodsDiscovered
	savedFreshness := metricsFreshnessStatus

	metricsCollectionDuration = nil
	metricsCollectionErrors = nil
	metricsPodsDiscovered = nil
	metricsFreshnessStatus = nil

	defer func() {
		metricsCollectionDuration = savedDuration
		metricsCollectionErrors = savedErrors
		metricsPodsDiscovered = savedPodsDiscovered
		metricsFreshnessStatus = savedFreshness
	}()

	// Should not panic when metrics are not initialized
	ObserveMetricsCollectionDuration(0.5, "replica_metrics")
	IncMetricsCollectionErrors(constants.QueryTypeRequestCount, "timeout")
	SetMetricsPodsDiscovered("default", 10)
	SetMetricsFreshnessStatus(testVariantLlama, "fresh", 5)
}
