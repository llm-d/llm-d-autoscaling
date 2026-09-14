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
	testVariantName  = "variant-a"
	testNamespace    = "test-namespace"
	testLimiterName  = "test-limiter"
	testVariantName2 = "variant-b"
)

func TestRecordDecisionsLimitedTotalMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit decisions limited metrics for different combinations
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName, testNamespace, testLimiterName)
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName2, testNamespace, testLimiterName)

	// Verify the counter was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVADecisionsLimitedTotal {
			found = true
			// Should have 2 metric series (one per variant)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				// Check labels
				variantName := getLabelValue(m, constants.LabelVariantName)
				namespace := getLabelValue(m, constants.LabelNamespace)
				limiterName := getLabelValue(m, constants.LabelLimiterName)

				if namespace != testNamespace {
					t.Errorf("Expected namespace=%s, got %s", testNamespace, namespace)
				}
				if limiterName != testLimiterName {
					t.Errorf("Expected limiter_name=%s, got %s", testLimiterName, limiterName)
				}

				switch variantName {
				case testVariantName:
					if c.GetValue() != 1 {
						t.Errorf("Expected counter value 1 for %s, got %f", testVariantName, c.GetValue())
					}
				case testVariantName2:
					if c.GetValue() != 1 {
						t.Errorf("Expected counter value 1 for %s, got %f", testVariantName2, c.GetValue())
					}
				default:
					t.Errorf("Unexpected variant_name label: %s", variantName)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVADecisionsLimitedTotal)
	}
}

func TestRecordDecisionsLimitedTotalMetric_Increment(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit the same metric multiple times (counter should increment)
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName, testNamespace, testLimiterName)
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName, testNamespace, testLimiterName)
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName, testNamespace, testLimiterName)

	// Verify the counter incremented
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVADecisionsLimitedTotal {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			c := m.GetCounter()
			if c == nil {
				t.Error("Expected counter metric")
			} else if c.GetValue() != 3 {
				t.Errorf("Expected counter value 3, got %f", c.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVADecisionsLimitedTotal)
	}
}

func TestRecordDecisionsLimitedTotalMetric_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedDecisionsLimitedTotal := decisionsLimitedTotal
	defer func() {
		controllerInstance = savedInstance
		decisionsLimitedTotal = savedDecisionsLimitedTotal
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	t.Setenv(ControllerInstanceEnvVar, testControllerInstance)

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit decisions limited metric
	emitter.RecordDecisionsLimitedTotalMetric(testVariantName, testNamespace, testLimiterName)

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVADecisionsLimitedTotal {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != testControllerInstance {
				t.Errorf("Expected controller_instance=%s, got %s", testControllerInstance, instance)
			}
			variantName := getLabelValue(m, constants.LabelVariantName)
			if variantName != testVariantName {
				t.Errorf("Expected variant_name=%s, got %s", testVariantName, variantName)
			}
			namespace := getLabelValue(m, constants.LabelNamespace)
			if namespace != testNamespace {
				t.Errorf("Expected namespace=%s, got %s", testNamespace, namespace)
			}
			limiterName := getLabelValue(m, constants.LabelLimiterName)
			if limiterName != testLimiterName {
				t.Errorf("Expected limiter_name=%s, got %s", testLimiterName, limiterName)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVADecisionsLimitedTotal)
	}
}

func TestRecordDecisionsLimitedTotalMetric_MultipleLimiters(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Test various combinations of variant, namespace, and limiter
	testCases := []struct {
		variantName string
		namespace   string
		limiterName string
		count       int
	}{
		{testVariantName, testNamespace, "gpu-limiter", 2},
		{testVariantName2, testNamespace, "gpu-limiter", 1},
		{testVariantName, "other-namespace", "cost-limiter", 3},
	}

	for _, tc := range testCases {
		for i := 0; i < tc.count; i++ {
			emitter.RecordDecisionsLimitedTotalMetric(tc.variantName, tc.namespace, tc.limiterName)
		}
	}

	// Verify all combinations were recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	foundCombinations := make(map[string]float64)
	for _, mf := range metrics {
		if mf.GetName() == constants.WVADecisionsLimitedTotal {
			found = true
			if len(mf.GetMetric()) != len(testCases) {
				t.Errorf("Expected %d metric series, got %d", len(testCases), len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				variantName := getLabelValue(m, constants.LabelVariantName)
				namespace := getLabelValue(m, constants.LabelNamespace)
				limiterName := getLabelValue(m, constants.LabelLimiterName)
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				key := variantName + ":" + namespace + ":" + limiterName
				foundCombinations[key] = c.GetValue()
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVADecisionsLimitedTotal)
	}

	// Verify all expected combinations were found with correct counts
	for _, tc := range testCases {
		key := tc.variantName + ":" + tc.namespace + ":" + tc.limiterName
		if count, ok := foundCombinations[key]; !ok {
			t.Errorf("Expected combination %s not found in metrics", key)
		} else if int(count) != tc.count {
			t.Errorf("Expected count %d for combination %s, got %f", tc.count, key, count)
		}
	}
}
