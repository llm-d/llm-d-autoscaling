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

const (
	testAcceleratorTypeA100 = "A100"
	testAcceleratorTypeH100 = "H100"
)

func TestRecordAvailableGPUsMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit available GPU metrics for different accelerator types
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-A100-PCIE-80GB", testAcceleratorTypeA100, 8)
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-H100-SXM5-80GB", testAcceleratorTypeH100, 4)

	// Verify the gauge was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAAvailableGpus {
			found = true
			// Should have 2 metric series (one per accelerator type)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				g := m.GetGauge()
				if g == nil {
					t.Error("Expected gauge metric")
					continue
				}
				// Check accelerator_type label
				acceleratorType := getLabelValue(m, constants.LabelAcceleratorType)
				switch acceleratorType {
				case testAcceleratorTypeA100:
					if g.GetValue() != 8 {
						t.Errorf("Expected A100 count to be 8, got %f", g.GetValue())
					}
				case testAcceleratorTypeH100:
					if g.GetValue() != 4 {
						t.Errorf("Expected H100 count to be 4, got %f", g.GetValue())
					}
				default:
					t.Errorf("Unexpected accelerator_type label: %s", acceleratorType)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAAvailableGpus)
	}
}

func TestRecordAvailableGPUsMetric_Update(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// First set A100 count to 8
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-A100-PCIE-80GB", testAcceleratorTypeA100, 8)

	// Then update A100 count to 5
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-A100-PCIE-80GB", testAcceleratorTypeA100, 5)

	// Verify the gauge reflects the latest value
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAAvailableGpus {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 5 {
				t.Errorf("Expected gauge value 5, got %f", g.GetValue())
			}
			acceleratorType := getLabelValue(m, constants.LabelAcceleratorType)
			if acceleratorType != testAcceleratorTypeA100 {
				t.Errorf("Expected accelerator_type=%s, got %s", testAcceleratorTypeA100, acceleratorType)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAAvailableGpus)
	}
}

func TestRecordAvailableGPUsMetric_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedAvailableGpus := availableGpus
	defer func() {
		controllerInstance = savedInstance
		availableGpus = savedAvailableGpus
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	t.Setenv(ControllerInstanceEnvVar, testControllerInstance)

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit available GPU metric
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-A100-PCIE-80GB", testAcceleratorTypeA100, 8)

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAAvailableGpus {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != testControllerInstance {
				t.Errorf("Expected controller_instance=%s, got %s", testControllerInstance, instance)
			}
			acceleratorType := getLabelValue(m, constants.LabelAcceleratorType)
			if acceleratorType != testAcceleratorTypeA100 {
				t.Errorf("Expected accelerator_type=%s, got %s", testAcceleratorTypeA100, acceleratorType)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAAvailableGpus)
	}
}

func TestRecordAvailableGPUsMetric_MultipleAcceleratorTypes(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Test various accelerator types
	testCases := []struct {
		vendor          string
		model           string
		acceleratorType string
		count           int32
	}{
		{"nvidia.com", "NVIDIA-A100-PCIE-80GB", testAcceleratorTypeA100, 8},
		{"nvidia.com", "NVIDIA-H100-SXM5-80GB", testAcceleratorTypeH100, 4},
		{"nvidia.com", "NVIDIA-V100-PCIE-16GB", "V100", 16},
		{"nvidia.com", "NVIDIA-T4-16GB", "T4", 2},
	}

	for _, tc := range testCases {
		emitter.RecordAvailableGPUsMetric(tc.vendor, tc.model, tc.acceleratorType, tc.count)
	}

	// Verify all accelerator types were recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	foundAcceleratorTypes := make(map[string]int32)
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAAvailableGpus {
			found = true
			if len(mf.GetMetric()) != len(testCases) {
				t.Errorf("Expected %d metric series, got %d", len(testCases), len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				acceleratorType := getLabelValue(m, constants.LabelAcceleratorType)
				g := m.GetGauge()
				if g == nil {
					t.Error("Expected gauge metric")
					continue
				}
				foundAcceleratorTypes[acceleratorType] = int32(g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAAvailableGpus)
	}

	// Verify all expected accelerator types were found with correct counts
	for _, tc := range testCases {
		if count, ok := foundAcceleratorTypes[tc.acceleratorType]; !ok {
			t.Errorf("Expected accelerator type %s not found in metrics", tc.acceleratorType)
		} else if count != tc.count {
			t.Errorf("Expected count %d for accelerator type %s, got %d", tc.count, tc.acceleratorType, count)
		}
	}
}

func TestRecordAvailableGPUsMetric_ZeroCount(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit zero count (all GPUs in use)
	emitter.RecordAvailableGPUsMetric("nvidia.com", "NVIDIA-A100-PCIE-80GB", "A100", 0)

	// Verify the gauge shows 0
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAAvailableGpus {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 0 {
				t.Errorf("Expected gauge value 0, got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAAvailableGpus)
	}
}
