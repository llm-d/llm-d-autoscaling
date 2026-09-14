package scaletarget

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

type deploymentAccessor struct {
	deployment *appsv1.Deployment
}

func NewDeploymentAccessor(deploy *appsv1.Deployment) ScaleTargetAccessor {
	if deploy == nil {
		return nil
	}
	accessor := deploymentAccessor{
		deployment: deploy,
	}
	return &accessor
}

func (r *deploymentAccessor) GetReplicas() *int32 {
	// r.deployment is always not nil
	return r.deployment.Spec.Replicas
}

func (r *deploymentAccessor) GetStatusReplicas() int32 {
	// r.deployment is always not nil
	return r.deployment.Status.Replicas
}

func (r *deploymentAccessor) GetStatusReadyReplicas() int32 {
	// r.deployment is always not nil
	return r.deployment.Status.ReadyReplicas
}

func (r *deploymentAccessor) GetTotalGPUsPerReplica() int {
	// r.deployment is always not nil
	total := resources.GetContainersGPUs(r.deployment.Spec.Template.Spec.Containers)
	// Default to 1 GPU if no explicit requests found
	// (common for inference workloads that may not have resource requests)
	if total == 0 {
		return 1
	}
	return total
}

func (r *deploymentAccessor) GetDeletionTimestamp() *v1.Time {
	// r.deployment is always not nil
	return r.deployment.DeletionTimestamp
}

func (r *deploymentAccessor) GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec {
	// r.deployment is always not nil
	return &r.deployment.Spec.Template
}

func (r *deploymentAccessor) GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec {
	return r.GetLeaderPodTemplateSpec()
}

func (r *deploymentAccessor) GetGroupSize() int32 {
	return 1
}

func (r *deploymentAccessor) GetName() string {
	// r.deployment is always not nil
	return r.deployment.Name
}

func (r *deploymentAccessor) GetNamespace() string {
	// r.deployment is always not nil
	return r.deployment.Namespace
}
