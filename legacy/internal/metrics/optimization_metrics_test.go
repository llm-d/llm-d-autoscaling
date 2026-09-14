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
	dto "github.com/prometheus/client_model/go"
)

func TestObserveOptimizationDuration(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Observe a successful optimization
	ObserveOptimizationDuration(0.15, "success")

	// Observe a failed optimization
	ObserveOptimizationDuration(2.5, "error")

	// Verify the histogram was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAOptimizationDurationSeconds {
			found = true
			// Should have 2 metrics (one per status label)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				h := m.GetHistogram()
				if h == nil {
					t.Error("Expected histogram metric")
					continue
				}
				if h.GetSampleCount() != 1 {
					t.Errorf("Expected 1 sample per status, got %d", h.GetSampleCount())
				}
				// Check status label
				status := getLabelValue(m, constants.LabelStatus)
				switch status {
				case "success":
					if h.GetSampleSum() < 0.1 || h.GetSampleSum() > 0.2 {
						t.Errorf("Expected success duration ~0.15, got %f", h.GetSampleSum())
					}
				case "error":
					if h.GetSampleSum() < 2.0 || h.GetSampleSum() > 3.0 {
						t.Errorf("Expected error duration ~2.5, got %f", h.GetSampleSum())
					}
				default:
					t.Errorf("Unexpected status label: %s", status)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAOptimizationDurationSeconds)
	}
}

func TestSetModelsProcessed(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set models processed (gauge should reflect the last value, not a sum)
	SetModelsProcessed(3)
	SetModelsProcessed(5)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAModelsProcessed {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			g := mf.GetMetric()[0].GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 5 {
				t.Errorf("Expected gauge value 5 (last set), got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAModelsProcessed)
	}
}

func TestOptimizationMetrics_NilSafety(t *testing.T) {
	// Reset the package-level vars to nil to simulate uninitialized state
	savedDuration := optimizationDuration
	savedGauge := modelsProcessedGauge
	optimizationDuration = nil
	modelsProcessedGauge = nil
	defer func() {
		optimizationDuration = savedDuration
		modelsProcessedGauge = savedGauge
	}()

	// Should not panic when metrics are not initialized
	ObserveOptimizationDuration(1.0, "success")
	SetModelsProcessed(5)
}

// Shared test constants
const (
	testControllerInstance = "controller-1"
)

// getLabelValue returns the value of a label by name from a metric.
func getLabelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
