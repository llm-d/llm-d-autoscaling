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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
)

const (
	MetricsPortNameSubstring = "metric"
	// Valid InferencePool API groups
	PoolGroupV1       = v1.GroupName
	PoolGroupV1Alpha2 = v1alpha2.GroupName
)

type EndpointPool struct {
	Name           string
	Namespace      string
	Selector       map[string]string
	EndpointPicker *EndpointPicker
}

type EndpointPicker struct {
	ServiceName       string
	Namespace         string
	MetricsPortNumber int32
}

// InferencePoolToEndpointPool converts an v1 InferencePool to an EndpointPool.
func InferencePoolToEndpointPool(ctx context.Context, client client.Client, inferencePool *v1.InferencePool) (*EndpointPool, error) {
	if inferencePool == nil {
		return nil, nil
	}

	epp, err := generateEndpointPickerObject(ctx, string(inferencePool.Spec.EndpointPickerRef.Name), inferencePool.Namespace, client)
	if err != nil {
		return nil, err
	}

	selector := make(map[string]string, len(inferencePool.Spec.Selector.MatchLabels))
	for k, v := range inferencePool.Spec.Selector.MatchLabels {
		selector[string(k)] = string(v)
	}
	endpointPool := &EndpointPool{
		Selector:       selector,
		Namespace:      inferencePool.Namespace,
		Name:           inferencePool.Name,
		EndpointPicker: epp,
	}
	return endpointPool, nil
}

// AlphaInferencePoolToEndpointPool converts an v1alpha2 inferencePool to an EndpointPool.
func AlphaInferencePoolToEndpointPool(ctx context.Context, client client.Client, inferencePool *v1alpha2.InferencePool) (*EndpointPool, error) {
	if inferencePool == nil {
		return nil, nil
	}

	epp, err := generateEndpointPickerObject(ctx, string(inferencePool.Spec.ExtensionRef.Name), inferencePool.Namespace, client)
	if err != nil {
		return nil, err
	}

	selector := make(map[string]string, len(inferencePool.Spec.Selector))
	for k, v := range inferencePool.Spec.Selector {
		selector[string(k)] = string(v)
	}
	endpointPool := &EndpointPool{
		Selector:       selector,
		Namespace:      inferencePool.Namespace,
		Name:           inferencePool.Name,
		EndpointPicker: epp,
	}
	return endpointPool, nil
}

func generateEndpointPickerObject(ctx context.Context, serviceName, namespace string, c client.Client) (*EndpointPicker, error) {
	service := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      serviceName,
	}, service)

	if err != nil {
		return nil, err
	}

	var portNumber int32
	existMetricPort := false

	// Find EPP metrics port number from EPP service
	for _, port := range service.Spec.Ports {
		if strings.Contains(port.Name, MetricsPortNameSubstring) {
			portNumber = port.Port
			existMetricPort = true
			break
		}
	}

	if !existMetricPort {
		return nil, errors.New("metrics port not found: service must have a named metrics port with 'metric' substring in its name")
	}

	epp := EndpointPicker{
		Namespace:         namespace,
		ServiceName:       serviceName,
		MetricsPortNumber: portNumber,
	}
	return &epp, nil
}

// IsSubset checks if the given subset labels are a subset of the superset labels.
func IsSubset(subsetLabels, supersetLabels map[string]string) bool {
	if len(subsetLabels) == 0 {
		return true
	}

	for key, subValue := range subsetLabels {
		if superValue, ok := supersetLabels[key]; !ok || superValue != subValue {
			return false
		}
	}
	return true
}

// GetPoolGKNN initializes a GKNN object for an inferencePool.
func GetPoolGKNN(poolGroup string) (common.GKNN, error) {
	var (
		poolName      = "defaultPool"
		poolNamespace = "default"
	)

	// Default to v1alpha2 group if empty
	if poolGroup == "" {
		poolGroup = PoolGroupV1Alpha2
	}

	// Validate poolGroup against valid values
	if poolGroup != PoolGroupV1 && poolGroup != PoolGroupV1Alpha2 {
		return common.GKNN{}, errors.New("invalid poolGroup: must be either 'inference.networking.k8s.io' or 'inference.networking.x-k8s.io'")
	}

	gknn := common.GKNN{
		NamespacedName: types.NamespacedName{Name: poolName, Namespace: poolNamespace},
		GroupKind: schema.GroupKind{
			Group: poolGroup,
			Kind:  "InferencePool",
		},
	}
	return gknn, nil
}
