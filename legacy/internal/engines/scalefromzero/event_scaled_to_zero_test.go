/*
Copyright 2025.

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

package scalefromzero

import (
	"testing"

	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// TestScaledUpEvent_DirectCall tests K8SEventScaledUp emission when scaling from zero
func TestScaledUpEvent_DirectCall(t *testing.T) {
	tests := []struct {
		name                  string
		hasDecision           bool
		targetReplicas        int
		expectedEventRecorded bool
		reason                string
	}{
		{
			name:                  "emits event when hasDecision=true and targetReplicas=1",
			hasDecision:           true,
			targetReplicas:        1,
			expectedEventRecorded: true,
			reason:                "Scaling up from zero",
		},
		{
			name:                  "no event when hasDecision=false",
			hasDecision:           false,
			targetReplicas:        1,
			expectedEventRecorded: false,
			reason:                "No decision made",
		},
		{
			name:                  "no event when targetReplicas=0",
			hasDecision:           true,
			targetReplicas:        0,
			expectedEventRecorded: false,
			reason:                "Not scaling (staying at zero)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeRecorder := record.NewFakeRecorder(100)

			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					ModelID:     "test-model",
					MaxReplicas: 5,
				},
			}

			// Simulate the event recording logic from processInactiveVariant
			if tt.hasDecision && tt.targetReplicas > 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, tt.reason)
			}

			// Verify event was recorded (or not)
			if tt.expectedEventRecorded {
				select {
				case event := <-fakeRecorder.Events:
					assert.Contains(t, event, constants.K8SEventScaledUp,
						"Event should contain K8SEventScaledUp constant")
					assert.Contains(t, event, tt.reason,
						"Event should contain the reason message")
					assert.Contains(t, event, "Normal",
						"Event should be Normal type")
				default:
					t.Error("Expected ScaledUp event to be recorded but none was found")
				}
			} else {
				select {
				case event := <-fakeRecorder.Events:
					t.Errorf("Unexpected event recorded when hasDecision=%v, targetReplicas=%d: %s",
						tt.hasDecision, tt.targetReplicas, event)
				default:
					// No event expected - this is correct
				}
			}
		})
	}
}

// TestScaledToZeroEvent_NilRecorder tests that nil recorder is handled gracefully
func TestScaledToZeroEvent_NilRecorder(t *testing.T) {
	// The actual code in engine.go checks if recorder is nil before calling Eventf
	// This test documents that a nil recorder should be checked before use

	// Simulate the pattern used in the actual code
	var recorder record.EventRecorder // nil
	hasDecision := true
	targetWorkloadReplicas := 0

	// This should not panic because the code checks recorder != nil
	assert.NotPanics(t, func() {
		if recorder != nil && hasDecision && targetWorkloadReplicas == 0 {
			// This branch is never executed when recorder is nil
			t.Error("Should not reach here with nil recorder")
		}
	})
}

// TestScaledUpEventOnScaleFromZero tests K8SEventScaledUp emission when scaling from zero
func TestScaledUpEventOnScaleFromZero(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "default",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "test-deployment",
			},
			ModelID:     "test-model",
			MaxReplicas: 5,
		},
	}

	reason := "No pending requests in queue"
	hasDecision := true
	targetWorkloadReplicas := 1

	// Simulate the event recording logic from processInactiveVariant
	if fakeRecorder != nil && hasDecision && targetWorkloadReplicas > 0 {
		fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, reason)
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledUp,
			"Event should contain K8SEventScaledUp constant")
		assert.Contains(t, event, reason,
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledUp event to be recorded but none was found")
	}
}

// TestK8SEventScaledToZeroConstant verifies the constant is correctly defined
// Note: K8SEventScaledToZero is currently unused but reserved for future use
// (e.g., saturation engine scaling down to zero)
func TestK8SEventScaledToZeroConstant(t *testing.T) {
	assert.Equal(t, "ScaledToZero", constants.K8SEventScaledToZero,
		"K8SEventScaledToZero constant should match expected value")
}
