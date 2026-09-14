package pipeline

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturationv1"
)

// RequestCountFuncType is the signature for functions that retrieve the total request count
// for a model over a specified retention period. This type alias improves readability
// and makes the function signature reusable across the codebase.
type RequestCountFuncType func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error)

// Enforcer applies scale-to-zero and minimum replica enforcement after saturation analysis.
type Enforcer struct {
	// requestCountFunc is a function that returns the total request count for a model.
	// Injected for testability.
	requestCountFunc RequestCountFuncType
	metricsEmitter   *metrics.MetricsEmitter
}

// NewEnforcer creates a new scale-to-zero enforcer.
func NewEnforcer(requestCountFunc RequestCountFuncType) *Enforcer {
	return &Enforcer{
		requestCountFunc: requestCountFunc,
		metricsEmitter:   metrics.NewMetricsEmitter(),
	}
}

// EnforcePolicyOnDecisions applies scale-to-zero and minimum replica enforcement
// directly on VariantDecision slices. It operates on decisions in-place.
//
// Returns true if scale-to-zero was applied (all variants scaled to zero).
func (e *Enforcer) EnforcePolicyOnDecisions(
	ctx context.Context,
	modelID string,
	namespace string,
	decisions []domain.VariantDecision,
	scaleToZeroConfig config.ScaleToZeroConfigData,
	satConfig *config.SaturationScalingConfig,
	optimizerName string,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	scaleToZeroEnabled := config.ResolveScaleToZeroEnabled(satConfig, scaleToZeroConfig, modelID)

	if scaleToZeroEnabled {
		applied := e.applyScaleToZeroOnDecisions(ctx, modelID, namespace, decisions, scaleToZeroConfig, optimizerName)
		logger.V(logging.DEBUG).Info("Scale-to-zero policy enforced",
			"modelID", modelID,
			"optimizer", optimizerName,
			"scaleToZeroEnabled", true,
			"scaledToZero", applied)
		return applied
	}

	// Scale-to-zero disabled: ensure minimum replicas
	applied := e.ensureMinimumReplicasOnDecisions(ctx, modelID, namespace, decisions, optimizerName)
	logger.V(logging.DEBUG).Info("Minimum replica policy enforced",
		"modelID", modelID,
		"optimizer", optimizerName,
		"scaleToZeroEnabled", false,
		"minimumPreserved", applied)
	return false
}

// applyScaleToZeroOnDecisions checks if the model has had any requests and sets
// all matching decisions' TargetReplicas to 0 if idle.
func (e *Enforcer) applyScaleToZeroOnDecisions(
	ctx context.Context,
	modelID string,
	namespace string,
	decisions []domain.VariantDecision,
	scaleToZeroConfig config.ScaleToZeroConfigData,
	optimizerName string,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	retentionPeriod := config.ScaleToZeroRetentionPeriod(scaleToZeroConfig, modelID)

	requestCount, err := e.requestCountFunc(ctx, modelID, namespace, retentionPeriod)
	if err != nil {
		errorType := "Failed to get request count, keeping current decisions"
		logger.Error(err, errorType,
			"modelID", modelID,
			"namespace", namespace)
		metrics.RecordError(constants.ComponentEnforcer, errorType)
		return false
	}

	if requestCount > 0 {
		logger.V(logging.DEBUG).Info("Model has recent requests, keeping decisions",
			"modelID", modelID,
			"requestCount", requestCount,
			"retentionPeriod", retentionPeriod)
		return false
	}

	// No requests: scale to zero
	logger.Info("No requests in retention period, scaling decisions to zero",
		"modelID", modelID,
		"namespace", namespace,
		"retentionPeriod", retentionPeriod)

	for i := range decisions {
		d := &decisions[i]
		if d.ModelID != modelID || d.Namespace != namespace {
			continue
		}
		d.TargetReplicas = 0
		updateDecisionAction(d, optimizerName, constants.EnforcerPolicyTypeScaleToZero, e.metricsEmitter)
	}

	return true
}

// ensureMinimumReplicasOnDecisions ensures at least 1 replica exists across all
// matching decisions when scale-to-zero is disabled. If total TargetReplicas is 0,
// preserves 1 replica on the cheapest variant.
func (e *Enforcer) ensureMinimumReplicasOnDecisions(
	ctx context.Context,
	modelID string,
	namespace string,
	decisions []domain.VariantDecision,
	optimizerName string,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	// Calculate total replicas for this model
	totalReplicas := 0
	for i := range decisions {
		if decisions[i].ModelID == modelID && decisions[i].Namespace == namespace {
			totalReplicas += decisions[i].TargetReplicas
		}
	}

	if totalReplicas > 0 {
		return false
	}

	// Total is 0 — find cheapest variant and set it to 1
	cheapestIdx := -1
	cheapestCost := float64(-1)

	for i := range decisions {
		d := &decisions[i]
		if d.ModelID != modelID || d.Namespace != namespace {
			continue
		}
		cost := d.Cost
		if cost <= 0 {
			cost = saturationv1.DefaultVariantCost
		}
		if cheapestCost < 0 || cost < cheapestCost || (cost == cheapestCost && d.VariantName < decisions[cheapestIdx].VariantName) {
			cheapestIdx = i
			cheapestCost = cost
		}
	}

	if cheapestIdx >= 0 {
		decisions[cheapestIdx].TargetReplicas = 1
		updateDecisionAction(&decisions[cheapestIdx], optimizerName, constants.EnforcerPolicyTypeMinimumReplicas, e.metricsEmitter)
		logger.Info("Preserving minimum replica on cheapest variant (scale-to-zero disabled)",
			"modelID", modelID,
			"variant", decisions[cheapestIdx].VariantName,
			"cost", cheapestCost)
		return true
	}

	return false
}

// updateDecisionAction recomputes a decision's Action from TargetReplicas vs
// CurrentReplicas after enforcement and refreshes its reason. SetDecisionReason
// is the single place that writes d.Action.
func updateDecisionAction(d *domain.VariantDecision, optimizerName, policyType string, metricsEmitter *metrics.MetricsEmitter) {
	var action domain.SaturationAction
	switch {
	case d.TargetReplicas > d.CurrentReplicas:
		action = domain.ActionScaleUp
	case d.TargetReplicas < d.CurrentReplicas:
		action = domain.ActionScaleDown
	default:
		action = domain.ActionNoChange
	}
	// Preserve the decision's original reason category (e.g. saturation-only on
	// the V1 path) instead of hardcoding V2, so an enforced decision is not
	// mis-attributed in the reason metric label. Fall back to V2 if it was unset.
	category := d.ReasonCategory()
	if category == "" {
		category = domain.DecisionReasonV2
	}
	d.SetDecisionReason(action, category, fmt.Sprintf("%s %s (optimizer: %s, enforced)", category, action, optimizerName))

	// finally record metric
	metricsEmitter.RecordEnforcerMetric(policyType)
}
