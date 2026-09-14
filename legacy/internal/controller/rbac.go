package controller

// This file centralizes the +kubebuilder:rbac markers for cluster resources the
// WVA controller and optimization engine read or scale. They previously lived on
// the VariantAutoscaling reconciler, which has been removed; the permissions are
// still required by the annotation-based discovery reconcilers, the collector, and
// the saturation / scale-from-zero engines. `make manifests` aggregates these into
// config/base/rbac/manager-clusterrole.yaml.

// Scale targets and their pods.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/scale,verbs=get;update

// Node inventory for accelerator/capacity discovery.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;watch

// Core resources read during optimization and metrics collection.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Note: Namespace watch permission is required for label-based namespace opt-in for namespace-local ConfigMaps.

// InferencePool discovery and controller-owned ServiceMonitor observation.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.networking.x-k8s.io;inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch
