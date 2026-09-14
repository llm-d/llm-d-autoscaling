package accelerator

import (
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// AcceleratorNameLabel is the label key used to specify the accelerator name for a VA.
const AcceleratorNameLabel = "inference.optimization/acceleratorName"

// NormalizeAcceleratorName converts a full GPU model name to a short name.
// This enables matching between VA labels (e.g., "A100") and discovery results
// (e.g., "NVIDIA-A100-PCIE-80GB").
//
// Examples:
//   - "NVIDIA-A100-PCIE-80GB" -> "A100"
//   - "NVIDIA-H100-SXM5-80GB" -> "H100"
//   - "AMD-MI300X-192G" -> "MI300X"
//   - "Intel-Gaudi-2-96GB" -> "Gaudi-2"
//   - "A100" -> "A100" (already short)
func NormalizeAcceleratorName(fullName string) string {
	// If already a short name (no hyphens or known pattern), return as-is
	if !strings.Contains(fullName, "-") {
		return fullName
	}

	// Common patterns for GPU model names:
	// NVIDIA-{model}-{variant} -> extract {model}
	// AMD-{model}-{memory} -> extract {model}
	// Intel-{model}-{memory} -> extract {model}

	parts := strings.Split(fullName, "-")
	if len(parts) < 2 {
		return fullName
	}

	// Check for known vendor prefixes
	vendor := strings.ToUpper(parts[0])
	switch vendor {
	case "NVIDIA":
		// NVIDIA-A100-PCIE-80GB -> A100
		// NVIDIA-H100-SXM5-80GB -> H100
		if len(parts) >= 2 {
			return parts[1]
		}
	case "AMD":
		// AMD-MI300X-192G -> MI300X
		if len(parts) >= 2 {
			return parts[1]
		}
	case "INTEL":
		// Intel-Gaudi-2-96GB -> Gaudi-2
		if len(parts) >= 3 {
			return parts[1] + "-" + parts[2]
		}
		if len(parts) >= 2 {
			return parts[1]
		}
	}

	// Fallback: return the second part (after vendor)
	return parts[1]
}

// GetProductKeys returns unique vendor product (node label) keys, in stable (sorted) order
func GetProductKeys() []string {
	labels := make(map[string]bool, len(constants.VendorResources))
	for _, res := range constants.VendorResources {
		labels[res.ProductLabel] = true
		for _, label := range res.ProductLabelAliases {
			labels[label] = true
		}
	}
	return slices.Sorted(maps.Keys(labels))
}

// GetAcceleratorNameFromScaleTarget extracts GPU product information from a scale target's nodeSelector or nodeAffinity.
// GPU product information is checked against keys listed in constants.VendorResources.
// If not found in nodeSelector or nodeAffinity, falls back to the AcceleratorNameLabel on the VariantAutoscaling.
// Returns the first matching value found, or constants.DefaultAcceleratorName ("unknown") if none are found.
// The sentinel allows callers to proceed without hard-stopping; the GPU limiter resolves
// it to the real type in homogeneous clusters before it reaches status or metrics.
func GetAcceleratorNameFromScaleTarget(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	// Check scaleTarget for accelerator name if it's not nil
	if scaleTarget != nil {
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec != nil {
			prodKeys := GetProductKeys()

			// Check nodeSelector first
			if podTemplateSpec.Spec.NodeSelector != nil {
				for _, key := range prodKeys {
					if val, ok := podTemplateSpec.Spec.NodeSelector[key]; ok {
						return val
					}
				}
			}

			// Check nodeAffinity
			if podTemplateSpec.Spec.Affinity != nil && podTemplateSpec.Spec.Affinity.NodeAffinity != nil {
				if val := extractGPUFromNodeAffinity(podTemplateSpec.Spec.Affinity.NodeAffinity, prodKeys); val != "" {
					return val
				}
			}
		}
	}

	// Fall back to VariantAutoscaling label
	if va != nil && va.Labels != nil {
		if accName, exists := va.Labels[AcceleratorNameLabel]; exists {
			return accName
		}
	}
	return constants.DefaultAcceleratorName
}

// extractGPUFromNodeAffinity extracts GPU product information from NodeAffinity.
// It checks both required and preferred node affinity terms for the given GPU keys.
func extractGPUFromNodeAffinity(nodeAffinity *corev1.NodeAffinity, gpuKeys []string) string {
	// Check required node affinity
	if nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if val := extractGPUFromNodeSelectorTerm(term, gpuKeys); val != "" {
				return val
			}
		}
	}

	// Check preferred node affinity
	if nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		for _, preferred := range nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if val := extractGPUFromNodeSelectorTerm(preferred.Preference, gpuKeys); val != "" {
				return val
			}
		}
	}

	return ""
}

// extractGPUFromNodeSelectorTerm extracts GPU product from a NodeSelectorTerm.
// It checks MatchExpressions for the given GPU keys with "In" or "Exists" operators.
func extractGPUFromNodeSelectorTerm(term corev1.NodeSelectorTerm, gpuKeys []string) string {
	for _, expr := range term.MatchExpressions {
		for _, key := range gpuKeys {
			if expr.Key == key {
				// For "In" operator, return the first value
				if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
					return expr.Values[0]
				}
				// For "Exists" operator, we found the key but no specific value
				// Continue searching for other keys that might have values
			}
		}
	}
	return ""
}
