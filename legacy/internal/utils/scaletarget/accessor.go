package scaletarget

import (
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScaleTargetAccessor provides a uniform interface to extract scaling-relevant
// information from any supported scale target kind (Deployment, LeaderWorkerSet).
type ScaleTargetAccessor interface {
	// GetName returns the name of the scale target.
	GetName() string

	// GetNamespace returns the namespace of the scale target.
	GetNamespace() string

	// GetReplicas returns current spec replicas.
	GetReplicas() *int32
	GetDeletionTimestamp() *v1.Time

	// GetStatusReplicas returns status replicas (actual running).
	GetStatusReplicas() int32
	GetStatusReadyReplicas() int32

	// GetTotalGPUsPerReplica returns total GPU count across all pods in a replica.
	// For Deployment: GPUs from the single pod template.
	// For LWS: leader_GPUs + (Size - 1) * worker_GPUs.
	GetTotalGPUsPerReplica() int

	// GetLeaderPodTemplateSpec returns the pod template for the leader/primary pod.
	// For Deployment: the single pod template.
	// For LWS: the leader template (falls back to worker template if not set).
	// Use this for: engine args extraction (leader starts the API server),
	// metrics port discovery, pod label matching.
	GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec

	// GetWorkerPodTemplateSpec returns the pod template for worker pods.
	// For Deployment: same as GetLeaderPodTemplateSpec() (single template).
	// For LWS: the worker template.
	// Use this for: GPU resource extraction when workers differ from leader.
	GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec

	// GetGroupSize returns the number of pods per replica.
	// For Deployment: always 1.
	// For LWS: spec.leaderWorkerTemplate.size (1 leader + N-1 workers).
	GetGroupSize() int32
}
