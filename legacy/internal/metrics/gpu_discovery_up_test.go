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

func TestSetGpuDiscoveryUp_Enabled(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set GPU discovery to enabled (1)
	SetGpuDiscoveryUp(1)

	// Verify the gauge was set to 1
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAGpuDiscoveryUp {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 1 {
				t.Errorf("Expected gauge value 1, got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAGpuDiscoveryUp)
	}
}

func TestSetGpuDiscoveryUp_Disabled(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set GPU discovery to disabled (0)
	SetGpuDiscoveryUp(0)

	// Verify the gauge was set to 0
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAGpuDiscoveryUp {
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
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAGpuDiscoveryUp)
	}
}

func TestSetGpuDiscoveryUp_Update(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// First set to enabled (1)
	SetGpuDiscoveryUp(1)

	// Verify it's 1
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	for _, mf := range metrics {
		if mf.GetName() == constants.WVAGpuDiscoveryUp {
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g != nil && g.GetValue() != 1 {
				t.Errorf("Expected gauge value 1 after first set, got %f", g.GetValue())
			}
		}
	}

	// Update to disabled (0)
	SetGpuDiscoveryUp(0)

	// Verify it's now 0
	metrics, err = registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAGpuDiscoveryUp {
			found = true
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 0 {
				t.Errorf("Expected gauge value 0 after update, got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAGpuDiscoveryUp)
	}
}

func TestSetGpuDiscoveryUp_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedGpuDiscoveryUp := gpuDiscoveryUp
	defer func() {
		controllerInstance = savedInstance
		gpuDiscoveryUp = savedGpuDiscoveryUp
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	t.Setenv(ControllerInstanceEnvVar, testControllerInstance)

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set GPU discovery status
	SetGpuDiscoveryUp(1)

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAGpuDiscoveryUp {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != testControllerInstance {
				t.Errorf("Expected controller_instance=%s, got %s", testControllerInstance, instance)
			}
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 1 {
				t.Errorf("Expected gauge value 1, got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAGpuDiscoveryUp)
	}
}
