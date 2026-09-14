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

package scaletarget

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeploymentAccessor_GetReplicas(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   *int32
	}{
		{
			name: "deployment with replicas set",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(5),
				},
			},
			expected: int32Ptr(5),
		},
		{
			name: "deployment with zero replicas",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(0),
				},
			},
			expected: int32Ptr(0),
		},
		{
			name: "deployment with nil replicas",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			result := accessor.GetReplicas()
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

func TestDeploymentAccessor_GetStatusReplicas(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   int32
	}{
		{
			name: "deployment with status replicas",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas: 10,
				},
			},
			expected: 10,
		},
		{
			name: "deployment with zero status replicas",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas: 0,
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			result := accessor.GetStatusReplicas()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeploymentAccessor_GetStatusReadyReplicas(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   int32
	}{
		{
			name: "deployment with some ready replicas",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:      10,
					ReadyReplicas: 7,
				},
			},
			expected: 7,
		},
		{
			name: "deployment with all replicas ready",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:      5,
					ReadyReplicas: 5,
				},
			},
			expected: 5,
		},
		{
			name: "deployment with no ready replicas",
			deployment: &appsv1.Deployment{
				Status: appsv1.DeploymentStatus{
					Replicas:      5,
					ReadyReplicas: 0,
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			result := accessor.GetStatusReadyReplicas()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeploymentAccessor_GetTotalGPUsPerReplica(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   int
	}{
		{
			name: "single container with NVIDIA GPUs",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"nvidia.com/gpu": resource.MustParse("2"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 2,
		},
		{
			name: "single container with AMD GPUs",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"amd.com/gpu": resource.MustParse("4"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 4,
		},
		{
			name: "single container with Intel GPUs",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"gpu.intel.com/xe": resource.MustParse("3"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 3,
		},
		{
			name: "multiple containers with GPUs",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "container1",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"nvidia.com/gpu": resource.MustParse("2"),
										},
									},
								},
								{
									Name: "container2",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"nvidia.com/gpu": resource.MustParse("3"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 5,
		},
		{
			name: "mixed GPU vendors across containers",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "nvidia-container",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"nvidia.com/gpu": resource.MustParse("2"),
										},
									},
								},
								{
									Name: "amd-container",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"amd.com/gpu": resource.MustParse("1"),
										},
									},
								},
								{
									Name: "intel-container",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"gpu.intel.com/xe": resource.MustParse("1"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 4,
		},
		{
			name: "no GPU resources defaults to 1",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"cpu":    resource.MustParse("1"),
											"memory": resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 1,
		},
		{
			name: "empty containers defaults to 1",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{},
						},
					},
				},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			result := accessor.GetTotalGPUsPerReplica()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeploymentAccessor_GetDeletionTimestamp(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   *metav1.Time
	}{
		{
			name: "deployment with deletion timestamp",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
			},
			expected: &now,
		},
		{
			name: "deployment without deletion timestamp",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: nil,
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			result := accessor.GetDeletionTimestamp()
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.expected.Unix(), result.Unix())
			}
		})
	}
}

func TestDeploymentAccessor_GetLeaderPodTemplateSpec(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		validate   func(t *testing.T, spec *corev1.PodTemplateSpec)
	}{
		{
			name: "deployment with pod template",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "test:latest"},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				require.NotNil(t, spec)
				assert.Equal(t, "test", spec.Labels["app"])
				assert.Equal(t, 1, len(spec.Spec.Containers))
				assert.Equal(t, "main", spec.Spec.Containers[0].Name)
			},
		},
		{
			name:       "nil deployment returns empty spec",
			deployment: nil,
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				// Not called since accessor is nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			if tt.deployment == nil {
				assert.Nil(t, accessor)
				return
			}
			result := accessor.GetLeaderPodTemplateSpec()
			tt.validate(t, result)
		})
	}
}

func TestDeploymentAccessor_GetWorkerPodTemplateSpec(t *testing.T) {
	deployment := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "test:latest"},
					},
				},
			},
		},
	}

	accessor := NewDeploymentAccessor(deployment)

	// For Deployment, worker and leader should be the same pointer
	leader := accessor.GetLeaderPodTemplateSpec()
	worker := accessor.GetWorkerPodTemplateSpec()

	require.NotNil(t, leader)
	require.NotNil(t, worker)
	assert.Equal(t, leader, worker, "For Deployment, GetWorkerPodTemplateSpec should return the same pointer as GetLeaderPodTemplateSpec")
}

func TestDeploymentAccessor_GetGroupSize(t *testing.T) {
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		expected   int32
	}{
		{
			name: "deployment always has group size 1",
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(10),
				},
			},
			expected: 1,
		},
		{
			name:       "nil deployment has group size 1",
			deployment: nil,
			expected:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewDeploymentAccessor(tt.deployment)
			if tt.deployment == nil {
				assert.Nil(t, accessor)
				return
			}
			result := accessor.GetGroupSize()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeploymentAccessor_GetObject(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deployment",
			Namespace: "default",
		},
	}

	accessor := NewDeploymentAccessor(deployment)

	assert.Equal(t, "test-deployment", accessor.GetName())
	assert.Equal(t, "default", accessor.GetNamespace())
}

func TestDeploymentAccessor_GetName_GetNamespace_Nil(t *testing.T) {
	accessor := NewDeploymentAccessor(nil)
	assert.Nil(t, accessor)
}
