package coordinator

import (
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// IsHPAUnderControl reports whether an HPA is under Coordinator control.
//
// An HPA qualifies iff all of:
//  1. It carries the canonical llm-d.ai/managed: "true" annotation.
//  2. Its spec.metrics does NOT include the wva_desired_replicas external
//     metric. When that metric is present, the WVA engine is already
//     steering replicas via the metric pipeline and the Coordinator must
//     stay out.
//  3. It is NOT owned by a KEDA ScaledObject. KEDA generates an HPA per
//     ScaledObject and reconciles spec.maxReplicas from the SO's
//     spec.maxReplicaCount; writes to such an HPA would be reverted on
//     KEDA's next reconcile. KEDA-generated HPAs reach plugins only via
//     their parent ScaledObject.
func IsHPAUnderControl(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	if hpa == nil {
		return false
	}
	if !annotations.IsManaged(hpa) {
		return false
	}
	if hpaHasWVADesiredReplicasMetric(hpa) {
		return false
	}
	if utils.IsOwnedByKEDAScaledObject(hpa) {
		return false
	}
	return true
}

// IsScaledObjectUnderControl reports whether a KEDA ScaledObject is under
// Coordinator control.
//
// A ScaledObject qualifies iff both of:
//  1. It carries the canonical llm-d.ai/managed: "true" annotation.
//  2. Its spec.triggers does NOT include a Prometheus trigger whose
//     metadata.query references wva_desired_replicas. Same intent as the
//     HPA rule: the Coordinator must not act when the WVA engine is
//     already steering this target via that metric.
func IsScaledObjectUnderControl(so *kedav1alpha1.ScaledObject) bool {
	if so == nil {
		return false
	}
	if !annotations.IsManaged(so) {
		return false
	}
	if scaledObjectHasWVADesiredReplicasTrigger(so) {
		return false
	}
	return true
}

// hpaHasWVADesiredReplicasMetric returns true if the HPA's spec.metrics
// contains an External metric named WVADesiredReplicas
// ("wva_desired_replicas").
func hpaHasWVADesiredReplicasMetric(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	for i := range hpa.Spec.Metrics {
		m := &hpa.Spec.Metrics[i]
		if m.Type != autoscalingv2.ExternalMetricSourceType || m.External == nil {
			continue
		}
		if m.External.Metric.Name == constants.WVADesiredReplicas {
			return true
		}
	}
	return false
}

// scaledObjectHasWVADesiredReplicasTrigger returns true if any Prometheus
// trigger on the ScaledObject queries wva_desired_replicas.
func scaledObjectHasWVADesiredReplicasTrigger(so *kedav1alpha1.ScaledObject) bool {
	for i := range so.Spec.Triggers {
		t := &so.Spec.Triggers[i]
		if t.Type != "prometheus" {
			continue
		}
		query := t.Metadata["query"]
		if query == "" {
			continue
		}
		if strings.Contains(query, constants.WVADesiredReplicas) {
			return true
		}
	}
	return false
}
