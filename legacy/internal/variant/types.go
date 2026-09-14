// Package variant defines the in-memory VariantAutoscaling representation used by
// the WVA optimization pipeline (collector, analyzers, engines, actuator).
//
// It was previously the llmd.ai/v1alpha1 VariantAutoscaling CRD API. The CRD has
// been removed; discovery now happens by synthesizing these structs from annotated
// HPAs and KEDA ScaledObjects (see internal/utils/variant_fromannotations.go). The
// types are kept as plain in-memory structs — they are never registered in a scheme
// or written to the Kubernetes API server.
package variant

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VariantAutoscalingConfigSpec holds the optional tuning fields for a VariantAutoscaling.
type VariantAutoscalingConfigSpec struct {
	// VariantCost specifies the cost per replica for this variant (used in saturation analysis).
	VariantCost string `json:"variantCost,omitempty"`
}

// VariantAutoscalingSpec defines the desired state for autoscaling a model variant.
type VariantAutoscalingSpec struct {
	// ScaleTargetRef references the scalable resource to manage.
	// This follows the same pattern as HorizontalPodAutoscaler.
	ScaleTargetRef autoscalingv2.CrossVersionObjectReference `json:"scaleTargetRef"`

	// ModelID specifies the unique identifier of the model to be autoscaled.
	ModelID string `json:"modelID"`

	// MinReplicas is the lower bound on the number of replicas for this variant.
	// A value of 0 enables scale-to-zero when the model is idle.
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound on the number of replicas for this variant.
	// The autoscaler will never scale beyond this value regardless of load.
	MaxReplicas int32 `json:"maxReplicas"`

	// VariantAutoscalingConfigSpec holds optional tuning fields that integrators can embed.
	VariantAutoscalingConfigSpec `json:",inline"`
}

// VariantAutoscalingStatus represents the current status of autoscaling for a variant,
// including the desired optimized allocation and actuation status.
type VariantAutoscalingStatus struct {
	// DesiredOptimizedAlloc indicates the target optimized allocation based on autoscaling logic.
	DesiredOptimizedAlloc OptimizedAlloc `json:"desiredOptimizedAlloc,omitempty"`

	// Actuation provides details about the actuation process and its current status.
	Actuation ActuationStatus `json:"actuation,omitempty"`

	// Conditions represent the latest available observations of the VariantAutoscaling's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// OptimizedAlloc describes the target optimized allocation for a model variant.
type OptimizedAlloc struct {
	// LastRunTime is the timestamp of the last optimization run.
	LastRunTime metav1.Time `json:"lastRunTime,omitempty"`

	// Accelerator is the type of accelerator for the optimized allocation.
	//
	// Deprecated: This field is deprecated and will be removed in a future version. Use node selector or node affinity from scale target instead.
	Accelerator string `json:"accelerator,omitempty"`

	// NumReplicas is the number of replicas for the optimized allocation.
	// nil means no optimization decision has been made yet.
	NumReplicas *int32 `json:"numReplicas,omitempty"`
}

// ActuationStatus provides details about the actuation process and its current status.
type ActuationStatus struct {
	// Applied indicates whether the actuation was successfully applied.
	Applied bool `json:"applied"`
}

// VariantAutoscaling is the in-memory representation of the autoscaling configuration
// and status for a model variant. It embeds metav1.TypeMeta/ObjectMeta so downstream
// code can continue to read Name/Namespace/Labels/Annotations and record Kubernetes
// Events against it, as it did with the former CRD.
//
// It implements runtime.Object so the shared EventRecorder can reference it, but it is
// never registered in a scheme or persisted to the Kubernetes API server — variants are
// synthesized from annotated HPAs/ScaledObjects.
type VariantAutoscaling struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state for autoscaling the model variant.
	Spec VariantAutoscalingSpec `json:"spec,omitempty"`

	// Status represents the current status of autoscaling for the model variant.
	Status VariantAutoscalingStatus `json:"status,omitempty"`
}

// VariantAutoscalingList contains a list of VariantAutoscaling resources.
type VariantAutoscalingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// Items is the list of VariantAutoscaling resources.
	Items []VariantAutoscaling `json:"items"`
}

// Condition Types for VariantAutoscaling
const (
	// TypeTargetResolved indicates whether the target model variant has been resolved successfully
	TypeTargetResolved = "TargetResolved"
	// TypeMetricsAvailable indicates whether vLLM metrics are available from Prometheus
	TypeMetricsAvailable = "MetricsAvailable"
	// TypeOptimizationReady indicates whether the optimization engine can run successfully
	TypeOptimizationReady = "OptimizationReady"
)

// Condition Reasons for MetricsAvailable
const (
	// ReasonMetricsFound indicates vLLM metrics were successfully retrieved
	ReasonMetricsFound = "MetricsFound"
	// ReasonMetricsMissing indicates vLLM metrics are not available (likely ServiceMonitor issue)
	ReasonMetricsMissing = "MetricsMissing"
	// ReasonMetricsStale indicates metrics exist but are outdated
	ReasonMetricsStale = "MetricsStale"
	// ReasonPrometheusError indicates error querying Prometheus
	ReasonPrometheusError = "PrometheusError"
)

// Condition messages for MetricsAvailable
const (
	// MessageMetricsAvailable indicates metrics are available for scaling decisions
	MessageMetricsAvailable = "Saturation metrics data is available for scaling decisions"
	// MessageMetricsUnavailable indicates metrics are not available
	MessageMetricsUnavailable = "No saturation metrics available - pods may not be ready or metrics not yet scraped"
)

// Condition Reasons for OptimizationReady
const (
	// ReasonOptimizationSucceeded indicates optimization completed successfully
	ReasonOptimizationSucceeded = "OptimizationSucceeded"
	// ReasonOptimizationFailed indicates optimization failed
	ReasonOptimizationFailed = "OptimizationFailed"
	// ReasonMetricsUnavailable indicates optimization cannot run due to missing metrics
	ReasonMetricsUnavailable = "MetricsUnavailable"
	// ReasonInvalidConfiguration indicates VA has invalid configuration (e.g., missing ModelID)
	ReasonInvalidConfiguration = "InvalidConfiguration"
	// ReasonSkippedProcessing indicates VA was skipped during processing
	ReasonSkippedProcessing = "SkippedProcessing"
	// ReasonOptimizationRefused indicates the engine declined to run the configured
	// optimize path at all, so replicas are held at their last-good value. Unlike
	// ReasonOptimizationSucceeded with no scaling change, this is a standing state
	// that persists until the configuration is corrected.
	ReasonOptimizationRefused = "OptimizationRefused"

	// ReasonTargetFound indicates the scale target was successfully resolved
	ReasonTargetFound = "TargetFound"
	// ReasonTargetNotFound indicates the scale target could not be found
	ReasonTargetNotFound = "TargetNotFound"
)

// GetScaleTargetAPI returns the API of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetAPI() string {
	return va.Spec.ScaleTargetRef.APIVersion
}

// GetScaleTargetName returns the name of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetName() string {
	return va.Spec.ScaleTargetRef.Name
}

// GetScaleTargetKind returns the kind of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetKind() string {
	return va.Spec.ScaleTargetRef.Kind
}

// SetCondition sets the specified condition on the VariantAutoscaling status.
func SetCondition(va *VariantAutoscaling, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: va.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&va.Status.Conditions, condition)
}

// GetCondition returns the condition with the specified type.
func GetCondition(va *VariantAutoscaling, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(va.Status.Conditions, conditionType)
}

// IsConditionTrue returns true if the condition with the specified type has status True.
func IsConditionTrue(va *VariantAutoscaling, conditionType string) bool {
	return meta.IsStatusConditionTrue(va.Status.Conditions, conditionType)
}

// IsConditionFalse returns true if the condition with the specified type has status False.
func IsConditionFalse(va *VariantAutoscaling, conditionType string) bool {
	return meta.IsStatusConditionFalse(va.Status.Conditions, conditionType)
}
