/*
Copyright 2025 The Kubernetes Authors.

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

package actuator

import (
	"context"
	"testing"

	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/stretchr/testify/require"
	appsV1 "k8s.io/api/apps/v1"
	autoscalingapi "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	scalefake "k8s.io/client-go/scale/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestDirectActuator(t *testing.T) {
	testSelector := map[string]string{"app": "vllm_v1"}
	gvr := schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}

	tests := []struct {
		name            string
		objName         string
		namespace       string
		initialReplicas int32
		desiredReplicas int32
		wantErr         bool
	}{
		{
			name:            "Scale from 0 to 1 replica",
			objName:         "test-scale-object",
			namespace:       "default",
			initialReplicas: int32(0),
			desiredReplicas: int32(1),
			wantErr:         false,
		},
		{
			name:            "Scale from 1 to 5 replicas",
			objName:         "test-scale-object",
			namespace:       "llm-d-sim",
			initialReplicas: int32(1),
			desiredReplicas: int32(5),
			wantErr:         false,
		},
		{
			name:            "Scale to same replica count (no-op)",
			objName:         "test-scale-object",
			namespace:       "default",
			initialReplicas: int32(3),
			desiredReplicas: int32(3),
			wantErr:         false,
		},
		{
			name:      "Error when deployment does not exist",
			objName:   "non-existent-deployment",
			namespace: "default",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create scheme and add types
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = appsV1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			// Create deployment
			deployment := unittestutil.MakeDeployment(tt.objName, tt.namespace, tt.initialReplicas, testSelector)

			// Create initial scale object
			initialScale := &autoscalingapi.Scale{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.objName,
					Namespace: tt.namespace,
				},
				Spec: autoscalingapi.ScaleSpec{
					Replicas: tt.initialReplicas,
				},
				Status: autoscalingapi.ScaleStatus{
					Replicas: tt.initialReplicas,
				},
			}

			// Create fake dynamic client with deployment
			var fakeDynamicClientObjects []runtime.Object
			if !tt.wantErr {
				fakeDynamicClientObjects = []runtime.Object{deployment}
			}
			fakeDynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, fakeDynamicClientObjects...)

			// Create fake scale client
			fakeScaleClient := &scalefake.FakeScaleClient{}

			// Add reactor to handle Get requests for scales
			if !tt.wantErr {
				fakeScaleClient.AddReactor("get", "deployments", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, initialScale, nil
				})
			}

			// Add reactor to handle Update requests for scales
			fakeScaleClient.AddReactor("update", "deployments", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
				updateAction := action.(clienttesting.UpdateAction)
				updatedScale := updateAction.GetObject().(*autoscalingapi.Scale)
				return true, updatedScale, nil
			})

			// Create REST mapper
			mapper := testrestmapper.TestOnlyStaticRESTMapper(scheme, schema.GroupVersion{Group: "apps", Version: "v1"})

			// Create DirectActuator with fake scale client
			actuator := &DirectActuator{
				scaleClient: fakeScaleClient,
				Mapper:      mapper,
			}

			// Get the deployment as unstructured object
			var deploy runtime.Object
			var err error
			if !tt.wantErr {
				deploy, err = fakeDynamicClient.Resource(gvr).Namespace(tt.namespace).Get(ctx, tt.objName, metav1.GetOptions{})
				require.NoError(t, err)
			} else {
				// For error case, create a minimal unstructured object
				deploy = deployment
			}

			// Convert to unstructured
			unstructuredDeploy, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deploy)
			require.NoError(t, err)
			unstructuredObj := &unstructured.Unstructured{}
			unstructuredObj.SetUnstructuredContent(unstructuredDeploy)

			// Call the ScaleTargetObject function
			err = actuator.ScaleTargetObject(ctx, unstructuredObj, tt.desiredReplicas)

			// Verify results
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)

				// Verify the scale client received the correct actions
				actions := fakeScaleClient.Actions()
				require.NotEmpty(t, actions, "Expected at least one action on scale client")

				// If replicas are different, we expect Get and Update actions
				if tt.initialReplicas != tt.desiredReplicas {
					require.GreaterOrEqual(t, len(actions), 2, "Expected Get and Update actions")

					// Verify Get action
					getAction := actions[0]
					require.Equal(t, "get", getAction.GetVerb())
					require.Equal(t, "deployments", getAction.GetResource().Resource)

					// Verify Update action
					updateAction := actions[1]
					require.Equal(t, "update", updateAction.GetVerb())
					require.Equal(t, "deployments", updateAction.GetResource().Resource)
				}
			}
		})
	}
}
