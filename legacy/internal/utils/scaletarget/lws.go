package scaletarget

import (
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

type lwsAccessor struct {
	lws *lwsv1.LeaderWorkerSet
}

func NewLWSAccessor(lws *lwsv1.LeaderWorkerSet) ScaleTargetAccessor {
	if lws == nil {
		return nil
	}
	accessor := lwsAccessor{
		lws: lws,
	}
	return &accessor
}

func (r *lwsAccessor) GetReplicas() *int32 {
	// r.lws is always not nil
	return r.lws.Spec.Replicas
}

func (r *lwsAccessor) GetStatusReplicas() int32 {
	// r.lws is always not nil
	return r.lws.Status.Replicas
}

func (r *lwsAccessor) GetStatusReadyReplicas() int32 {
	// r.lws is always not nil
	return r.lws.Status.ReadyReplicas
}

// leader_GPUs + (Size - 1) * worker_GPUs.
func (r *lwsAccessor) GetTotalGPUsPerReplica() int {
	// r.lws is always not nil
	leaderGPUs := 0
	if r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderGPUs = resources.GetContainersGPUs(r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers)
	}

	workerGPUs := resources.GetContainersGPUs(r.lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers)
	total := leaderGPUs + (int(r.GetGroupSize())-1)*workerGPUs

	// Default to 1 GPU if no explicit requests found
	// (common for inference workloads that may not have resource requests)
	if total == 0 {
		return 1
	}
	return total
}

func (r *lwsAccessor) GetDeletionTimestamp() *v1.Time {
	// r.lws is always not nil
	return r.lws.DeletionTimestamp
}

func (r *lwsAccessor) GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec {
	// r.lws is always not nil
	if r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate == nil {
		return r.GetWorkerPodTemplateSpec()
	}
	return r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate
}

func (r *lwsAccessor) GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec {
	// r.lws is always not nil
	return &r.lws.Spec.LeaderWorkerTemplate.WorkerTemplate
}

func (r *lwsAccessor) GetGroupSize() int32 {
	// r.lws is always not nil
	if r.lws.Spec.LeaderWorkerTemplate.Size == nil {
		// As documented, this is optional (nil) and default to 1
		// https://pkg.go.dev/sigs.k8s.io/lws@v0.8.0/api/leaderworkerset/v1#LeaderWorkerTemplate.Size
		return 1
	}
	return *r.lws.Spec.LeaderWorkerTemplate.Size
}

func (r *lwsAccessor) GetName() string {
	// r.lws is always not nil
	return r.lws.Name
}

func (r *lwsAccessor) GetNamespace() string {
	// r.lws is always not nil
	return r.lws.Namespace
}
