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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func TestRecordOptimizationFailedEvent(t *testing.T) {
	tests := []struct {
		name         string
		numVAs       int
		reason       string
		expectedEvts int
	}{
		{
			name:         "records event for single VA",
			numVAs:       1,
			reason:       "Saturation data preparation failed",
			expectedEvts: 1,
		},
		{
			name:         "records event for multiple VAs",
			numVAs:       3,
			reason:       "V2 analysis failed: context deadline exceeded",
			expectedEvts: 3,
		},
		{
			name:         "handles empty VA list",
			numVAs:       0,
			reason:       "Model data preparation failed",
			expectedEvts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeRecorder := record.NewFakeRecorder(100)

			scheme := runtime.NewScheme()
			_ = llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			_ = v1.AddToScheme(scheme)

			// Create fake k8s client
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				Build()

			// Create source registry with prometheus source
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(context.Background(), nil, prometheus.DefaultPrometheusSourceConfig())
			_ = sourceRegistry.Register("prometheus", promSource)

			testConfig := config.NewTestConfig()

			engine := NewEngine(k8sClient, k8sClient, scheme, fakeRecorder, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			variantAutoscalings := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, tt.numVAs)
			for i := 0; i < tt.numVAs; i++ {
				vaName := "test-va"
				if i > 0 {
					vaName += string(rune('0' + i))
				}
				variantAutoscalings = append(variantAutoscalings, llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      vaName,
						Namespace: "default",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: vaName + "-deployment",
						},
						ModelID:     "test-model",
						MaxReplicas: 5,
					},
				})
			}

			engine.recordOptimizationFailedEvent(variantAutoscalings, tt.reason)

			// Count recorded events
			eventCount := 0
			for {
				select {
				case event := <-fakeRecorder.Events:
					assert.Contains(t, event, constants.K8SEventOptimizationFailed,
						"Event should contain K8SEventOptimizationFailed constant")
					assert.Contains(t, event, tt.reason,
						"Event should contain the reason message")
					eventCount++
				default:
					goto done
				}
			}
		done:
			assert.Equal(t, tt.expectedEvts, eventCount,
				"Should record correct number of events")
		})
	}
}

func TestRecordOptimizationFailedEvent_NilRecorder(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	// Create fake k8s client
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	// Create source registry with prometheus source
	sourceRegistry := source.NewSourceRegistry()
	promSource := prometheus.NewPrometheusSource(context.Background(), nil, prometheus.DefaultPrometheusSourceConfig())
	_ = sourceRegistry.Register("prometheus", promSource)

	testConfig := config.NewTestConfig()

	// Create engine with nil recorder
	engine := NewEngine(k8sClient, k8sClient, scheme, nil, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

	variantAutoscalings := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		{
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
		},
	}

	// Should not panic with nil recorder
	assert.NotPanics(t, func() {
		engine.recordOptimizationFailedEvent(variantAutoscalings, "Test failure")
	})
}

func TestK8SEventOptimizationFailedConstant(t *testing.T) {
	// Verify the constant is correctly defined
	assert.Equal(t, "OptimizationFailed", constants.K8SEventOptimizationFailed,
		"K8SEventOptimizationFailed constant should match expected value")
}
