package fixtures

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GPUQuotaResource is the ResourceQuota key the gpu-rebalance plugin reads to size
// a namespace's GPU budget. It mirrors the plugin's gpuQuotaResource constant.
const GPUQuotaResource = "requests.nvidia.com/gpu"

func resourceQuotaRef(namespace, name string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
}

func buildGPUResourceQuota(namespace, name string, gpus int64) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceName(GPUQuotaResource): *resource.NewQuantity(gpus, resource.DecimalSI),
			},
		},
	}
}

// EnsureGPUResourceQuota creates or replaces a ResourceQuota capping the namespace's
// GPU requests, which is what gpu-rebalance divides across managed scalers.
//
// The quota is deliberately scoped to requests.nvidia.com/gpu only. Adding pod or CPU
// limits here would make the quota admission controller reject the spec's workloads
// for unrelated reasons and turn a rebalance assertion into a scheduling failure.
func EnsureGPUResourceQuota(ctx context.Context, crClient client.Client, namespace, name string, gpus int64) error {
	obj := buildGPUResourceQuota(namespace, name, gpus)
	existing := resourceQuotaRef(namespace, name)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	err := crClient.Get(ctx, key, existing)
	switch {
	case errors.IsNotFound(err):
		return crClient.Create(ctx, obj)
	case err != nil:
		return fmt.Errorf("check existing ResourceQuota %s/%s: %w", namespace, name, err)
	}

	existing.Spec = obj.Spec
	if err := crClient.Update(ctx, existing); err != nil {
		return fmt.Errorf("update ResourceQuota %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DeleteGPUResourceQuota removes the ResourceQuota. Idempotent; ignores NotFound.
func DeleteGPUResourceQuota(ctx context.Context, crClient client.Client, namespace, name string) error {
	err := crClient.Delete(ctx, resourceQuotaRef(namespace, name))
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ResourceQuota %s/%s: %w", namespace, name, err)
	}
	return nil
}
