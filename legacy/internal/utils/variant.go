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
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// VariantFilter is a function that determines if a VA should be included.
type VariantFilter func(scaletarget.ScaleTargetAccessor) bool

// ActiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func ActiveVariantAutoscalingByModel(ctx context.Context, client client.Client) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := ActiveVariantAutoscaling(ctx, client)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// InactiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func InactiveVariantAutoscalingByModel(ctx context.Context, client client.Client) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := InactiveVariantAutoscaling(ctx, client)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// GroupVariantAutoscalingByModel groups VariantAutoscalings by model ID and namespace.
// Variants of the same model on different accelerators are grouped together to enable
// cost-based optimization (scale up cheaper variants, scale down expensive variants).
// The key format is "modelID|namespace".
func GroupVariantAutoscalingByModel(
	vas []wvav1alpha1.VariantAutoscaling,
) map[string][]wvav1alpha1.VariantAutoscaling {
	groups := make(map[string][]wvav1alpha1.VariantAutoscaling)
	for _, va := range vas {
		// Use modelID + namespace as key to group all variants of same model
		key := va.Spec.ModelID + "|" + va.Namespace
		groups[key] = append(groups[key], va)
	}
	return groups
}

// ActiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func ActiveVariantAutoscaling(ctx context.Context, client client.Client) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, isActive, "active")
}

// InactiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func InactiveVariantAutoscaling(ctx context.Context, client client.Client) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, isInactive, "inactive")
}

// filterVariantsByScaleTargetAccessors is a generic function to filter VAs based on scaleTarget state.
// Returns filtered VAs and a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func filterVariantsByScaleTargetAccessor(ctx context.Context, client client.Client, filter VariantFilter, filterName string) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	readyVAs := readyVariantAutoscalings(ctx, client)

	filteredVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(readyVAs))
	scaleTargetAccessors := make(map[string]scaletarget.ScaleTargetAccessor)

	for _, va := range readyVAs {
		// Check if the context is done
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		// Skip VAs without scaleTargetRef (required to know which deployment to look up)
		// TODO: Remove this check once scaleTargetRef.name is made a required field in the CRD.
		// This defensive check exists because the CRD currently allows empty scaleTargetRef,
		// but it should be enforced at the schema level instead.
		if va.Spec.ScaleTargetRef.Name == "" {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping VA without scaleTargetRef", "namespace", va.Namespace, "name", va.Name)
			continue
		}

		scaleTargetName := va.Spec.ScaleTargetRef.Name
		scaleTargetAccessor, err := scaletarget.FetchScaleTarget(ctx, client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Deployment/LWS doesn't exist yet, this is expected for VAs without corresponding scale targets
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Scale target not found for VariantAutoscaling, skipping",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			} else {
				// Unexpected error (permissions, network issues, etc.)
				ctrl.LoggerFrom(ctx).Error(err, "Failed to get scale target",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			}
			continue
		}

		// Skip deleted scaleTargetAccessor
		if scaleTargetAccessor.GetDeletionTimestamp() != nil && !scaleTargetAccessor.GetDeletionTimestamp().IsZero() {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping deleted scale target", "namespace", va.Namespace, "scaleTargetName", scaleTargetName)
			continue
		}

		// Apply the filter function
		if filter(scaleTargetAccessor) {
			filteredVAs = append(filteredVAs, va)
			// Store scaleTargetAccessor in map using namespace/scaleTargetName as key
			key := GetNamespacedKey(va.Namespace, scaleTargetName)
			scaleTargetAccessors[key] = scaleTargetAccessor
		}
	}
	ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Found filtered VariantAutoscaling resources",
		"filterType", filterName,
		"count", len(filteredVAs))

	return filteredVAs, scaleTargetAccessors, nil
}

// readyVariantAutoscalings retrieves all variants ready for optimization by
// synthesizing in-memory VariantAutoscaling objects from annotated ScaledObjects
// and HPAs (annotation-based discovery). When CONTROLLER_INSTANCE is configured,
// only variants whose source object carries a matching controller-instance label
// are returned, enabling multi-controller isolation.
//
// Errors from annotation discovery are non-fatal (logged), so this always returns
// whatever was discovered and never an error.
func readyVariantAutoscalings(ctx context.Context, k8sClient client.Client) []wvav1alpha1.VariantAutoscaling {
	logger := ctrl.LoggerFrom(ctx)

	annotated, err := annotationSourcedVariants(ctx, k8sClient)
	if err != nil {
		// Non-fatal: log and continue with whatever was discovered.
		logger.Error(err, "Error while listing annotation-sourced variants (non-fatal)")
	}

	controllerInstance := metrics.GetControllerInstance()
	readyVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(annotated))
	for _, va := range annotated {
		// Filter by controller-instance label for multi-controller isolation.
		if controllerInstance != "" && va.Labels[constants.ControllerInstanceLabelKey] != controllerInstance {
			continue
		}
		readyVAs = append(readyVAs, va)
	}

	logger.V(logging.DEBUG).Info("Found variants ready for optimization",
		"count", len(readyVAs),
		"controllerInstance", controllerInstance)

	return readyVAs
}

// annotationSourcedVariants lists HPAs and KEDA ScaledObjects bearing llm-d.ai/managed: "true"
// and synthesizes in-memory VariantAutoscaling objects from them. ScaledObject discovery is
// skipped gracefully when the KEDA CRD is not installed. When both an HPA and a ScaledObject
// target the same scale target, the ScaledObject entry wins.
func annotationSourcedVariants(ctx context.Context, k8sClient client.Client) ([]wvav1alpha1.VariantAutoscaling, error) {
	logger := ctrl.LoggerFrom(ctx)
	// keyed by namespace/kind/name for deduplication; ScaledObject entries overwrite HPA entries.
	byTarget := make(map[string]wvav1alpha1.VariantAutoscaling)

	// HPAs are a core Kubernetes type — always available (lower priority for deduplication).
	// TODO(#1134): scope to tracked namespaces only (client.InNamespace per ds.ListTrackedNamespaces())
	// to avoid iterating the full cluster cache on every engine tick.
	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := k8sClient.List(ctx, &hpaList); err != nil {
		return nil, fmt.Errorf("listing HPAs: %w", err)
	}
	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if !annotations.IsManaged(hpa) || !hpa.DeletionTimestamp.IsZero() {
			continue
		}
		va, err := annotations.VariantAutoscalingFromHPA(hpa)
		if err != nil {
			logger.V(logging.DEBUG).Info("Skipping HPA with invalid WVA annotations",
				"namespace", hpa.Namespace, "name", hpa.Name, "error", err)
			continue
		}
		key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
		byTarget[key] = *va
	}

	// KEDA ScaledObjects — may not be installed; handle gracefully.
	// ScaledObject takes precedence over HPA for the same scale target.
	// TODO(#1134): scope to tracked namespaces only (client.InNamespace per ds.ListTrackedNamespaces())
	// to avoid iterating the full cluster cache on every engine tick.
	var soList kedav1alpha1.ScaledObjectList
	if err := k8sClient.List(ctx, &soList); err != nil {
		if apimeta.IsNoMatchError(err) {
			logger.V(logging.DEBUG).Info("KEDA ScaledObject CRD not available, skipping annotation discovery for ScaledObjects")
		} else {
			result := make([]wvav1alpha1.VariantAutoscaling, 0, len(byTarget))
			for _, va := range byTarget {
				result = append(result, va)
			}
			return result, fmt.Errorf("listing ScaledObjects: %w", err)
		}
	} else {
		for i := range soList.Items {
			so := &soList.Items[i]
			if !annotations.IsManaged(so) || !so.DeletionTimestamp.IsZero() {
				continue
			}
			va, err := annotations.VariantAutoscalingFromScaledObject(so)
			if err != nil {
				logger.V(logging.DEBUG).Info("Skipping ScaledObject with invalid WVA annotations",
					"namespace", so.Namespace, "name", so.Name, "error", err)
				continue
			}
			key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
			byTarget[key] = *va
		}
	}

	result := make([]wvav1alpha1.VariantAutoscaling, 0, len(byTarget))
	for _, va := range byTarget {
		result = append(result, va)
	}
	return result, nil
}

// isActive explicitly requires that replicas > 0
func isActive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) > 0
}

// isInactive explicitly requires that replicas == 0
func isInactive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) == 0
}

// Helper function makes behavior explicit
func GetDesiredReplicas(scaleTargetAccessor scaletarget.ScaleTargetAccessor) int32 {
	if scaleTargetAccessor.GetReplicas() == nil {
		return 1 // Kubernetes default
	}
	return *scaleTargetAccessor.GetReplicas()
}

// GetNamespacedKey is a helper for building namespaced resource keys.
func GetNamespacedKey(namespace, name string) string {
	return namespace + "/" + name
}
