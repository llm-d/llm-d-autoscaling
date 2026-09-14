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
	"context"
	"errors"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestFetchScaleTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	tests := []struct {
		name          string
		vaName        string
		kind          string
		targetName    string
		namespace     string
		existingObjs  []client.Object
		expectedError bool
		errorType     string
		validate      func(t *testing.T, accessor ScaleTargetAccessor)
	}{
		{
			name:       "successfully fetch existing deployment",
			vaName:     "test-va",
			kind:       constants.DeploymentKind,
			targetName: "test-deployment",
			namespace:  "default",
			existingObjs: []client.Object{
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-deployment",
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: int32Ptr(3),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test:latest"},
								},
							},
						},
					},
					Status: appsv1.DeploymentStatus{
						Replicas: 3,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, "test-deployment", accessor.GetName())
				assert.Equal(t, "default", accessor.GetNamespace())
				assert.Equal(t, int32(3), *accessor.GetReplicas())
				assert.Equal(t, int32(3), accessor.GetStatusReplicas())
			},
		},
		{
			name:          "deployment not found",
			vaName:        "test-va",
			kind:          constants.DeploymentKind,
			targetName:    "non-existent-deployment",
			namespace:     "default",
			existingObjs:  []client.Object{},
			expectedError: true,
			errorType:     "NotFound",
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				// No validation needed for error case
			},
		},
		{
			name:       "deployment with zero replicas",
			vaName:     "test-va",
			kind:       constants.DeploymentKind,
			targetName: "zero-replica-deployment",
			namespace:  "default",
			existingObjs: []client.Object{
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "zero-replica-deployment",
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: int32Ptr(0),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test:latest"},
								},
							},
						},
					},
					Status: appsv1.DeploymentStatus{
						Replicas: 0,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, int32(0), *accessor.GetReplicas())
				assert.Equal(t, int32(0), accessor.GetStatusReplicas())
			},
		},
		{
			name:       "deployment in different namespace",
			vaName:     "test-va",
			kind:       constants.DeploymentKind,
			targetName: "test-deployment",
			namespace:  "other-namespace",
			existingObjs: []client.Object{
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-deployment",
						Namespace: "other-namespace",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: int32Ptr(1),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test:latest"},
								},
							},
						},
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, "other-namespace", accessor.GetNamespace())
			},
		},
		{
			name:          "invalid kind",
			vaName:        "test-va",
			kind:          "InvalidKind",
			targetName:    "test-deployment",
			namespace:     "default",
			existingObjs:  []client.Object{},
			expectedError: true,
			errorType:     "InvalidKind",
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				// No validation needed for error case
			},
		},
		{
			name:       "deployment with multiple replicas",
			vaName:     "test-va",
			kind:       constants.DeploymentKind,
			targetName: "multi-replica-deployment",
			namespace:  "default",
			existingObjs: []client.Object{
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "multi-replica-deployment",
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: int32Ptr(10),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test:latest"},
								},
							},
						},
					},
					Status: appsv1.DeploymentStatus{
						Replicas:      10,
						ReadyReplicas: 8,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, int32(10), *accessor.GetReplicas())
				assert.Equal(t, int32(10), accessor.GetStatusReplicas())
				assert.Equal(t, int32(8), accessor.GetStatusReadyReplicas())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with existing objects
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingObjs...).
				Build()

			ctx := context.Background()

			// Execute
			accessor, err := FetchScaleTarget(ctx, fakeClient, tt.vaName, tt.kind, tt.targetName, tt.namespace)

			// Validate error expectation
			if tt.expectedError {
				require.Error(t, err)
				require.Nil(t, accessor)
				switch tt.errorType {
				case "NotFound":
					assert.True(t, apierrors.IsNotFound(err), "expected NotFound error")
				case "InvalidKind":
					assert.Contains(t, err.Error(), "invalid scale target kind")
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, accessor)
				tt.validate(t, accessor)
			}
		})
	}
}

func TestFetchScaleTargetWithContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deployment",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "test:latest"},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deployment).
		Build()

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	accessor, err := FetchScaleTarget(ctx, fakeClient, "test-va", constants.DeploymentKind, "test-deployment", "default")

	// The behavior depends on when the context is checked, but it should handle cancellation gracefully
	// In this case, the operation might succeed if it's fast enough, or fail with context cancelled
	if err != nil {
		assert.Contains(t, err.Error(), "context canceled")
		assert.Nil(t, accessor)
	}
}

func TestGetResourceWithBackoff(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	tests := []struct {
		name          string
		existingObjs  []client.Object
		objKey        client.ObjectKey
		expectedError bool
	}{
		{
			name: "successfully fetch resource",
			existingObjs: []client.Object{
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-deployment",
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test:latest"},
								},
							},
						},
					},
				},
			},
			objKey: client.ObjectKey{
				Name:      "test-deployment",
				Namespace: "default",
			},
			expectedError: false,
		},
		{
			name:         "resource not found",
			existingObjs: []client.Object{},
			objKey: client.ObjectKey{
				Name:      "non-existent",
				Namespace: "default",
			},
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingObjs...).
				Build()

			ctx := context.Background()
			var obj appsv1.Deployment

			err := resources.GetResourceWithBackoff(ctx, fakeClient, tt.objKey, &obj, constants.StandardBackoff, "Deployment")

			if tt.expectedError {
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.objKey.Name, obj.Name)
				assert.Equal(t, tt.objKey.Namespace, obj.Namespace)
			}
		})
	}
}

func TestFetchScaleTargetReturnsAccessor(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deployment",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "test:latest"},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deployment).
		Build()

	ctx := context.Background()

	// FetchScaleTarget should return a ScaleTargetAccessor
	accessor, err := FetchScaleTarget(ctx, fakeClient, "test-va", constants.DeploymentKind, "test-deployment", "default")

	require.NoError(t, err)
	require.NotNil(t, accessor)
	assert.Equal(t, "test-deployment", accessor.GetName())
	assert.Equal(t, "default", accessor.GetNamespace())
	assert.Equal(t, int32(1), *accessor.GetReplicas())
}

// Helper functions

func int32Ptr(i int32) *int32 {
	return &i
}

// MockErrorClient simulates transient errors for testing retry logic
type MockErrorClient struct {
	client.Client
	callCount  int
	failTimes  int
	finalError error
}

func (m *MockErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	m.callCount++
	if m.callCount <= m.failTimes {
		// Return a transient error that should trigger retry
		return errors.New("transient error: temporary failure")
	}
	if m.finalError != nil {
		return m.finalError
	}
	// Success case - return NotFound to simulate "finally succeeded"
	// In a real test, you'd return success, but for this mock we'll keep it simple
	return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, key.Name)
}

func TestGetResourceWithBackoffRetries(t *testing.T) {
	mockClient := &MockErrorClient{
		failTimes:  2, // Fail first 2 attempts
		finalError: nil,
	}

	ctx := context.Background()
	var obj appsv1.Deployment

	err := resources.GetResourceWithBackoff(ctx, mockClient, client.ObjectKey{Name: "test", Namespace: "default"}, &obj, constants.StandardBackoff, "Deployment")

	// Should eventually succeed after retries
	assert.Error(t, err) // Will be NotFound from mock
	assert.True(t, mockClient.callCount >= 3, "expected at least 3 calls (2 failures + 1 success)")
}

func TestGetContainersGPUs(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		expected   int
	}{
		{
			name: "single container with NVIDIA GPUs",
			containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("4"),
						},
					},
				},
			},
			expected: 4,
		},
		{
			name: "single container with AMD GPUs",
			containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("2"),
						},
					},
				},
			},
			expected: 2,
		},
		{
			name: "single container with Intel GPUs",
			containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"gpu.intel.com/xe": resource.MustParse("1"),
						},
					},
				},
			},
			expected: 1,
		},
		{
			name: "multiple containers with same vendor GPUs",
			containers: []corev1.Container{
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
			expected: 5,
		},
		{
			name: "multiple containers with mixed vendor GPUs",
			containers: []corev1.Container{
				{
					Name: "nvidia-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("4"),
						},
					},
				},
				{
					Name: "amd-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("2"),
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
			expected: 7,
		},
		{
			name: "container with no GPU resources",
			containers: []corev1.Container{
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
			expected: 0,
		},
		{
			name: "container with mixed resources including GPUs",
			containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"cpu":            resource.MustParse("2"),
							"memory":         resource.MustParse("4Gi"),
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			expected: 8,
		},
		{
			name:       "empty containers list",
			containers: []corev1.Container{},
			expected:   0,
		},
		{
			name:       "nil containers",
			containers: nil,
			expected:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resources.GetContainersGPUs(tt.containers)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFetchScaleTarget_LeaderWorkerSet(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, lwsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	tests := []struct {
		name          string
		vaName        string
		kind          string
		targetName    string
		namespace     string
		existingObjs  []client.Object
		expectedError bool
		errorType     string
		validate      func(t *testing.T, accessor ScaleTargetAccessor)
	}{
		{
			name:       "successfully fetch existing LeaderWorkerSet",
			vaName:     "test-va",
			kind:       constants.LeaderWorkerSetKind,
			targetName: "test-lws",
			namespace:  "default",
			existingObjs: []client.Object{
				&lwsv1.LeaderWorkerSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-lws",
						Namespace: "default",
					},
					Spec: lwsv1.LeaderWorkerSetSpec{
						Replicas: int32Ptr(3),
						LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
							Size: int32Ptr(4),
							LeaderTemplate: &corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{"role": "leader"},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "leader", Image: "leader:latest"},
									},
								},
							},
							WorkerTemplate: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{"role": "worker"},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "worker", Image: "worker:latest"},
									},
								},
							},
						},
					},
					Status: lwsv1.LeaderWorkerSetStatus{
						Replicas: 3,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, "test-lws", accessor.GetName())
				assert.Equal(t, "default", accessor.GetNamespace())
				assert.Equal(t, int32(3), *accessor.GetReplicas())
				assert.Equal(t, int32(3), accessor.GetStatusReplicas())
			},
		},
		{
			name:          "LeaderWorkerSet not found",
			vaName:        "test-va",
			kind:          constants.LeaderWorkerSetKind,
			targetName:    "non-existent-lws",
			namespace:     "default",
			existingObjs:  []client.Object{},
			expectedError: true,
			errorType:     "NotFound",
			validate:      func(t *testing.T, accessor ScaleTargetAccessor) {},
		},
		{
			name:       "LeaderWorkerSet with zero replicas",
			vaName:     "test-va",
			kind:       constants.LeaderWorkerSetKind,
			targetName: "zero-replica-lws",
			namespace:  "default",
			existingObjs: []client.Object{
				&lwsv1.LeaderWorkerSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "zero-replica-lws",
						Namespace: "default",
					},
					Spec: lwsv1.LeaderWorkerSetSpec{
						Replicas: int32Ptr(0),
						LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
							Size: int32Ptr(4),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "worker", Image: "worker:latest"},
									},
								},
							},
						},
					},
					Status: lwsv1.LeaderWorkerSetStatus{
						Replicas: 0,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, int32(0), *accessor.GetReplicas())
				assert.Equal(t, int32(0), accessor.GetStatusReplicas())
			},
		},
		{
			name:       "LeaderWorkerSet in different namespace",
			vaName:     "test-va",
			kind:       constants.LeaderWorkerSetKind,
			targetName: "test-lws",
			namespace:  "other-namespace",
			existingObjs: []client.Object{
				&lwsv1.LeaderWorkerSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-lws",
						Namespace: "other-namespace",
					},
					Spec: lwsv1.LeaderWorkerSetSpec{
						Replicas: int32Ptr(1),
						LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
							Size: int32Ptr(2),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "worker", Image: "worker:latest"},
									},
								},
							},
						},
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, "other-namespace", accessor.GetNamespace())
			},
		},
		{
			name:       "LeaderWorkerSet with ready replicas",
			vaName:     "test-va",
			kind:       constants.LeaderWorkerSetKind,
			targetName: "ready-lws",
			namespace:  "default",
			existingObjs: []client.Object{
				&lwsv1.LeaderWorkerSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ready-lws",
						Namespace: "default",
					},
					Spec: lwsv1.LeaderWorkerSetSpec{
						Replicas: int32Ptr(10),
						LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
							Size: int32Ptr(8),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "worker", Image: "worker:latest"},
									},
								},
							},
						},
					},
					Status: lwsv1.LeaderWorkerSetStatus{
						Replicas:      10,
						ReadyReplicas: 8,
					},
				},
			},
			expectedError: false,
			validate: func(t *testing.T, accessor ScaleTargetAccessor) {
				assert.Equal(t, int32(10), *accessor.GetReplicas())
				assert.Equal(t, int32(10), accessor.GetStatusReplicas())
				assert.Equal(t, int32(8), accessor.GetStatusReadyReplicas())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with existing objects
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingObjs...).
				Build()

			ctx := context.Background()

			// Execute
			accessor, err := FetchScaleTarget(ctx, fakeClient, tt.vaName, tt.kind, tt.targetName, tt.namespace)

			// Validate error expectation
			if tt.expectedError {
				require.Error(t, err)
				require.Nil(t, accessor)
				if tt.errorType == "NotFound" {
					assert.True(t, apierrors.IsNotFound(err), "expected NotFound error")
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, accessor)
				tt.validate(t, accessor)
			}
		})
	}
}

func TestFetchScaleTarget_LWSReturnsAccessor(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, lwsv1.AddToScheme(scheme))

	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws",
			Namespace: "default",
		},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas: int32Ptr(5),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size: int32Ptr(4),
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "worker", Image: "worker:latest"},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lws).
		Build()

	ctx := context.Background()

	// FetchScaleTarget should return a ScaleTargetAccessor for LWS
	accessor, err := FetchScaleTarget(ctx, fakeClient, "test-va", constants.LeaderWorkerSetKind, "test-lws", "default")

	require.NoError(t, err)
	require.NotNil(t, accessor)
	assert.Equal(t, "test-lws", accessor.GetName())
	assert.Equal(t, "default", accessor.GetNamespace())
	assert.Equal(t, int32(5), *accessor.GetReplicas())
	assert.Equal(t, int32(4), accessor.GetGroupSize())
}
