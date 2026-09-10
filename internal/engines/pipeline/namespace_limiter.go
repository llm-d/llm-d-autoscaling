package pipeline

import (
	"context"
	"fmt"
	"sort"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// NamespaceLimiterName is the stable limiter identifier recorded on limited
// decisions (LimitedBy and the wva_decisions_limited_total metric label). The
// per-namespace and per-type detail is surfaced in the DecisionStep reason
// instead, to keep the metric label cardinality bounded.
const NamespaceLimiterName = "namespace-inventory"

// NamespaceLimiter constrains scaling decisions against per-namespace GPU
// pools held by a NamespaceInventory. It mirrors DefaultLimiter but performs
// usage accounting per (namespace bucket, accelerator type) rather than per
// type cluster-wide, because the Inventory.SetUsed contract cannot carry the
// namespace dimension.
//
// It serves both scaling paths: Limit caps decisions in place on the V1
// saturation path, and ComputeConstraints exposes the same per-namespace pools
// to the V2 optimizer via ResourceConstraints.NamespacePools.
type NamespaceLimiter struct {
	name           string
	inventory      *NamespaceInventory
	algorithm      AllocationAlgorithm
	metricsEmitter *metrics.MetricsEmitter
}

// NewNamespaceLimiter creates a limiter backed by a NamespaceInventory and an
// allocation algorithm.
func NewNamespaceLimiter(inventory *NamespaceInventory, algorithm AllocationAlgorithm) *NamespaceLimiter {
	return &NamespaceLimiter{
		name:           NamespaceLimiterName,
		inventory:      inventory,
		algorithm:      algorithm,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// Name returns the limiter identifier.
func (l *NamespaceLimiter) Name() string {
	return l.name
}

// Limit applies per-namespace GPU constraints to scaling decisions in place.
func (l *NamespaceLimiter) Limit(ctx context.Context, decisions []*domain.VariantDecision) error {
	if len(decisions) == 0 {
		return nil
	}

	if err := l.inventory.Refresh(ctx); err != nil {
		return fmt.Errorf("failed to refresh namespace inventory: %w", err)
	}

	// Resolve empty/unknown accelerator names against the cluster-aggregate
	// pools so a homogeneous cluster's single type is filled in before usage
	// accounting and allocation. Mirrors DefaultLimiter.resolveUnknownAccelerators.
	l.resolveUnknownAccelerators(decisions)

	l.inventory.SetUsedByBucket(l.calculateUsedByBucket(decisions))

	allocator := l.inventory.CreateAllocator(ctx)
	if err := l.algorithm.Allocate(ctx, decisions, allocator); err != nil {
		return fmt.Errorf("allocation algorithm failed: %w", err)
	}

	l.updateDecisionMetadata(decisions)
	return nil
}

// resolveUnknownAccelerators fills in unresolved accelerator names against the
// decision's own namespace pool: when that bucket holds exactly one accelerator
// type, the name resolves to it. Resolving per bucket (rather than against the
// cluster-wide aggregate) is required so existing replicas in a single-type
// namespace still debit that pool even when the cluster as a whole is
// heterogeneous; otherwise their usage would go uncounted and a sibling
// decision could over-allocate.
//
// The bucket comes from chargeBucket, not resolve, so excluded namespaces are
// resolved too. Resolution only needs the pool a namespace draws from, and an
// excluded namespace still occupies GPUs there: skipping it would leave the
// accelerator unresolved, calculateUsedByBucket would drop the decision, and
// chargeBucket would never charge it — letting a shared pool hand out GPUs that
// are physically occupied. Resolving does not cap the namespace, since the
// allocator still passes excluded namespaces through.
func (l *NamespaceLimiter) resolveUnknownAccelerators(decisions []*domain.VariantDecision) {
	for _, d := range decisions {
		if constants.IsAcceleratorResolved(d.AcceleratorName) {
			continue
		}
		bucket, ok := l.inventory.resolver.chargeBucket(d.Namespace)
		if !ok {
			continue
		}
		if types := l.inventory.bucketAcceleratorTypes(bucket); len(types) == 1 {
			d.AcceleratorName = types[0]
		}
	}
}

// calculateUsedByBucket sums current GPU usage (CurrentReplicas * GPUsPerReplica)
// per (namespace bucket, accelerator type). Usage is charged to the bucket the
// namespace draws from via chargeBucket, which ignores exclusion: excluded
// namespaces bypass the cap but their running replicas still occupy physical
// GPUs in their pool, so charging them keeps shared pools from overcommitting
// (matching the cluster-wide path, which charges all usage). The bucket keys
// match those the allocator resolves to, so default-fallback namespaces'
// usage aggregates under the shared default bucket rather than their own names.
func (l *NamespaceLimiter) calculateUsedByBucket(decisions []*domain.VariantDecision) map[string]map[string]int {
	used := make(map[string]map[string]int)
	for _, d := range decisions {
		// Skip unresolved accelerators (empty or the "unknown" sentinel): their
		// usage cannot be attributed to a real type pool. resolveUnknownAccelerators
		// resolves single-type buckets; in a multi-type bucket an unresolved
		// replica stays unattributable, the same limitation the cluster-wide
		// limiter has. Booking it under a synthetic key would not debit any real
		// pool anyway, so skip it rather than create a phantom entry.
		if !constants.IsAcceleratorResolved(d.AcceleratorName) {
			continue
		}
		bucket, ok := l.inventory.resolver.chargeBucket(d.Namespace)
		if !ok {
			continue
		}
		if used[bucket] == nil {
			used[bucket] = make(map[string]int)
		}
		used[bucket][d.AcceleratorName] += d.CurrentReplicas * d.GPUsPerReplica
	}
	return used
}

// updateDecisionMetadata sets LimitedBy and records the limiting metric and a
// DecisionStep for each limited decision.
func (l *NamespaceLimiter) updateDecisionMetadata(decisions []*domain.VariantDecision) {
	for _, d := range decisions {
		if d.WasLimited {
			d.LimitedBy = l.name
			l.metricsEmitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)
		}
		d.AddDecisionStep(l.name, l.buildStepReason(d), d.WasLimited)
	}
}

// buildStepReason describes the limiting outcome, including the namespace
// bucket and accelerator type for limited scale-ups so operators can see which
// pool was exhausted.
func (l *NamespaceLimiter) buildStepReason(d *domain.VariantDecision) string {
	replicaChange := d.TargetReplicas - d.CurrentReplicas
	if replicaChange <= 0 {
		return fmt.Sprintf("no scale-up (target=%d, current=%d)", d.TargetReplicas, d.CurrentReplicas)
	}
	if d.WasLimited {
		bucket, excluded, hasPool := l.inventory.resolver.resolve(d.Namespace)
		switch {
		case excluded:
			// Excluded namespaces are unconstrained, so they are never limited
			// here; fall through to the generic message defensively.
		case !hasPool:
			return fmt.Sprintf("limited by %s[ns=%s, type=%s]: no inventory (namespace has no node selector and no default)",
				l.name, d.Namespace, d.AcceleratorName)
		default:
			return fmt.Sprintf("limited by %s[ns=%s, type=%s]: allocated %d GPUs for +%d replicas",
				l.name, bucket, d.AcceleratorName, d.GPUsAllocated, replicaChange)
		}
	}
	return fmt.Sprintf("allocated %d GPUs for +%d replicas", d.GPUsAllocated, replicaChange)
}

// ComputeConstraints implements ConstraintProvider, the V2 counterpart of
// Limit: instead of capping decisions in place it exposes the per-namespace GPU
// pools so the optimizer can partition capacity per tenant. usageByType is
// unused because per-namespace usage carries the same totals with the namespace
// dimension intact.
//
// The cluster per-type aggregate in Pools is derived from the ACTIVE namespace
// pools, never from the static all-buckets GetResourcePools sum, so the budget
// the optimizer partitions matches the per-namespace budgets exactly. Reporting
// the static sum would advertise capacity in buckets no active namespace can
// draw from, and would feed an over-broad cluster cap into any later
// intersection with another provider.
//
// The one exception is when every active namespace is excluded: this limiter
// then constrains nothing, and the binding limit really is the physical cluster
// capacity, so the full aggregate is reported deliberately.
func (l *NamespaceLimiter) ComputeConstraints(ctx context.Context, _ map[string]int, usageByNamespace map[string]map[string]int) (*ResourceConstraints, error) {
	if err := l.inventory.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("failed to refresh namespace inventory: %w", err)
	}
	l.inventory.SetUsedByNamespace(usageByNamespace)

	// The keys of usageByNamespace define the active set, so a namespace with a
	// pool but no current usage is still materialized and constrained.
	active := make([]string, 0, len(usageByNamespace))
	for ns := range usageByNamespace {
		active = append(active, ns)
	}
	sort.Strings(active)

	rc := &ResourceConstraints{ProviderName: l.name}
	nsPools := l.inventory.NamespaceResourcePools(active)
	if len(nsPools) == 0 {
		rc.Pools = l.inventory.GetResourcePools()
	} else {
		rc.NamespacePools = nsPools
		rc.Pools = aggregateNamespacePools(nsPools)
	}
	rc.TotalLimit, rc.TotalUsed, rc.TotalAvail = poolTotals(rc.Pools)
	return rc, nil
}

// Ensure NamespaceLimiter implements Limiter and, on the V2 path,
// ConstraintProvider.
var (
	_ Limiter            = (*NamespaceLimiter)(nil)
	_ ConstraintProvider = (*NamespaceLimiter)(nil)
)
