package scaletarget

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

func FetchScaleTarget(ctx context.Context, c client.Client, vaName, kind, name, namespace string) (ScaleTargetAccessor, error) {
	switch kind {
	case constants.DeploymentKind, "": // matching "" for backward compatibility
		var deployment appsv1.Deployment
		if err := resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, &deployment, constants.StandardBackoff, kind); err != nil {
			if apierrors.IsNotFound(err) {
				// Deployment doesn't exist yet, this is expected for VAs without corresponding deployments
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Deployment not found for VariantAutoscaling, skipping",
					"namespace", namespace,
					"deploymentName", name,
					"vaName", vaName)
			} else {
				// Unexpected error (permissions, network issues, etc.)
				ctrl.LoggerFrom(ctx).Error(err, "Failed to get deployment",
					"namespace", namespace,
					"deploymentName", name,
					"vaName", vaName)
			}
			return nil, err
		}
		return NewDeploymentAccessor(&deployment), nil
	case constants.LeaderWorkerSetKind:
		var lws lwsv1.LeaderWorkerSet
		if err := resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, &lws, constants.StandardBackoff, kind); err != nil {
			if apierrors.IsNotFound(err) {
				// LWS doesn't exist yet, this is expected for VAs without corresponding LWSs
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("LWS not found for VariantAutoscaling, skipping",
					"namespace", namespace,
					"leaderWorkerSetName", name,
					"vaName", vaName)
			} else {
				// Unexpected error (permissions, network issues, etc.)
				ctrl.LoggerFrom(ctx).Error(err, "Failed to get leaderWorkerSet",
					"namespace", namespace,
					"leaderWorkerSetName", name,
					"vaName", vaName)
			}
			return nil, err
		}
		return NewLWSAccessor(&lws), nil
	}
	return nil, fmt.Errorf("invalid scale target kind %q", kind)
}
