package accelerator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func TestGetAcceleratorNameFromScaleTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		va         *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		deployment *appsv1.Deployment
		expected   string
	}{
		{
			name: "nvidia_gpu_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "Tesla-T4",
							},
						},
					},
				},
			},
			expected: "Tesla-T4",
		},
		{
			name: "amd_gpu_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"amd.com/gpu.product-name": "MI250",
							},
						},
					},
				},
			},
			expected: "MI250",
		},
		{
			name: "gke_accelerator_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"cloud.google.com/gke-accelerator": "nvidia-tesla-v100",
							},
						},
					},
				},
			},
			expected: "nvidia-tesla-v100",
		},
		{
			name: "intel_gaudi_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"habana.ai/product.name": "Intel-Gaudi-2-96GB",
							},
						},
					},
				},
			},
			expected: "Intel-Gaudi-2-96GB",
		},
		{
			name: "intel_gpu_from_nodeSelector",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"gpu.intel.com/product": "Max_1100",
							},
						},
					},
				},
			},
			expected: "Max_1100",
		},
		{
			name: "nvidia_gpu_from_required_nodeAffinity",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"A100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "amd_gpu_from_preferred_nodeAffinity",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
										{
											Weight: 1,
											Preference: corev1.NodeSelectorTerm{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "amd.com/gpu.product-name",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"MI300"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "MI300",
		},
		{
			name: "fallback_to_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: "H100",
		},
		{
			name: "nodeSelector_takes_precedence_over_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "A100",
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "nodeAffinity_takes_precedence_over_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"V100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "V100",
		},
		{
			name: "nil_deployment_with_va_label",
			va: &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						AcceleratorNameLabel: "T4",
					},
				},
			},
			deployment: nil,
			expected:   "T4",
		},
		{
			name:       "nil_va_and_deployment_returns_default",
			va:         nil,
			deployment: nil,
			expected:   constants.DefaultAcceleratorName,
		},
		{
			name: "no_gpu_info_returns_default",
			va:   &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: constants.DefaultAcceleratorName,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var scaleTarget scaletarget.ScaleTargetAccessor
			if tc.deployment != nil {
				scaleTarget = scaletarget.NewDeploymentAccessor(tc.deployment)
			}
			result := GetAcceleratorNameFromScaleTarget(tc.va, scaleTarget)
			assert.Equal(t, tc.expected, result)
		})
	}
}
