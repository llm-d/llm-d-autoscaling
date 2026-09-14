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

package actuator

import (
	"context"

	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cached "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type DirectActuator struct {
	scaleClient scale.ScalesGetter
	Mapper      meta.RESTMapper
}

func NewDirectActuator(config *rest.Config) (*DirectActuator, error) {
	scaleClient, mapper, err := initScaleClient(config)
	if err != nil {
		return nil, err
	}

	return &DirectActuator{
		scaleClient: scaleClient,
		Mapper:      mapper,
	}, nil
}

func (da *DirectActuator) ScaleTargetObject(ctx context.Context, scaledObject *unstructured.Unstructured, replicas int32) error {
	logger := log.FromContext(ctx)
	scale, gr, err := da.getScaleTargetScale(ctx, scaledObject)
	if err != nil {
		return err
	}

	if scale.Spec.Replicas == replicas {
		logger.Info("Scale Object already has the desired number of replicas. Skipping scaling", "scaleName", scale.Name)
		return nil
	}

	currentReplicas, err := da.updateScaleOnScaleTarget(ctx, scaledObject, scale, replicas, gr)
	if err == nil {
		logger.Info("Successfully updated ScaleTarget",
			"Original Replicas Count", currentReplicas,
			"New Replicas Count", replicas)
	} else {
		logger.Error(err, "Failed to scale Target", "kind", scaledObject.GetKind(), "namespace", scaledObject.GetNamespace(), "name", scaledObject.GetName())
		return err
	}
	return nil
}

func (da *DirectActuator) getScaleTargetScale(ctx context.Context, scaledObject *unstructured.Unstructured) (*autoscalingv1.Scale, schema.GroupResource, error) {
	// Get the scale subresource for the target Object
	logger := log.FromContext(ctx)
	gvr, err := poolutil.GetResourceForKind(da.Mapper, scaledObject.GetAPIVersion(), scaledObject.GetKind())
	if err != nil {
		msg := "Failed to parse Group, Version, Kind, Resource"
		logger.Error(err, msg, "apiVersion", scaledObject.GetAPIVersion(), "kind", scaledObject.GetKind())
		return nil, schema.GroupResource{}, err
	}

	gr := gvr.GroupResource()
	scale, err := da.scaleClient.Scales(scaledObject.GetNamespace()).Get(ctx, gr, scaledObject.GetName(), metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "Error getting scale subresource object")
		return nil, schema.GroupResource{}, err
	}
	return scale, gr, nil
}

func (da *DirectActuator) updateScaleOnScaleTarget(ctx context.Context, scaledObject *unstructured.Unstructured, scale *autoscalingv1.Scale, replicas int32, gr schema.GroupResource) (int32, error) {
	// Update with requested replicas.
	currentReplicas := scale.Spec.Replicas
	scale.Spec.Replicas = replicas

	_, err := da.scaleClient.Scales(scaledObject.GetNamespace()).Update(ctx, gr, scale, metav1.UpdateOptions{})
	return currentReplicas, err
}

// initScaleClient initializes scale client
func initScaleClient(config *rest.Config) (scale.ScalesGetter, meta.RESTMapper, error) {
	clientset, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	cachedDiscoveryClient := cached.NewMemCacheClient(clientset)
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)

	return scale.New(
		clientset.RESTClient(), restMapper,
		dynamic.LegacyAPIPathResolverFunc,
		scale.NewDiscoveryScaleKindResolver(clientset),
	), restMapper, nil
}
