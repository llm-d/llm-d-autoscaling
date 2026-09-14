/*
Copyright 2025.

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

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

// HPAReconciler tracks namespaces for annotation-based WVA discovery via HPA objects.
// Its sole job is to call NamespaceTrack / NamespaceUntrack so the engine's polling
// loop can scope its List calls to namespaces that contain managed scalers.
type HPAReconciler struct {
	client.Client
	Datastore datastore.Datastore
}

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// Note: Kubernetes RBAC does not support annotation-based selectors, so this grant is
// necessarily cluster-wide. Access is read-only (get;list;watch). AnnotatedScalerPredicate
// limits event processing to managed objects (see TODO #1134 for scoped List calls).

// Reconcile tracks or untracks the namespace of an HPA bearing llm-d.ai/managed: "true".
func (r *HPAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, hpa); err != nil {
		if apierrors.IsNotFound(err) {
			r.Datastore.NamespaceUntrack("AnnotatedScaler", req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !hpa.DeletionTimestamp.IsZero() || !annotations.IsManaged(hpa) {
		r.Datastore.NamespaceUntrack("AnnotatedScaler", req.Name, req.Namespace)
	} else {
		r.Datastore.NamespaceTrack("AnnotatedScaler", req.Name, req.Namespace)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers HPAReconciler with the controller manager.
func (r *HPAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv2.HorizontalPodAutoscaler{},
			builder.WithPredicates(AnnotatedScalerPredicate()),
		).
		Named("hpa").
		Complete(r)
}
