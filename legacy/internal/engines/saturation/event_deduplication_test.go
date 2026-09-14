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
	"strings"
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

// TestEventDeduplication verifies that only one event is recorded per VA in an optimization cycle
func TestEventDeduplication(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)
	engine := &Engine{
		Recorder:       fakeRecorder,
		vaEventTracker: make(map[string]bool),
	}

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

	// First scaling event should be recorded
	engine.recordScalingEvent(va, domain.ActionScaleUp, 3, "First scale up")

	// Second scaling event should be deduplicated (not recorded)
	engine.recordScalingEvent(va, domain.ActionScaleDown, 2, "Scale down attempt")

	// Third scaling event should also be deduplicated (not recorded)
	engine.recordScalingEvent(va, domain.ActionScaleUp, 4, "Second scale up")

	// Count recorded events - should only be 1
	eventCount := 0
	for {
		select {
		case event := <-fakeRecorder.Events:
			eventCount++
			// Only the first event should be present
			assert.Contains(t, event, constants.K8SEventScaledUp,
				"First event should be ScaledUp")
			assert.Contains(t, event, "First scale up",
				"First event should contain the first reason")
		default:
			goto done
		}
	}

done:
	assert.Equal(t, 1, eventCount,
		"Should only record one event per VA in an optimization cycle")
}

// TestEventDeduplicationResourceConstrainedException verifies ResourceConstrained events are always recorded
func TestEventDeduplicationResourceConstrainedException(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)
	engine := &Engine{
		Recorder:       fakeRecorder,
		vaEventTracker: make(map[string]bool),
	}

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

	// Record a scaling event first
	engine.recordScalingEvent(va, domain.ActionScaleUp, 3, "Scale up")

	// ResourceConstrained event should still be recorded (exception to deduplication)
	engine.recordEvent(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, "Limited by resources")

	// Count recorded events - should be 2 (ScaledUp + ResourceConstrained)
	eventCount := 0
	foundScaleUp := false
	foundResourceConstrained := false

	for {
		select {
		case event := <-fakeRecorder.Events:
			t.Logf("Received event: %s", event)
			eventCount++
			if strings.Contains(event, constants.K8SEventScaledUp) {
				foundScaleUp = true
			}
			if strings.Contains(event, constants.K8SEventResourceConstrained) {
				foundResourceConstrained = true
			}
		default:
			goto done
		}
	}

done:
	t.Logf("Total events: %d, foundScaleUp: %v, foundResourceConstrained: %v", eventCount, foundScaleUp, foundResourceConstrained)
	assert.Equal(t, 2, eventCount,
		"Should record both ScaledUp and ResourceConstrained events")
	assert.True(t, foundScaleUp, "Should have recorded ScaledUp event")
	assert.True(t, foundResourceConstrained, "Should have recorded ResourceConstrained event")
}

// TestEventDeduplicationNilTracker verifies the engine handles nil vaEventTracker gracefully
func TestEventDeduplicationNilTracker(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)
	engine := &Engine{
		Recorder:       fakeRecorder,
		vaEventTracker: nil, // Nil tracker, as in unit tests
	}

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

	// Should not panic with nil tracker
	assert.NotPanics(t, func() {
		engine.recordScalingEvent(va, domain.ActionScaleUp, 3, "Scale up")
	})

	// Event should still be recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledUp,
			"Event should be recorded even with nil tracker")
	default:
		t.Error("Expected event to be recorded with nil tracker")
	}
}

// TestEventDeduplicationMultipleVAs verifies deduplication is per-VA
func TestEventDeduplicationMultipleVAs(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)
	engine := &Engine{
		Recorder:       fakeRecorder,
		vaEventTracker: make(map[string]bool),
	}

	va1 := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
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
	}

	va2 := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
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
	}

	// Record event for first VA
	engine.recordScalingEvent(va1, domain.ActionScaleUp, 3, "VA1 scale up")

	// Record event for second VA (should be recorded - different VA)
	engine.recordScalingEvent(va2, domain.ActionScaleUp, 2, "VA2 scale up")

	// Try to record another event for first VA (should be deduplicated)
	engine.recordScalingEvent(va1, domain.ActionScaleDown, 1, "VA1 scale down")

	// Try to record another event for second VA (should be deduplicated)
	engine.recordScalingEvent(va2, domain.ActionScaleDown, 1, "VA2 scale down")

	// Count recorded events - should be 2 (one for each VA)
	eventCount := 0
	foundVA1 := false
	foundVA2 := false

	for eventCount < 2 {
		select {
		case event := <-fakeRecorder.Events:
			eventCount++
			if !foundVA1 && assert.Contains(t, event, "VA1 scale up") {
				foundVA1 = true
			} else if !foundVA2 && assert.Contains(t, event, "VA2 scale up") {
				foundVA2 = true
			}
		default:
			goto done
		}
	}

done:
	assert.Equal(t, 2, eventCount,
		"Should record one event per VA")
	assert.True(t, foundVA1, "Should have recorded event for VA1")
	assert.True(t, foundVA2, "Should have recorded event for VA2")
}
