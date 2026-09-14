package actuator

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Actuator struct {
	Client         client.Client
	MetricsEmitter *metrics.MetricsEmitter
}

func NewActuator(k8sClient client.Client) *Actuator {
	return &Actuator{
		Client:         k8sClient,
		MetricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// GetCurrentScaleTargetReplicasFromVA gets the real current replica count from the actual Deployment/LWS
func (a *Actuator) GetCurrentScaleTargetReplicasFromVA(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling) (int32, error) {
	// Use ScaleTargetRef to get the scale target name
	scaleTarget, err := scaletarget.FetchScaleTarget(ctx, a.Client, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
	if err != nil {
		return 0, fmt.Errorf("failed to get scale target %s/%s: %w", va.Namespace, va.GetScaleTargetName(), err)
	}
	return a.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaleTarget)
}

// GetCurrentScaleTargetReplicasFromScaleTarget gets the real current replica count from the actual Deployment/LWS
func (a *Actuator) GetCurrentScaleTargetReplicasFromScaleTarget(va *llmdOptv1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) (int32, error) {
	if scaleTarget == nil {
		return 0, fmt.Errorf("scale target cannot be nil for %s/%s", va.Namespace, va.GetScaleTargetName())
	}

	// Prefer status replicas (actual current state)
	if scaleTarget.GetStatusReplicas() >= 0 {
		return scaleTarget.GetStatusReplicas(), nil
	}

	// Fallback to spec if status not ready
	if scaleTarget.GetReplicas() != nil {
		return *scaleTarget.GetReplicas(), nil
	}

	// Final fallback
	return 1, nil
}

func (a *Actuator) EmitMetrics(ctx context.Context, variantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	logger := log.FromContext(ctx)
	if variantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas == nil {
		logger.Info("Skipping EmitReplicaMetrics - no optimization decision yet",
			"variantName", variantAutoscaling.Name)
		return nil
	}

	desiredReplicas := *variantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas

	// Get real current replicas from Deployment (not stale variantAutoscaling status)
	currentReplicas, err := a.GetCurrentScaleTargetReplicasFromVA(ctx, variantAutoscaling)
	if err != nil {
		logger.Error(err, "Could not get current scale target replicas, using variantAutoscaling status",
			"variantName", variantAutoscaling.Name)
		currentReplicas = 0 // Fallback to 0 since CurrentAlloc is removed
	}

	if err := a.MetricsEmitter.EmitReplicaMetrics(
		ctx,
		variantAutoscaling,
		currentReplicas,
		desiredReplicas, // Inferno's optimization target
		variantAutoscaling.Status.DesiredOptimizedAlloc.Accelerator,
	); err != nil {
		logger.Error(err, "Failed to emit optimization signals for variantAutoscaling",
			"variantName", variantAutoscaling.Name)
		// Don't fail the reconciliation for metric emission errors
		// Metrics are critical for HPA, but emission failures shouldn't break core functionality
		return nil
	}
	logger.Info("EmitReplicaMetrics completed",
		"variantName", variantAutoscaling.Name,
		"currentReplicas", currentReplicas,
		"desiredReplicas", desiredReplicas,
		"accelerator", variantAutoscaling.Status.DesiredOptimizedAlloc.Accelerator)
	return nil
}

// RecordSaturationMetrics records saturation analysis and KV cache capacity
// metrics from a decision. The controller does not actively push metrics —
// Prometheus scrapes them — so the verb is "Record", not "Emit".
func (a *Actuator) RecordSaturationMetrics(ctx context.Context, decision domain.VariantDecision) {
	a.MetricsEmitter.RecordSaturationMetrics(
		ctx,
		decision.VariantName,
		decision.Namespace,
		decision.ModelID,
		decision.AcceleratorName,
		decision.RequiredCapacityUnit,
		decision.Utilization,
		decision.SpareCapacity,
		decision.RequiredCapacity,
		decision.KvCacheTokensUsed,
		decision.KvCacheTokensCapacity,
	)
}

// RecordSaturationFreshness publishes the per-VA freshness signal for the
// saturation/capacity gauges. fresh=true is set in cycles where the optimizer
// produced a fresh decision for the variant (i.e. RecordSaturationMetrics was
// just called); fresh=false is set in cycles where the analyzer was aware of
// the variant but emitted no fresh decision.
func (a *Actuator) RecordSaturationFreshness(ctx context.Context, variantName, namespace string, fresh bool) {
	a.MetricsEmitter.RecordSaturationFreshness(ctx, variantName, namespace, fresh)
}
