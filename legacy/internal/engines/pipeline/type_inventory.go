package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// TypeInventory tracks GPU capacity, usage, and availability per accelerator type (H100, A100, etc.).
//
// Unlike ClusterInventory which maintains a single pool of all GPUs, TypeInventory
// maintains separate pools for each accelerator type. This ensures that:
//   - H100 workloads can only use H100 GPUs
//   - A100 workloads can only use A100 GPUs
//   - Different accelerator types don't compete for the same pool
//
// The inventory tracks three values per accelerator type:
//   - Limit: total capacity discovered from the cluster
//   - Used: GPUs currently in use (discovered from pods or set manually)
//   - Available: Limit - Used (computed)
//
// This is essential for heterogeneous clusters where workloads have specific
// hardware requirements that cannot be satisfied by other accelerator types.
type TypeInventory struct {
	name           string
	discovery      discovery.CapacityDiscovery
	usageDiscovery discovery.UsageDiscovery // Optional: if set, RefreshAll will auto-discover usage

	mu sync.RWMutex
	// limitByType maps accelerator type (e.g., "H100", "A100") to total GPU capacity
	limitByType map[string]int
	// usedByType maps accelerator type to currently used GPU count
	usedByType map[string]int
	// totalLimit is the sum of all GPU capacity across types
	totalLimit int
	// totalUsed is the sum of all used GPUs across types
	totalUsed int
}

// NewTypeInventory creates a TypeInventory that tracks GPUs per accelerator type.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface to discover accelerator capacity from the cluster
//
// For automatic usage discovery, use NewTypeInventoryWithUsage instead.
func NewTypeInventory(name string, disc discovery.CapacityDiscovery) *TypeInventory {
	return &TypeInventory{
		name:        name,
		discovery:   disc,
		limitByType: make(map[string]int),
		usedByType:  make(map[string]int),
	}
}

// NewTypeInventoryWithUsage creates a TypeInventory with automatic usage discovery.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface implementing both CapacityDiscovery and UsageDiscovery
//
// When using this constructor, call RefreshAll() to update both limits and usage
// in a single operation.
func NewTypeInventoryWithUsage(name string, disc discovery.FullDiscovery) *TypeInventory {
	return &TypeInventory{
		name:           name,
		discovery:      disc,
		usageDiscovery: disc,
		limitByType:    make(map[string]int),
		usedByType:     make(map[string]int),
	}
}

// Name returns the inventory identifier.
func (i *TypeInventory) Name() string {
	return i.name
}

// RefreshAll updates both limits (capacity) and usage in a single operation.
//
// This is the preferred method when using NewTypeInventoryWithUsage.
// It discovers GPU capacity from nodes and calculates current usage from pods.
//
// Returns an error if usage discovery is not configured (use Refresh + SetUsed instead).
func (i *TypeInventory) RefreshAll(ctx context.Context) error {
	if i.usageDiscovery == nil {
		return errors.New("usage discovery not configured; use SetUsed() or NewTypeInventoryWithUsage()")
	}

	// Refresh limits first
	if err := i.Refresh(ctx); err != nil {
		return err
	}

	// Discover current usage
	usedByType, err := i.usageDiscovery.DiscoverUsage(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU usage: %w", err)
	}

	// Update usage
	i.SetUsed(usedByType)

	return nil
}

// Refresh updates the inventory limits from the cluster using the discovery interface.
//
// This aggregates GPU capacity across all nodes for each accelerator type.
// Accelerator names are normalized from full model names (e.g., "NVIDIA-A100-PCIE-80GB")
// to short names (e.g., "A100") to match VA label conventions.
// Should be called before CreateAllocator to ensure fresh data.
// Note: This only updates limits; call SetUsed or RefreshAll to update usage.
func (i *TypeInventory) Refresh(ctx context.Context) error {
	// Discover node -> accelerator type -> count
	nodeInventory, err := i.discovery.Discover(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover accelerator capacity: %w", err)
	}

	// Aggregate by accelerator type across all nodes
	// Normalize full model names to short names for matching with VA labels
	byType := make(map[string]int)
	total := 0

	for _, accelerators := range nodeInventory {
		for fullModelName, info := range accelerators {
			// Normalize "NVIDIA-A100-PCIE-80GB" -> "A100"
			shortName := accelerator.NormalizeAcceleratorName(fullModelName)
			byType[shortName] += info.Count
			total += info.Count
		}
	}

	i.mu.Lock()
	i.limitByType = byType
	i.totalLimit = total
	i.mu.Unlock()

	return nil
}

// SetUsed updates the used GPU counts per accelerator type.
// This should be called with current usage (e.g., from replica counts) before creating an allocator.
func (i *TypeInventory) SetUsed(usedByType map[string]int) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Copy the map to avoid external mutations
	i.usedByType = make(map[string]int, len(usedByType))
	total := 0
	for accType, count := range usedByType {
		i.usedByType[accType] = count
		total += count
	}
	i.totalUsed = total
}

// CreateAllocator returns a ResourceAllocator that allocates from type-specific pools.
//
// The returned allocator ensures that allocations for a given accelerator type
// only consume GPUs from that type's pool.
// Available GPUs = Limit - Used for each accelerator type.
func (i *TypeInventory) CreateAllocator(ctx context.Context) ResourceAllocator {
	i.mu.RLock()
	defer i.mu.RUnlock()

	// Compute available = limit - used for each type
	remaining := make(map[string]int, len(i.limitByType))
	total := 0
	for accType, limit := range i.limitByType {
		used := i.usedByType[accType]
		available := limit - used
		if available < 0 {
			available = 0 // Don't go negative if over-allocated
		}
		remaining[accType] = available
		total += available
	}

	return &typeAllocator{
		remainingByType: remaining,
		totalRemaining:  total,
	}
}

// TotalLimit returns total GPU capacity across all types.
func (i *TypeInventory) TotalLimit() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalLimit
}

// TotalUsed returns total GPUs currently in use across all types.
func (i *TypeInventory) TotalUsed() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalUsed
}

// TotalAvailable returns total available GPUs (Limit - Used) across all types.
func (i *TypeInventory) TotalAvailable() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.totalLimit - i.totalUsed
	if available < 0 {
		return 0
	}
	return available
}

// LimitByType returns the GPU capacity limit for a specific accelerator type.
func (i *TypeInventory) LimitByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitByType[accType]
}

// UsedByType returns the used GPU count for a specific accelerator type.
func (i *TypeInventory) UsedByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.usedByType[accType]
}

// AvailableByType returns available GPUs (Limit - Used) for a specific accelerator type.
func (i *TypeInventory) AvailableByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.limitByType[accType] - i.usedByType[accType]
	if available < 0 {
		return 0
	}
	return available
}

// GetResourcePools returns per-type resource availability as ResourcePool structs.
func (i *TypeInventory) GetResourcePools() map[string]ResourcePool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	pools := make(map[string]ResourcePool, len(i.limitByType))
	for accType, limit := range i.limitByType {
		used := i.usedByType[accType]
		pools[accType] = ResourcePool{
			Limit: limit,
			Used:  used,
		}
	}
	return pools
}

// AcceleratorTypes returns all known accelerator types.
func (i *TypeInventory) AcceleratorTypes() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	types := make([]string, 0, len(i.limitByType))
	for t := range i.limitByType {
		types = append(types, t)
	}
	return types
}

// typeAllocator implements ResourceAllocator with per-type tracking.
//
// This allocator is NOT thread-safe and must not be shared across goroutines.
// Create a new allocator per scaling decision batch using TypeInventory.CreateAllocator().
//
// This allocator ensures that:
// - Each accelerator type has its own independent pool
// - Allocations are tracked per-type
// - Cross-type allocation is prevented
type typeAllocator struct {
	remainingByType map[string]int
	totalRemaining  int
}

// TryAllocate attempts to allocate GPUs from the type-specific pool.
//
// The accelerator type is determined from the decision's AcceleratorName field.
// Returns the actual GPUs allocated (may be less than requested if the type's
// pool is exhausted).
func (a *typeAllocator) TryAllocate(ctx context.Context, decision *domain.VariantDecision, gpusRequested int) (int, error) {
	if gpusRequested <= 0 {
		return 0, nil
	}

	accType := decision.AcceleratorName
	if !constants.IsAcceleratorResolved(accType) {
		// Normally resolved by DefaultLimiter.resolveUnknownAccelerators before
		// reaching here. This is a safety net for direct callers.
		if len(a.remainingByType) == 1 {
			for resolvedType := range a.remainingByType {
				decision.AcceleratorName = resolvedType
				accType = resolvedType
			}
		} else {
			// Heterogeneous cluster — can't determine which pool to debit.
			// Note: #995 proposed "treat as unlimited (return all requested GPUs)"
			// but that allows the same physical GPUs to be double-allocated: once
			// here for the unresolved decision, and again from a typed pool for a
			// subsequent known-type decision. Returning 0 is safer — the variant
			// keeps its current replicas without over-allocating. Operator must
			// set nodeSelector or the VA label for scaling in mixed-GPU clusters.
			ctrl.LoggerFrom(ctx).WithName("typeAllocator").V(logging.DEBUG).Info(
				"Skipping allocation: accelerator unresolved in heterogeneous cluster — operator must set nodeSelector or VA label",
				"variant", decision.VariantName,
				"namespace", decision.Namespace,
				"availableTypes", len(a.remainingByType),
				"gpusRequested", gpusRequested)
			return 0, nil
		}
	}

	available := a.remainingByType[accType]
	if available <= 0 {
		return 0, nil // No GPUs available for this type
	}

	// Allocate up to what's available
	allocated := gpusRequested
	if allocated > available {
		allocated = available
	}

	a.remainingByType[accType] -= allocated
	a.totalRemaining -= allocated

	return allocated, nil
}

// Remaining returns total remaining GPUs across all types.
func (a *typeAllocator) Remaining() int {
	return a.totalRemaining
}

// RemainingForType returns remaining GPUs for a specific accelerator type.
func (a *typeAllocator) RemainingForType(accType string) int {
	return a.remainingByType[accType]
}

// Ensure TypeInventory implements Inventory interface
var _ Inventory = (*TypeInventory)(nil)

// Ensure typeAllocator implements ResourceAllocator interface
var _ ResourceAllocator = (*typeAllocator)(nil)
