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

package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

var (
	selector_v1           = map[string]string{"app": "vllm_v1"}
	errMetricPortNotFound = errors.New("metrics port not found: service must have a named metrics port with 'metric' substring in its name")
)

func TestInferencePoolToEndpointPool(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}

	pool1 := utiltest.MakeInferencePool("pool").
		Namespace("pool-ns1").
		Selector(selector_v1).
		TargetPorts(9090).
		EndpointPickerRef("epp-svc1").ObjRef()
	pool1.SetGroupVersionKind(gvk)
	pool2 := utiltest.MakeInferencePool("pool2").Namespace("pool-ns2").EndpointPickerRef("epp-svc2").ObjRef()
	pool2.SetGroupVersionKind(gvk)

	type testCase struct {
		name                 string
		inferencePool        *v1.InferencePool
		expectedEndpointPool *EndpointPool
		readFuncErr          error
	}

	// Test cases
	testCases := []testCase{
		{
			name:          "Successful conversion",
			inferencePool: pool1,
			expectedEndpointPool: &EndpointPool{
				Selector:       map[string]string{"app": "vllm_v1"},
				Namespace:      "pool-ns1",
				Name:           "pool",
				EndpointPicker: &EndpointPicker{ServiceName: "epp-svc1", Namespace: "pool-ns1", MetricsPortNumber: int32(9090)},
			},
		},
		{
			name:                 "Nil inferencePool",
			inferencePool:        nil,
			expectedEndpointPool: nil,
			readFuncErr:          nil,
		},
		{
			name:                 "Error during endpoint picker generation",
			inferencePool:        pool2,
			expectedEndpointPool: nil,
			readFuncErr:          errMetricPortNotFound,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {

			// Define the EPP service object
			eppSvc1 := unittestutil.MakeService("epp-svc1", "pool-ns1")
			eppSvc2 := unittestutil.MakeService("epp-svc2", "pool-ns2")
			eppSvc2.Spec.Ports = []corev1.ServicePort{}
			initialObjects := []client.Object{eppSvc1, eppSvc2}

			// Set up the scheme.
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(initialObjects...).
				Build()

			ctx := context.Background()
			endpointPool, err := InferencePoolToEndpointPool(ctx, fakeClient, tt.inferencePool)

			if diff := cmp.Diff(tt.expectedEndpointPool, endpointPool); diff != "" {
				t.Errorf("Unexpected pool diff (+got/-want): %s", diff)
			}

			if tt.readFuncErr != nil {
				require.Error(t, err, "Expected an error due to readFunc")
				require.Equal(t, tt.readFuncErr, err, "Unexpected error")
			} else {
				require.NoError(t, err, "Unexpected error")
			}
		})
	}
}

func TestAlphaInferencePoolToEndpointPool(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1alpha2.GroupVersion.Group,
		Version: v1alpha2.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeAlphaInferencePool("pool1").
		Namespace("pool-ns1").
		Selector(selector_v1).
		ExtensionRef("epp-svc1").
		TargetPortNumber(8080).ObjRef()
	pool1.SetGroupVersionKind(gvk)
	pool2 := utiltest.MakeAlphaInferencePool("pool2").
		Namespace("pool-ns2").
		ExtensionRef("epp-svc2").
		TargetPortNumber(8080).ObjRef()
	pool2.SetGroupVersionKind(gvk)

	type testCase struct {
		name                 string
		inferencePool        *v1alpha2.InferencePool
		expectedEndpointPool *EndpointPool
		readFuncErr          error
	}

	// Test cases
	testCases := []testCase{
		{
			name:          "Successful conversion",
			inferencePool: pool1,
			expectedEndpointPool: &EndpointPool{
				Selector:       map[string]string{"app": "vllm_v1"},
				Namespace:      "pool-ns1",
				Name:           "pool1",
				EndpointPicker: &EndpointPicker{ServiceName: "epp-svc1", Namespace: "pool-ns1", MetricsPortNumber: int32(9090)},
			},
		},
		{
			name:                 "Nil inferencePool",
			inferencePool:        nil,
			expectedEndpointPool: nil,
			readFuncErr:          nil,
		},
		{
			name:                 "Error during endpoint picker generation",
			inferencePool:        pool2,
			expectedEndpointPool: nil,
			readFuncErr:          errMetricPortNotFound,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {

			// Define the EPP service object
			eppSvc1 := unittestutil.MakeService("epp-svc1", "pool-ns1")
			eppSvc2 := unittestutil.MakeService("epp-svc2", "pool-ns2")
			eppSvc2.Spec.Ports = []corev1.ServicePort{}
			initialObjects := []client.Object{eppSvc1, eppSvc2}

			// Set up the scheme.
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(initialObjects...).
				Build()

			ctx := context.Background()
			endpointPool, err := AlphaInferencePoolToEndpointPool(ctx, fakeClient, tt.inferencePool)

			if diff := cmp.Diff(tt.expectedEndpointPool, endpointPool); diff != "" {
				t.Errorf("Unexpected pool diff (+got/-want): %s", diff)
			}

			if tt.readFuncErr != nil {
				require.Error(t, err, "Expected an error due to readFunc")
				require.Equal(t, tt.readFuncErr, err, "Unexpected error")
			} else {
				require.NoError(t, err, "Unexpected error")
			}
		})
	}
}
