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
	trueStr  = "true"
	falseStr = "false"
)

func TestSetConfigInfo(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set config info with different values
	SetConfigInfo("saturation_analyzer_v2", true, false)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAConfigInfo {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 1 {
				t.Errorf("Expected gauge value 1 (info-style metric), got %f", g.GetValue())
			}

			// Verify labels
			analyzerName := getLabelValue(m, constants.LabelAnalyzerName)
			if analyzerName != "saturation_analyzer_v2" {
				t.Errorf("Expected analyzer_name 'saturation_analyzer_v2', got '%s'", analyzerName)
			}

			limiterEnabled := getLabelValue(m, constants.LabelLimiterEnabled)
			if limiterEnabled != trueStr {
				t.Errorf("Expected limiter_enabled 'true', got '%s'", limiterEnabled)
			}

			scaleToZeroEnabled := getLabelValue(m, constants.LabelScaleToZeroEnabled)
			if scaleToZeroEnabled != falseStr {
				t.Errorf("Expected scale_to_zero_enabled 'false', got '%s'", scaleToZeroEnabled)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAConfigInfo)
	}
}

func TestSetConfigInfo_OnlyLatestConfigPresent(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set multiple different configurations
	// Due to Reset(), only the last one should be present
	SetConfigInfo("saturation_analyzer_v1", false, false)
	SetConfigInfo("saturation_analyzer_v2", true, true)

	// Verify only the last configuration is present
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAConfigInfo {
			found = true
			// Should have exactly 1 metric (the last configuration set)
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected exactly 1 metric series (current config only), got %d", len(mf.GetMetric()))
			}

			m := mf.GetMetric()[0]
			g := m.GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 1 {
				t.Errorf("Expected gauge value 1, got %f", g.GetValue())
			}

			// Verify it's the last configuration that was set
			analyzerName := getLabelValue(m, constants.LabelAnalyzerName)
			if analyzerName != "saturation_analyzer_v2" {
				t.Errorf("Expected analyzer_name 'saturation_analyzer_v2' (last set), got '%s'", analyzerName)
			}
			if getLabelValue(m, constants.LabelLimiterEnabled) != trueStr {
				t.Error("Expected limiter_enabled 'true'")
			}
			if getLabelValue(m, constants.LabelScaleToZeroEnabled) != trueStr {
				t.Error("Expected scale_to_zero_enabled 'true'")
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAConfigInfo)
	}
}

func TestSetConfigKvSpareThreshold(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set KV spare threshold (gauge should reflect the last value)
	SetConfigKvSpareThreshold(0.05)
	SetConfigKvSpareThreshold(0.10)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAConfigKvSpareThreshold {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			g := mf.GetMetric()[0].GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 0.10 {
				t.Errorf("Expected gauge value 0.10 (last set), got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAConfigKvSpareThreshold)
	}
}

func TestSetConfigQueueSpareThreshold(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set queue spare threshold (gauge should reflect the last value)
	SetConfigQueueSpareThreshold(2.0)
	SetConfigQueueSpareThreshold(5.0)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAConfigQueueSpareThreshold {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			g := mf.GetMetric()[0].GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 5.0 {
				t.Errorf("Expected gauge value 5.0 (last set), got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAConfigQueueSpareThreshold)
	}
}

func TestSetConfigOptimizationInterval(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set optimization interval (gauge should reflect the last value)
	SetConfigOptimizationInterval(30.0)
	SetConfigOptimizationInterval(60.0)

	// Verify the gauge
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAConfigOptimizationIntervalSeconds {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			g := mf.GetMetric()[0].GetGauge()
			if g == nil {
				t.Error("Expected gauge metric")
			} else if g.GetValue() != 60.0 {
				t.Errorf("Expected gauge value 60.0 (last set), got %f", g.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAConfigOptimizationIntervalSeconds)
	}
}

func TestConfigStateMetrics_NilSafety(t *testing.T) {
	// Save the package-level vars and set to nil to simulate uninitialized state
	savedConfigInfo := configInfoGauge
	savedKvThreshold := configKvSpareThresholdGauge
	savedQueueThreshold := configQueueSpareThresholdGauge
	savedInterval := configOptimizationIntervalSecsGauge

	configInfoGauge = nil
	configKvSpareThresholdGauge = nil
	configQueueSpareThresholdGauge = nil
	configOptimizationIntervalSecsGauge = nil

	defer func() {
		configInfoGauge = savedConfigInfo
		configKvSpareThresholdGauge = savedKvThreshold
		configQueueSpareThresholdGauge = savedQueueThreshold
		configOptimizationIntervalSecsGauge = savedInterval
	}()

	// Should not panic when metrics are not initialized
	SetConfigInfo("test_analyzer", true, false)
	SetConfigKvSpareThreshold(0.05)
	SetConfigQueueSpareThreshold(2.0)
	SetConfigOptimizationInterval(30.0)
}

func TestConfigStateMetrics_AllMetricsRegistered(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	// Set all config metrics
	SetConfigInfo("saturation_analyzer_v2", true, true)
	SetConfigKvSpareThreshold(0.10)
	SetConfigQueueSpareThreshold(5.0)
	SetConfigOptimizationInterval(60.0)

	// Gather all metrics
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Check that all 4 config metrics are present
	expectedMetrics := map[string]bool{
		constants.WVAConfigInfo:                        false,
		constants.WVAConfigKvSpareThreshold:            false,
		constants.WVAConfigQueueSpareThreshold:         false,
		constants.WVAConfigOptimizationIntervalSeconds: false,
	}

	for _, mf := range metrics {
		if _, exists := expectedMetrics[mf.GetName()]; exists {
			expectedMetrics[mf.GetName()] = true
		}
	}

	for metricName, found := range expectedMetrics {
		if !found {
			t.Errorf("Expected metric %s to be registered and set, but it was not found", metricName)
		}
	}
}
