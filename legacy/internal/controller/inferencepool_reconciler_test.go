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

package controller

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

var (
	selector_v1 = map[string]string{"app": "vllm_v1"}
)

func TestInferencePoolReconcile(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}

	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace("pool1-ns").
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	tests := []struct {
		name            string
		pool            *v1.InferencePool
		listResultLen   int
		DeleteResultLen int
	}{
		{
			name:            "reconcile with v1 InferencePool",
			pool:            pool1,
			listResultLen:   1,
			DeleteResultLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := tt.pool

			// Define the EPP service object
			eppSvc := unittestutil.MakeService("epp-svc", "pool1-ns")

			// Set up the scheme.
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1.Install(scheme)
			initialObjects := []client.Object{pool, eppSvc} // I need to add a service to the list of object created HERE!!!

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(initialObjects...).
				Build()

			// Step 1: create inferencePool and add the respectice endpoint to the datastore.
			namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool.GroupVersionKind().Group,
					Kind:  pool.GroupVersionKind().Kind,
				},
			}
			req := ctrl.Request{NamespacedName: namespacedName}
			ctx := context.Background()

			ds := datastore.NewDatastore(nil)
			inferencePoolReconciler := &InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.listResultLen, "There should be one EndpointPool in the datastore")

			// Check the value of the EndpointPool to confirm that we have the correct one ??
			endpointPool, err := poolutil.InferencePoolToEndpointPool(ctx, fakeClient, pool)
			if err != nil {
				t.Errorf("Unexpected InferencePoolToEndpointPool error: %v", err)
			}

			if diff := diffStore(ds, endpointPool); diff != "" {
				t.Errorf("Unexpected diff (+got/-want): %s", diff)
			}

			// Step 2: update the inferencePool port
			newPool1 := &v1.InferencePool{}
			if err := fakeClient.Get(ctx, req.NamespacedName, newPool1); err != nil {
				t.Errorf("Unexpected inferencePool get error: %v", err)
			}
			newPool1.Spec.TargetPorts = []v1.Port{{Number: 9090}}
			if err := fakeClient.Update(ctx, newPool1, &client.UpdateOptions{}); err != nil {
				t.Errorf("Unexpected inferencePool update error: %v", err)
			}
			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}
			newEndpointPool1, _ := poolutil.InferencePoolToEndpointPool(ctx, fakeClient, newPool1)
			if diff := diffStore(ds, newEndpointPool1); diff != "" {
				t.Errorf("Unexpected diff (+got/-want): %s", diff)
			}

			// Step 3: delete the inferencePool to trigger a datastore clear
			if err := fakeClient.Get(ctx, req.NamespacedName, newPool1); err != nil {
				t.Errorf("Unexpected inferencePool get error: %v", err)
			}

			if err := fakeClient.Delete(ctx, newPool1, &client.DeleteOptions{}); err != nil {
				t.Errorf("Unexpected inferencePool delete error: %v", err)
			}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.DeleteResultLen, "Datastore should be empty")
		})
	}
}

func diffStore(store datastore.Datastore, ep *poolutil.EndpointPool) string {
	gotPool, _ := store.PoolGet(ep.Namespace + "/" + ep.Name)
	if diff := cmp.Diff(ep, gotPool); diff != "" {
		return "inferencePool:" + diff
	}
	return ""
}

func TestAlphaInferencePoolReconcile(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1alpha2.GroupVersion.Group,
		Version: v1alpha2.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeAlphaInferencePool("pool1").
		Namespace("pool1-ns").
		Selector(selector_v1).
		ExtensionRef("epp-svc").
		TargetPortNumber(8080).ObjRef()
	pool1.SetGroupVersionKind(gvk)

	tests := []struct {
		name            string
		pool            *v1alpha2.InferencePool
		listResultLen   int
		DeleteResultLen int
	}{
		{
			name:            "reconcile with v1alpha2 InferencePool",
			pool:            pool1,
			listResultLen:   1,
			DeleteResultLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := tt.pool

			// Define the EPP service object
			eppSvc := unittestutil.MakeService("epp-svc", "pool1-ns")

			// Set up the scheme.
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			initialObjects := []client.Object{pool, eppSvc} // I need to add a service to the list of object created HERE!!!

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(initialObjects...).
				Build()

			// Step 1: create inferencePool and add the respectice endpoint to the datastore.
			namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool.GroupVersionKind().Group,
					Kind:  pool.GroupVersionKind().Kind,
				},
			}
			req := ctrl.Request{NamespacedName: namespacedName}
			ctx := context.Background()

			ds := datastore.NewDatastore(nil)
			inferencePoolReconciler := &InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.listResultLen, "There should be one EndpointPool in the datastore")

			// Check the value of the EndpointPool to confirm that we have the correct one ??
			endpointPool, err := poolutil.AlphaInferencePoolToEndpointPool(ctx, fakeClient, pool)
			if err != nil {
				t.Errorf("Unexpected InferencePoolToEndpointPool error: %v", err)
			}

			if diff := diffStore(ds, endpointPool); diff != "" {
				t.Errorf("Unexpected diff (+got/-want): %s", diff)
			}

			// Step 2: update the inferencePool port
			newPool1 := &v1alpha2.InferencePool{}
			if err := fakeClient.Get(ctx, req.NamespacedName, newPool1); err != nil {
				t.Errorf("Unexpected inferencePool get error: %v", err)
			}
			newPool1.Spec.TargetPortNumber = 9090
			if err := fakeClient.Update(ctx, newPool1, &client.UpdateOptions{}); err != nil {
				t.Errorf("Unexpected inferencePool update error: %v", err)
			}
			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}
			newEndpointPool1, _ := poolutil.AlphaInferencePoolToEndpointPool(ctx, fakeClient, newPool1)
			if diff := diffStore(ds, newEndpointPool1); diff != "" {
				t.Errorf("Unexpected diff (+got/-want): %s", diff)
			}

			// Step 3: delete the inferencePool to trigger a datastore clear
			if err := fakeClient.Get(ctx, req.NamespacedName, newPool1); err != nil {
				t.Errorf("Unexpected inferencePool get error: %v", err)
			}

			if err := fakeClient.Delete(ctx, newPool1, &client.DeleteOptions{}); err != nil {
				t.Errorf("Unexpected inferencePool delete error: %v", err)
			}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.DeleteResultLen, "Datastore should be empty")
		})
	}
}
