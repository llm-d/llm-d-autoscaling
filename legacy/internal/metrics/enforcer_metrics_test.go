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

func TestRecordEnforcerMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit enforcer metric for scale_to_zero policy
	emitter.RecordEnforcerMetric(constants.EnforcerPolicyTypeScaleToZero)

	// Emit enforcer metric for minimum_replicas policy
	emitter.RecordEnforcerMetric(constants.EnforcerPolicyTypeMinimumReplicas)

	// Emit multiple times for the same policy (counter should increment)
	emitter.RecordEnforcerMetric(constants.EnforcerPolicyTypeScaleToZero)

	// Verify the counter was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			// Should have 2 metric series (one per policy_type)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				// Check policy_type label
				policyType := getLabelValue(m, constants.LabelPolicyType)
				switch policyType {
				case constants.EnforcerPolicyTypeScaleToZero:
					if c.GetValue() != 2 {
						t.Errorf("Expected %s counter to be 2, got %f", constants.EnforcerPolicyTypeScaleToZero, c.GetValue())
					}
				case constants.EnforcerPolicyTypeMinimumReplicas:
					if c.GetValue() != 1 {
						t.Errorf("Expected %s counter to be 1, got %f", constants.EnforcerPolicyTypeMinimumReplicas, c.GetValue())
					}
				default:
					t.Errorf("Unexpected policy_type label: %s", policyType)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}
}

func TestRecordEnforcerMetric_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedEnforcerModificationsTotal := enforcerModificationsTotal
	defer func() {
		controllerInstance = savedInstance
		enforcerModificationsTotal = savedEnforcerModificationsTotal
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	t.Setenv(ControllerInstanceEnvVar, "controller-1")

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit enforcer metric
	emitter.RecordEnforcerMetric(constants.EnforcerPolicyTypeScaleToZero)

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != "controller-1" {
				t.Errorf("Expected controller_instance=controller-1, got %s", instance)
			}
			policyType := getLabelValue(m, constants.LabelPolicyType)
			if policyType != constants.EnforcerPolicyTypeScaleToZero {
				t.Errorf("Expected policy_type=%s, got %s", constants.EnforcerPolicyTypeScaleToZero, policyType)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}
}

func TestRecordEnforcerMetric_MultiplePolicyTypes(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Test various policy types
	policyTypes := []string{
		constants.EnforcerPolicyTypeScaleToZero,
		constants.EnforcerPolicyTypeMinimumReplicas,
		"custom-policy",
	}

	for _, policyType := range policyTypes {
		emitter.RecordEnforcerMetric(policyType)
	}

	// Verify all policy types were recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	foundPolicyTypes := make(map[string]bool)
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			if len(mf.GetMetric()) != len(policyTypes) {
				t.Errorf("Expected %d metric series, got %d", len(policyTypes), len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				policyType := getLabelValue(m, constants.LabelPolicyType)
				foundPolicyTypes[policyType] = true
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				if c.GetValue() != 1 {
					t.Errorf("Expected counter value 1 for policy %s, got %f", policyType, c.GetValue())
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}

	// Verify all expected policy types were found
	for _, expected := range policyTypes {
		if !foundPolicyTypes[expected] {
			t.Errorf("Expected policy type %s not found in metrics", expected)
		}
	}
}
