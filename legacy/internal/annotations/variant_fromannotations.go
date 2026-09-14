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

package annotations

import (
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// IsSynthetic reports whether va was synthesized from annotations on a ScaledObject or HPA
// rather than read from a VariantAutoscaling CRD instance.
// Synthetic VAs exist only in-memory and must never be written to the Kubernetes API server.
func IsSynthetic(va *wvav1alpha1.VariantAutoscaling) bool {
	return va.GetAnnotations()[Synthetic] == enabledValue
}

// VariantAutoscalingFromScaledObject builds an in-memory VariantAutoscaling from a KEDA
// ScaledObject that bears the llm-d.ai/managed: "true" annotation.
// Returns an error if required annotations are absent or the scaleTargetRef is empty.
func VariantAutoscalingFromScaledObject(so *kedav1alpha1.ScaledObject) (*wvav1alpha1.VariantAutoscaling, error) {
	parsed, err := Parse(so)
	if err != nil {
		return nil, err
	}
	if so.Spec.ScaleTargetRef == nil || so.Spec.ScaleTargetRef.Name == "" {
		return nil, fmt.Errorf("ScaledObject %s/%s has no scaleTargetRef", so.Namespace, so.Name)
	}

	kind := so.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = "Deployment"
	}
	apiVersion := so.Spec.ScaleTargetRef.APIVersion
	if apiVersion == "" {
		apiVersion = "apps/v1"
	}

	minReplicas := so.Spec.MinReplicaCount
	maxReplicas := so.GetHPAMaxReplicas()

	return &wvav1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      so.Name,
			Namespace: so.Namespace,
			Labels:    so.Labels,
			Annotations: map[string]string{
				Synthetic: enabledValue,
			},
		},
		Spec: wvav1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       so.Spec.ScaleTargetRef.Name,
			},
			ModelID:     parsed.ModelID,
			MinReplicas: minReplicas,
			MaxReplicas: maxReplicas,
			VariantAutoscalingConfigSpec: wvav1alpha1.VariantAutoscalingConfigSpec{
				VariantCost: parsed.VariantCost,
			},
		},
	}, nil
}

// VariantAutoscalingFromHPA builds an in-memory VariantAutoscaling from a Kubernetes HPA
// that bears the llm-d.ai/managed: "true" annotation.
// Returns an error if required annotations are absent or the scaleTargetRef is empty.
func VariantAutoscalingFromHPA(hpa *autoscalingv2.HorizontalPodAutoscaler) (*wvav1alpha1.VariantAutoscaling, error) {
	parsed, err := Parse(hpa)
	if err != nil {
		return nil, err
	}
	if hpa.Spec.ScaleTargetRef.Name == "" {
		return nil, fmt.Errorf("HPA %s/%s has no scaleTargetRef", hpa.Namespace, hpa.Name)
	}

	kind := hpa.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = "Deployment"
	}
	apiVersion := hpa.Spec.ScaleTargetRef.APIVersion
	if apiVersion == "" {
		apiVersion = "apps/v1"
	}

	minReplicas := ptr.To(int32(1))
	if hpa.Spec.MinReplicas != nil {
		minReplicas = hpa.Spec.MinReplicas
	}

	return &wvav1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hpa.Name,
			Namespace: hpa.Namespace,
			Labels:    hpa.Labels,
			Annotations: map[string]string{
				Synthetic: enabledValue,
			},
		},
		Spec: wvav1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       hpa.Spec.ScaleTargetRef.Name,
			},
			ModelID:     parsed.ModelID,
			MinReplicas: minReplicas,
			MaxReplicas: hpa.Spec.MaxReplicas,
			VariantAutoscalingConfigSpec: wvav1alpha1.VariantAutoscalingConfigSpec{
				VariantCost: parsed.VariantCost,
			},
		},
	}, nil
}
