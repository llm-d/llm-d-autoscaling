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

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestLWSAccessor_GetReplicas(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected *int32
	}{
		{
			name: "lws with replicas set",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					Replicas: int32Ptr(5),
				},
			},
			expected: int32Ptr(5),
		},
		{
			name: "lws with zero replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					Replicas: int32Ptr(0),
				},
			},
			expected: int32Ptr(0),
		},
		{
			name: "lws with nil replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					Replicas: nil,
				},
			},
			expected: nil,
		},
		{
			name:     "nil lws",
			lws:      nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			if tt.lws == nil {
				assert.Nil(t, accessor)
				return
			}
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

func TestLWSAccessor_GetStatusReplicas(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected int32
	}{
		{
			name: "lws with status replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Status: lwsv1.LeaderWorkerSetStatus{
					Replicas: 10,
				},
			},
			expected: 10,
		},
		{
			name: "lws with zero status replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Status: lwsv1.LeaderWorkerSetStatus{
					Replicas: 0,
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			result := accessor.GetStatusReplicas()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLWSAccessor_GetStatusReadyReplicas(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected int32
	}{
		{
			name: "lws with some ready replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Status: lwsv1.LeaderWorkerSetStatus{
					Replicas:      10,
					ReadyReplicas: 7,
				},
			},
			expected: 7,
		},
		{
			name: "lws with all replicas ready",
			lws: &lwsv1.LeaderWorkerSet{
				Status: lwsv1.LeaderWorkerSetStatus{
					Replicas:      5,
					ReadyReplicas: 5,
				},
			},
			expected: 5,
		},
		{
			name: "lws with no ready replicas",
			lws: &lwsv1.LeaderWorkerSet{
				Status: lwsv1.LeaderWorkerSetStatus{
					Replicas:      5,
					ReadyReplicas: 0,
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			result := accessor.GetStatusReadyReplicas()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLWSAccessor_GetTotalGPUsPerReplica(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected int
	}{
		{
			name: "leader with 2 GPUs, 3 workers with 1 GPU each (size=4)",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(4), // 1 leader + 3 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("2"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 5, // 2 + (4-1)*1 = 2 + 3 = 5
		},
		{
			name: "leader with 4 GPUs, 7 workers with 8 GPUs each (size=8)",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(8), // 1 leader + 7 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("4"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("8"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 60, // 4 + (8-1)*8 = 4 + 56 = 60
		},
		{
			name: "nil leader template, workers have GPUs",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size:           int32Ptr(3), // 1 leader + 2 workers
						LeaderTemplate: nil,         // nil leader template
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
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
			},
			expected: 4, // 0 (no leader GPUs) + (3-1)*2 = 0 + 4 = 4
		},
		{
			name: "leader with no GPUs, workers have GPUs",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(5), // 1 leader + 4 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"cpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
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
			},
			expected: 8, // 0 (no leader GPUs) + (5-1)*2 = 0 + 8 = 8
		},
		{
			name: "leader has GPUs, workers have no GPUs",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(4), // 1 leader + 3 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("8"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"cpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 8, // 8 + (4-1)*0 = 8
		},
		{
			name: "CPU-only leader and workers default to 1",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(3), // 1 leader + 2 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"cpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"cpu": resource.MustParse("1"),
											},
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
			name: "size 1 (only leader, no workers)",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(1), // only leader
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("4"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker"},
								},
							},
						},
					},
				},
			},
			expected: 4, // 4 + (1-1)*0 = 4
		},
		{
			name: "multiple containers in leader",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(2), // 1 leader + 1 worker
						LeaderTemplate: &corev1.PodTemplateSpec{
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
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 6, // (2+3) + (2-1)*1 = 5 + 1 = 6
		},
		{
			name: "mixed GPU vendors",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(3), // 1 leader + 2 workers
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "leader",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"nvidia.com/gpu": resource.MustParse("2"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "worker",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												"amd.com/gpu": resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: 4, // 2 + (3-1)*1 = 2 + 2 = 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			result := accessor.GetTotalGPUsPerReplica()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLWSAccessor_GetTotalGPUsPerReplica_SupportedGPUResources(t *testing.T) {
	tests := []struct {
		name         string
		resourceName corev1.ResourceName
		size         int32
		leaderGPUs   string
		workerGPUs   string
		expected     int
	}{
		{
			name:         "NVIDIA GPUs",
			resourceName: "nvidia.com/gpu",
			size:         4,
			leaderGPUs:   "2",
			workerGPUs:   "1",
			expected:     5,
		},
		{
			name:         "AMD GPUs",
			resourceName: "amd.com/gpu",
			size:         3,
			leaderGPUs:   "2",
			workerGPUs:   "3",
			expected:     8,
		},
		{
			name:         "Intel Gaudi GPUs",
			resourceName: "habana.ai/gaudi",
			size:         4,
			leaderGPUs:   "1",
			workerGPUs:   "2",
			expected:     7,
		},
		{
			name:         "Intel i915 GPUs",
			resourceName: "gpu.intel.com/i915",
			size:         2,
			leaderGPUs:   "1",
			workerGPUs:   "2",
			expected:     3,
		},
		{
			name:         "Intel Xe GPUs",
			resourceName: "gpu.intel.com/xe",
			size:         5,
			leaderGPUs:   "3",
			workerGPUs:   "1",
			expected:     7,
		},
	}

	assert.Len(t, tests, len(constants.VendorResources), "add a row when constants.VendorResources changes")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lws := lwsWithGPURequests(tt.size, tt.resourceName, tt.leaderGPUs, tt.workerGPUs)

			accessor := NewLWSAccessor(lws)
			result := accessor.GetTotalGPUsPerReplica()

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLWSAccessor_GetTotalGPUsPerReplica_MixedSupportedGPUResources(t *testing.T) {
	// Synthetic case: verify summation across recognized GPU resource names,
	// not a recommended production topology.
	lws := &lwsv1.LeaderWorkerSet{
		Spec: lwsv1.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size: int32Ptr(3), // 1 leader + 2 workers
				LeaderTemplate: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							containerWithGPURequest("leader-nvidia", "nvidia.com/gpu", "2"),
							containerWithGPURequest("leader-gaudi", "habana.ai/gaudi", "1"),
						},
					},
				},
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							containerWithGPURequest("worker-amd", "amd.com/gpu", "1"),
							containerWithGPURequest("worker-xe", "gpu.intel.com/xe", "2"),
						},
					},
				},
			},
		},
	}

	accessor := NewLWSAccessor(lws)
	result := accessor.GetTotalGPUsPerReplica()

	assert.Equal(t, 9, result)
}

func lwsWithGPURequests(size int32, resourceName corev1.ResourceName, leaderGPUs string, workerGPUs string) *lwsv1.LeaderWorkerSet {
	return &lwsv1.LeaderWorkerSet{
		Spec: lwsv1.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size: int32Ptr(size),
				LeaderTemplate: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							containerWithGPURequest("leader", resourceName, leaderGPUs),
						},
					},
				},
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							containerWithGPURequest("worker", resourceName, workerGPUs),
						},
					},
				},
			},
		},
	}
}

func containerWithGPURequest(name string, resourceName corev1.ResourceName, gpus string) corev1.Container {
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				resourceName: resource.MustParse(gpus),
			},
		},
	}
}

func TestLWSAccessor_GetDeletionTimestamp(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected *metav1.Time
	}{
		{
			name: "lws with deletion timestamp",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
			},
			expected: &now,
		},
		{
			name: "lws without deletion timestamp",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: nil,
				},
			},
			expected: nil,
		},
		{
			name:     "nil lws",
			lws:      nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			if tt.lws == nil {
				assert.Nil(t, accessor)
				return
			}
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

func TestLWSAccessor_GetLeaderPodTemplateSpec(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		validate func(t *testing.T, spec *corev1.PodTemplateSpec)
	}{
		{
			name: "lws with leader template",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						LeaderTemplate: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"role": "leader"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "leader-container", Image: "leader:latest"},
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				require.NotNil(t, spec)
				assert.Equal(t, "leader", spec.Labels["role"])
				assert.Equal(t, 1, len(spec.Spec.Containers))
				assert.Equal(t, "leader-container", spec.Spec.Containers[0].Name)
			},
		},
		{
			name: "lws with nil leader template returns worker template",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						LeaderTemplate: nil,
						WorkerTemplate: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"role": "worker"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker-container", Image: "worker:latest"},
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				require.NotNil(t, spec)
				assert.Equal(t, "worker", spec.Labels["role"])
				assert.Equal(t, 1, len(spec.Spec.Containers))
				assert.Equal(t, "worker-container", spec.Spec.Containers[0].Name)
			},
		},
		{
			name: "nil lws returns empty spec",
			lws:  nil,
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				// Not called since accessor is nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			if tt.lws == nil {
				assert.Nil(t, accessor)
				return
			}
			result := accessor.GetLeaderPodTemplateSpec()
			tt.validate(t, result)
		})
	}
}

func TestLWSAccessor_GetWorkerPodTemplateSpec(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		validate func(t *testing.T, spec *corev1.PodTemplateSpec)
	}{
		{
			name: "lws with worker template",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						WorkerTemplate: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"role": "worker"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker-container", Image: "worker:latest"},
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				require.NotNil(t, spec)
				assert.Equal(t, "worker", spec.Labels["role"])
				assert.Equal(t, 1, len(spec.Spec.Containers))
				assert.Equal(t, "worker-container", spec.Spec.Containers[0].Name)
			},
		},
		{
			name: "nil lws returns empty spec",
			lws:  nil,
			validate: func(t *testing.T, spec *corev1.PodTemplateSpec) {
				// Not called since accessor is nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			if tt.lws == nil {
				assert.Nil(t, accessor)
				return
			}
			result := accessor.GetWorkerPodTemplateSpec()
			tt.validate(t, result)
		})
	}
}

func TestLWSAccessor_GetGroupSize(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected int32
	}{
		{
			name: "lws with size 4",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(4),
					},
				},
			},
			expected: 4,
		},
		{
			name: "lws with size 1",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(1),
					},
				},
			},
			expected: 1,
		},
		{
			name: "lws with size 8",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: int32Ptr(8),
					},
				},
			},
			expected: 8,
		},
		{
			name: "lws with nil size returns fallback",
			lws: &lwsv1.LeaderWorkerSet{
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: nil,
					},
				},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accessor := NewLWSAccessor(tt.lws)
			result := accessor.GetGroupSize()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLWSAccessor_GetObject(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws",
			Namespace: "default",
		},
	}

	accessor := NewLWSAccessor(lws)

	assert.Equal(t, "test-lws", accessor.GetName())
	assert.Equal(t, "default", accessor.GetNamespace())
}

func TestLWSAccessor_GetName_GetNamespace_Nil(t *testing.T) {
	accessor := NewLWSAccessor(nil)
	assert.Nil(t, accessor)
}
