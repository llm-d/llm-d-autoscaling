package saturationv1

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// Analyzer implements the V1 percentage-based saturation analyzer.
// It uses KV cache utilization and queue length thresholds to determine saturation.
// The V2 token-based analyzer (saturation_v2 package) replaces this when
// analyzerName is set to "saturation" in config.
type Analyzer struct{}

// NewAnalyzer creates a new saturation analyzer instance
func NewAnalyzer() *Analyzer {
	return &Analyzer{}
}

// AnalyzeModelSaturation analyzes Saturation for all variants of a model.
// It aggregates metrics across all replicas (from all variants) and determines:
// 1. Which replicas are non-saturated
// 2. Average spare Saturation across non-saturated replicas
// 3. Whether to scale up (spare Saturation < trigger)
// 4. Whether scale-down is safe (worst-case simulation)
func (a *Analyzer) AnalyzeModelSaturation(
	ctx context.Context,
	modelID string,
	namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.SaturationScalingConfig,
) (*domain.ModelSaturationAnalysis, error) {

	if len(replicaMetrics) == 0 {
		return &domain.ModelSaturationAnalysis{
			ModelID:       modelID,
			Namespace:     namespace,
			AnalyzedAt:    time.Now(),
			TotalReplicas: 0,
			ShouldScaleUp: false,

			ScaleDownSafe:   false,
			VariantAnalyses: []domain.VariantSaturationAnalysis{},
		}, nil
	}

	analysis := &domain.ModelSaturationAnalysis{
		ModelID:    modelID,
		Namespace:  namespace,
		AnalyzedAt: time.Now(),
	}

	// Step 1: Group metrics by variant and calculate per-variant analysis
	// Pre-count variants to pre-allocate slices (avoids repeated slice reallocation)
	variantCounts := make(map[string]int)
	for _, metric := range replicaMetrics {
		variantCounts[metric.VariantName]++
	}

	// Pre-allocate slices with exact Saturation
	variantMap := make(map[string][]domain.ReplicaMetrics, len(variantCounts))
	for variant, count := range variantCounts {
		variantMap[variant] = make([]domain.ReplicaMetrics, 0, count)
	}

	// Populate with metrics (no reallocation needed)
	for _, metric := range replicaMetrics {
		variantMap[metric.VariantName] = append(variantMap[metric.VariantName], metric)
	}

	// Aggregate statistics across all replicas
	var totalSpareKv float64
	var totalSpareQueue float64
	var nonSaturatedCount int

	variantAnalyses := make([]domain.VariantSaturationAnalysis, 0, len(variantMap))

	for variantName, metrics := range variantMap {
		variantAnalysis := a.analyzeVariant(ctx, variantName, metrics, config)
		variantAnalyses = append(variantAnalyses, variantAnalysis)

		// Aggregate across variants
		nonSaturatedCount += variantAnalysis.NonSaturatedCount
		totalSpareKv += variantAnalysis.AvgSpareKvCapacity * float64(variantAnalysis.NonSaturatedCount)
		totalSpareQueue += variantAnalysis.AvgSpareQueueLength * float64(variantAnalysis.NonSaturatedCount)
	}

	analysis.TotalReplicas = len(replicaMetrics)
	analysis.NonSaturatedCount = nonSaturatedCount
	analysis.VariantAnalyses = variantAnalyses

	// Step 2: Calculate average spare Saturation across all non-saturated replicas
	if nonSaturatedCount > 0 {
		analysis.AvgSpareKvCapacity = totalSpareKv / float64(nonSaturatedCount)
		analysis.AvgSpareQueueLength = totalSpareQueue / float64(nonSaturatedCount)
	}

	// Step 3: Determine scale-up recommendation
	analysis.ShouldScaleUp, analysis.ScaleUpReason = a.shouldScaleUp(
		analysis.AvgSpareKvCapacity,
		analysis.AvgSpareQueueLength,
		config,
	)

	// Step 4: Determine if scale-down is safe
	// Pass pre-calculated average spare capacities to avoid redundant iteration
	analysis.ScaleDownSafe = a.isScaleDownSafe(
		ctx,
		nonSaturatedCount,
		analysis.AvgSpareKvCapacity,
		analysis.AvgSpareQueueLength,
		config,
	)

	ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("saturation analysis completed",
		"modelID", modelID,
		"namespace", namespace,
		"totalReplicas", analysis.TotalReplicas,
		"nonSaturated", nonSaturatedCount,
		"avgSpareKv", analysis.AvgSpareKvCapacity,
		"avgSpareQueue", analysis.AvgSpareQueueLength,
		"shouldScaleUp", analysis.ShouldScaleUp,
		"scaleDownSafe", analysis.ScaleDownSafe)

	return analysis, nil
}

// analyzeVariant analyzes Saturation for a single variant
func (a *Analyzer) analyzeVariant(
	ctx context.Context,
	variantName string,
	metrics []domain.ReplicaMetrics,
	config config.SaturationScalingConfig,
) domain.VariantSaturationAnalysis {

	analysis := domain.VariantSaturationAnalysis{
		VariantName:       variantName,
		ReplicaCount:      len(metrics),
		SaturatedReplicas: []string{},
	}

	if len(metrics) > 0 {
		analysis.AcceleratorName = metrics[0].AcceleratorName
		analysis.Cost = metrics[0].Cost
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Variant analysis initialized",
			"variant", variantName,
			"accelerator", analysis.AcceleratorName,
			"cost", analysis.Cost,
			"replicaCount", len(metrics))
	}

	var totalSpareKv float64
	var totalSpareQueue float64
	var totalKvUsage float64
	var nonSaturatedCount int

	for _, metric := range metrics {
		// Check if replica is saturated
		isSaturated := metric.KvCacheUsage >= config.KvCacheThreshold ||
			float64(metric.QueueLength) >= config.QueueLengthThreshold

		if isSaturated {
			analysis.SaturatedReplicas = append(analysis.SaturatedReplicas, metric.PodName)
		} else {
			// Calculate spare Saturation for non-saturated replica
			spareKv := config.KvCacheThreshold - metric.KvCacheUsage
			spareQueue := config.QueueLengthThreshold - float64(metric.QueueLength)

			totalSpareKv += spareKv
			totalSpareQueue += spareQueue
			nonSaturatedCount++
		}

		// Track max usage and total usage across all replicas
		if metric.KvCacheUsage > analysis.MaxKvCacheUsage {
			analysis.MaxKvCacheUsage = metric.KvCacheUsage
		}
		totalKvUsage += metric.KvCacheUsage
		if metric.QueueLength > analysis.MaxQueueLength {
			analysis.MaxQueueLength = metric.QueueLength
		}
	}

	analysis.NonSaturatedCount = nonSaturatedCount

	// Calculate averages for non-saturated replicas
	if nonSaturatedCount > 0 {
		analysis.AvgSpareKvCapacity = totalSpareKv / float64(nonSaturatedCount)
		analysis.AvgSpareQueueLength = totalSpareQueue / float64(nonSaturatedCount)
	}

	// Calculate mean KV cache usage across all replicas for Utilization field
	if len(metrics) > 0 {
		analysis.AvgKvCacheUsage = totalKvUsage / float64(len(metrics))
	}

	return analysis
}

// shouldScaleUp determines if scale-up is needed based on spare Saturation triggers
func (a *Analyzer) shouldScaleUp(
	avgSpareKv float64,
	avgSpareQueue float64,
	config config.SaturationScalingConfig,
) (bool, string) {

	kvTriggered := avgSpareKv < config.KvSpareTrigger
	queueTriggered := avgSpareQueue < config.QueueSpareTrigger

	// Early return if no triggers fired
	if !kvTriggered && !queueTriggered {
		return false, ""
	}

	// Build reason string based on which trigger(s) fired
	switch {
	case kvTriggered && queueTriggered:
		return true, fmt.Sprintf("both KV spare (%.3f < %.3f) and queue spare (%.1f < %.1f)",
			avgSpareKv, config.KvSpareTrigger, avgSpareQueue, config.QueueSpareTrigger)
	case kvTriggered:
		return true, fmt.Sprintf("KV spare Saturation low (%.3f < %.3f)",
			avgSpareKv, config.KvSpareTrigger)
	default: // only queueTriggered is true
		return true, fmt.Sprintf("queue spare Saturation low (%.1f < %.1f)",
			avgSpareQueue, config.QueueSpareTrigger)
	}
}

// isScaleDownSafe simulates realistic load redistribution after removing one replica.
// Returns isSafe where:
// - isSafe: true if removing one replica would leave adequate headroom
//
// Algorithm: Calculates total current load across non-saturated replicas, then simulates
// redistributing that load across (N-1) replicas to determine if spare Saturation remains adequate.
func (a *Analyzer) isScaleDownSafe(
	ctx context.Context,
	nonSaturatedCount int,
	avgSpareKv float64,
	avgSpareQueue float64,
	config config.SaturationScalingConfig,
) bool {

	// Require minimum non-saturated replicas for scale-down safety
	// With fewer replicas, we cannot safely redistribute load without risking saturation
	if nonSaturatedCount < MinNonSaturatedReplicasForScaleDown {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Scale-down unsafe: insufficient non-saturated replicas",
			"nonSaturated", nonSaturatedCount, "required", MinNonSaturatedReplicasForScaleDown)
		return false
	}

	// Calculate current average load per replica
	// Load = Threshold - Spare
	avgKvLoad := config.KvCacheThreshold - avgSpareKv
	avgQueueLoad := config.QueueLengthThreshold - avgSpareQueue

	// Simulate removing one replica: load increases by factor of N/(N-1)
	// New avg load = current avg load × N/(N-1)
	remainingCount := nonSaturatedCount - 1
	scaleFactor := float64(nonSaturatedCount) / float64(remainingCount)
	avgKvAfterRemoval := avgKvLoad * scaleFactor
	avgQueueAfterRemoval := avgQueueLoad * scaleFactor

	// Calculate spare capacity after redistribution
	// Spare = Threshold - Load
	remainingSpareKv := config.KvCacheThreshold - avgKvAfterRemoval
	remainingSpareQueue := config.QueueLengthThreshold - avgQueueAfterRemoval

	// Safe if both spare margins still exceed triggers
	kvSafe := remainingSpareKv >= config.KvSpareTrigger
	queueSafe := remainingSpareQueue >= config.QueueSpareTrigger

	isSafe := kvSafe && queueSafe

	if !isSafe {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Scale-down unsafe: insufficient headroom after redistribution",
			"remainingSpareKv", remainingSpareKv, "kvTrigger", config.KvSpareTrigger, "kvSafe", kvSafe,
			"remainingSpareQueue", remainingSpareQueue, "queueTrigger", config.QueueSpareTrigger, "queueSafe", queueSafe)
	}

	// Saturation analyzer never initiates scale-down, only approves/denies
	return isSafe
}

// CalculateSaturationTargets determines target replicas per variant based on saturation analysis.
// Step 1: Pure saturation-based target calculation
// Replica counts come from the scale-target status (CurrentReplicas / PendingReplicas),
// which are group-consistent for LeaderWorkerSet; they are not derived from the number
// of metric-reporting pods, which would be in pod units for LWS group size > 1.
// Rules:
// - If ANY variant is transitioning (desired ≠ current OR pending > 0): block all scaling for the model
// - Else if Saturation needs scale-up: cheapest variant (without pending replicas) gets currentReplicas+1
// - Else if Saturation allows scale-down: most expensive variant gets currentReplicas-1
// - Else: target = currentReplicas
func (a *Analyzer) CalculateSaturationTargets(
	ctx context.Context,
	saturationAnalysis *domain.ModelSaturationAnalysis,
	variantStates []domain.VariantReplicaState,
) map[string]int {

	targets := make(map[string]int)
	logger := ctrl.LoggerFrom(ctx)

	// Nil safety
	if saturationAnalysis == nil || len(saturationAnalysis.VariantAnalyses) == 0 {
		// Default: current replicas
		for _, state := range variantStates {
			targets[state.VariantName] = state.CurrentReplicas
		}
		return targets
	}

	// Build state map for quick lookup
	stateMap := make(map[string]domain.VariantReplicaState)
	for _, state := range variantStates {
		stateMap[state.VariantName] = state
	}

	// STEP 1: Check model-level transition state
	// If ANY variant is transitioning, block all scaling decisions for the entire model.
	// This prevents making decisions based on incomplete capacity data.
	modelInTransition := false
	var transitionReasons []string

	for _, va := range saturationAnalysis.VariantAnalyses {
		state := stateMap[va.VariantName]

		// Check 1: Desired vs Current mismatch (scaling in progress)
		desiredCurrentMismatch := state.DesiredReplicas != 0 && state.DesiredReplicas != state.CurrentReplicas
		if desiredCurrentMismatch {
			modelInTransition = true
			transitionReasons = append(transitionReasons,
				fmt.Sprintf("%s: desired(%d)!=current(%d)", va.VariantName, state.DesiredReplicas, state.CurrentReplicas))
		}

		// Check 2: Pods not yet ready (scale-up in progress / cold start)
		// Use PendingReplicas (CurrentReplicas - ReadyReplicas, from scale-target
		// status) rather than comparing va.ReplicaCount (count of metric-reporting
		// pod instances) against state.CurrentReplicas. Those two are in different
		// units for LeaderWorkerSet: CurrentReplicas counts groups while
		// va.ReplicaCount counts pods, so for a group size > 1 the equality check
		// was permanently true and blocked all scaling ("model in transition"
		// forever). PendingReplicas is group-consistent and is already the signal
		// used for per-variant pending-skip below.
		if state.PendingReplicas > 0 {
			modelInTransition = true
			transitionReasons = append(transitionReasons,
				fmt.Sprintf("%s: pending(%d)", va.VariantName, state.PendingReplicas))
		}
	}

	// STEP 2: Initialize targets
	// If model is transitioning, preserve desired (if set) or current replicas
	// If model is stable, use metrics count as the base
	for _, va := range saturationAnalysis.VariantAnalyses {
		state := stateMap[va.VariantName]

		if modelInTransition {
			// Model in transition: preserve desired replicas if set, otherwise current
			if state.DesiredReplicas != 0 && state.DesiredReplicas != state.CurrentReplicas {
				targets[va.VariantName] = state.DesiredReplicas
				logger.V(logging.DEBUG).Info("Target set to desired (model transitioning)",
					"variant", va.VariantName, "desired", state.DesiredReplicas)
			} else {
				targets[va.VariantName] = state.CurrentReplicas
				logger.V(logging.DEBUG).Info("Target set to current (model transitioning)",
					"variant", va.VariantName, "current", state.CurrentReplicas)
			}
		} else {
			// Model stable: base the target on current replicas (scale-target
			// status), not va.ReplicaCount. va.ReplicaCount is the number of
			// metric-reporting pod instances, which for a LeaderWorkerSet group
			// size > 1 exceeds the replica (group) count and would over-scale.
			// The model only reaches this branch when not in transition, so
			// PendingReplicas == 0 and CurrentReplicas == ReadyReplicas; using
			// CurrentReplicas preserves the prior "ready replicas" base for
			// Deployments while staying group-consistent for LWS.
			targets[va.VariantName] = state.CurrentReplicas
			logger.V(logging.DEBUG).Info("Target initialized to current replicas (stable)",
				"variant", va.VariantName, "count", state.CurrentReplicas)
		}
	}

	// STEP 3: If model is transitioning, log and return early (no scaling decisions)
	if modelInTransition {
		logger.Info("Model in transition, blocking scaling decisions",
			"modelID", saturationAnalysis.ModelID,
			"reasons", transitionReasons)
		return targets
	}

	// STEP 4: Model is stable - proceed with scaling decisions
	switch {
	case saturationAnalysis.ShouldScaleUp:
		// Find cheapest variant for scale-up, skipping variants with pending replicas
		var cheapestVariant *domain.VariantSaturationAnalysis
		for i := range saturationAnalysis.VariantAnalyses {
			va := &saturationAnalysis.VariantAnalyses[i]

			// Skip variants with pending replicas to prevent cascade scaling
			state := stateMap[va.VariantName]
			if state.PendingReplicas > 0 {
				logger.V(logging.DEBUG).Info("Skipping variant with pending replicas for scale-up",
					"variant", va.VariantName, "pendingReplicas", state.PendingReplicas)
				continue
			}

			// Select cheapest, with stable tie-breaking by variant name (alphabetically first)
			if cheapestVariant == nil ||
				va.Cost < cheapestVariant.Cost ||
				(va.Cost == cheapestVariant.Cost && va.VariantName < cheapestVariant.VariantName) {
				cheapestVariant = va
			}
		}

		if cheapestVariant != nil {
			state := stateMap[cheapestVariant.VariantName]
			baseTarget := targets[cheapestVariant.VariantName]
			targets[cheapestVariant.VariantName] = baseTarget + 1
			logger.V(logging.VERBOSE).Info("Saturation target: scale-up cheapest variant",
				"variant", cheapestVariant.VariantName, "cost", cheapestVariant.Cost, "currentReplicas", state.CurrentReplicas,
				"readyReplicas", cheapestVariant.ReplicaCount, "baseTarget", baseTarget, "target", targets[cheapestVariant.VariantName], "reason", saturationAnalysis.ScaleUpReason)
		}

	case saturationAnalysis.ScaleDownSafe:
		// Find most expensive variant for scale-down
		var mostExpensiveVariant *domain.VariantSaturationAnalysis
		for i := range saturationAnalysis.VariantAnalyses {
			va := &saturationAnalysis.VariantAnalyses[i]
			// Can't scale down if at or below minimum (1 replica)
			baseTarget := targets[va.VariantName]
			if baseTarget <= 1 {
				continue
			}
			// Select most expensive, with stable tie-breaking by variant name
			if mostExpensiveVariant == nil ||
				va.Cost > mostExpensiveVariant.Cost ||
				(va.Cost == mostExpensiveVariant.Cost && va.VariantName > mostExpensiveVariant.VariantName) {
				mostExpensiveVariant = va
			}
		}

		if mostExpensiveVariant != nil {
			state := stateMap[mostExpensiveVariant.VariantName]
			baseTarget := targets[mostExpensiveVariant.VariantName]
			targets[mostExpensiveVariant.VariantName] = baseTarget - 1
			logger.V(logging.VERBOSE).Info("Saturation target: scale-down most expensive variant",
				"variant", mostExpensiveVariant.VariantName, "cost", mostExpensiveVariant.Cost, "currentReplicas", state.CurrentReplicas,
				"readyReplicas", mostExpensiveVariant.ReplicaCount, "baseTarget", baseTarget, "target", targets[mostExpensiveVariant.VariantName])
		}
	default:
		// No scaling action needed - Saturation is adequate and stable
		logger.V(logging.DEBUG).Info("Saturation targets: no scaling needed",
			"avgSpareKvCapacity", saturationAnalysis.AvgSpareKvCapacity,
			"avgSpareQueueLength", saturationAnalysis.AvgSpareQueueLength)
	}

	// Apply min/max replica bounds from VA spec fields
	for _, state := range variantStates {
		if target, ok := targets[state.VariantName]; ok {
			if state.MinReplicas != nil && target < *state.MinReplicas {
				logger.V(logging.DEBUG).Info("Clamping target to minReplicas",
					"variant", state.VariantName, "target", target, "minReplicas", *state.MinReplicas)
				targets[state.VariantName] = *state.MinReplicas
			}
			if state.MaxReplicas != nil && *state.MaxReplicas > 0 && target > *state.MaxReplicas {
				logger.V(logging.DEBUG).Info("Clamping target to maxReplicas",
					"variant", state.VariantName, "target", target, "maxReplicas", *state.MaxReplicas)
				targets[state.VariantName] = *state.MaxReplicas
			}
		}
	}

	return targets
}
