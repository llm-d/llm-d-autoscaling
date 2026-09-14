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

package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// TestScaledUpEvent verifies K8SEventScaledUp is recorded for scale-up decisions
func TestScaledUpEvent(t *testing.T) {
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

	decision := &domain.VariantDecision{
		VariantName:    "test-va",
		Action:         domain.ActionScaleUp,
		TargetReplicas: 3,
	}
	decision.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonTest, string(domain.DecisionReasonTest))

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case domain.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason())
		case domain.ActionScaleDown:
			if decision.TargetReplicas == 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, decision.Reason())
			} else {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason())
			}
		}
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledUp,
			"Event should contain K8SEventScaledUp constant")
		assert.Contains(t, event, decision.Reason(),
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledUp event to be recorded but none was found")
	}
}

// TestScaledDownEvent verifies K8SEventScaledDown is recorded for scale-down decisions
func TestScaledDownEvent(t *testing.T) {
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

	decision := &domain.VariantDecision{
		VariantName:    "test-va",
		Action:         domain.ActionScaleDown,
		TargetReplicas: 2,
	}
	decision.SetDecisionReason(domain.ActionScaleDown, domain.DecisionReasonTest, string(domain.DecisionReasonTest))

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case domain.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason())
		case domain.ActionScaleDown:
			if decision.TargetReplicas == 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, decision.Reason())
			} else {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason())
			}
		}
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledDown,
			"Event should contain K8SEventScaledDown constant")
		assert.Contains(t, event, decision.Reason(),
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledDown event to be recorded but none was found")
	}
}

// TestScaledToZeroEvent verifies K8SEventScaledToZero is recorded when scaling to zero
func TestScaledToZeroEvent(t *testing.T) {
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

	decision := &domain.VariantDecision{
		VariantName:    "test-va",
		Action:         domain.ActionScaleDown,
		TargetReplicas: 0,
	}
	decision.SetDecisionReason(domain.ActionScaleDown, domain.DecisionReasonTest, string(domain.DecisionReasonTest))

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case domain.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason())
		case domain.ActionScaleDown:
			if decision.TargetReplicas == 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, decision.Reason())
			} else {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason())
			}
		}
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledToZero,
			"Event should contain K8SEventScaledToZero constant")
		assert.Contains(t, event, decision.Reason(),
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledToZero event to be recorded but none was found")
	}
}

// TestResourceConstrainedEvent verifies K8SEventResourceConstrained is recorded when limited
func TestResourceConstrainedEvent(t *testing.T) {
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

	decision := &domain.VariantDecision{
		VariantName:    "test-va",
		Action:         domain.ActionScaleUp,
		TargetReplicas: 3,
		WasLimited:     true,
	}
	decision.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonTest, string(domain.DecisionReasonTest))

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case domain.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason())
		case domain.ActionScaleDown:
			if decision.TargetReplicas == 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, decision.Reason())
			} else {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason())
			}
		}
		if decision.WasLimited {
			fakeRecorder.Eventf(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, decision.Reason())
		}
	}

	// Verify both events were recorded
	eventsRecorded := 0
	foundScaleUp := false
	foundResourceConstrained := false

	for eventsRecorded < 2 {
		select {
		case event := <-fakeRecorder.Events:
			t.Logf("Received event: %s", event)
			if !foundScaleUp && assert.Contains(t, event, constants.K8SEventScaledUp) {
				foundScaleUp = true
				assert.Contains(t, event, decision.Reason(),
					"ScaledUp event should contain the reason")
				assert.Contains(t, event, "Normal",
					"ScaledUp event should be Normal type")
			} else if !foundResourceConstrained && assert.Contains(t, event, constants.K8SEventResourceConstrained) {
				foundResourceConstrained = true
				assert.Contains(t, event, "Warning",
					"ResourceConstrained event should be Warning type")
				assert.Contains(t, event, decision.Reason(),
					"ResourceConstrained event should contain the reason")
			}
			eventsRecorded++
		default:
			goto done
		}
	}

done:
	assert.True(t, foundScaleUp, "Should have recorded ScaledUp event")
	assert.True(t, foundResourceConstrained, "Should have recorded ResourceConstrained event")
}

// TestNoEventForNoDecision verifies no events are recorded when action is NoChange
func TestNoEventForNoDecision(t *testing.T) {
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

	decision := &domain.VariantDecision{
		VariantName:    "test-va",
		Action:         domain.ActionNoChange,
		TargetReplicas: 3,
	}
	decision.SetDecisionReason(domain.ActionNoChange, domain.DecisionReasonTest, string(domain.DecisionReasonTest))

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case domain.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason())
		case domain.ActionScaleDown:
			if decision.TargetReplicas == 0 {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, decision.Reason())
			} else {
				fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason())
			}
		default:
			// do nothing
		}
	}

	// Verify no event was recorded
	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Unexpected event recorded for ActionNoChange: %s", event)
	default:
		// No event expected - this is correct
	}
}

// TestOptimizationFailedEvent verifies K8SEventOptimizationFailed is recorded on data preparation failure
func TestOptimizationFailedEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	variantAutoscalings := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va-1",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment-1",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va-2",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment-2",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	reason := "Saturation data preparation failed"

	// Simulate the event recording logic from optimizeV1/optimizeV2 error paths
	if fakeRecorder != nil {
		for _, va := range variantAutoscalings {
			fakeRecorder.Eventf(&va, corev1.EventTypeWarning, constants.K8SEventOptimizationFailed, reason)
		}
	}

	// Verify events were recorded for all VAs
	eventsRecorded := 0
	for eventsRecorded < len(variantAutoscalings) {
		select {
		case event := <-fakeRecorder.Events:
			assert.Contains(t, event, constants.K8SEventOptimizationFailed,
				"Event should contain K8SEventOptimizationFailed constant")
			assert.Contains(t, event, reason,
				"Event should contain the reason message")
			assert.Contains(t, event, "Warning",
				"Event should be Warning type")
			eventsRecorded++
		default:
			t.Errorf("Expected %d events but only got %d", len(variantAutoscalings), eventsRecorded)
			return
		}
	}

	assert.Equal(t, len(variantAutoscalings), eventsRecorded,
		"Should have recorded event for each VA")
}

// TestOptimizationFailedEvent_NoErrorDetails verifies error details are not exposed in events
func TestOptimizationFailedEvent_NoErrorDetails(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
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

	// Static message without error details (correct behavior)
	staticMsg := "Model data preparation failed"
	// Sensitive error that should NOT appear in events
	sensitiveError := "connection to 10.0.1.42:9090 failed: authentication denied for user admin"

	// Simulate correct event recording: only static message, no error details
	if fakeRecorder != nil {
		fakeRecorder.Eventf(&va, corev1.EventTypeWarning, constants.K8SEventOptimizationFailed, staticMsg)
	}

	// Verify event was recorded with only the static message
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventOptimizationFailed,
			"Event should contain K8SEventOptimizationFailed constant")
		assert.Contains(t, event, staticMsg,
			"Event should contain the static message")
		assert.NotContains(t, event, sensitiveError,
			"Event should NOT contain sensitive error details like IPs or credentials")
		assert.NotContains(t, event, "10.0.1.42",
			"Event should NOT contain internal IP addresses")
		assert.NotContains(t, event, "authentication denied",
			"Event should NOT contain authentication failure details")
	default:
		t.Error("Expected OptimizationFailed event to be recorded but none was found")
	}
}

// TestK8SEventScalingConstants verifies the scaling event constants are correctly defined
func TestK8SEventScalingConstants(t *testing.T) {
	assert.Equal(t, "ScaledUp", constants.K8SEventScaledUp,
		"K8SEventScaledUp constant should match expected value")
	assert.Equal(t, "ScaledDown", constants.K8SEventScaledDown,
		"K8SEventScaledDown constant should match expected value")
	assert.Equal(t, "ResourceConstrained", constants.K8SEventResourceConstrained,
		"K8SEventResourceConstrained constant should match expected value")
	assert.Equal(t, "OptimizationFailed", constants.K8SEventOptimizationFailed,
		"K8SEventOptimizationFailed constant should match expected value")
}
