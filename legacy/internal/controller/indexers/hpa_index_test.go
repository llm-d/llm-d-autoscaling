package indexers

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// A KEDA-generated HPA inherits the ScaledObject's llm-d.ai/managed annotation
// (kedacore/keda#5468). It must NOT be indexed as a managed scaler, or the
// collector's locator sees two managed scalers for one target and fails with
// "ambiguous scale target". Regression for #1333.
func TestHPAByScaleTargetIndexFunc_SkipsKEDAOwnedHPA(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "keda-hpa-my-model",
			Namespace:   "inference",
			Annotations: map[string]string{annotations.Managed: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: constants.ScaledObjectAPIGroup + "/v1alpha1",
				Kind:       constants.ScaledObjectKind,
				Name:       "my-model",
				Controller: ptr.To(true),
			}},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: constants.DeploymentKind,
				Name: "my-model",
			},
		},
	}

	if got := HPAByScaleTargetIndexFunc(hpa); got != nil {
		t.Errorf("KEDA-owned managed HPA must not be indexed, got %v", got)
	}

	// Sanity: a managed HPA that is NOT owned by a ScaledObject is still indexed.
	hpa.OwnerReferences = nil
	if got := HPAByScaleTargetIndexFunc(hpa); len(got) != 1 {
		t.Errorf("managed non-KEDA HPA should be indexed, got %v", got)
	}
}
