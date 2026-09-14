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

func TestRecordOptimizerActiveMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Set cost-aware optimizer as active
	emitter.RecordOptimizerActiveMetric("cost-aware", true)

	// Set greedy-by-score optimizer as inactive
	emitter.RecordOptimizerActiveMetric("greedy-by-score", false)

	// Verify the gauge was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAOptimizerActive {
			found = true
			// Should have 2 metrics (one per optimizer_name label)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				g := m.GetGauge()
				if g == nil {
					t.Error("Expected gauge metric")
					continue
				}
				// Check optimizer_name label
				optimizerName := getLabelValue(m, constants.LabelOptimizerName)
				switch optimizerName {
				case "cost-aware":
					if g.GetValue() != 1 {
						t.Errorf("Expected cost-aware optimizer to be active (1), got %f", g.GetValue())
					}
				case "greedy-by-score":
					if g.GetValue() != 0 {
						t.Errorf("Expected greedy-by-score optimizer to be inactive (0), got %f", g.GetValue())
					}
				default:
					t.Errorf("Unexpected optimizer_name label: %s", optimizerName)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAOptimizerActive)
	}
}

func TestRecordOptimizerActiveMetric_Toggle(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// First set optimizer as active
	emitter.RecordOptimizerActiveMetric("cost-aware", true)

	// Then toggle it to inactive
	emitter.RecordOptimizerActiveMetric("cost-aware", false)

	// Verify the gauge reflects the latest value (inactive)
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAOptimizerActive {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 0 {
				t.Errorf("Expected gauge value 0 (inactive), got %f", g.GetValue())
			}
			optimizerName := getLabelValue(m, constants.LabelOptimizerName)
			if optimizerName != "cost-aware" {
				t.Errorf("Expected optimizer_name=cost-aware, got %s", optimizerName)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAOptimizerActive)
	}
}

func TestRecordOptimizerActiveMetric_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedOptimizerActive := optimizerActive
	defer func() {
		controllerInstance = savedInstance
		optimizerActive = savedOptimizerActive
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	// InitMetrics reads from os.Getenv(ControllerInstanceEnvVar)
	t.Setenv(ControllerInstanceEnvVar, testControllerInstance)

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit optimizer active metric
	emitter.RecordOptimizerActiveMetric("cost-aware", true)

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAOptimizerActive {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != testControllerInstance {
				t.Errorf("Expected controller_instance=%s, got %s", testControllerInstance, instance)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAOptimizerActive)
	}
}
