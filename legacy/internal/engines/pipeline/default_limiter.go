package pipeline

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// DefaultLimiter combines an Inventory with an AllocationAlgorithm to constrain
// scaling decisions based on resource availability.
//
// The limiter follows the pipeline pattern:
//  1. Refresh inventory to get latest resource limits from cluster
//  2. Calculate current GPU usage from decisions
//  3. Create allocator with available resources
//  4. Run allocation algorithm to distribute resources
//  5. Update decision metadata (WasLimited, LimitedBy, DecisionSteps)
type DefaultLimiter struct {
	name           string
	inventory      Inventory
	algorithm      AllocationAlgorithm
	metricsEmitter *metrics.MetricsEmitter
}

// NewDefaultLimiter creates a limiter that combines inventory tracking with
// an allocation algorithm.
func NewDefaultLimiter(name string, inventory Inventory, algorithm AllocationAlgorithm) *DefaultLimiter {
	return &DefaultLimiter{
		name:           name,
		inventory:      inventory,
		algorithm:      algorithm,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// Name returns the limiter identifier for logging/metrics.
func (l *DefaultLimiter) Name() string {
	return l.name
}

// Limit applies resource constraints to scaling decisions.
// Modifies decisions in place - may reduce TargetReplicas based on available resources.
func (l *DefaultLimiter) Limit(ctx context.Context, decisions []*domain.VariantDecision) error {
	if len(decisions) == 0 {
		return nil
	}

	// Step 1: Refresh inventory to get latest limits from cluster
	if err := l.inventory.Refresh(ctx); err != nil {
		return fmt.Errorf("failed to refresh inventory: %w", err)
	}

	// Step 1b: Resolve empty/unknown accelerator names using inventory.
	// In homogeneous clusters (single GPU type), map to the real type so
	// usage counting and allocation both use the correct pool. Decisions
	// are mutated in place — the resolved type flows to status and metrics.
	l.resolveUnknownAccelerators(decisions)

	// Step 2: Calculate current GPU usage from decisions
	usedByType := l.calculateUsedGPUs(decisions)
	l.inventory.SetUsed(usedByType)

	// Step 2b: Feed namespace-aware inventories the per-namespace usage map.
	// The per-type SetUsed above is always safe (namespace-only inventories
	// treat it as a no-op); namespace-scoped inventories additionally need
	// usage bucketed by namespace to enforce per-tenant caps correctly.
	if nai, ok := l.inventory.(NamespaceAwareInventory); ok {
		nai.SetUsedByNamespace(l.calculateUsedGPUsByNamespace(decisions))
	}

	// Step 3: Create allocator with available resources
	allocator := l.inventory.CreateAllocator(ctx)

	// Snapshot each decision's WasLimited and TargetReplicas before the algorithm
	// runs. updateDecisionMetadata derives two DIFFERENT signals from them,
	// because "which limiter to credit" and "did this pass tighten the target"
	// are distinct questions in a CompositeLimiter (WasLimited is sticky — the
	// algorithm only ever sets it true — and every constituent runs Limit over
	// the same slice):
	//   - Attribution (LimitedBy + the limited metric) fires on the WasLimited
	//     false→true transition, so the FIRST constituent to constrain a decision
	//     is credited exactly once (no double-count across constituents, and a
	//     GPU denial a MinReplicas floor later hides is still counted).
	//   - The DecisionStep's WasConstrained flag / "limited" wording fires when
	//     THIS pass actually reduced the target due to a resource limit (not a
	//     MaxReplicas user ceiling), so every constituent that tightens the target
	//     is recorded accurately in the per-step trace.
	limitedBefore := make([]bool, len(decisions))
	targetBefore := make([]int, len(decisions))
	for i, d := range decisions {
		limitedBefore[i] = d.WasLimited
		targetBefore[i] = d.TargetReplicas
	}

	// Step 4: Run allocation algorithm to distribute resources
	if err := l.algorithm.Allocate(ctx, decisions, allocator); err != nil {
		return fmt.Errorf("allocation algorithm failed: %w", err)
	}

	// Step 5: Update decision metadata
	l.updateDecisionMetadata(decisions, limitedBefore, targetBefore)

	return nil
}

// resolveUnknownAccelerators maps empty or "unknown" accelerator names to the
// real GPU type when the inventory has exactly one type (homogeneous cluster).
// This must run before calculateUsedGPUs so existing replicas are counted
// against the correct pool, and before TryAllocate so new allocations use it.
func (l *DefaultLimiter) resolveUnknownAccelerators(decisions []*domain.VariantDecision) {
	pools := l.inventory.GetResourcePools()
	if len(pools) != 1 {
		return // heterogeneous or empty — can't resolve
	}
	var realType string
	for t := range pools {
		realType = t
	}
	for _, d := range decisions {
		if !constants.IsAcceleratorResolved(d.AcceleratorName) {
			d.AcceleratorName = realType
		}
	}
}

// calculateUsedGPUs computes current GPU usage per accelerator type.
// Uses CurrentReplicas * GPUsPerReplica for each decision.
func (l *DefaultLimiter) calculateUsedGPUs(decisions []*domain.VariantDecision) map[string]int {
	usedByType := make(map[string]int)
	for _, d := range decisions {
		if d.AcceleratorName == "" {
			continue
		}
		usedByType[d.AcceleratorName] += d.CurrentReplicas * d.GPUsPerReplica
	}
	return usedByType
}

// calculateUsedGPUsByNamespace computes current GPU usage bucketed by
// (namespace, accelerator type). Outer key: namespace; inner key: accelerator
// type. Used to feed NamespaceAwareInventory implementations the per-namespace
// usage map needed for per-tenant cap enforcement.
func (l *DefaultLimiter) calculateUsedGPUsByNamespace(decisions []*domain.VariantDecision) map[string]map[string]int {
	usedByNS := make(map[string]map[string]int)
	for _, d := range decisions {
		if d.AcceleratorName == "" || d.Namespace == "" {
			continue
		}
		perType, ok := usedByNS[d.Namespace]
		if !ok {
			perType = make(map[string]int)
			usedByNS[d.Namespace] = perType
		}
		perType[d.AcceleratorName] += d.CurrentReplicas * d.GPUsPerReplica
	}
	return usedByNS
}

// updateDecisionMetadata sets LimitedBy and adds DecisionSteps. limitedBefore and
// targetBefore are the per-decision WasLimited / TargetReplicas snapshots taken
// before this limiter's algorithm ran, indexed to match decisions. See Limit for
// why attribution and the per-step flag use different signals.
func (l *DefaultLimiter) updateDecisionMetadata(decisions []*domain.VariantDecision, limitedBefore []bool, targetBefore []int) {
	for i, d := range decisions {
		// Attribution: credit the first limiter that newly marks the decision
		// resource-limited (WasLimited is sticky across composite constituents).
		if d.WasLimited && !limitedBefore[i] {
			d.LimitedBy = l.name
			l.metricsEmitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)
		}

		// Per-step trace: mark the step constrained (and word the reason
		// "limited:") when THIS pass reduced the target due to a resource limit,
		// so a later constituent that further tightens the target is still
		// recorded. A MaxReplicas user ceiling reduces the target without setting
		// WasLimited, so it is not counted as a resource limit here.
		reducedHere := d.WasLimited && d.TargetReplicas < targetBefore[i]
		reason := l.buildStepReason(d, reducedHere)
		d.AddDecisionStep(l.name, reason, reducedHere)
	}
}

// buildStepReason creates a human-readable reason for the decision step. limited
// reflects whether THIS limiter constrained the decision this pass (matching the
// step's limited flag), not the sticky WasLimited.
func (l *DefaultLimiter) buildStepReason(d *domain.VariantDecision, limited bool) string {
	replicaChange := d.TargetReplicas - d.CurrentReplicas

	if replicaChange <= 0 {
		return fmt.Sprintf("no scale-up (target=%d, current=%d)", d.TargetReplicas, d.CurrentReplicas)
	}
	if limited {
		return fmt.Sprintf("limited: allocated %d GPUs for +%d replicas", d.GPUsAllocated, replicaChange)
	}
	return fmt.Sprintf("allocated %d GPUs for +%d replicas", d.GPUsAllocated, replicaChange)
}

// ComputeConstraints refreshes the inventory and returns its resource availability.
// This is the V2 path: expose constraints for the optimizer instead of modifying
// decisions directly (which is what Limit() does for the V1 path).
//
// It always exposes per-type (cluster) availability via Pools. When the
// underlying inventory is namespace-aware (e.g. a namespace-scoped quota), it
// additionally exposes per-(namespace, type) caps via NamespacePools for the
// active namespaces (the keys of usageByNamespace) as a closed allowlist; the
// optimizer caps a model's allocation at min(per-type pool, that namespace's
// pool) for listed types and denies types the namespace does not list (full
// V1/V2 parity — see NamespaceResourcePools). Multi-entry quotas are handled
// by the engine computing constraints from each constituent of a CompositeLimiter.
//
// The keys of usageByNamespace define the active-namespace set: a namespace with
// a quota but zero current usage must still appear here (with an empty inner
// map) for its caps to be materialized and enforced.
//
// Remaining gap (tracked by the limiter chain, sub-issue #1003): composing a
// physical-inventory limiter with a quota limiter as min(physical, quota) within
// a single ComputeConstraints. See docs/developer-guide/quota-limiter.md.
func (l *DefaultLimiter) ComputeConstraints(ctx context.Context, usageByType map[string]int, usageByNamespace map[string]map[string]int) (*ResourceConstraints, error) {
	// Step 1: Refresh inventory (same as Limit step 1)
	if err := l.inventory.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("failed to refresh inventory: %w", err)
	}

	// Step 2: Set current usage (same as Limit step 2)
	l.inventory.SetUsed(usageByType)

	// Step 3: Expose per-type availability
	rc := &ResourceConstraints{
		ProviderName: l.name,
		Pools:        l.inventory.GetResourcePools(),
		TotalLimit:   l.inventory.TotalLimit(),
		TotalUsed:    l.inventory.TotalUsed(),
		TotalAvail:   l.inventory.TotalAvailable(),
	}

	// Step 4: For namespace-aware inventories carrying per-namespace caps for the
	// active namespaces, expose them via NamespacePools and derive the cluster
	// per-type aggregate (Pools/Totals) from the SAME active set. Deriving the
	// aggregate from the active namespaces — rather than the static
	// GetResourcePools sum — keeps it consistent with the per-namespace budgets
	// the optimizer partitions: it includes default fall-through namespaces and
	// never under-counts, so the optimizer's per-type budget cannot be driven
	// negative as namespaces draw against it. When the inventory carries no
	// namespace caps (e.g. a cluster-scoped quota), Pools is left as the
	// per-type GetResourcePools result above.
	if nai, ok := l.inventory.(NamespaceAwareInventory); ok {
		nai.SetUsedByNamespace(usageByNamespace)
		activeNamespaces := slices.Collect(maps.Keys(usageByNamespace))
		if nsPools := nai.NamespaceResourcePools(activeNamespaces); len(nsPools) > 0 {
			rc.NamespacePools = nsPools
			rc.Pools = aggregateNamespacePools(nsPools)
			rc.TotalLimit, rc.TotalUsed, rc.TotalAvail = poolTotals(rc.Pools)
		}
	}
	return rc, nil
}

// aggregateNamespacePools sums per-(namespace, type) pools into a per-type
// cluster aggregate — the total finite budget that the per-namespace caps
// partition. Unlimited (negative-Limit sentinel) pools are skipped: they impose
// no finite cap, so a type that is only ever unlimited across namespaces is
// absent from the aggregate (no cluster cap), consistent with GetResourcePools.
func aggregateNamespacePools(nsPools map[string]map[string]ResourcePool) map[string]ResourcePool {
	agg := make(map[string]ResourcePool)
	for _, perType := range nsPools {
		for accType, pool := range perType {
			if pool.Limit < 0 {
				continue
			}
			a := agg[accType]
			a.Limit += pool.Limit
			a.Used += pool.Used
			agg[accType] = a
		}
	}
	return agg
}

// poolTotals returns the summed Limit, Used, and available (clamped at 0)
// across the given pools.
func poolTotals(pools map[string]ResourcePool) (limit, used, avail int) {
	for _, p := range pools {
		limit += p.Limit
		used += p.Used
	}
	avail = limit - used
	if avail < 0 {
		avail = 0
	}
	return limit, used, avail
}

// Ensure DefaultLimiter implements Limiter interface
var _ Limiter = (*DefaultLimiter)(nil)

// Ensure DefaultLimiter implements ConstraintProvider interface
var _ ConstraintProvider = (*DefaultLimiter)(nil)
