package pipeline

import (
	"context"
	"fmt"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CostAwareOptimizer is a per-model optimizer that minimizes total cost while
// meeting capacity requirements. It processes each model independently:
//
//   - Scale-up: adds replicas to the most cost-efficient variant (lowest cost / perReplicaCapacity)
//   - Scale-down: removes replicas from the most expensive variant (highest absolute cost)
//   - Only the cheapest variant is protected at >=1 replica; others can scale to 0
//   - Variants with pending replicas are skipped for scale-up
//
// This optimizer ignores ResourceConstraints (unlimited mode). For GPU-limited
// environments, use GreedyByScoreOptimizer instead.
type CostAwareOptimizer struct{}

// NewCostAwareOptimizer creates a new CostAwareOptimizer.
func NewCostAwareOptimizer() *CostAwareOptimizer {
	return &CostAwareOptimizer{}
}

// Name returns the optimizer identifier.
func (o *CostAwareOptimizer) Name() string {
	return "cost-aware"
}

// Optimize produces VariantDecisions for all models.
// Constraints are ignored in unlimited mode (CostAwareOptimizer).
func (o *CostAwareOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	var allDecisions []domain.VariantDecision

	for _, req := range requests {
		anchor := bindingAnchor(req.AnalyzerResults)
		if anchor == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		targets := initTargets(req.VariantStates)

		// Unified dispatch: one path for all models via (model, role) math.
		// Non-disaggregated uses synthetic "both" role; disaggregated uses actual roles.
		// Combine (RC/SC) math consumes only the voting subset of the ballot.
		s := votingResults(req.AnalyzerResults)
		roles, ps := initRoleState(s)
		if anyRoleNeedsScaleUp(ps, roles) {
			allocateForModelPaired(ctx, s, anchor.VariantCapacities, stateMap, nil, targets,
				costGreedyRolePick, ps, roles)
		} else {
			scaleDownRoleIterated(ctx, s, anchor.VariantCapacities, targets, stateMap)
		}

		// Built after allocation so a multi-vote refresh (refreshAnchorSizing,
		// invoked per iteration inside allocateForModelPaired) is reflected in
		// the decisions below, not a pre-allocation snapshot.
		vcMap := buildCapacityMap(anchor.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "cost-aware")
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costGreedyRolePick is a RolePickFn that picks the cheapest-by-cost-efficiency
// variant in the given role. For "both" (non-disaggregated), all variants are
// eligible. For role-tagged roles, only variants with a matching Role are picked.
func costGreedyRolePick(
	role string,
	_ []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
	_ map[string]int,
	targets map[string]int,
) (string, int) {
	roleVCs := variantsForRole(variants, role)
	for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
		// Unpriced on the anchor's topology: with no per-replica capacity there
		// is no conversion between replicas and served demand, so this variant
		// cannot be sized or charged for. It is passed over as unpriceable, not
		// treated as a variant whose capacity happens to be zero -- nothing
		// claims it and nothing is spent on it.
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		state := stateMap[vc.VariantName]
		// Skip on an exhausted ceiling, never return a cap of 0: a returned 0
		// makes the caller compute deltaUtil == 0 and break out of the whole
		// model's allocation loop, so a bounded-out variant would take every
		// variant behind it down with it.
		if maxTarget, bounded := maxTargetReplicas(vc, state); bounded {
			headroom := maxTarget - targets[vc.VariantName]
			if headroom <= 0 {
				continue
			}
			return vc.VariantName, headroom
		}
		return vc.VariantName, math.MaxInt
	}
	return "", 0
}

// scaleDownVariantSet sheds replicas from sortedVariants (PRE-SORTED cost-desc,
// cheapest last). minReplicas floor and cheapest-at-1 protection are enforced
// here. maxRemovable returns how many replicas of vc the caller permits to remove;
// onRemove is invoked after committing n so the caller can update its spare bookkeeping.
func scaleDownVariantSet(
	ctx context.Context,
	sortedVariants []domain.VariantCapacity,
	targets map[string]int,
	states map[string]domain.VariantReplicaState,
	maxRemovable func(vc domain.VariantCapacity) int,
	onRemove func(vc domain.VariantCapacity, n int),
) {
	logger := ctrl.LoggerFrom(ctx)
	for i, vc := range sortedVariants {
		// Unpriced on this topology: removing a replica of a variant with no
		// per-replica capacity releases no accountable capacity, so there is
		// nothing to credit against the reduction. It is passed over as
		// unpriceable rather than counted as a removal that costs the model
		// nothing to make.
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		current := targets[vc.VariantName]
		minReplicas := 0
		if states != nil {
			if st, ok := states[vc.VariantName]; ok && st.MinReplicas != nil {
				minReplicas = *st.MinReplicas
			}
		}
		removable := current - minReplicas
		if removable <= 0 {
			continue
		}
		n := maxRemovable(vc)
		if n > removable {
			n = removable
		}
		// cheapest-at-1: the last (cheapest) variant is protected at 1 only when no
		// more-expensive variant still holds replicas (#1237's positional rule).
		if i == len(sortedVariants)-1 && current-n < 1 && !anyHasReplicas(sortedVariants[:i], targets) {
			n = current - 1
		}
		if n <= 0 {
			continue
		}
		targets[vc.VariantName] = current - n
		onRemove(vc, n)
		logger.V(logging.DEBUG).Info("scale-down: removed replicas",
			"variant", vc.VariantName, "removed", n, "cost", vc.Cost)
	}
}

// sortVariantsForScaleDown orders a role's variants for cost-greedy scale-down:
//  1. Cost descending — shed the most expensive first.
//  2. Tie: coverage per GPU freed, ascending — maxᵢ PRC_i[v] ÷ GPUsPerReplica[v].
//  3. Tie: variant name ascending — full determinism.
//
// Shedding one replica of v returns GPUsPerReplica[v] GPUs to the pool and gives
// up PRC[v] of serving capacity, so the ratio is what one freed GPU costs in
// coverage. Ascending order sheds the replica that gives up the least per GPU it
// returns. Dividing by GPUsPerReplica is what makes two differently-sized
// variants comparable: raw PRC alone prefers shedding the smaller variant even
// when the larger one is strictly less efficient per GPU.
//
// The combine across analyzers is a MAXIMUM, never a sum. A sum answers a
// question nobody asked, since no single removal gives up the total of every
// analyzer's separate estimate, and it grows with the number of configured
// analyzers — so adding a voter would reorder the shed with no observation
// having changed. The maximum takes the most pessimistic estimate of what
// shedding this variant costs, which is the same dominance rule the sizing
// combine follows.
//
// Score does not appear. It is a belief weight consumed by the sizing combine and
// it stops there. This key is a comparator input that is never spent — it orders
// candidates and reduces no budget — so it needs neither a weighting nor a
// currency. With a single analyzer, and for a role whose variants share a
// GPUsPerReplica, this reduces to Cost-desc then PRC-asc: #1237's tie-break.
func sortVariantsForScaleDown(
	s []NamedAnalyzerResult,
	roleVCs []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
) []domain.VariantCapacity {
	coveragePerGPU := func(name string) float64 {
		gpusPR := gpusPerReplicaFromState(stateMap, name)
		best := 0.0
		for _, e := range s {
			if e.Result == nil {
				continue
			}
			prc := prcForVariant(e.Result, name)
			if prc <= 0 {
				// Cannot price v, so it abstains: no estimate to offer, which is
				// not the same as an estimate of zero.
				continue
			}
			if r := prc / float64(gpusPR); r > best {
				best = r
			}
		}
		return best
	}
	out := append([]domain.VariantCapacity(nil), roleVCs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		ci, cj := coveragePerGPU(out[i].VariantName), coveragePerGPU(out[j].VariantName)
		if ci != cj {
			return ci < cj
		}
		return out[i].VariantName < out[j].VariantName
	})
	return out
}

// anyHasReplicas reports whether any of the given variants has a positive target.
func anyHasReplicas(variants []domain.VariantCapacity, targets map[string]int) bool {
	for _, vc := range variants {
		if targets[vc.VariantName] > 0 {
			return true
		}
	}
	return false
}

// buildStateMap creates a lookup map from variant name to VariantReplicaState.
func buildStateMap(states []domain.VariantReplicaState) map[string]domain.VariantReplicaState {
	m := make(map[string]domain.VariantReplicaState, len(states))
	for _, s := range states {
		m[s.VariantName] = s
	}
	return m
}

// buildCapacityMap creates a lookup map from variant name to VariantCapacity.
func buildCapacityMap(capacities []domain.VariantCapacity) map[string]domain.VariantCapacity {
	m := make(map[string]domain.VariantCapacity, len(capacities))
	for _, vc := range capacities {
		m[vc.VariantName] = vc
	}
	return m
}

// initTargets creates initial targets from current replica counts.
func initTargets(states []domain.VariantReplicaState) map[string]int {
	targets := make(map[string]int, len(states))
	for _, s := range states {
		targets[s.VariantName] = s.CurrentReplicas
	}
	return targets
}

// sortByCostEfficiencyAsc returns variants sorted by cost/perReplicaCapacity ascending.
func sortByCostEfficiencyAsc(capacities []domain.VariantCapacity) []domain.VariantCapacity {
	sorted := make([]domain.VariantCapacity, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return costEfficiency(sorted[i]) < costEfficiency(sorted[j])
	})
	return sorted
}

// costEfficiency returns the cost per unit of capacity. An unpriced variant --
// one with no per-replica capacity on this topology -- has no cost per unit to
// report, so it sorts last rather than first. MaxFloat64 keeps it out of every
// cost-ascending pick without pretending its efficiency is zero, which would
// make the one variant nobody can price the cheapest thing in the pool.
func costEfficiency(vc domain.VariantCapacity) float64 {
	if vc.PerReplicaCapacity <= 0 {
		return math.MaxFloat64
	}
	return vc.Cost / vc.PerReplicaCapacity
}

// buildDecisionsWithOptimizer converts targets map into VariantDecision slice.
// optimizerName is included in reason strings for observability.
func buildDecisionsWithOptimizer(
	req ModelScalingRequest,
	stateMap map[string]domain.VariantReplicaState,
	vcMap map[string]domain.VariantCapacity,
	targets map[string]int,
	optimizerName string,
) []domain.VariantDecision {
	decisions := make([]domain.VariantDecision, 0, len(targets))
	// anchor carries the model-level RequiredCapacity/SpareCapacity computed by
	// applyUniversalThreshold; per-variant Utilization is on each VariantCapacity.
	// These feed the saturation gauges (utilization/required/spare). SpareCapacity is
	// additionally an input to the GPU limiter's GreedyBySaturation ordering, so setting
	// it here also makes that ordering reflect real spare tokens on V2 (it was 0 before).
	anchor := bindingAnchor(req.AnalyzerResults)
	for name, target := range targets {
		state := stateMap[name]
		vc := vcMap[name]

		var action domain.SaturationAction
		var decisionReason domain.DecisionReason
		var detailedReason string
		switch {
		case target > state.CurrentReplicas:
			action = domain.ActionScaleUp
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		case target < state.CurrentReplicas:
			action = domain.ActionScaleDown
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		default:
			action = domain.ActionNoChange
			decisionReason = domain.DecisionReasonV2
			detailedReason = string(decisionReason)
		}

		decision := domain.VariantDecision{
			VariantName:     name,
			ModelID:         req.ModelID,
			Namespace:       req.Namespace,
			AcceleratorName: vc.AcceleratorName,
			Cost:            vc.Cost,
			Role:            state.Role,
			CurrentReplicas: state.CurrentReplicas,
			TargetReplicas:  target,
			MinReplicas:     state.MinReplicas,
			MaxReplicas:     state.MaxReplicas,
		}
		// SetDecisionReason is the single place that sets d.Action (avoids a
		// redundant Action assignment in the struct literal above).
		decision.SetDecisionReason(action, decisionReason, detailedReason)

		// Observability fields consumed by RecordSaturationMetrics
		// (wva_saturation_utilization / wva_required_capacity / wva_spare_capacity).
		// Without these the three V2 gauges read zero. RequiredCapacity and
		// SpareCapacity are the matched scale-up/scale-down token signals from
		// applyUniversalThreshold; Utilization is the per-variant demand/capacity ratio.
		// For P/D-disaggregated models use the variant's per-role capacity; otherwise
		// fall back to the model-level totals.
		decision.Utilization = vc.Utilization
		if anchor != nil {
			reqCap, spareCap := anchor.RequiredCapacity, anchor.SpareCapacity
			role := state.Role
			if role == "" {
				role = domain.RoleBoth
			}
			// A role tagged ReasonRoleUnmodeled has no demand model behind
			// its RequiredCapacity/SpareCapacity -- publishing them would
			// present a structural non-answer as a measurement, so fall back
			// to the model-level totals, same as when no per-role entry
			// exists at all.
			if rc, ok := anchor.RoleCapacities[role]; ok && rc.Reason != ReasonRoleUnmodeled {
				reqCap, spareCap = rc.RequiredCapacity, rc.SpareCapacity
			}
			decision.RequiredCapacity = reqCap
			decision.SpareCapacity = spareCap
		}

		decisions = append(decisions, decision)
	}
	return decisions
}

// mergeConstraints combines GPU budget constraints from multiple providers.
// Used by GreedyByScoreOptimizer; lives here since CostAwareOptimizer owns the shared helpers.
func mergeConstraints(constraints []*ResourceConstraints) map[string]int {
	merged := make(map[string]int)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for accType, pool := range c.Pools {
			if pool.Limit < 0 {
				// Unlimited sentinel: no finite cap. Represent it as an
				// unbounded budget (math.MaxInt) so the optimizer allocates the
				// type up to the model's fair-share demand. Leaving it absent
				// would let fairShareRolePick read a 0 budget and silently deny
				// the type — inverting the -1 = unlimited semantic. A finite
				// pool from any provider still wins via the min() below, since
				// Available() < math.MaxInt; only set the sentinel when no finite
				// budget is present yet.
				if _, ok := merged[accType]; !ok {
					merged[accType] = math.MaxInt
				}
				continue
			}
			if existing, ok := merged[accType]; !ok || pool.Available() < existing {
				merged[accType] = pool.Available()
			}
		}
	}
	return merged
}

// mergeNamespaceConstraints merges the per-(namespace, accelerator type) pools
// across providers into available-GPU budgets, taking the most restrictive
// (minimum available) where providers overlap. Returns nil when no provider
// carries namespace pools, so the per-type-only path is unaffected.
//
// A namespace present in any provider's NamespacePools is materialized in the
// result even when its inner map is empty — its presence marks a CLOSED
// allowlist (deny-all when empty) that the optimizer enforces by allocating
// only the listed types. An unlimited (-1 sentinel) pool is carried through as
// a negative budget so the optimizer can distinguish "unlimited" from a finite
// cap; tighterBudget treats it as +infinity when taking the minimum.
func mergeNamespaceConstraints(constraints []*ResourceConstraints) map[string]map[string]int {
	var merged map[string]map[string]int
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			if merged == nil {
				merged = make(map[string]map[string]int)
			}
			inner, ok := merged[ns]
			if !ok {
				inner = make(map[string]int)
				merged[ns] = inner // present (even if empty) marks a closed namespace
			}
			for accType, pool := range perType {
				budget := nsPoolBudget(pool)
				if existing, ok := inner[accType]; ok {
					inner[accType] = tighterBudget(existing, budget)
				} else {
					inner[accType] = budget
				}
			}
		}
	}
	return merged
}

// nsPoolBudget returns a namespace ResourcePool's remaining budget for the
// optimizer — the available (not total) GPU count. A negative Limit is the
// "unlimited" sentinel and is preserved as -1; any other Limit yields the
// finite available count (Limit - Used, clamped to 0).
func nsPoolBudget(pool ResourcePool) int {
	if pool.Limit < 0 {
		return -1
	}
	return pool.Available()
}

// tighterBudget returns the more restrictive of two namespace budgets, treating
// a negative (unlimited) budget as +infinity. The result is unlimited only when
// both inputs are unlimited.
func tighterBudget(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// scaleDownRoleIterated removes replicas role-by-role using the generalized
// scaleDownVariantSet primitive. Roles are sorted for determinism.
// Arity-1 (roles=["both"]) handles non-disaggregated models.
func scaleDownRoleIterated(
	ctx context.Context,
	s []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	targets map[string]int,
	stateMap ...map[string]domain.VariantReplicaState,
) {
	var states map[string]domain.VariantReplicaState
	if len(stateMap) > 0 {
		states = stateMap[0]
	}
	roles := rolesOf(variants)

	for _, role := range roles {
		if !needsScaleDownForRole(s, role) {
			continue
		}
		roleVCs := variantsForRole(variants, role)
		if len(roleVCs) == 0 {
			continue
		}
		sorted := sortVariantsForScaleDown(s, roleVCs, states)
		scaleDownVariantSet(ctx, sorted, targets, states,
			func(vc domain.VariantCapacity) int {
				return safeRemovalReplicasForRole(s, vc.VariantName, role)
			},
			func(vc domain.VariantCapacity, n int) {
				applyDeallocationForRole(s, vc.VariantName, role, n)
			},
		)
	}
}

// Ensure CostAwareOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*CostAwareOptimizer)(nil)
