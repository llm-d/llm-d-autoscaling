package saturation_v2

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// SaturationAnalyzer implements the domain.Analyzer interface using a
// token-based capacity model with memory-bound (k1) and compute-bound (k2)
// constraints. It replaces the V1 percentage-based analyzer when
// analyzerName is set to "saturation".
type SaturationAnalyzer struct {
	// mu protects computeCapacityHistory from concurrent access.
	mu sync.Mutex
	// computeCapacityHistory stores rolling averages of observed k2 values,
	// keyed by "modelID|accelerator|gpuCount|outputBucket".
	// TODO: check if we need to use other model parameters as key in the future.
	computeCapacityHistory map[string]*rollingAverage
	capacityStore          *CapacityKnowledgeStore
}

// NewSaturationAnalyzer creates a new V2 saturation analyzer backed by the
// given capacity store.
func NewSaturationAnalyzer(store *CapacityKnowledgeStore) *SaturationAnalyzer {
	return &SaturationAnalyzer{
		computeCapacityHistory: make(map[string]*rollingAverage),
		capacityStore:          store,
	}
}

// Name returns the analyzer identifier for logging and result metadata.
// Note: the config value "saturation" (in analyzerName YAML field) selects this analyzer,
// but the descriptive name here is used in AnalyzerResult.AnalyzerName for observability.
func (a *SaturationAnalyzer) Name() string {
	return "saturation-token-based"
}

// EvictStaleHistory removes k2 history entries that have not been updated
// within the given timeout. This prevents unbounded memory growth from
// deleted models or workload buckets that are no longer active.
func (a *SaturationAnalyzer) EvictStaleHistory(timeout time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	evicted := 0
	for key, ra := range a.computeCapacityHistory {
		if time.Since(ra.lastUpdated) > timeout {
			delete(a.computeCapacityHistory, key)
			evicted++
		}
	}
	return evicted
}

// Analyze computes capacity signals for a model across all its variants.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.SaturationScalingConfig)
	if !ok {
		return nil, fmt.Errorf("expected *SaturationScalingConfig, got %T", input.Config)
	}

	// Build GPU count and P/D role lookups from variant states. The role decides
	// how a waiting request is charged against a replica's KV capacity, so it must
	// be known before per-replica demand is computed.
	gpusByVariant := make(map[string]int, len(input.VariantStates))
	rolesByVariant := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
		rolesByVariant[vs.VariantName] = vs.Role
	}

	// Phase 1: Per-replica capacity computation
	replicaCapacities := make([]ReplicaCapacity, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := gpusByVariant[rm.VariantName]
		rc := a.computeReplicaCapacity(rm, satConfig, input.ModelID, input.Namespace, gpuCount, rolesByVariant[rm.VariantName])
		if rc != nil {
			replicaCapacities = append(replicaCapacities, *rc)
		}
	}

	// Phase 2: Per-variant aggregation
	variantCapacities := a.aggregateByVariant(replicaCapacities, input.ReplicaMetrics, input.VariantStates, input.ModelID, input.Namespace, satConfig.KvCacheThreshold)

	// Phase 3: Model-level aggregation via shared helpers (enforces linearity invariant).
	totalSupply := aggregation.SumTotalSupply(variantCapacities)
	totalAnticipatedSupply := aggregation.SumTotalAnticipatedSupply(variantCapacities)
	totalDemand := aggregation.SumTotalDemand(variantCapacities)

	// Track active roles for queue demand attribution.
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		activeRoles[canonicalRole(vc.Role)] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control).
	queueDemand := estimateSchedulerQueueDemand(input.SchedulerQueue, input.ReplicaMetrics, activeRoles)
	totalDemand += queueDemand.total

	var utilization float64
	if totalSupply > 0 {
		utilization = totalDemand / totalSupply
	}

	// Phase 4: Per-role aggregation (P/D disaggregation).
	// RequiredCapacity and SpareCapacity are NOT computed here — the engine's
	// universal threshold post-step writes them after Analyze() returns.
	roleCapacities := a.aggregateByRole(variantCapacities, queueDemand.byRole)

	// Phase 5: Build result. RequiredCapacity and SpareCapacity left zero;
	// the engine post-step overwrites them using TotalDemand, TotalSupply,
	// and TotalAnticipatedSupply with the resolved thresholds.
	result := &domain.AnalyzerResult{
		AnalyzerName:           a.Name(),
		ModelID:                input.ModelID,
		Namespace:              input.Namespace,
		AnalyzedAt:             time.Now(),
		VariantCapacities:      variantCapacities,
		TotalSupply:            totalSupply,
		TotalDemand:            totalDemand,
		TotalAnticipatedSupply: totalAnticipatedSupply,
		Utilization:            utilization,
		RoleCapacities:         roleCapacities,
	}

	return result, nil
}

// computeReplicaCapacity computes the capacity breakdown for a single replica.
// The role argument is the replica's P/D role, which determines how requests
// waiting in the local engine queue are charged (see waitingQueueDemand).
// An empty role is treated as domain.RoleBoth.
// Returns nil if the replica has no V2 capacity data (TotalKvCapacityTokens == 0).
func (a *SaturationAnalyzer) computeReplicaCapacity(
	rm domain.ReplicaMetrics,
	config *config.SaturationScalingConfig,
	modelID, namespace string,
	gpuCount int,
	role string,
) *ReplicaCapacity {
	if rm.TotalKvCapacityTokens <= 0 {
		// TODO: implement proper demand estimation when vllm:cache_config_info is absent.
		// Currently we fall back to percentage-based demand using the deployment-derived
		// capacity from the capacity store. A better approach would be to estimate
		// TotalKvCapacityTokens from deployment args (num_gpu_blocks_override, block_size)
		// or use a dedicated percentage-based demand signal.
		return a.computeReplicaCapacityFallback(rm, config, modelID, namespace, role)
	}

	// Compute demand: tokens already resident in KV cache plus the role-aware
	// footprint of requests still waiting in the local engine queue.
	replicaDemand := rm.TokensInUse + waitingQueueDemand(rm, role)

	// k1: memory-bound capacity
	k1 := int64(float64(rm.TotalKvCapacityTokens) * config.KvCacheThreshold)

	// k2: compute-bound capacity
	var engineParams *EngineParams
	if rec := a.capacityStore.Get(namespace, modelID, rm.VariantName); rec != nil {
		engineParams = rec.EngineParams
	}
	k2, k2Priority := a.computeK2(
		modelID, rm.AcceleratorName,
		gpuCount,
		rm.QueueLength, rm.TokensInUse,
		rm.AvgOutputTokens, rm.AvgInputTokens,
		config.QueueLengthThreshold,
		engineParams,
		k1,
	)

	effectiveCapacity := k1
	if k2 < k1 {
		effectiveCapacity = k2
	}

	isSaturated := replicaDemand >= effectiveCapacity

	// Update capacity store with live data, preserving EngineParams from any
	// existing record (parsed from deployment args and needed for FindCompatible).
	var existingParams *EngineParams
	if existing := a.capacityStore.Get(namespace, modelID, rm.VariantName); existing != nil && existing.EngineParams != nil {
		existingParams = existing.EngineParams
	}
	a.capacityStore.Update(namespace, modelID, rm.VariantName, CapacityRecord{
		AcceleratorName:       rm.AcceleratorName,
		GpuCount:              gpuCount,
		NumGpuBlocks:          rm.NumGpuBlocks,
		BlockSize:             rm.BlockSize,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		EffectiveCapacity:     effectiveCapacity,
		EngineParams:          existingParams,
		LearnedFrom:           learnedFromLive,
	})

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       rm.AcceleratorName,
		TokensInUse:           rm.TokensInUse,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		MemoryBoundCapacity:   k1,
		ComputeBoundCapacity:  k2,
		K2Priority:            k2Priority,
		EffectiveCapacity:     effectiveCapacity,
		IsSaturated:           isSaturated,
		ReplicaDemand:         replicaDemand,
	}
}

// computeReplicaCapacityFallback handles the case where vllm:cache_config_info
// is not available (TotalKvCapacityTokens == 0). It uses the deployment-derived
// capacity from the capacity store and estimates demand from KvCacheUsage percentage.
// This allows V2 to work with model servers that don't emit cache_config_info
// (e.g., the llm-d-inference-sim).
func (a *SaturationAnalyzer) computeReplicaCapacityFallback(
	rm domain.ReplicaMetrics,
	cfg *config.SaturationScalingConfig,
	modelID, namespace string,
	role string,
) *ReplicaCapacity {
	rec := a.capacityStore.Get(namespace, modelID, rm.VariantName)
	if rec == nil || rec.EffectiveCapacity <= 0 {
		return nil
	}

	// Apply KvCacheThreshold to match the main path (where k1 = totalTokens * threshold).
	// For deployment-derived records, EffectiveCapacity is the raw estimate; the threshold
	// reduces it to the usable portion, consistent with the normal code path.
	effectiveCapacity := int64(float64(rec.EffectiveCapacity) * cfg.KvCacheThreshold)
	if effectiveCapacity <= 0 {
		return nil
	}

	// Estimate demand from KV cache usage percentage applied to the thresholded capacity.
	// This is a coarse approximation — KvCacheUsage reflects memory pressure, not
	// exact token demand — but it's sufficient when token-level metrics are absent.
	replicaDemand := int64(rm.KvCacheUsage * float64(effectiveCapacity))

	// Add the role-aware footprint of requests waiting in the local engine queue,
	// matching the main path.
	//
	// Caveat, pre-existing and not introduced here: for a deployment-derived
	// record, EffectiveCapacity is EffectiveMaxBatchedTokens — a *per-step* token
	// budget the store itself calls "a safe lower bound" — while this addend is in
	// absolute KV tokens. The two are not the same unit, so on that record a deep
	// queue can push replicaDemand past effectiveCapacity and report saturation
	// that the replica's actual KV occupancy does not support. Raising the
	// per-request charge lowers the queue depth at which that happens. Tracked
	// separately; fixing it means pairing the fallback's demand and capacity units,
	// not adjusting this line.
	replicaDemand += waitingQueueDemand(rm, role)

	isSaturated := replicaDemand >= effectiveCapacity

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       rm.AcceleratorName,
		TokensInUse:           replicaDemand,
		TotalKvCapacityTokens: effectiveCapacity, // synthetic: store-derived
		MemoryBoundCapacity:   effectiveCapacity,
		ComputeBoundCapacity:  effectiveCapacity,
		K2Priority:            k2SrcFallback,
		EffectiveCapacity:     effectiveCapacity,
		IsSaturated:           isSaturated,
		ReplicaDemand:         replicaDemand,
	}
}

// computeK2 determines the compute-bound capacity using a priority chain:
// 1. Observed (queue saturated) → use tokensInUse as k2
// 2. Historical → rolling average from previous observations
// 3. Derived (from deployment args) → formula-based estimate
// 4. Fallback → k1 (memory-bound only)
// Returns the k2 value and the priority level (1–4) that produced it.
func (a *SaturationAnalyzer) computeK2(
	modelID, accelerator string,
	gpuCount int,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	engineParams *EngineParams,
	k1 int64,
) (int64, k2Source) {
	outputBucket := classifyOutputLength(avgOutput)
	historyKey := fmt.Sprintf("%s|%s|%d|%s", modelID, accelerator, gpuCount, outputBucket)

	// Priority 1: Observed (queue saturated)
	if queueLen >= int(queueThreshold) && tokensInUse > 0 {
		k2Observed := tokensInUse
		a.mu.Lock()
		ra, ok := a.computeCapacityHistory[historyKey]
		if !ok {
			ra = newRollingAverage(RollingAverageWindowSize)
			a.computeCapacityHistory[historyKey] = ra
		}
		ra.Add(float64(k2Observed))
		a.mu.Unlock()
		return k2Observed, k2SrcObserved
	}

	// Priority 2: Historical — lock must cover Average() since Add() mutates
	// the same slice from Priority 1 under the same lock.
	a.mu.Lock()
	var histAvg float64
	if ra, ok := a.computeCapacityHistory[historyKey]; ok {
		histAvg = ra.Average()
	}
	a.mu.Unlock()
	if histAvg > 0 {
		return int64(histAvg), k2SrcHistorical
	}

	// Priority 3: Derived from deployment args
	if k2Derived := estimateCapacityFromParams(engineParams, avgInput, avgOutput); k2Derived > 0 {
		return k2Derived, k2SrcDerived
	}

	// Priority 4: Fallback to k1
	return k1, k2SrcFallback
}

// aggregateByVariant groups replica capacities by variant and computes
// per-variant capacity metrics.
func (a *SaturationAnalyzer) aggregateByVariant(
	replicaCapacities []ReplicaCapacity,
	inputMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState,
	modelID, namespace string,
	kvCacheThreshold float64,
) []domain.VariantCapacity {
	// Group replicas by variant
	byVariant := make(map[string][]ReplicaCapacity)
	for _, rc := range replicaCapacities {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], rc)
	}

	// Build cost and accelerator lookup from input metrics
	variantCost := make(map[string]float64)
	variantAccel := make(map[string]string)
	for _, rm := range inputMetrics {
		if _, ok := variantCost[rm.VariantName]; !ok {
			variantCost[rm.VariantName] = rm.Cost
			variantAccel[rm.VariantName] = rm.AcceleratorName
		}
	}

	// Compute model-level workload averages from live replica metrics.
	// Used for capacity estimation of zero-replica variants with deployment-derived params.
	modelAvgInput, modelAvgOutput, _ := computeModelWorkloadAverages(inputMetrics)

	result := make([]domain.VariantCapacity, 0, len(variantStates))
	for _, vs := range variantStates {
		replicas := byVariant[vs.VariantName]

		var perReplicaCapacity float64
		var totalDemand float64
		accelerator := variantAccel[vs.VariantName]
		cost := variantCost[vs.VariantName]

		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		// replicaCount multiplies perReplicaCapacity to form TotalCapacity. It also
		// sets VariantCapacity.ReplicaCount, which aggregation.go uses to recompute
		// supply totals — so both must stay in sync. Defaults to readyCount (pod
		// count) for the fallback branches, which have no per-instance data.
		replicaCount := readyCount
		// pendingCount must match replicaCount's unit (SumTotalAnticipatedSupply
		// adds them); converted to instances below when replicaCount switches.
		pendingCount := vs.PendingReplicas

		var capacityLabel string
		if len(replicas) > 0 {
			// len(replicas) counts vLLM engine instances (DP ranks), not pods: a DP=8
			// pod hosts 8 independently-capacitied instances. Using the pod count would
			// undercount TotalCapacity by the DP factor.
			capacities := make([]int64, 0, len(replicas))
			for _, rc := range replicas {
				capacities = append(capacities, rc.EffectiveCapacity)
				totalDemand += float64(rc.ReplicaDemand)
			}
			perReplicaCapacity = float64(median(capacities))
			replicaCount = len(replicas)
			// Convert pending to instance units. readyCount and vs.PendingReplicas
			// share the scale-target unit (pods for Deployment, groups for LWS), so
			// len(replicas)/readyCount approximates the instances-per-unit (DP)
			// factor. This is exact only in steady state: len(replicas) is live
			// metrics while readyCount is (lagging) scale-target status, so during a
			// scale-up — when new instances report metrics before their unit is
			// counted ready — the ratio is inflated and pendingCount overshoots
			// (understating RequiredCapacity). A metrics-derived instances-per-unit
			// (grouping by pod/LWS group) would remove this skew; tracked as a
			// follow-up. TODO: scale-from-zero (readyCount == 0) can't infer DP at
			// all and is left unconverted here.
			if readyCount > 0 {
				if instancesPerUnit := len(replicas) / readyCount; instancesPerUnit > 1 {
					pendingCount = vs.PendingReplicas * instancesPerUnit
				}
			}
			if accelerator == "" {
				accelerator = replicas[0].AcceleratorName
			}
			capacityLabel = k2SourceLabel(replicas)
		} else if rec := a.capacityStore.Get(namespace, modelID, vs.VariantName); rec != nil && rec.EffectiveCapacity > 0 {
			// No ready replicas — use stored capacity, enhanced with k2 derivation
			// for deployment-derived records when workload data is available.
			perReplicaCapacity = a.estimateStoredCapacity(rec, modelID, kvCacheThreshold, modelAvgInput, modelAvgOutput)
			capacityLabel = satReasonP0Store
		} else if rec := a.lookupCompatibleCapacity(namespace, modelID, vs.VariantName, accelerator, vs.GPUsPerReplica); rec != nil {
			// No own record — try cross-variant estimation from a compatible variant
			perReplicaCapacity = float64(rec.EffectiveCapacity)
			capacityLabel = satReasonP0Store
		} else {
			capacityLabel = satReasonNoData
		}

		totalCapacity := float64(replicaCount) * perReplicaCapacity

		var utilization float64
		if totalCapacity > 0 {
			utilization = totalDemand / totalCapacity
		}

		result = append(result, domain.VariantCapacity{
			VariantName:        vs.VariantName,
			AcceleratorName:    accelerator,
			Cost:               cost,
			Role:               vs.Role,
			ReplicaCount:       replicaCount,
			PendingReplicas:    pendingCount,
			PerReplicaCapacity: perReplicaCapacity,
			TotalCapacity:      totalCapacity,
			TotalDemand:        totalDemand,
			Utilization:        utilization,
			Reason:             capacityLabel,
		})
	}

	return result
}

// aggregateByRole groups variant capacities by role and returns per-role
// Total* aggregates for the engine post-step to compute RC/SC from.
// Returns nil when no disaggregation is active (all variants are role "both"
// or empty). The queueDemandByRole map adds scheduler queue demand attributed
// to each role (nil when there's no queue demand).
//
// RequiredCapacity and SpareCapacity are left zero — the engine post-step
// writes them after Analyze() returns using the universal threshold formula.
func (a *SaturationAnalyzer) aggregateByRole(
	variantCapacities []domain.VariantCapacity,
	queueDemandByRole map[string]float64,
) map[string]domain.RoleCapacity {
	// Check if any variant has a non-"both" role.
	hasDisaggregation := false
	for _, vc := range variantCapacities {
		if vc.Role != "" && vc.Role != domain.RoleBoth {
			hasDisaggregation = true
			break
		}
	}
	if !hasDisaggregation {
		return nil
	}

	// Aggregate supply/demand/anticipated per role via shared helpers.
	totals := aggregation.AggregateByRole(variantCapacities)

	// Add scheduler queue demand attributed to each role.
	for role, qd := range queueDemandByRole {
		if t, ok := totals[role]; ok {
			t.TotalDemand += qd
			totals[role] = t
		}
	}

	result := make(map[string]domain.RoleCapacity, len(totals))
	for role, t := range totals {
		result[role] = domain.RoleCapacity{
			Role:                   role,
			TotalSupply:            t.TotalSupply,
			TotalDemand:            t.TotalDemand,
			TotalAnticipatedSupply: t.TotalAnticipatedSupply,
		}
	}
	return result
}

// lookupCompatibleCapacity searches the capacity store for a record from
// another variant with matching hardware and engine parameters. This enables
// capacity estimation for zero-replica variants that have no prior data.
// The search is cross-namespace since capacity depends on hardware + config,
// not namespace.
func (a *SaturationAnalyzer) lookupCompatibleCapacity(namespace, modelID, variantName, accelerator string, gpuCount int) *CapacityRecord {
	// Get EngineParams for this variant (from deployment-derived record)
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return nil
	}
	return a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.EngineParams)
}

// estimateStoredCapacity returns a capacity estimate for a zero-replica variant
// using its stored CapacityRecord. For learnedFromLive records (from a previously running
// pod), the stored EffectiveCapacity is authoritative. For "deployment" records,
// it tries to compute a better estimate using the k2 derivation formula with
// model-level workload averages, bounded by:
//  1. A compatible variant's live EffectiveCapacity (already min(k1,k2))
//  2. Own k1 if TotalKvCapacityTokens is known (from num_gpu_blocks_override)
//
// Falls back to stored EffectiveCapacity (EffectiveMaxBatchedTokens) when no
// workload data is available.
func (a *SaturationAnalyzer) estimateStoredCapacity(rec *CapacityRecord, modelID string, kvCacheThreshold float64, modelAvgInput, modelAvgOutput float64) float64 {
	if rec == nil {
		return 0
	}

	// Live records have observed capacity — use directly
	if rec.LearnedFrom == learnedFromLive {
		return float64(rec.EffectiveCapacity)
	}

	// For deployment-derived records, try k2 derivation with workload data
	if rec.EngineParams != nil && modelAvgOutput > 0 {
		if derived := estimateCapacityFromParams(rec.EngineParams, modelAvgInput, modelAvgOutput); derived > 0 {
			bounded := derived

			// Bound by own k1 if TotalKvCapacityTokens is known (num_gpu_blocks_override)
			if rec.TotalKvCapacityTokens > 0 && kvCacheThreshold > 0 {
				k1 := int64(float64(rec.TotalKvCapacityTokens) * kvCacheThreshold)
				if k1 > 0 && k1 < bounded {
					bounded = k1
				}
			}

			// Bound by compatible variant's live EffectiveCapacity (already min(k1,k2))
			if compatible := a.capacityStore.FindCompatible(modelID, rec.AcceleratorName, rec.GpuCount, rec.EngineParams); compatible != nil && compatible.LearnedFrom == learnedFromLive && compatible.EffectiveCapacity > 0 {
				if compatible.EffectiveCapacity < bounded {
					bounded = compatible.EffectiveCapacity
				}
			}

			return float64(bounded)
		}
	}

	// Fallback: stored EffectiveCapacity (EffectiveMaxBatchedTokens from LoadFromDeployment)
	return float64(rec.EffectiveCapacity)
}

// estimateCapacityFromParams computes a capacity estimate using the k2 derivation
// formula: N_steady = min(B * O / (I + O), S), capacity = N_steady * (I + O/2).
// Used by computeK2 (Priority 3) for per-replica estimation and by
// estimateStoredCapacity for zero-replica variants with model-level workload averages.
// Returns 0 if estimation is not possible.
func estimateCapacityFromParams(params *EngineParams, avgInput, avgOutput float64) int64 {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 || avgOutput <= 0 {
		return 0
	}

	B := float64(params.EffectiveMaxBatchedTokens)
	S := float64(params.MaxNumSeqs)
	I := avgInput
	O := avgOutput

	nSteady := B * O / (I + O)
	if nSteady > S {
		nSteady = S
	}
	k2Derived := int64(nSteady * (I + O/2))
	if k2Derived > 0 {
		return k2Derived
	}
	return 0
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
func computeModelWorkloadAverages(replicaMetrics []domain.ReplicaMetrics) (avgInput, avgOutput, avgHitRate float64) {
	var count int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgOutput += rm.AvgOutputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgOutput /= float64(count)
		avgHitRate /= float64(count)
	}
	return avgInput, avgOutput, avgHitRate
}

// canonicalRole normalizes an empty variant role to domain.RoleBoth, matching
// aggregation.AggregateByRole.
func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// waitingQueueDemand estimates the KV-token demand of the requests waiting in a
// replica's local engine queue (vllm:num_requests_waiting / sglang:num_queue_reqs).
// Their footprint is projected from the replica's average request shape, since
// the queue metric carries no per-request token counts.
//
// Note these requests are not uniformly blockless: on the decode side a
// transfer-pending request (vLLM's WAITING_FOR_REMOTE_KVS) has already had
// blocks allocated, so part of its prompt KV is also inside KvCacheUsage and is
// counted twice. The overlap shrinks toward zero under KV pressure, when block
// allocation starts failing — i.e. in the saturated regime where the scaling
// decision is actually made.
//
// The per-request cost is role-aware, because a replica only pays for the KV it
// actually materializes:
//
//	Prefill:      AvgInputTokens                   (prompt KV only; output is generated elsewhere)
//	Decode/Both:  AvgInputTokens + AvgOutputTokens (holds prompt KV and grows it per generated token)
//
// Charging decode replicas for input alone understates demand for
// long-generation workloads, which are precisely the ones whose KV pressure is
// output-driven. That is the under-reporting this addresses. It mirrors the role
// attribution estimateSchedulerQueueDemand already applies model-wide.
//
// # Why the full output length, and how it relates to the capacity side
//
// I+O is a request's KV footprint at its LAST decode step, not its mean. It is
// deliberately a peak, no-preemption planning charge: once admitted, a decode
// request's KV grows monotonically and the engine cannot shed it without
// preemption and recompute, so a replica expected to host the request needs room
// for its final size.
//
// I+O is this analyzer's demand-side unit. It is deliberately not the unit used
// by the time-averaged models elsewhere, and the distinction matters when reading
// the two together:
//
//   - Saturation V2 (here, demand): I+O — peak footprint. Sizes for what a
//     replica must be able to hold, not what it holds on average.
//   - Throughput analyzer (throughput.WorkloadShape.KVreq): ILeff + O/2 —
//     time-averaged. A SEPARATE analyzer with its own model and its own
//     supply/demand pairing; it does not constrain this term and is not
//     inconsistent with it.
//
// One asymmetry is genuinely internal to this analyzer and should not be confused
// with the above: estimateCapacityFromParams prices a *concurrent* request at
// I + O/2 when deriving k2, so a queued request is charged more than it will
// occupy on average, and its charge drops once it is admitted and starts being
// measured through KvCacheUsage instead. Both effects bias this term toward
// scale-up.
//
// Note also that this term and the resident term are both derived from 1-minute
// maxima (max_over_time on kv_cache_usage_perc and num_requests_waiting), whose
// peaks need not coincide, so their sum can exceed any demand the replica
// actually saw at a single instant. That is a pre-existing property of the
// collector's queries, not of this function, but it compounds the bias above.
//
// That trade is chosen on purpose: under-provisioning decode capacity causes
// preemption and recompute thrash, which costs more than a spare replica.
// Revisiting it means changing the demand/capacity pair together, not this
// function alone.
//
// Returns 0 when the queue is empty, or when token metrics are absent or
// non-finite.
func waitingQueueDemand(rm domain.ReplicaMetrics, role string) int64 {
	if rm.QueueLength <= 0 {
		return 0
	}

	tokensPerRequest := rm.AvgInputTokens
	if canonicalRole(role) != domain.RolePrefill {
		tokensPerRequest += rm.AvgOutputTokens
	}
	// Compute in float64 and validate before converting, so one check covers
	// NaN, infinities, and finite values too large for an int64. Converting an
	// out-of-range float64 to int64 is implementation-defined in Go (amd64
	// yields math.MinInt64, arm64 saturates), and either way the result is
	// garbage that reads downstream as "idle, remove replicas" or as enormous
	// demand. Note `x <= 0` would NOT catch NaN, since every NaN comparison is
	// false. The collector filters NaN/Inf on the token averages, so this is a
	// guard rather than a live bug; it does not filter QueueLength, which is a
	// bare int conversion, but a garbage value there is caught either by the
	// QueueLength <= 0 check above or by the range bound below.
	//
	// Multiplying before truncating also drops the per-request rounding the
	// previous truncate-then-multiply form accumulated: at queue 3 and an average
	// of 100.9 tokens this yields 302 rather than 300.
	// The bound is >=, not >: float64(math.MaxInt64) rounds up to 2^63, which is
	// one past the largest representable int64, so a demand of exactly 2^63 would
	// pass a > check and then overflow to MinInt64.
	demand := float64(rm.QueueLength) * tokensPerRequest
	if !(demand > 0) || demand >= float64(math.MaxInt64) {
		return 0
	}

	return int64(demand)
}

// schedulerQueueDemand holds the estimated token demand from scheduler-queued
// requests, broken down by P/D role for disaggregated models.
type schedulerQueueDemand struct {
	total  float64            // model-level total (inputTokens + outputTokens)
	byRole map[string]float64 // per-role demand: "prefill", "decode", "both"
}

// estimateSchedulerQueueDemand estimates the token demand from requests queued
// in the llm-d inference scheduler's flow control layer, with per-role
// attribution for P/D disaggregated models.
//
// These requests have not yet reached any engine pod, so we estimate their
// token footprint using two independent signals:
//
//	inputTokens = max(queueBytes / BytesPerToken, queueSize * avgInputTokens)
//	             * (1 - prefixCacheHitRate)
//	outputTokens = queueSize * avgOutputTokens
//
// Role attribution:
//   - Prefill: inputTokens (prompt KV must be computed and stored)
//   - Decode:  inputTokens + outputTokens (receives KV transfer + generates output)
//   - Both:    inputTokens + outputTokens (handles full request lifecycle)
//   - Model-level total: inputTokens + outputTokens (unchanged for backward compat)
//
// The prefix cache hit rate reduces expected input token KV demand because
// a fraction of prompt tokens will hit the prefix cache and reuse existing
// KV blocks. This does NOT apply to the local engine queue
// (vllm:num_requests_waiting / sglang:num_queue_reqs) because those requests
// have not yet had prefix cache lookup performed.
func estimateSchedulerQueueDemand(
	sq *domain.SchedulerQueueMetrics,
	replicaMetrics []domain.ReplicaMetrics,
	activeRoles map[string]bool,
) schedulerQueueDemand {
	if sq == nil || (sq.QueueSize == 0 && sq.QueueBytes == 0) {
		return schedulerQueueDemand{}
	}

	// Compute model-level averages from replica metrics
	avgInput, avgOutput, avgHitRate := computeModelWorkloadAverages(replicaMetrics)

	// Estimate input tokens from two signals, take the max for robustness
	tokensFromBytes := float64(sq.QueueBytes) / BytesPerToken
	tokensFromCount := float64(sq.QueueSize) * avgInput
	inputTokens := tokensFromBytes
	if tokensFromCount > inputTokens {
		inputTokens = tokensFromCount
	}

	// Apply prefix cache hit rate reduction to input tokens only
	inputTokens *= (1 - avgHitRate)

	// Estimate output tokens (no cache reduction — output must be generated)
	outputTokens := float64(sq.QueueSize) * avgOutput

	total := inputTokens + outputTokens

	// Build per-role attribution
	byRole := make(map[string]float64)
	if len(activeRoles) > 0 {
		for role := range activeRoles {
			switch role {
			case domain.RolePrefill:
				byRole[domain.RolePrefill] = inputTokens
			case domain.RoleDecode:
				byRole[domain.RoleDecode] = inputTokens + outputTokens
			default: // domain.RoleBoth or unknown
				byRole[role] = total
			}
		}
	}

	return schedulerQueueDemand{total: total, byRole: byRole}
}

// k2SourceLabel returns the K2Priority label for the lower-median replica by
// EffectiveCapacity. Sorts a copy and picks index (n-1)/2, which always
// resolves to an actual replica — no average is taken, so even-length slices
// never produce a value that matches no element.
// Returns "" when replicas is empty.
func k2SourceLabel(replicas []ReplicaCapacity) string {
	if len(replicas) == 0 {
		return ""
	}
	sorted := make([]ReplicaCapacity, len(replicas))
	copy(sorted, replicas)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EffectiveCapacity < sorted[j].EffectiveCapacity
	})
	medIdx := (len(sorted) - 1) / 2
	if label, ok := k2Labels[sorted[medIdx].K2Priority]; ok {
		return label
	}
	return pipeline.ReasonError
}

// median returns the median value from a sorted slice of int64 values.
// Returns 0 if the slice is empty.
func median(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}

	sorted := make([]int64, n)
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}
