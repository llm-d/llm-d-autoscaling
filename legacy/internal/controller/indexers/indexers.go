/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// scaleTargetIndexKey returns the composite index key for a scale target reference.
// Format: Namespace/APIVersion/Kind/Name (e.g., "default/apps/v1/Deployment/my-app").
// Shared by all per-resource index files (variantautoscaling.go, hpa.go, scaledobject.go).
func scaleTargetIndexKey(namespace string, ref autoscalingv2.CrossVersionObjectReference) string {
	if ref.APIVersion == "" {
		switch ref.Kind {
		case constants.DeploymentKind:
			ref.APIVersion = constants.DeploymentAPIVersion
		case constants.LeaderWorkerSetKind:
			ref.APIVersion = constants.LeaderWorkerSetAPIVersion
		default:
			logger := ctrl.LoggerFrom(context.TODO())
			logger.V(logging.DEBUG).Info("APIVersion not specified for scale target; defaulting to apps/v1", "kind", ref.Kind, "name", ref.Name)
			ref.APIVersion = constants.DeploymentAPIVersion
		}
	}
	return fmt.Sprintf("%s/%s/%s/%s", namespace, ref.APIVersion, ref.Kind, ref.Name)
}

// SetupIndexes registers custom field indexes with the manager's cache.
// kedaEnabled controls whether the ScaledObject index is registered; set to false when KEDA CRDs are not installed.
func SetupIndexes(ctx context.Context, mgr manager.Manager, kedaEnabled bool) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &autoscalingv2.HorizontalPodAutoscaler{}, HPAByScaleTargetKey, HPAByScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for HPA: %w", err)
	}
	if kedaEnabled {
		if err := mgr.GetFieldIndexer().IndexField(ctx, &kedav1alpha1.ScaledObject{}, ScaledObjectByScaleTargetKey, ScaledObjectByScaleTargetIndexFunc); err != nil {
			return fmt.Errorf("failed to set up index by scale target for ScaledObject: %w", err)
		}
	}
	return nil
}
