package saturation

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	queueingmodel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// refuseQueueingModel is the dispatch target selected when a queueing-model
// ConfigMap is present. The queueing-model optimize path (optimizeQueueingModel,
// below) is deferred — it is not yet a first-class voting analyzer in the
// multi-analyzer engine — so the engine refuses to dispatch it rather than
// silently running the parked path or silently falling through to the
// saturation-only / V1 path (either would mask a misconfiguration). It logs an
// error and produces no decisions; the dispatch case leaves the decision set
// empty, so the caller's unconditional applySaturationDecisions re-affirms each
// model's last-good replica count and still emits the HPA/KEDA scaling metric
// this cycle — affected models are held in place rather than dropped. To
// re-enable the path later, restore the dispatch to optimizeQueueingModel.
//
// The hold is reported as well as logged: a Warning event is raised on every
// held variant here, and the caller's refused flag drives
// TypeOptimizationReady=False/OptimizationRefused on each one (vestigial today
// — nothing reads that condition back, see applySaturationDecisions). Without
// the event, a cluster that has stopped autoscaling entirely is
// indistinguishable from a healthy idle one, with a repeating controller log
// line as the only evidence.
func (e *Engine) refuseQueueingModel(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) {
	logger := ctrl.LoggerFrom(ctx)
	logger.Error(
		errors.New("queueing-model optimization path is disabled"),
		"refusing to dispatch the queueing-model path; enable the saturation and/or throughput analyzers instead",
		"modelGroups", len(modelGroups),
	)
	e.recordOptimizationRefusedEvent(modelGroups)
}

// optimizeQueueingModel and the two helpers below (runQueueingModelAnalysis,
// buildQMConfig) are no longer dispatched — refuseQueueingModel replaced them at
// the engine's dispatch switch. They are retained in-tree as a deferred design
// direction (a queueing-model-driven analyzer), parked until the multi-analyzer
// engine can host it as a first-class voting analyzer. The blank reference below
// keeps this parked call-subtree reachable so the unused linter does not flag it;
// drop the reference when the path is re-dispatched.
var _ = (*Engine).optimizeQueueingModel

// optimizeQueueingModel runs the queueing model-based analysis path.
// Follows the same three-stage pattern as optimizeV2:
//  1. Collect ModelScalingRequests (metrics + analysis per model)
//  2. Call optimizer to produce VariantDecisions
//  3. Apply enforcer constraints per model
func (e *Engine) optimizeQueueingModel(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// update analyzer given current models
	currentModelKeys := make(map[string]bool, len(modelGroups))
	for _, modelVAs := range modelGroups {
		namespace := modelVAs[0].Namespace // there should be at least one VA in a model group
		modelID := modelVAs[0].Spec.ModelID
		currentModelKeys[queueingmodel.MakeModelKey(namespace, modelID)] = true
	}
	e.queueingModelAnalyzer.Update(currentModelKeys)

	// Stage 1: Collect ModelScalingRequests for all models
	requests := make([]pipeline.ModelScalingRequest, 0, len(modelGroups))
	// modelScaleTargets carries each model's scale targets into stage 3, where
	// applyScaleToZeroEnforcement needs them to gate the enforcer. Captured here
	// because data.scaleTargets is only in scope during this collection loop.
	modelScaleTargets := make(map[string]map[string]scaletarget.ScaleTargetAccessor)

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (queueing-model)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			logger.Error(err, "Model data preparation failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			continue
		}

		qmConfigMap := e.Config.QMAnalyzerConfigForNamespace(namespace)
		qConfig := buildQMConfig(qmConfigMap, namespace, modelID)

		result, err := e.runQueueingModelAnalysis(ctx, modelID, namespace,
			data.replicaMetrics, qConfig, data.variantStates)
		if err != nil {
			logger.Error(err, "Queueing model analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			continue
		}

		requests = append(requests, pipeline.ModelScalingRequest{
			ModelID:   modelID,
			Namespace: namespace,
			AnalyzerResults: []pipeline.NamedAnalyzerResult{{
				Name:      domain.SaturationAnalyzerName,
				Result:    result,
				Score:     1.0, // QM path: single analyzer, no per-entry score config
				Remaining: result.RequiredCapacity,
				Spare:     result.SpareCapacity,
				// Enabled is statically true: the queueing-model path runs a single
				// analyzer, so it is always the voting member and the anchor's
				// binding/(a) carrier for this model.
				Enabled: true,
				// Live is statically true: the queueing-model path is not yet a
				// per-analyzer-liveness participant (it doesn't run through
				// updateLivenessAndSetLive), so it must not be caught by the
				// liveness safety floor. Its own liveness tracking will be added
				// when it becomes a first-class multi-analyzer participant.
				Live: true,
			}},
			VariantStates: data.variantStates,
		})
		modelScaleTargets[utils.GetNamespacedKey(namespace, modelID)] = data.scaleTargets
	}

	if len(requests) == 0 {
		return nil
	}

	// Stage 2: Call optimizer
	allDecisions := e.optimizer.Optimize(ctx, requests, nil)

	logger.Info("Queueing model optimizer produced decisions",
		"optimizer", e.optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions). Routed through the
	// shared gate so a non-vLLM (e.g. SGLang) model is not falsely zeroed — the
	// queueing-model path previously enforced ungated (see applyScaleToZeroEnforcement).
	for _, req := range requests {
		e.applyScaleToZeroEnforcement(
			ctx, req.ModelID, req.Namespace, e.optimizer.Name(),
			allDecisions,
			modelScaleTargets[utils.GetNamespacedKey(req.Namespace, req.ModelID)],
			req.VariantStates,
		)
	}

	return allDecisions
}

// runQueueingModelAnalysis runs the queueing model analyzer for a single model
// and returns the raw AnalyzerResult.
func (e *Engine) runQueueingModelAnalysis(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config *queueingmodel.QMConfig,
	variantStates []domain.VariantReplicaState,
) (*domain.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	input := domain.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         config,
	}

	result, err := e.queueingModelAnalyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("queueing model analysis failed: %w", err)
	}

	logger.Info("Queueing model analysis completed",
		"modelID", modelID,
		"totalSupply", result.TotalSupply,
		"totalDemand", result.TotalDemand,
		"utilization", result.Utilization,
		"requiredCapacity", result.RequiredCapacity,
		"spareCapacity", result.SpareCapacity)

	return result, nil
}

// buildQMConfig creates a QMConfig for a specific model.
// It starts from the "default" entry in allConfigs, then applies any per-model
// override whose ModelID and Namespace match. Per-model entries can override
// sloMultiplier, tuningEnabled, and provide explicit SLO targets (targetTTFT/targetITL).
// Falls back to defaults when fields are zero/nil.
func buildQMConfig(
	allConfigs map[string]domain.QueueingModelScalingConfig,
	namespace, modelID string,
) *queueingmodel.QMConfig {
	cfg := &queueingmodel.QMConfig{
		TuningEnabled: true,
		SLOMultiplier: queueingmodel.DefaultSLOMultiplier,
	}

	// Apply "default" entry as base
	if defaultCfg, ok := allConfigs["default"]; ok {
		if defaultCfg.TuningEnabled != nil {
			cfg.TuningEnabled = *defaultCfg.TuningEnabled
		}
		if defaultCfg.SLOMultiplier > 1.0 {
			cfg.SLOMultiplier = defaultCfg.SLOMultiplier
		}
	}

	// Scan for a per-model override matching this model
	for key, entry := range allConfigs {
		if key == "default" {
			continue
		}
		if entry.ModelID != modelID || entry.Namespace != namespace {
			continue
		}

		// Override sloMultiplier and tuningEnabled from per-model entry
		if entry.SLOMultiplier > 1.0 {
			cfg.SLOMultiplier = entry.SLOMultiplier
		}
		if entry.TuningEnabled != nil {
			cfg.TuningEnabled = *entry.TuningEnabled
		}

		// Populate explicit SLO targets if both are set
		if entry.TargetTTFT > 0 && entry.TargetITL > 0 {
			modelKey := queueingmodel.MakeModelKey(namespace, modelID)
			cfg.SLOTargets = map[string]*queueingmodel.SLOTarget{
				modelKey: {
					TargetTTFT: entry.TargetTTFT,
					TargetITL:  entry.TargetITL,
				},
			}
		}
		break // only one per-model entry should match
	}

	return cfg
}
