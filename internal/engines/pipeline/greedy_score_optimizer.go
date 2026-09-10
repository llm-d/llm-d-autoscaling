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
// GPUs across competing models, ordered by fair-share priority value: priority
// times the model's claim, where the claim is a GPU count — the maximum over
// analyzers within each role, summed across the model's roles.
//
// An analyzer's score is a belief weight consumed by the sizing combine
// (combineVotes) and takes no part in fair share; priority is the only
// fair-share weight.
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
	remaining float64                // fair-share claim in priority-scaled GPUs (negative = fully satisfied)
	targets   map[string]int         // variant name → target replicas (ALL variants)
}

// claimGPUs is one model's outstanding fair-share claim, in GPUs. It is the
// numerator of the fair-share metric, and every step of it is a GPU count:
//
//   - each ballot entry's picker-local role demand is converted at entry, using
//     that entry's own per-replica capacity for the role's reference variant. An
//     entry that cannot price that variant has no conversion factor and so
//     contributes nothing — it does not contribute a raw metric, and it does not
//     contribute a zero that could mask another entry's claim.
//   - across analyzers within one role the claim is the MAXIMUM, never a sum: a
//     role needs as many GPUs as its most demanding analyzer says it does, not
//     the total of their separate opinions.
//   - across a model's roles the claims SUM. GPUs are the only currency in which
//     that sum is meaningful, and it is meaningful because prefill and decode
//     compete for the same physical GPUs.
//
// The maximum is deliberately UNWEIGHTED. An analyzer's score is a belief weight
// about how much a variant serves, and this number is spent: it becomes the
// model's fair-share budget. A ranking weight must not scale a quantity that is
// later spent, so score is consumed in the sizing combine and stops there.
func claimGPUs(
	s []NamedAnalyzerResult,
	ps RolePairedState,
	roles []string,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
) float64 {
	claim := 0.0
	for _, role := range roles {
		vc, ok := referenceVariantForRole(variants, role)
		if !ok {
			continue // no variant in this role can be priced by anyone
		}
		gpusPR := gpusPerReplicaFromState(stateMap, vc.VariantName)

		roleClaim := 0.0
		for i, e := range s {
			if e.Result == nil || i >= len(ps) {
				continue
			}
			gpus, ok := toGPUs(ps[i][role], prcForVariant(e.Result, vc.VariantName), gpusPR)
			if !ok {
				continue // no conversion factor ⇒ contributes nothing
			}
			if gpus > roleClaim {
				roleClaim = gpus
			}
		}
		claim += roleClaim
	}
	return claim
}

// fairShareValue computes the fair-share priority metric for one model: its
// claim in GPUs (claimGPUs), scaled by priority for ordering.
//
//	fsv = priority × Σ_role max_i toGPUs(pickerState[i][role], PRC_i[v_role], GPUsPerReplica[v_role])
//
// The returned number is therefore priority-scaled GPUs rather than GPUs.
// Priority belongs in the ordering key and not in the quantity that is spent;
// dividing it back out is a separate change and is deliberately not made here.
//
// Falls back to the unweighted claim when the priority-scaled value is not
// positive, which needs a non-positive priority — reachable only from a
// hand-built request, since ApplyDefaults rewrites an unset priority to 1.0 and
// validation rejects negatives. The fallback is the primary expression minus the
// priority factor precisely so that both paths return GPUs: a fallback in raw
// demand units would re-inflate a model whose demand nobody can act on, which is
// the value the participation filter above exists to exclude.
func fairShareValue(
	priority float64,
	s []NamedAnalyzerResult,
	ps RolePairedState,
	roles []string,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
) float64 {
	claim := claimGPUs(s, ps, roles, variants, stateMap)
	if fsv := priority * claim; fsv > 0 {
		return fsv
	}
	return claim
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
		fsv := fairShareValue(req.Priority, s, ps, roles, anchor.VariantCapacities, buildStateMap(req.VariantStates))

		var w *modelWork
		if anyRoleNeedsScaleUp(ps, roles) || fsv > 0 {
			w = o.buildScaleUpWork(req, anchor, s, ps, roles, fsv)
		}
		if w != nil {
			scaleUpWork = append(scaleUpWork, w)
		} else {
			// A model whose demand is entirely unpriceable claims 0 GPUs, so it
			// has nothing to fair-share for even when a role reports demand. It
			// still belongs on the non-scale-up path — the fair-share queue is
			// the only thing it is excluded from, not the cycle — so that its
			// current state and any safe removals are still reported.
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

// debitCommittedDemand subtracts what a model has already been given from the
// freshly seeded picker-local demand, for every entry whose demand was seeded
// per role.
//
// initRoleState seeds a per-role entry from RoleCapacities[role].RequiredCapacity,
// and nothing ever decrements that field: the allocation loop decrements only the
// caller's working RolePairedState, which is rebuilt on the next call, and
// applyAllocation refreshes only the model-level scalar that the per-role branch
// of initRoleState does not read. Re-seeding therefore restores the demand the
// model started the cycle with, however much of it has been served.
//
// That was harmless while each role was handed the whole model entitlement,
// because the allocation loop then always ran a model's demand to exhaustion
// inside a single call and no second call had anything to re-serve. Once the
// entitlement is one shared balance the loop stops mid-model whenever the
// balance runs out, which is a defer — the model keeps what it committed and
// comes back for the next round's entitlement — and the round after would serve
// the same demand a second time.
//
// The debit is read from the authoritative record of what was given: the target
// replica count against the observed current count, per variant, priced at each
// entry's OWN per-replica capacity for that variant. That is the same quantity
// the allocation loop subtracts when it commits (Bug #1), so a role served
// entirely within one call lands on exactly the same demand either way.
//
// Entries seeded from the model-level scalar are skipped: applyAllocation already
// debits that scalar, so re-seeding reads a value that is current, and debiting it
// again would charge the same replicas twice.
func debitCommittedDemand(
	ps RolePairedState,
	s []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
) {
	for i, e := range s {
		if e.Result == nil || i >= len(ps) || e.Result.RoleCapacities == nil {
			continue
		}
		for _, vc := range variants {
			role := vc.Role
			if role == "" {
				role = domain.RoleBoth
			}
			if _, seeded := ps[i][role]; !seeded {
				continue
			}
			given := targets[vc.VariantName] - stateMap[vc.VariantName].CurrentReplicas
			if given <= 0 {
				continue // nothing committed for this variant, or a reclaim
			}
			prc := prcForVariant(e.Result, vc.VariantName)
			if prc <= 0 {
				// Cannot price the variant, so it abstained on it and charged
				// nothing: there is no committed demand of its own to debit.
				continue
			}
			ps[i][role] = math.Max(0, ps[i][role]-float64(given)*prc)
		}
	}
}

// allocateForModel allocates replicas to bring the model's outstanding claim
// below the mean. Dispatches to the paired path for disaggregated models.
// After allocation, w.remaining is recomputed from the working slice.
//
// target — the model's entitlement this iteration — is priority-scaled GPUs, not
// a replica count and not any analyzer's metric. It reads like a resource count
// and is not one: priority is still folded into it.
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

	// Re-seed picker-state each call so multi-iteration fair-sharing sees the
	// correct post-allocation demand: from the model-level scalar applyAllocation
	// decrements, and — for the per-role seed, which no allocation writes back —
	// from that seed less what the model has already been given
	// (debitCommittedDemand). Then cap at the fair-share budget so the loop exits
	// when it is exhausted.
	//
	// target is priority-scaled GPUs while picker-local demand is each analyzer's
	// own metric, so the bound is converted down into that analyzer's metric
	// rather than compared directly: GPUs → replicas → metric, through the
	// entry's OWN per-replica capacity. This is the only place a per-replica
	// capacity is applied on the way back out of GPU space, and it converts a
	// bound, never a quantity — picker-local demand stays raw for every
	// downstream consumer that divides it by a per-replica capacity again.
	//
	// The budget is ONE balance per model, so the roles draw against it in
	// sequence: each role's clamped demand is charged back in GPUs before the
	// next role is bounded. Handing every (entry, role) pair the whole target
	// instead lets each pair claim the entire budget, which for a P/D model is
	// one entitlement drawn |roles| times — a double-spend, not an over-cap.
	// The balance is per entry because the currency is: two analyzers price the
	// same model in different metrics, and only the combine of them is ever
	// spent. Roles are drawn in w.roles order, so the sequence is deterministic.
	type roleRef struct {
		role   string
		name   string
		gpusPR int
	}
	roleRefs := make([]roleRef, 0, len(w.roles))
	for _, role := range w.roles {
		vc, ok := referenceVariantForRole(w.anchor.VariantCapacities, role)
		if !ok {
			continue
		}
		roleRefs = append(roleRefs, roleRef{
			role:   role,
			name:   vc.VariantName,
			gpusPR: gpusPerReplicaFromState(stateMap, vc.VariantName),
		})
	}
	_, ps := initRoleState(w.s)
	debitCommittedDemand(ps, w.s, w.anchor.VariantCapacities, stateMap, w.targets)
	for i, e := range w.s {
		if e.Result == nil || i >= len(ps) {
			continue
		}
		balance := target
		for _, ref := range roleRefs {
			prc := prcForVariant(e.Result, ref.name)
			bound, ok := fromGPUs(balance, prc, ref.gpusPR)
			if !ok {
				continue // no conversion factor ⇒ no budget to bind this entry
			}
			// One replica's worth of demand is the floor, on the same
			// indivisible-unit policy replicasToCover states for a single role
			// and fairShareRolePick applies to the pick: a role owed less than
			// a whole replica still gets the whole replica. Without it a role
			// whose predecessors drained the balance would be bounded to zero
			// demand, which reads downstream as "this role needs nothing" and
			// silently breaks the joint P/D commit rather than deferring it.
			if bound < prc {
				bound = prc
			}
			if ps[i][ref.role] > bound {
				ps[i][ref.role] = bound
			}
			// Charge the shared balance for what this role now claims.
			if spent, ok := toGPUs(ps[i][ref.role], prc, ref.gpusPR); ok {
				balance = math.Max(0, balance-spent)
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
		w.remaining = fairShareValue(w.req.Priority, w.s, freshPs, w.roles, w.anchor.VariantCapacities, stateMap)
	} else {
		w.remaining = fairShareValue(w.req.Priority, w.s, ps, w.roles, w.anchor.VariantCapacities, stateMap)
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

// fairShareRolePick returns a RolePickFn for the unified allocateForModelPaired
// loop. The joint Δ_util commit inside that loop enforces P/D coupling — α is no
// longer needed.
//
// The model's entitlement is ONE balance in priority-scaled GPUs, not one per
// role. Prefill and decode compete for the same GPUs, so what binds a multi-role
// model is a joint constraint, Σ_role spend[role] ≤ target, and the roles are a
// sequenced draw against a shared remainder rather than a static split — a split
// would under-serve whichever role is cheaper to satisfy. Draw order is the
// caller's roles order, so the sequence is deterministic.
//
// Two ledgers, because two different things are being counted. Committed spend is
// read back out of the targets map, so a role is charged for the replicas
// actually taken, each at its own variant's GPUs per replica. Within one iteration
// of the caller's loop nothing is committed yet — every role is picked before any
// of them is sized — so a grant is also held as a reservation against the same
// balance, and the reservations are dropped once the commit they anticipated shows
// up in targets. That makes the accounting exact across iterations and
// conservative within one, which is the safe direction for a reservation.
//
// Two floors keep the sequence from starving whoever draws last, and they are not
// the same rule — one withholds, the other grants, and only the one that grants is
// rationed.
//
// The first is a holdback: sizing a role sets aside one GPU for each role still to
// draw, so an early role is never sized against GPUs a later role is already owed.
// One GPU apiece because the pool is counted in whole GPUs, and a share below one
// buys nothing. This applies on EVERY draw. It creates nothing — it only moves
// room from an earlier role to a later one — so it cannot inflate the spend, and
// dropping it after the first draw is what starves the last role: with the
// reservation being conservative (what a role could take, not what it will), the
// role drawing first can hold the whole remainder and the role drawing last then
// picks nothing, which ends the caller's loop a full iteration early and sends the
// model back for a second entitlement it does not need.
//
// The second is the indivisible unit: a role may take one replica whether or not
// the shared remainder still covers it — the policy replicasToCover states for a
// single role, applied jointly. This one grants beyond the balance, so it is for
// the model's FIRST draw only, and the caller's contract is why. It reads an empty
// pick as "this model cannot be served" and abandons the model, so before anything
// is committed a starved role costs every role its allocation rather than just its
// own; once the model holds something, an exhausted balance ends the caller's loop
// with the commitment intact, which is a defer. Kept on past the first draw it
// would be a per-iteration drip that the entitlement never bounds. With the
// holdback in place it is reachable only when the entitlement is itself smaller
// than one GPU per role.
func fairShareRolePick(target float64, s []NamedAnalyzerResult, roles []string) RolePickFn {
	_ = s // slice available for future multi-analyzer demand inspection

	var committed0 map[string]int        // targets as of this entitlement's first draw
	reserved := make(map[string]float64) // GPUs granted this iteration, per role
	return func(
		role string,
		_ []NamedAnalyzerResult,
		variants []domain.VariantCapacity,
		stateMap map[string]domain.VariantReplicaState,
		available map[string]int,
		targets map[string]int,
	) (string, int) {
		if committed0 == nil {
			committed0 = maps.Clone(targets)
			if committed0 == nil {
				committed0 = map[string]int{}
			}
		}
		// A role drawing twice means the caller has moved on to its next
		// iteration, so whatever the earlier grants became is now in targets.
		if _, drawn := reserved[role]; drawn {
			reserved = make(map[string]float64, len(roles))
		}

		spentGPUs := 0
		for v, n := range targets {
			if k := n - committed0[v]; k > 0 {
				spentGPUs += k * gpusPerReplicaFromState(stateMap, v)
			}
		}
		balance := target - float64(spentGPUs)
		for _, g := range reserved {
			balance -= g
		}

		// Nothing committed yet means an empty pick makes the caller abandon the
		// model outright rather than defer it, which is the one case worth
		// granting past the balance for. See the second floor below.
		firstDraw := spentGPUs == 0

		// Holdback: sizing a role must not spend the GPU each role still to draw
		// is owed. One apiece — the pool is counted in whole GPUs, so a share
		// below one buys nothing anyway. Every draw, not just the first: this
		// only moves room between roles, and without it the role drawing last
		// picks nothing whenever a predecessor's conservative reservation
		// swallows the remainder.
		share := balance
		for _, r := range roles {
			if r == role {
				continue
			}
			if _, drawn := reserved[r]; !drawn {
				share--
			}
		}

		roleVCs := variantsForRole(variants, role)
		for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
			// Unpriced on the anchor's topology: no per-replica capacity means no
			// conversion between the entitlement, which is in GPUs, and the
			// demand this variant would serve, so it can neither be sized
			// against the balance nor charged to it. The gate asks whether the
			// variant has a price, not whether some number is zero -- a variant
			// admitted at a sentinel price is priced, and passes.
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			state := stateMap[vc.VariantName]
			gpusPR := gpusPerReplicaFromState(stateMap, vc.VariantName)
			gpusAvail := available[vc.AcceleratorName]
			if gpusAvail < gpusPR {
				continue
			}
			// The entitlement is already GPUs, so the cap divides by whichever
			// candidate this loop landed on — there is no reference capacity to
			// compensate for. The two terms round in opposite directions on
			// purpose: the entitlement rounds up, because a replica is the
			// indivisible unit allocation happens in, while the real pool rounds
			// down, because those GPUs either exist or they do not.
			capN := replicasToCover(share, gpusPR)
			if firstDraw && capN < 1 {
				// This role's own replica is not the shared remainder's to
				// withhold — the same indivisible-unit policy replicasToCover
				// states for one role, applied jointly. First draw only: it
				// grants past the balance, and only before the first commit is
				// an empty pick fatal to the whole model rather than a defer.
				//
				// This raises capN, so every bound must be applied after it, not
				// before. The two clamps below rely on that ordering.
				capN = 1
			}
			capN = min(capN, gpusAvail/gpusPR)
			// Skip on an exhausted ceiling rather than falling through with
			// capN == 0: the capN > 0 guard below would return an empty pick and
			// abandon the role, when the variants behind this one are still
			// perfectly allocatable.
			if maxTarget, bounded := maxTargetReplicas(vc, state); bounded {
				headroom := maxTarget - targets[vc.VariantName]
				if headroom <= 0 {
					continue
				}
				capN = min(capN, headroom)
			}
			if capN > 0 {
				// Charge the balance, never beyond the share that sized the
				// grant: the round-up can exceed it, and a reservation larger
				// than the remainder would take back the room this role's
				// successors were just left.
				reserved[role] = math.Max(0, math.Min(float64(capN*gpusPR), share))
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

// computeMean returns the water level of the fair-share fill: an unweighted
// arithmetic mean, in priority-scaled GPUs, over the claims of the models still
// active this cycle. Unweighted across models is the point — a mean that
// re-applied each model's own weight would not be a common level to fill toward.
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

// sortByRemainingDesc sorts active models by outstanding claim descending, so the
// fill serves the largest claim first. The key is priority × claim: a comparator
// input and nothing else, never a quantity that is spent.
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

// toGPUs converts a quantity in one analyzer's own metric into GPUs, the single
// currency the fair-share arithmetic is denominated in. It is the only place a
// per-replica capacity is applied on the way IN: dividing by that analyzer's own
// per-replica capacity gives replicas, multiplying by the variant's GPUs per
// replica gives GPUs.
//
// ok is false when there is no conversion factor. A caller must then skip the
// entry rather than substitute a zero or carry the raw metric forward: an
// analyzer that cannot price the variant has no opinion to convert, and a raw
// metric compared against GPUs is the unit mixing this conversion exists to end.
func toGPUs(metric, perReplicaCapacity float64, gpusPerReplica int) (float64, bool) {
	if perReplicaCapacity <= 0 || gpusPerReplica <= 0 {
		return 0, false
	}
	return metric / perReplicaCapacity * float64(gpusPerReplica), true
}

// fromGPUs converts a GPU bound back down into one analyzer's own metric: GPUs →
// replicas → metric, through that analyzer's own per-replica capacity. It is the
// only place a per-replica capacity is applied on the way OUT, and it is for
// bounds only — converting a stored quantity would re-denominate state that every
// downstream consumer reads as a raw metric.
//
// ok is false when there is no conversion factor, on the same terms as toGPUs.
func fromGPUs(gpus, perReplicaCapacity float64, gpusPerReplica int) (float64, bool) {
	if perReplicaCapacity <= 0 || gpusPerReplica <= 0 {
		return 0, false
	}
	return gpus / float64(gpusPerReplica) * perReplicaCapacity, true
}

// replicasToCover returns how many whole replicas it takes to cover a GPU
// entitlement, rounding up.
//
// The entitlement is a fair-share water-level gap, not a pool of GPUs on hand, so
// rounding up here cannot overcommit hardware: the caller mins this against the
// real pool, which is floored separately. What the rounding decides is whether a
// model owed a fraction of a replica may take the one indivisible unit that
// allocation happens in. Rounding up says yes, and the caller's water-level check
// then stops it from taking a second.
func replicasToCover(entitlementGPUs float64, gpusPerReplica int) int {
	if entitlementGPUs <= 0 || gpusPerReplica <= 0 {
		return 0
	}
	return int(math.Ceil(entitlementGPUs / float64(gpusPerReplica)))
}

// referenceVariantForRole returns the variant that denominates a role's
// fair-share claim: the role's first sortByCostEfficiencyAsc candidate carrying a
// usable per-replica capacity.
//
// Its job is to price the claim, not to choose what gets allocated. The picker
// loops take the first FEASIBLE candidate and are expected to land on a different
// variant when the cheaper accelerator pool is dry or the cheaper variant is at
// its replica ceiling. That disagreement needs no correction in GPU space: the
// cap divides by whichever candidate the picker landed on, and GPUs per replica
// is immutable deployment topology rather than a re-derived capacity.
//
// Pricing a whole role through one representative variant is an approximation
// whenever the role's variants have unequal per-replica capacities. It is
// pre-existing, and denominating in GPUs neither introduces nor removes it.
func referenceVariantForRole(vcs []domain.VariantCapacity, role string) (domain.VariantCapacity, bool) {
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(vcs, role)) {
		if vc.PerReplicaCapacity > 0 {
			return vc, true
		}
	}
	return domain.VariantCapacity{}, false
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
