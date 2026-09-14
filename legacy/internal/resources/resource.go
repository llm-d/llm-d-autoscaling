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

package resources

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GetContainersGPUs returns the total GPU count across all containers
func GetContainersGPUs(containers []corev1.Container) int {
	total := 0
	for _, container := range containers {
		for _, resource := range constants.VendorResources {
			resName := corev1.ResourceName(resource.ResourceName)
			if qty, ok := container.Resources.Requests[resName]; ok {
				total += int(qty.Value())
			}
		}
	}
	return total
}

// GetResourceWithBackoff performs a Get operation with exponential backoff retry logic
func GetResourceWithBackoff[T client.Object](ctx context.Context, c client.Client, objKey client.ObjectKey, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, objKey, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err // Don't retry on notFound errors
			}

			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Error(err, "transient error getting resource, retrying",
				"resourceType", resourceType,
				"name", objKey.Name,
				"namespace", objKey.Namespace)
			return false, nil // Retry on transient errors
		}

		return true, nil
	})
}
