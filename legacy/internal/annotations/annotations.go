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

// Package annotations defines the WVA annotation schema used for annotation-based
// discovery on KEDA ScaledObjects and Kubernetes HPAs. Placing this schema in a
// dedicated package gives a single source of truth for annotation key names and
// parsing logic across the controller, engine, and test fixtures.
package annotations

import (
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Managed is the annotation that opts a ScaledObject or HPA into WVA management.
	// Value must be "true".
	Managed = "llm-d.ai/managed"

	// ModelID is the required annotation identifying the model served by the variant.
	// It maps to VariantAutoscalingSpec.ModelID and is used for multi-variant grouping.
	ModelID = "llm-d.ai/model-id"

	// VariantCost is the optional annotation specifying cost per replica for
	// cost-aware optimization. Must be a non-negative decimal string. Defaults to "10.0".
	VariantCost = "llm-d.ai/variant-cost"

	// Synthetic marks an in-memory VariantAutoscaling as annotation-sourced.
	// Objects carrying this annotation are never written to the Kubernetes API server;
	// they exist only within the WVA optimization pipeline.
	Synthetic = "llm-d.ai/synthetic"

	// defaultVariantCost matches the kubebuilder default on VariantAutoscalingConfigSpec.
	defaultVariantCost = "10.0"

	// enabledValue is the annotation value that turns a boolean WVA annotation
	// (Managed, Synthetic) on.
	enabledValue = "true"
)

// Parsed holds the validated WVA annotation values extracted from a Kubernetes object.
type Parsed struct {
	ModelID     string
	VariantCost string
}

// IsManaged returns true if obj bears llm-d.ai/managed: "true".
func IsManaged(obj metav1.Object) bool {
	return obj.GetAnnotations()[Managed] == enabledValue
}

// Parse extracts and validates WVA annotations from obj.
// Returns an error if required annotations are missing or have invalid values.
func Parse(obj metav1.Object) (*Parsed, error) {
	ann := obj.GetAnnotations()
	if ann[Managed] != enabledValue {
		return nil, fmt.Errorf("annotation %s must be \"true\"", Managed)
	}
	modelID := ann[ModelID]
	if modelID == "" {
		return nil, fmt.Errorf("required annotation %s is missing or empty", ModelID)
	}
	cost := ann[VariantCost]
	if cost == "" {
		cost = defaultVariantCost
	}
	costVal, err := strconv.ParseFloat(cost, 64)
	if err != nil {
		return nil, fmt.Errorf("annotation %s must be a numeric string, got %q: %w", VariantCost, cost, err)
	}
	if costVal < 0 {
		return nil, fmt.Errorf("annotation %q must be non-negative, got %v", VariantCost, costVal)
	}
	return &Parsed{
		ModelID:     modelID,
		VariantCost: cost,
	}, nil
}
