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

package annotations_test

import (
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

func wvaAnnotations(modelID, cost string) map[string]string {
	ann := map[string]string{
		annotations.Managed: "true",
		annotations.ModelID: modelID,
	}
	if cost != "" {
		ann[annotations.VariantCost] = cost
	}
	return ann
}

func TestVariantAutoscalingFromScaledObject(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-scaler",
			Namespace:   "production",
			Annotations: wvaAnnotations("ibm/granite-13b", "40.0"),
			Labels:      map[string]string{"inference.optimization/acceleratorName": "h100"},
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "granite-13b",
			},
			MinReplicaCount: ptr.To(int32(1)),
			MaxReplicaCount: ptr.To(int32(5)),
		},
	}

	va, err := annotations.VariantAutoscalingFromScaledObject(so)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !annotations.IsSynthetic(va) {
		t.Error("expected IsSynthetic to return true")
	}
	if va.Name != "my-scaler" {
		t.Errorf("Name = %q, want %q", va.Name, "my-scaler")
	}
	if va.Namespace != "production" {
		t.Errorf("Namespace = %q, want %q", va.Namespace, "production")
	}
	if va.Spec.ModelID != "ibm/granite-13b" {
		t.Errorf("ModelID = %q, want %q", va.Spec.ModelID, "ibm/granite-13b")
	}
	if va.Spec.VariantCost != "40.0" {
		t.Errorf("VariantCost = %q, want %q", va.Spec.VariantCost, "40.0")
	}
	if va.Spec.ScaleTargetRef.Name != "granite-13b" {
		t.Errorf("ScaleTargetRef.Name = %q, want %q", va.Spec.ScaleTargetRef.Name, "granite-13b")
	}
	if va.Spec.MaxReplicas != 5 {
		t.Errorf("MaxReplicas = %d, want %d", va.Spec.MaxReplicas, 5)
	}
	if va.Labels["inference.optimization/acceleratorName"] != "h100" {
		t.Errorf("Labels[acceleratorName] = %q, want %q", va.Labels["inference.optimization/acceleratorName"], "h100")
	}
}

func TestVariantAutoscalingFromScaledObject_DefaultKind(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "scaler",
			Namespace:   "ns",
			Annotations: wvaAnnotations("model/x", ""),
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "my-deploy"},
		},
	}
	va, err := annotations.VariantAutoscalingFromScaledObject(so)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if va.Spec.ScaleTargetRef.Kind != "Deployment" {
		t.Errorf("Kind = %q, want Deployment", va.Spec.ScaleTargetRef.Kind)
	}
	if va.Spec.VariantCost != "10.0" {
		t.Errorf("VariantCost = %q, want 10.0 (default)", va.Spec.VariantCost)
	}
}

func TestVariantAutoscalingFromScaledObject_NoScaleTargetRef(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "scaler",
			Namespace:   "ns",
			Annotations: wvaAnnotations("model/x", ""),
		},
		Spec: kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: nil},
	}
	if _, err := annotations.VariantAutoscalingFromScaledObject(so); err == nil {
		t.Error("expected error for nil scaleTargetRef")
	}
}

func TestVariantAutoscalingFromHPA(t *testing.T) {
	minR := int32(2)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-hpa",
			Namespace:   "staging",
			Annotations: wvaAnnotations("model/llama", "20.0"),
			Labels:      map[string]string{"inference.optimization/acceleratorName": "cpu"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "llama-deploy",
			},
			MinReplicas: &minR,
			MaxReplicas: 8,
		},
	}

	va, err := annotations.VariantAutoscalingFromHPA(hpa)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !annotations.IsSynthetic(va) {
		t.Error("expected IsSynthetic to return true")
	}
	if va.Name != "my-hpa" {
		t.Errorf("Name = %q, want %q", va.Name, "my-hpa")
	}
	if va.Spec.ModelID != "model/llama" {
		t.Errorf("ModelID = %q, want %q", va.Spec.ModelID, "model/llama")
	}
	if va.Spec.MaxReplicas != 8 {
		t.Errorf("MaxReplicas = %d, want %d", va.Spec.MaxReplicas, 8)
	}
	if va.Spec.MinReplicas == nil || *va.Spec.MinReplicas != 2 {
		t.Errorf("MinReplicas = %v, want 2", va.Spec.MinReplicas)
	}
	if va.Labels["inference.optimization/acceleratorName"] != "cpu" {
		t.Errorf("Labels[acceleratorName] = %q, want %q", va.Labels["inference.optimization/acceleratorName"], "cpu")
	}
}

func TestVariantAutoscalingFromHPA_MissingAnnotations(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Name: "d"},
			MaxReplicas:    2,
		},
	}
	if _, err := annotations.VariantAutoscalingFromHPA(hpa); err == nil {
		t.Error("expected error for missing managed annotation")
	}
}

func TestIsSynthetic_False(t *testing.T) {
	va, _ := annotations.VariantAutoscalingFromHPA(&autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "h", Namespace: "ns",
			Annotations: wvaAnnotations("m", ""),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Name: "d"},
			MaxReplicas:    1,
		},
	})
	if !annotations.IsSynthetic(va) {
		t.Error("synthesized VA must be marked synthetic")
	}

	// Remove the synthetic annotation to simulate a CRD-sourced VA
	delete(va.Annotations, annotations.Synthetic)
	if annotations.IsSynthetic(va) {
		t.Error("VA without synthetic annotation must not be synthetic")
	}
}

// Regression: a ScaledObject with minReplicaCount:0 must yield MinReplicas=0 so the
// analyzer can scale the variant to zero. Before the fix, annotation mode read
// so.GetHPAMinReplicas() which floors at 1 (an HPA's minReplicas can't be 0), hiding the
// real minReplicaCount:0 and pinning the variant at >=1.
func TestVariantAutoscalingFromScaledObject_ScaleToZero(t *testing.T) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "idle-variant",
			Namespace:   "production",
			Annotations: wvaAnnotations("ibm/granite-13b", "40.0"),
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "granite-13b",
			},
			MinReplicaCount: ptr.To(int32(0)),
			MaxReplicaCount: ptr.To(int32(5)),
		},
	}
	va, err := annotations.VariantAutoscalingFromScaledObject(so)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if va.Spec.MinReplicas == nil || *va.Spec.MinReplicas != 0 {
		t.Errorf("MinReplicas = %v, want 0 (scale-to-zero must be honored, not floored to 1)", va.Spec.MinReplicas)
	}
}
