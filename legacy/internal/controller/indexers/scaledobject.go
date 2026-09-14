package indexers

import (
	"context"
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ScaledObjectByScaleTargetKey indexes managed KEDA ScaledObjects by their
// spec.scaleTargetRef. Index value: scaleTargetIndexKey(namespace, ref).
const ScaledObjectByScaleTargetKey = ".spec.scaleTargetRef.managedSO"

// ScaledObjectByScaleTargetIndexFunc indexes managed ScaledObjects
// (llm-d.ai/managed=true) by their scaleTargetRef.
func ScaledObjectByScaleTargetIndexFunc(o client.Object) []string {
	so := o.(*kedav1alpha1.ScaledObject)
	if !annotations.IsManaged(so) {
		return nil
	}
	if so.Spec.ScaleTargetRef == nil || so.Spec.ScaleTargetRef.Name == "" {
		return nil
	}
	kind := so.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = constants.DeploymentKind
	}
	if !scaleTargetKindSupported(kind) {
		return nil
	}
	apiVersion := so.Spec.ScaleTargetRef.APIVersion
	ref := autoscalingv2.CrossVersionObjectReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       so.Spec.ScaleTargetRef.Name,
	}
	return []string{scaleTargetIndexKey(so.Namespace, ref)}
}

// FindSOForScaleTarget returns the managed ScaledObject targeting the given
// scale resource, or nil if none. Errors when more than one managed
// ScaledObject targets the same resource.
func FindSOForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*kedav1alpha1.ScaledObject, error) {
	var list kedav1alpha1.ScaledObjectList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFields{ScaledObjectByScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("list managed ScaledObjects for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple managed ScaledObjects target %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &list.Items[0], nil
}
