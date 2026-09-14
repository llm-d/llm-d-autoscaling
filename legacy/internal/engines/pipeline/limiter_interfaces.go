// Limiter interfaces for resource limiting algorithms.
//
// # Design Rationale
//
// The limiter interfaces separate two orthogonal concerns:
//
//  1. Inventory (Granularity): How resources are tracked and what constraints apply.
//     Examples: cluster-wide pool, per-accelerator-type limits, node-level with scheduling.
//
//  2. AllocationAlgorithm (Strategy): How resources are distributed across decisions.
//     Examples: greedy by saturation, round-robin, priority-based, weighted fair share.
//
// This separation enables:
//   - Algorithms work with ANY inventory - they don't care if resources are tracked
//     at cluster, type, or node level
//   - Inventories work with ANY algorithm - the distribution strategy is independent
//   - Easy testing: algorithms can be tested with mock allocators
//   - Runtime flexibility: swap algorithms via configuration without code changes
//
// # Interface Hierarchy
//
//	Limiter (public API)
//	   │
//	   ├── Inventory (resource granularity)
//	   │      └── creates ResourceAllocator
//	   │
//	   └── AllocationAlgorithm (distribution strategy)
//	          └── uses ResourceAllocator
//
// Users typically only interact with Limiter. The internal interfaces (Inventory,
// AllocationAlgorithm, ResourceAllocator) are exposed for extensibility - custom
// implementations can be plugged in without modifying the package.
//
// # Pipeline Model
//
// The limiter operates on VariantDecision as shared state. Following the pipeline
// pattern, each stage reads and modifies the decision:
//   - Input: VariantDecision with TargetReplicas set by previous stage (e.g., saturation analyzer)
//   - Processing: Limiter may constrain TargetReplicas based on available resources
//   - Output: Same VariantDecision with updated TargetReplicas, GPUsAllocated, WasLimited, etc.
//
// Each stage adds its contribution to DecisionSteps for observability.
//
// The Inventory interface here is separate from collector's inventory because:
//   - Collector inventory: knows how to collect metrics, track staleness, emit events
//   - Limiter inventory: knows resource availability for allocation decisions
//   - Different responsibilities, different lifecycles
package pipeline

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Limiter constrains scaling decisions based on resource availability.
//
// This is the primary interface users interact with. It combines an Inventory
// (resource granularity) with an AllocationAlgorithm (distribution strategy)
// to constrain scaling decisions.
//
// The Limiter modifies VariantDecision in place, following the pipeline pattern
// where each stage reads and writes to shared state:
//   - Reads: TargetReplicas, GPUsPerReplica, AcceleratorName, SpareCapacity
//   - Writes: TargetReplicas (may be reduced), GPUsAllocated, WasLimited, LimitedBy
//   - Appends: DecisionSteps (adds limiting step)
//
// Example usage:
//
//	limiter := NewLimiter("prod", nodeInventory, greedyAlgorithm)
//	err := limiter.Limit(ctx, decisions) // modifies decisions in place
type Limiter interface {
	// Name returns limiter identifier for logging/metrics.
	Name() string

	// Limit applies resource constraints to scaling decisions.
	// Modifies decisions in place - may reduce TargetReplicas based on available resources.
	// Sets GPUsAllocated, WasLimited, LimitedBy fields and appends to DecisionSteps.
	Limit(ctx context.Context, decisions []*domain.VariantDecision) error
}

// AllocationAlgorithm defines how to distribute limited resources across decisions.
//
// Algorithms are independent of resource granularity - they work with any Inventory
// through the ResourceAllocator abstraction. This enables mixing any algorithm
// with any inventory type.
//
// Built-in algorithms include:
//   - GreedyBySaturation: allocates to most saturated (lowest spare capacity) first
//   - RoundRobin: distributes evenly, one replica at a time
//   - PriorityBased: allocates to highest priority first
//   - WeightedFairShare: allocates proportionally based on weights
//   - BinPacking: consolidates on fewer nodes (requires NodeAwareAllocator)
type AllocationAlgorithm interface {
	// Name returns algorithm identifier for logging/metrics.
	Name() string

	// Allocate distributes available resources across decisions.
	// Modifies decisions in place - may reduce TargetReplicas and sets GPUsAllocated.
	//
	// The allocator parameter abstracts resource reservation - the algorithm
	// doesn't need to know if resources are cluster-wide, per-type, or per-node.
	//
	// Algorithms read from decisions:
	//   - TargetReplicas, CurrentReplicas: to determine GPUs needed
	//   - GPUsPerReplica: to calculate total GPU requirement
	//   - AcceleratorName: passed to allocator for type-aware allocation
	//   - SpareCapacity: for ordering (GreedyBySaturation)
	//
	// Algorithms write to decisions:
	//   - TargetReplicas: may be reduced if allocation is partial
	//   - GPUsAllocated: number of GPUs actually allocated
	Allocate(
		ctx context.Context,
		decisions []*domain.VariantDecision,
		allocator ResourceAllocator,
	) error
}

// ResourceAllocator abstracts resource reservation at different granularities.
//
// Created by Inventory to handle granularity-specific allocation logic.
// Algorithms use this interface without knowing the underlying inventory type.
//
// For example:
//   - ClusterInventory creates an allocator that tracks total GPUs
//   - TypeInventory creates an allocator that tracks GPUs per accelerator type
//   - NodeInventory creates an allocator that tracks GPUs per node with scheduling checks
type ResourceAllocator interface {
	// TryAllocate attempts to allocate GPUs for a decision.
	// Returns actual GPUs allocated (may be less than requested if constrained).
	//
	// The decision parameter provides context (AcceleratorName, GPUsPerReplica,
	// ScaleTargetRef) that some allocators need for type-aware or node-aware allocation.
	// ctx carries the request-scoped logger so per-cycle traces emitted from the
	// allocator are correlated with the rest of the engine cycle's structured logs.
	TryAllocate(ctx context.Context, decision *domain.VariantDecision, gpusRequested int) (gpusAllocated int, err error)

	// Remaining returns total remaining allocatable GPUs across all resources.
	Remaining() int
}

// ResourcePool represents available resources for one accelerator type.
type ResourcePool struct {
	Limit int // total capacity (from cluster discovery)
	Used  int // currently in use
}

// Available returns the number of allocatable resources (Limit - Used),
// clamped to a minimum of 0.
//
// Available is only meaningful for a FINITE pool (Limit >= 0). A pool carrying
// the unlimited sentinel (Limit < 0, emitted per-(namespace, type) by
// NamespaceResourcePools) would clamp to 0 here, which reads as "deny" —
// the opposite of "unlimited". Callers that may see the sentinel must branch on
// Limit < 0 first (see nsPoolBudget and mergeConstraints); do not call
// Available on such a pool.
func (p ResourcePool) Available() int {
	avail := p.Limit - p.Used
	if avail < 0 {
		return 0
	}
	return avail
}

// ResourceConstraints represents hard resource constraints from a single provider.
// Multiple providers can each contribute constraints; the optimizer respects all of them.
type ResourceConstraints struct {
	ProviderName string                  // e.g., "gpu-limiter", "quota-limiter"
	Pools        map[string]ResourcePool // accelerator type → pool
	// NamespacePools carries per-(namespace, accelerator type) caps for
	// namespace-scoped providers (e.g. a namespace-scoped quota limiter).
	// Outer key: namespace; inner key: accelerator type. Nil for providers
	// that only impose cluster/per-type constraints. The optimizer caps a
	// model's allocation at min(per-type pool, this namespace's pool).
	NamespacePools map[string]map[string]ResourcePool
	TotalLimit     int
	TotalUsed      int
	TotalAvail     int
}

// ConstraintProvider exposes hard constraints for the optimizer.
// Implementations discover resource availability without making allocation decisions.
//
// This separates constraint discovery from decision-making:
//   - ConstraintProvider: "Here are the limits" (hard facts)
//   - ScalingOptimizer: "Here's what to do given those limits" (decision logic)
type ConstraintProvider interface {
	// Name returns provider identifier, used as ProviderName in ResourceConstraints.
	Name() string

	// ComputeConstraints refreshes resource data and returns hard constraints.
	// usageByType maps accelerator type → GPUs currently in use (cluster-wide).
	// usageByNamespace maps namespace → accelerator type → GPUs in use, and its
	// keys also define the set of active namespaces for which namespace-scoped
	// caps are materialized (so a namespace with a quota but zero current usage
	// is still constrained). It may be nil for providers that ignore namespaces.
	ComputeConstraints(ctx context.Context, usageByType map[string]int, usageByNamespace map[string]map[string]int) (*ResourceConstraints, error)
}

// Inventory provides resource availability and creates allocators.
//
// Implementations define the granularity of resource tracking:
//   - ClusterInventory: single pool of all GPUs
//   - TypeInventory: separate pools per accelerator type (H100, A100, etc.)
//   - NodeInventory: per-node tracking with scheduling constraint awareness
//
// The Inventory tracks three values per resource pool:
//   - Limit: total capacity (discovered from cluster)
//   - Used: currently allocated/in-use GPUs
//   - Available: Limit - Used (what can still be allocated)
//
// The Inventory is responsible for:
//   - Refreshing capacity data (limits) from the cluster
//   - Tracking current usage
//   - Creating ResourceAllocator instances that handle granularity-specific logic
//
// Note: This is separate from collector.Inventory which handles metrics collection.
// The limiter Inventory focuses solely on resource availability for allocation.
type Inventory interface {
	// Name returns inventory identifier for logging/metrics.
	Name() string

	// Refresh updates inventory limits from the cluster.
	// Should be called before CreateAllocator to ensure fresh data.
	Refresh(ctx context.Context) error

	// SetUsed updates the used GPU counts.
	// This should be called with current usage before creating an allocator.
	// The usedByType map contains accelerator type -> used GPU count.
	SetUsed(usedByType map[string]int)

	// CreateAllocator returns a ResourceAllocator for this inventory.
	// The allocator encapsulates granularity-specific allocation logic.
	// Available GPUs = Limit - Used for each resource pool.
	CreateAllocator(ctx context.Context) ResourceAllocator

	// TotalLimit returns total GPU capacity across all resources.
	TotalLimit() int

	// TotalUsed returns total GPUs currently in use across all resources.
	TotalUsed() int

	// TotalAvailable returns total available GPUs (Limit - Used).
	TotalAvailable() int

	// GetResourcePools returns per-type resource availability.
	// Each key is an accelerator type (e.g., "A100", "H100").
	GetResourcePools() map[string]ResourcePool
}

// NamespaceAwareInventory is an Inventory whose constraints depend on the
// namespace of the decision, not just the accelerator type. Implementations
// bucket usage by namespace in addition to type; the chain orchestrator is
// expected to feed them the per-namespace usage map alongside the per-type
// map.
//
// Use feature detection at the chain level:
//
//	if nai, ok := inv.(NamespaceAwareInventory); ok {
//	    nai.SetUsedByNamespace(usedByNS)
//	}
//	inv.SetUsed(usedByType) // always safe; namespace-only inventories ignore it
//
// Implementations should remain valid Inventory values: SetUsed must not
// panic when called on a namespace-scoped inventory (it may simply be a
// no-op).
type NamespaceAwareInventory interface {
	Inventory

	// SetUsedByNamespace updates the per-namespace usage map.
	// Outer key: namespace; inner key: accelerator type.
	// Should be called before CreateAllocator, alongside SetUsed.
	SetUsedByNamespace(usedByNS map[string]map[string]int)

	// NamespaceResourcePools returns per-(namespace, accelerator type) pools
	// for the given active namespaces as a CLOSED allowlist, resolving each
	// namespace's caps via the inventory's lookup rules (e.g. explicit entry →
	// default fall-through → exclude). Outer key: namespace; inner key:
	// accelerator type. A namespace that is unconstrained (excluded) is omitted,
	// signalling "open" — bound only by the cluster/per-type constraint. Every
	// other active namespace is present (a closed allowlist; an empty inner map
	// is a deny-all). A listed type with an unlimited cap is emitted as a
	// sentinel pool with Limit < 0 (so it stays distinct from a type the
	// namespace does not list, which the optimizer must deny). SetUsedByNamespace
	// should be called first so the returned pools carry current usage.
	NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool
}
