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
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	poolutils "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

type InferencePoolReconciler struct {
	client.Client
	Datastore datastore.Datastore
	PoolGKNN  common.GKNN
}

func (c *InferencePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("group", c.PoolGKNN.Group).V(logutil.DEFAULT)
	ctx = ctrl.LoggerInto(ctx, logger)

	logger.Info("Reconciling InferencePool", "namespace", req.Namespace, "name", req.Name)

	// 1. Initialize a generic client.Object based on the group.
	var obj client.Object
	switch c.PoolGKNN.Group {
	case v1.GroupName:
		obj = &v1.InferencePool{}
	case v1alpha2.GroupName:
		obj = &v1alpha2.InferencePool{}
	default:
		// Handle unsupported groups gracefully.
		return ctrl.Result{}, fmt.Errorf("cannot reconcile InferencePool - unsupported API group: %s", c.PoolGKNN.Group)
	}

	// 2. Perform a single, generic fetch for the object.
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("InferencePool not found. Removing pool from datastore")
			c.Datastore.PoolDelete(req.Namespace + "/" + req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("unable to get InferencePool - %w", err)
	}

	// 3. Perform common checks using the client.Object interface.
	if !obj.GetDeletionTimestamp().IsZero() {
		logger.Info("InferencePool is marked for deletion. Removing pool from datastore")
		c.Datastore.PoolDelete(obj.GetNamespace() + "/" + obj.GetName()) // remove the pool from the datastore
		return ctrl.Result{}, nil
	}

	var (
		endpointPool *poolutils.EndpointPool
		err          error
	)

	switch pool := obj.(type) {
	case *v1.InferencePool:
		endpointPool, err = poolutils.InferencePoolToEndpointPool(ctx, c.Client, pool)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to convert InferencePool v1 to EndPointPool - %w", err)
		}
	case *v1alpha2.InferencePool:
		endpointPool, err = poolutils.AlphaInferencePoolToEndpointPool(ctx, c.Client, pool)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to convert InferencePool v1alpha2 to EndPointPool - %w", err)
		}
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported API group: %s", c.PoolGKNN.Group)
	}

	if endpointPool != nil {
		if err := c.Datastore.PoolSet(ctx, c.Client, endpointPool); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add endpoint into the datastore: - %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (c *InferencePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	switch c.PoolGKNN.Group {
	case v1alpha2.GroupName:
		return ctrl.NewControllerManagedBy(mgr).
			For(&v1alpha2.InferencePool{}).
			Complete(c)
	case v1.GroupName:
		return ctrl.NewControllerManagedBy(mgr).
			For(&v1.InferencePool{}).
			Complete(c)
	default:
		return fmt.Errorf("unknown group %s", c.PoolGKNN.Group)
	}
}
