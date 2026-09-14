package indexers

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HPAByScaleTargetKey indexes managed HorizontalPodAutoscalers by their
// spec.scaleTargetRef. Index value: scaleTargetIndexKey(namespace, ref).
const HPAByScaleTargetKey = ".spec.scaleTargetRef.managedHPA"

// scaleTargetKindSupported reports whether a scaleTargetRef.Kind is one of the
// kinds WVA's locator handles. Today: Deployment and LeaderWorkerSet.
func scaleTargetKindSupported(kind string) bool {
	return kind == constants.DeploymentKind || kind == constants.LeaderWorkerSetKind
}

// HPAByScaleTargetIndexFunc indexes managed HPAs (llm-d.ai/managed=true) by
// their scaleTargetRef. Returns no entries for unmanaged HPAs or unsupported
// scale-target kinds.
func HPAByScaleTargetIndexFunc(o client.Object) []string {
	hpa := o.(*autoscalingv2.HorizontalPodAutoscaler)
	if !annotations.IsManaged(hpa) {
		return nil
	}
	// Exclude KEDA-generated child HPAs. KEDA creates an HPA per ScaledObject and
	// propagates the ScaledObject's annotations (including llm-d.ai/managed) onto it,
	// so the child HPA looks managed too. The ScaledObject is the real managed scaler;
	// indexing its child HPA would make the locator match two managed scalers for one
	// target and fail with "ambiguous scale target".
	if utils.IsOwnedByKEDAScaledObject(hpa) {
		return nil
	}
	ref := hpa.Spec.ScaleTargetRef
	if ref.Name == "" || !scaleTargetKindSupported(ref.Kind) {
		return nil
	}
	return []string{scaleTargetIndexKey(hpa.Namespace, ref)}
}

// FindHPAForScaleTarget returns the managed HPA targeting the given scale
// resource, or nil if none. Errors when more than one managed HPA targets
// the same resource.
func FindHPAForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{HPAByScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("list managed HPAs for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple managed HPAs target %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &list.Items[0], nil
}
