package utils

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// IsOwnedByKEDAScaledObject reports whether obj has a controller OwnerReference
// whose APIVersion is in the keda.sh API group and whose Kind is "ScaledObject".
//
// KEDA generates an HPA per ScaledObject and propagates the ScaledObject's
// annotations (including llm-d.ai/managed) onto that child HPA. Callers that scan
// for WVA-managed scalers must exclude such KEDA-generated HPAs: the ScaledObject
// is the managed scaler, and treating its child HPA as a second managed scaler for
// the same target is ambiguous.
func IsOwnedByKEDAScaledObject(obj metav1.Object) bool {
	ctrl := metav1.GetControllerOf(obj)
	if ctrl == nil {
		return false
	}
	if ctrl.Kind != constants.ScaledObjectKind {
		return false
	}
	// APIVersion is "<group>/<version>"; match the group prefix.
	if i := strings.IndexByte(ctrl.APIVersion, '/'); i > 0 {
		return ctrl.APIVersion[:i] == constants.ScaledObjectAPIGroup
	}
	return ctrl.APIVersion == constants.ScaledObjectAPIGroup
}
