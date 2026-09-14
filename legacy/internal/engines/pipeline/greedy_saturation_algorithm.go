package pipeline

import (
	"context"
	"sort"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// GreedyBySaturation allocates resources to the most saturated variants first.
//
// Algorithm:
//  1. Filter decisions that need scale-up (TargetReplicas > CurrentReplicas)
//  2. Sort by SpareCapacity ascending (most saturated = lowest spare capacity first)
//  3. For each decision, try to allocate GPUs for the requested replicas
//  4. If partial allocation, adjust TargetReplicas accordingly
//
// This prioritizes models under the most pressure, ensuring they get resources
// before less constrained models.
type GreedyBySaturation struct{}

// NewGreedyBySaturation creates a new greedy-by-saturation algorithm.
func NewGreedyBySaturation() *GreedyBySaturation {
	return &GreedyBySaturation{}
}

// Name returns the algorithm identifier.
func (g *GreedyBySaturation) Name() string {
	return "greedy-by-saturation"
}

// Allocate distributes available resources across decisions, prioritizing
// the most saturated variants (lowest spare capacity).
func (g *GreedyBySaturation) Allocate(
	ctx context.Context,
	decisions []*domain.VariantDecision,
	allocator ResourceAllocator,
) error {
	// Filter and sort decisions that need scale-up
	candidates := g.filterScaleUpCandidates(decisions)
	g.sortByPriority(candidates)

	// Allocate GPUs to each candidate in priority order
	for _, d := range candidates {
		g.allocateForDecision(ctx, d, allocator)
	}

	return nil
}

// filterScaleUpCandidates returns decisions that want to scale up.
func (g *GreedyBySaturation) filterScaleUpCandidates(decisions []*domain.VariantDecision) []*domain.VariantDecision {
	var candidates []*domain.VariantDecision
	for _, d := range decisions {
		if d.TargetReplicas > d.CurrentReplicas {
			candidates = append(candidates, d)
		}
	}
	return candidates
}

// sortByPriority sorts decisions by:
//  1. SpareCapacity ascending (most saturated first)
//  2. Cost ascending (cheaper variants as tie-breaker)
func (g *GreedyBySaturation) sortByPriority(decisions []*domain.VariantDecision) {
	sort.Slice(decisions, func(i, j int) bool {
		// Primary: lowest spare capacity first (most saturated)
		if decisions[i].SpareCapacity != decisions[j].SpareCapacity {
			return decisions[i].SpareCapacity < decisions[j].SpareCapacity
		}
		// Secondary: lowest cost first (tie-breaker)
		return decisions[i].Cost < decisions[j].Cost
	})
}

// allocateForDecision attempts to allocate GPUs for a single decision.
// If partial allocation, adjusts TargetReplicas accordingly.
// Respects MaxReplicas (caps scale-up) and MinReplicas (floor even under GPU scarcity).
func (g *GreedyBySaturation) allocateForDecision(ctx context.Context, d *domain.VariantDecision, allocator ResourceAllocator) {
	replicasNeeded := d.TargetReplicas - d.CurrentReplicas
	if replicasNeeded <= 0 {
		return
	}

	// Cap by maxReplicas if set
	if d.MaxReplicas != nil && *d.MaxReplicas > 0 && d.TargetReplicas > *d.MaxReplicas {
		d.TargetReplicas = *d.MaxReplicas
		replicasNeeded = d.TargetReplicas - d.CurrentReplicas
		if replicasNeeded <= 0 {
			return
		}
	}

	gpusPerReplica := d.GPUsPerReplica
	if gpusPerReplica <= 0 {
		gpusPerReplica = 1 // Default to 1 GPU per replica if not specified
	}

	gpusRequested := replicasNeeded * gpusPerReplica
	gpusAllocated, _ := allocator.TryAllocate(ctx, d, gpusRequested)

	// Calculate how many replicas we can actually add
	replicasAllocated := 0
	if gpusPerReplica > 0 {
		replicasAllocated = gpusAllocated / gpusPerReplica
	}

	// Update decision with actual allocation
	d.GPUsAllocated = replicasAllocated * gpusPerReplica // Only count full replicas
	d.TargetReplicas = d.CurrentReplicas + replicasAllocated

	// MinReplicas is a hard floor — even if GPU availability is insufficient,
	// set TargetReplicas to minReplicas (deployment may be unschedulable, but user intent is preserved).
	if d.MinReplicas != nil && d.TargetReplicas < *d.MinReplicas {
		d.TargetReplicas = *d.MinReplicas
	}

	// Mark as limited if we couldn't allocate all requested
	if replicasAllocated < replicasNeeded {
		d.WasLimited = true
	}
}

// Ensure GreedyBySaturation implements AllocationAlgorithm interface
var _ AllocationAlgorithm = (*GreedyBySaturation)(nil)
