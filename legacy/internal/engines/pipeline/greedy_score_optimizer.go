package pipeline

import (
	"context"
	"maps"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyByScoreOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models, ordered by fair-share priority value
// (priority × Σᵢ(Remainingᵢ × Scoreᵢ) across analyzers).
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (highest-priority model gets GPUs first)
//   - Disaggregated models use paired (n_P, n_D) allocation via the paired helpers
//   - Scale-down uses scaleDownRoleIterated (role-iterated unified path)
type GreedyByScoreOptimizer struct {
	// Rescale carries the resolved, scope-coupled rescale enablement for the
	// current cycle (set by the engine before Optimize). Zero value = off, which
	// keeps the additive fair-share behaviour unchanged.
	Rescale RescaleFlags
}

// NewGreedyByScoreOptimizer creates a new GreedyByScoreOptimizer.
func NewGreedyByScoreOptimizer() *GreedyByScoreOptimizer {
	return &GreedyByScoreOptimizer{}
}

// Name returns the optimizer identifier.
func (o *GreedyByScoreOptimizer) Name() string {
	return "greedy-by-score"
}

// modelWork tracks per-model allocation state during fair-share iteration.
type modelWork struct {
	req       ModelScalingRequest
	s         []NamedAnalyzerResult  // working slice; Remaining/Spare decremented in place
	anchor    *domain.AnalyzerResult // merged per-model anchor (topology + sizing); see bindingAnchor
	ps        RolePairedState        // picker-local per-role demand (from initRoleState)
	roles     []string               // active roles for this model
	remaining float64                // fair-share priority metric (negative = fully satisfied)
	targets   map[string]int         // variant name → target replicas (ALL variants)
}

// fairShareValue computes the fair-share priority metric for one model.
// Phase 3: reads picker-local role-remaining (sum over roles × analyzer Score)
// so the metric reflects actual per-role demand remaining rather than the
// P-anchor model-level scalar.
//
//	fsv = priority × Σᵢ Score_i × Σ_role pickerState[i][role]
//
// Falls back to max remaining demand when the weighted result is zero.
func fairShareValue(priority float64, s []NamedAnalyzerResult, ps RolePairedState, roles []string) float64 {
	weighted := 0.0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		roleSum := 0.0
		for _, role := range roles {
			if i < len(ps) {
				roleSum += ps[i][role]
			}
		}
		weighted += roleSum * e.Score
	}
	if fsv := priority * weighted; fsv > 0 {
		return fsv
	}
	// Fallback: max remaining demand across roles when Score=0 or priority=0.
	maxDemand := 0.0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		if i < len(ps) {
			for _, role := range roles {
				if ps[i][role] > maxDemand {
					maxDemand = ps[i][role]
				}
			}
		}
	}
	return maxDemand
}

// Optimize produces VariantDecisions for all models, fair-sharing GPUs across
// models that need to scale up. Scale-down models are handled independently.
func (o *GreedyByScoreOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	available := mergeConstraints(constraints)
	availableByNS := mergeNamespaceConstraints(constraints)

	// Rescale pre-pass: for enabled, contended (type, budget-scope) groups, compute
	// priority-weighted targets and produce reclaim/fill decisions, consuming free
	// GPUs from `available`/`availableByNS` so the additive path below sees the
	// reduced budget. Models it handles are excluded from the additive path. When
	// rescale is off or no group is contended, `handled` is empty and behaviour is
	// unchanged.
	var rescaleDecisions []domain.VariantDecision
	var handled map[string]bool
	if o.Rescale.any() {
		rescaleDecisions, handled = o.applyRescale(ctx, requests, available, availableByNS)
	}

	var scaleUpWork []*modelWork
	var otherRequests []ModelScalingRequest

	for _, req := range requests {
		if handled[modelKey(req)] {
			continue
		}
		anchor := bindingAnchor(req.AnalyzerResults)
		if anchor == nil {
			continue
		}

		// Combine (RC/SC) math consumes only the voting subset of the ballot.
		s := votingResults(req.AnalyzerResults)
		roles, ps := initRoleState(s)
		fsv := fairShareValue(req.Priority, s, ps, roles)
		if anyRoleNeedsScaleUp(ps, roles) || fsv > 0 {
			w := o.buildScaleUpWork(req, anchor, s, ps, roles, fsv)
			if w != nil {
				scaleUpWork = append(scaleUpWork, w)
			}
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	o.fairShareScaleUp(ctx, scaleUpWork, available, availableByNS)

	allDecisions := make([]domain.VariantDecision, 0, len(scaleUpWork))

	for _, w := range scaleUpWork {
		stateMap := buildStateMap(w.req.VariantStates)
		vcMap := buildCapacityMap(w.anchor.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(w.req, stateMap, vcMap, w.targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (scale-up)",
			"modelID", w.req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		anchor := bindingAnchor(req.AnalyzerResults)
		if anchor == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(anchor.VariantCapacities)
		targets := initTargets(req.VariantStates)

		// Unified scale-down path via scaleDownRoleIterated.
		// Combine (RC/SC) math consumes only the voting subset of the ballot.
		s := votingResults(req.AnalyzerResults)
		_, _ = initRoleState(s) // populates RoleSpare for all roles
		scaleDownRoleIterated(ctx, s, anchor.VariantCapacities, targets, stateMap)

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	allDecisions = append(allDecisions, rescaleDecisions...)
	return allDecisions
}

// buildScaleUpWork creates a single work unit for a scale-up request.
func (o *GreedyByScoreOptimizer) buildScaleUpWork(req ModelScalingRequest, anchor *domain.AnalyzerResult, s []NamedAnalyzerResult, ps RolePairedState, roles []string, fsv float64) *modelWork {
	if fsv <= 0 {
		return nil
	}
	return &modelWork{
		req:       req,
		s:         s,
		anchor:    anchor,
		ps:        ps,
		roles:     roles,
		remaining: fsv,
		targets:   initTargets(req.VariantStates),
	}
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
func (o *GreedyByScoreOptimizer) fairShareScaleUp(
	ctx context.Context,
	work []*modelWork,
	available map[string]int,
	availableByNS map[string]map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	for {
		active := filterActive(work)
		if len(active) == 0 {
			break
		}

		totalGPUs := 0
		for _, v := range available {
			// An unbounded (math.MaxInt) budget marks an unlimited quota type;
			// saturate rather than overflow the sum, which is only used for the
			// "== 0" stop check below.
			if v == math.MaxInt {
				totalGPUs = math.MaxInt
				break
			}
			totalGPUs += v
		}
		if totalGPUs == 0 {
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs remaining, stopping fair-share")
			break
		}

		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyByScore: iteration",
			"activeModels", len(active), "meanRemaining", mean)

		sortByRemainingDesc(active)
		w := active[0]

		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

		allocated := o.allocateForModel(ctx, w, allocationMean, available, availableByNS)

		if !allocated {
			w.remaining = -1
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs available for model, removing",
				"model", w.req.ModelID)
			continue
		}

		if w.remaining > mean {
			logger.V(logging.DEBUG).Info("GreedyByScore: model still above mean, removing",
				"model", w.req.ModelID, "remaining", w.remaining, "mean", mean)
			w.remaining = -1
		}
	}
}

// allocateForModel allocates replicas to bring the model's remaining score below
// the mean. Dispatches to the paired path for disaggregated models.
// After allocation, w.remaining is recomputed from the working slice.
func (o *GreedyByScoreOptimizer) allocateForModel(
	ctx context.Context,
	w *modelWork,
	mean float64,
	available map[string]int,
	availableByNS map[string]map[string]int,
) bool {
	target := w.remaining - mean
	if target <= 0 {
		return false
	}

	stateMap := buildStateMap(w.req.VariantStates)
	oldRemaining := w.remaining

	// Re-initialize picker-state from current s[i].Remaining each call so
	// multi-iteration fair-sharing sees the correct post-allocation demand.
	// Cap at target so the loop exits when the fair-share budget is exhausted.
	_, ps := initRoleState(w.s)
	for i := range ps {
		for _, role := range w.roles {
			if ps[i][role] > target {
				ps[i][role] = target
			}
		}
	}

	// This model belongs to one namespace, so its per-type budget is the
	// minimum of the cluster-wide budget and this namespace's quota. The shared
	// allocateForModelPaired only understands a flat per-type budget, so we pass
	// the effective copy and reconcile consumption back to both budgets
	// afterwards. A non-nil nsBudget means a closed namespace-quota allowlist
	// (see effectiveAvailable); nil means the namespace is open (cluster-scope
	// quota or an excluded namespace).
	//
	// nsBudget is a reference into the cycle-wide availableByNS map, shared by
	// every model in this namespace; the reconcile below decrements it in place,
	// so later same-namespace models correctly see the reduced remaining budget.
	nsBudget := availableByNS[w.req.Namespace]
	effAvail := effectiveAvailable(available, nsBudget)
	beforeEff := maps.Clone(effAvail)

	// Unified path: fairShareRolePick behind the RolePickFn interface.
	// α logic removed in commit 3.
	pick := fairShareRolePick(target, w.s, w.roles)
	allocateForModelPaired(ctx, w.s, w.anchor.VariantCapacities, stateMap, effAvail,
		w.targets, pick, ps, w.roles)

	// Reconcile: apply what was consumed (before − after) to the cluster-wide
	// budget and, where this namespace caps the type, to the namespace budget.
	// Only decrement the cluster budget for types it actually constrains (a type
	// present in `available`); a type bounded solely by the namespace must not
	// drive the cluster budget negative and pollute the loop's totalGPUs check.
	for accType, before := range beforeEff {
		consumed := before - effAvail[accType]
		if consumed <= 0 {
			continue
		}
		// Decrement only a FINITE cluster budget. An unbounded (math.MaxInt)
		// budget marks an unlimited-quota type and must stay exactly math.MaxInt:
		// depleting it to MaxInt-consumed would (a) be meaningless (you cannot
		// draw down infinity) and (b) defeat the fairShareScaleUp stop-check,
		// whose `== math.MaxInt` guard would then miss it and let the totalGPUs
		// sum overflow with two or more unlimited types.
		if cur, clusterCapped := available[accType]; clusterCapped && cur != math.MaxInt {
			available[accType] -= consumed
		}
		if nsBudget != nil {
			// Decrement only finite namespace caps; the unlimited sentinel
			// (negative) imposes no budget to draw down.
			if nsCap, capped := nsBudget[accType]; capped && nsCap >= 0 {
				nsBudget[accType] -= consumed
			}
		}
	}

	// Recompute w.remaining for fair-share ordering.
	// For "both" (non-disag): use fresh ps so applyAllocation-decremented
	// s[i].Remaining is read (budget-capped ps is already 0).
	// For P/D: use local capped ps which correctly reaches 0 when both roles served.
	if len(w.roles) == 1 && w.roles[0] == domain.RoleBoth {
		_, freshPs := initRoleState(w.s)
		w.remaining = fairShareValue(w.req.Priority, w.s, freshPs, w.roles)
	} else {
		w.remaining = fairShareValue(w.req.Priority, w.s, ps, w.roles)
	}
	return w.remaining < oldRemaining
}

// effectiveAvailable returns the per-type budget the optimizer may spend on a
// model in this namespace, given the cluster-wide budget and the namespace's
// quota. A budget value < 0 in nsBudget is the "unlimited" sentinel for that
// (namespace, type); all other values are finite GPU counts.
//
// nsBudget == nil → the namespace is OPEN (a cluster-scope quota, or an
// excluded namespace): the model is bound only by the cluster budget, so the
// result is a copy of `available`.
//
// nsBudget != nil → the namespace is a CLOSED allowlist (namespace-scope
// quota), mirroring the V1 tryAllocateNamespace contract: the model may use
// ONLY the accelerator types the namespace lists. The result is therefore built
// from nsBudget alone — a type the namespace does not list is absent, and the
// optimizer's gpusAvail==0 check denies it (no fall-through to the cluster
// aggregate, which previously let one namespace draw on another's quota). For a
// listed type: a finite cap binds at min(cluster, cap); an unlimited cap binds
// at the cluster budget for that type, or is unbounded (math.MaxInt) when the
// cluster does not constrain it.
func effectiveAvailable(available, nsBudget map[string]int) map[string]int {
	if nsBudget == nil {
		return maps.Clone(available)
	}
	eff := make(map[string]int, len(nsBudget))
	for accType, nsAvail := range nsBudget {
		if nsAvail < 0 { // unlimited for this (namespace, type)
			if cv, ok := available[accType]; ok {
				eff[accType] = cv
			} else {
				eff[accType] = math.MaxInt
			}
			continue
		}
		eff[accType] = nsAvail
		if cv, ok := available[accType]; ok && cv < nsAvail {
			eff[accType] = cv
		}
	}
	return eff
}

// fairShareRolePick returns a RolePickFn for the unified allocateForModelPaired loop.
// Each role receives the same target fair-share budget. The joint Δ_util commit
// inside allocateForModelPaired enforces P/D coupling — α is no longer needed.
func fairShareRolePick(target float64, s []NamedAnalyzerResult, roles []string) RolePickFn {
	_ = s     // slice available for future multi-analyzer demand inspection
	_ = roles // roles available for future per-role budget splitting
	return func(
		role string,
		_ []NamedAnalyzerResult,
		variants []domain.VariantCapacity,
		stateMap map[string]domain.VariantReplicaState,
		available map[string]int,
		targets map[string]int,
	) (string, int) {
		roleVCs := variantsForRole(variants, role)
		for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			state := stateMap[vc.VariantName]
			gpusPR := state.GPUsPerReplica
			if gpusPR <= 0 {
				gpusPR = 1
			}
			gpusAvail := available[vc.AcceleratorName]
			if gpusAvail < gpusPR {
				continue
			}
			fairShareCap := int(math.Ceil(target / vc.PerReplicaCapacity))
			capN := min(fairShareCap, gpusAvail/gpusPR)
			if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
				headroom := *state.MaxReplicas - targets[vc.VariantName]
				if headroom <= 0 {
					continue
				}
				capN = min(capN, headroom)
			}
			if capN > 0 {
				return vc.VariantName, capN
			}
		}
		return "", 0
	}
}

// filterActive returns modelWork entries that still have remaining > 0.
func filterActive(work []*modelWork) []*modelWork {
	var active []*modelWork
	for _, w := range work {
		if w.remaining > 0 {
			active = append(active, w)
		}
	}
	return active
}

// computeMean returns the average remaining across active models.
func computeMean(active []*modelWork) float64 {
	if len(active) == 0 {
		return 0
	}
	total := 0.0
	for _, w := range active {
		total += w.remaining
	}
	return total / float64(len(active))
}

// sortByRemainingDesc sorts active models by remaining descending.
func sortByRemainingDesc(active []*modelWork) {
	sort.Slice(active, func(i, j int) bool {
		return active[i].remaining > active[j].remaining
	})
}

// prcFromVCs returns the PerReplicaCapacity for variant v from a slice of VCs.
func prcFromVCs(vcs []domain.VariantCapacity, v string) float64 {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// accFromVCs returns the AcceleratorName for variant v from a slice of VCs.
func accFromVCs(vcs []domain.VariantCapacity, v string) string {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc.AcceleratorName
		}
	}
	return ""
}

// gpusPerReplicaFromState returns GPUsPerReplica for variant v, defaulting to 1.
func gpusPerReplicaFromState(stateMap map[string]domain.VariantReplicaState, v string) int {
	if state, ok := stateMap[v]; ok && state.GPUsPerReplica > 0 {
		return state.GPUsPerReplica
	}
	return 1
}

// Ensure GreedyByScoreOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyByScoreOptimizer)(nil)
