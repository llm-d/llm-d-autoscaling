/*
Copyright 2025 The llm-d Authors

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

package utils

import (
	"context"
	"errors"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func TestGroupVariantAutoscalingByModel(t *testing.T) {
	tests := []struct {
		name           string
		vas            []wvav1alpha1.VariantAutoscaling
		expectedGroups int
		expectedKeys   []string
	}{
		{
			name: "same model different accelerators groups together for cost optimization",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-a100",
						Namespace: "default",
						Labels: map[string]string{
							accelerator.AcceleratorNameLabel: "A100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-h100",
						Namespace: "default",
						Labels: map[string]string{
							accelerator.AcceleratorNameLabel: "H100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "same model same namespace groups together",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "different namespaces creates separate groups",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "ns1",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "ns2",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 2,
			expectedKeys:   []string{"llama-8b|ns1", "llama-8b|ns2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupVariantAutoscalingByModel(tt.vas)

			if len(result) != tt.expectedGroups {
				t.Errorf("GroupVariantAutoscalingByModel() returned %d groups, want %d", len(result), tt.expectedGroups)
			}

			for _, key := range tt.expectedKeys {
				if _, exists := result[key]; !exists {
					t.Errorf("GroupVariantAutoscalingByModel() missing expected key %q", key)
				}
			}
		})
	}
}

// variantTestScheme builds a scheme with WVA, core Kubernetes (incl. HPA), and KEDA types.
func variantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgoscheme: %v", err)
	}
	if err := wvav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add wvav1alpha1: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add kedav1alpha1: %v", err)
	}
	return s
}

func managedHPA(ns, name, targetName, modelID string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				annotations.Managed: "true",
				annotations.ModelID: modelID,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: targetName},
			MaxReplicas:    5,
		},
	}
}

func managedSO(ns, name, targetName, modelID string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				annotations.Managed: "true",
				annotations.ModelID: modelID,
			},
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Kind: "Deployment", Name: targetName},
		},
	}
}

func TestAnnotationSourcedVariants(t *testing.T) {
	ctx := context.Background()

	t.Run("HPAs only", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedHPA("ns1", "hpa-b", "deploy-b", "model-x"),
			// unmanaged HPA — must be filtered out
			&autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "hpa-unmanaged", Namespace: "ns1"},
				Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3},
			},
		).Build()

		result, err := annotationSourcedVariants(ctx, cl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs, got %d", len(result))
		}
	})

	t.Run("ScaledObjects only", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedSO("ns1", "so-a", "deploy-a", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA, got %d", len(result))
		}
	})

	t.Run("mixed HPAs and ScaledObjects", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedSO("ns1", "so-b", "deploy-b", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs, got %d", len(result))
		}
	})

	t.Run("KEDA not installed — NoMatchError skipped gracefully", func(t *testing.T) {
		s := variantTestScheme(t)
		soGK := schema.GroupKind{Group: "keda.sh", Kind: "ScaledObject"}
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return &apimeta.NoKindMatchError{GroupKind: soGK}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := annotationSourcedVariants(ctx, cl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA from HPA, got %d", len(result))
		}
	})

	t.Run("KEDA non-NoMatch error propagated", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return errors.New("keda api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		_, err := annotationSourcedVariants(ctx, cl)
		if err == nil {
			t.Fatal("want error for non-NoMatch ScaledObject list failure, got nil")
		}
	})

	t.Run("deduplication: ScaledObject wins over HPA for same scale target", func(t *testing.T) {
		s := variantTestScheme(t)
		// Both an HPA and a ScaledObject point at the same Deployment — ScaledObject wins.
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-hpa"),
			managedSO("ns1", "so-a", "deploy-a", "model-so"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA (deduplicated), got %d", len(result))
		}
		if result[0].Spec.ModelID != "model-so" {
			t.Errorf("want ScaledObject to win, got modelID %q", result[0].Spec.ModelID)
		}
	})
}

func TestReadyVariantAutoscalings(t *testing.T) {
	ctx := context.Background()

	t.Run("annotated HPA yields a synthetic variant", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-ann", "deploy-ann", "model-ann"),
		).Build()

		result := readyVariantAutoscalings(ctx, cl)
		if len(result) != 1 {
			t.Fatalf("want 1 annotation-sourced variant, got %d", len(result))
		}
		if result[0].Name != "hpa-ann" || result[0].Spec.ModelID != "model-ann" {
			t.Errorf("unexpected synthetic variant: name=%q modelID=%q", result[0].Name, result[0].Spec.ModelID)
		}
		if !annotations.IsSynthetic(&result[0]) {
			t.Error("want annotation-sourced variant to be marked synthetic")
		}
	})

	t.Run("annotated HPA and ScaledObject with different targets — both returned", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns-hpa", "hpa-ann", "deploy-hpa", "model-hpa"),
			managedSO("ns-so", "so-ann", "deploy-so", "model-so"),
		).Build()

		result := readyVariantAutoscalings(ctx, cl)
		if len(result) != 2 {
			t.Errorf("want 2 variants, got %d", len(result))
		}
	})

	t.Run("annotated HPA, KEDA listing fails — HPA variant still returned", func(t *testing.T) {
		// annotationSourcedVariants successfully lists the HPA but then fails listing
		// ScaledObjects with a non-NoMatch error (e.g. transient API error).
		// readyVariantAutoscalings logs the error as non-fatal and still returns the
		// partial annotation results, so the HPA-sourced variant is returned.
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-ann", "deploy-ann", "model-ann"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return errors.New("keda api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result := readyVariantAutoscalings(ctx, cl)
		if len(result) != 1 {
			t.Errorf("want 1 variant (HPA-sourced, KEDA error is non-fatal), got %d", len(result))
		}
	})
}
