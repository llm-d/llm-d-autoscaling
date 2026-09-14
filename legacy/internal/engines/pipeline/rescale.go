package pipeline

import (
	"cmp"
	"context"
	"maps"
	"math"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// modelKey is a model's identity for rescale bookkeeping. A model is identified by
// (namespace, ModelID), never ModelID alone: the same ModelID can be served in two
// namespaces, and both can land in the same cluster-scope competition group. Keying
// maps by the bare ModelID would collide those models — overwriting targets, double-
// counting floors, and freezing one namespace's model out of the additive path.
func modelKey(req ModelScalingRequest) string {
	return utils.GetNamespacedKey(req.Namespace, req.ModelID)
}

// RescaleFlags carries the resolved, scope-coupled rescale enablement for one
// optimize cycle. Cluster governs the cluster budget group; ByNamespace[ns]
// governs namespace ns's quota budget group. The zero value disables rescale.
type RescaleFlags struct {
	Cluster     bool
	ByNamespace map[string]bool
}

func (f RescaleFlags) any() bool { return f.Cluster || len(f.ByNamespace) > 0 }

// enabledForScope reports whether rescale is on at a group's budget scope:
// the namespace flag for a namespace-quota group, else the cluster flag.
func (f RescaleFlags) enabledForScope(namespace string, namespaceScoped bool) bool {
	if namespaceScoped {
		return f.ByNamespace[namespace]
	}
	return f.Cluster
}

// rescaleInput is one model's contribution to the priority-weighted water-filling.
// All GPU quantities are whole GPUs; Demand is in the analyzer's own unit and is
// used only inside the weight ratio (priority x demand), so its unit cancels.
type rescaleInput struct {
	ID        string
	Priority  float64 // model priority multiplier (>= 0)
	Demand    float64 // model demand for the weight (any unit; ratio only)
	FloorGPUs int     // reserved: sum over variants of minReplicas x gpusPerReplica
	CapGPUs   int     // upper bound: min(demand-in-GPUs, maxReplicas x gpusPerReplica); >= FloorGPUs
}

// computeRescaleTargets distributes `budget` GPUs across models by priority-weighted
// water-filling, returning each model's target GPU allocation.
//
//	target_i = floor_i + (budget - Sum floor) * weight_i / Sum weight     (weight_i = priority_i * demand_i)
//
// A model wanting less than its share is capped at CapGPUs_i and the freed excess
// re-splits over the still-hungry models by the same weights, until none exceeds its
// cap. Floors are always reserved first; when Sum floor > budget the floors cannot be
// met — every model is clamped to its floor and overBudget is true (a Conflict the
// caller surfaces). Fractional shares are rounded to whole GPUs by the largest-remainder
// method, never exceeding a model's cap, so the returned targets sum to at most `budget`.
func computeRescaleTargets(in []rescaleInput, budget int) (targets map[string]int, overBudget bool) {
	targets = make(map[string]int, len(in))

	sumFloor := 0
	for _, m := range in {
		targets[m.ID] = m.FloorGPUs
		sumFloor += m.FloorGPUs
	}
	if sumFloor >= budget {
		// Floors already consume (or exceed) the budget: nothing to redistribute.
		return targets, sumFloor > budget
	}

	pool := budget - sumFloor // redistributable GPUs above the floors

	// Iterative water-fill in fractional space over `pool`, capping at each model's
	// headroom (CapGPUs - FloorGPUs) and re-splitting the freed remainder.
	type active struct {
		id       string
		weight   float64
		headroom int
	}
	act := make([]active, 0, len(in))
	for _, m := range in {
		hr := m.CapGPUs - m.FloorGPUs
		if hr < 0 {
			hr = 0
		}
		w := m.Priority * m.Demand
		if w > 0 && hr > 0 {
			act = append(act, active{m.ID, w, hr})
		}
	}

	extra := make(map[string]float64, len(act))
	remaining := float64(pool)
	for len(act) > 0 && remaining > 1e-9 {
		totalW := 0.0
		for _, a := range act {
			totalW += a.weight
		}
		if totalW <= 0 {
			break
		}
		// Cap any model whose proportional share reaches its headroom; if none does,
		// assign the remainder proportionally and finish.
		capped := false
		used := 0.0
		next := act[:0:0]
		for _, a := range act {
			share := remaining * a.weight / totalW
			if share >= float64(a.headroom)-1e-9 {
				extra[a.id] = float64(a.headroom)
				used += float64(a.headroom)
				capped = true
			} else {
				next = append(next, a)
			}
		}
		if capped {
			remaining -= used
			act = next
			continue
		}
		for _, a := range act {
			extra[a.id] = remaining * a.weight / totalW
		}
		remaining = 0
		act = nil
	}

	// Round fractional extras to whole GPUs (largest remainder), respecting each
	// model's headroom, so the integer total does not exceed the fractional total.
	roundExtras(in, extra, targets)
	return targets, false
}

// roundExtras adds each model's fractional above-floor allocation to its target using
// the largest-remainder method: floor everything, then hand out the leftover GPUs one at
// a time to the largest fractional parts, skipping models already at their cap.
func roundExtras(in []rescaleInput, extra map[string]float64, targets map[string]int) {
	capOf := make(map[string]int, len(in))
	for _, m := range in {
		capOf[m.ID] = m.CapGPUs
	}

	remainder := make(map[string]float64, len(extra))
	totalFrac := 0.0
	sumWhole := 0
	for id, e := range extra {
		whole := int(math.Floor(e))
		targets[id] += whole
		sumWhole += whole
		totalFrac += e
		remainder[id] = e - float64(whole)
	}
	// leftover = round(total above-floor allocation) - the whole GPUs already applied.
	leftover := int(math.Round(totalFrac)) - sumWhole
	apportionLeftover(targets, remainder, leftover, capOf)
}

// apportionLeftover hands out `leftover` whole units one at a time to the keys with
// the largest fractional remainders (deterministic tie-break by key ascending),
// skipping any key already at capOf[key]. A nil capOf means uncapped. It mutates out.
func apportionLeftover(out map[string]int, remainder map[string]float64, leftover int, capOf map[string]int) {
	if leftover <= 0 {
		return
	}
	type kr struct {
		key  string
		frac float64
	}
	order := make([]kr, 0, len(remainder))
	for k, v := range remainder {
		order = append(order, kr{k, v})
	}
	// Largest fractional remainder first; deterministic tie-break by key.
	slices.SortFunc(order, func(a, b kr) int {
		return cmp.Or(cmp.Compare(b.frac, a.frac), cmp.Compare(a.key, b.key))
	})
	for _, o := range order {
		if leftover <= 0 {
			break
		}
		if capOf != nil {
			if c, ok := capOf[o.key]; ok && out[o.key] >= c {
				continue
			}
		}
		out[o.key]++
		leftover--
	}
}

// applyRescale runs the priority-weighted rescale pass for enabled, contended
// (accelerator type, budget scope) groups. It returns the decisions it produced and
// the set of model IDs it handled (which the caller excludes from the additive path).
// Fills consume free GPUs from `available` / `availableByNS` in place; reclaims do not
// free budget this cycle (usage is CurrentReplicas-based), so fills never over-subscribe.
//
// P/D models are handled: a model's GPU target is split across its roles by role
// demand, and reclaim/fill run per role. Multi-accelerator models (variants spanning
// GPU types, incl. P/D across types) are skipped (deferred).
func (o *GreedyByScoreOptimizer) applyRescale(
	ctx context.Context,
	requests []ModelScalingRequest,
	available map[string]int,
	availableByNS map[string]map[string]int,
) ([]domain.VariantDecision, map[string]bool) {
	logger := ctrl.LoggerFrom(ctx).WithName("rescale")
	handled := make(map[string]bool)
	var decisions []domain.VariantDecision

	type groupKey struct {
		accType string
		scope   string // namespace for namespace-scoped groups, "" for cluster
	}
	groups := make(map[groupKey][]ModelScalingRequest)
	for _, req := range requests {
		anchor := bindingAnchor(req.AnalyzerResults)
		if anchor == nil {
			continue
		}
		accType, ok := singleAccType(anchor.VariantCapacities)
		if !ok {
			continue // multi-accelerator (incl. P/D spanning types): deferred
		}
		// A namespace present in availableByNS is a closed quota allowlist → namespace scope.
		_, nsScoped := availableByNS[req.Namespace]
		if !o.Rescale.enabledForScope(req.Namespace, nsScoped) {
			continue
		}
		scope := ""
		if nsScoped {
			scope = req.Namespace
		}
		k := groupKey{accType, scope}
		groups[k] = append(groups[k], req)
	}

	// Process groups in a deterministic order. A namespace fill debits the shared
	// cluster budget, so a cluster-scoped group on the same accelerator type observes
	// the result — a stable order keeps a cycle reproducible for identical input.
	keys := slices.Collect(maps.Keys(groups))
	slices.SortFunc(keys, func(a, b groupKey) int {
		return cmp.Or(cmp.Compare(a.accType, b.accType), cmp.Compare(a.scope, b.scope))
	})

	for _, k := range keys {
		reqs := groups[k]
		free := available[k.accType]
		if k.scope != "" {
			free = availableByNS[k.scope][k.accType]
		}
		// Skip unlimited/unknown budgets — there is nothing to redistribute.
		if free < 0 || free == math.MaxInt {
			continue
		}

		currentUsage := 0
		for _, req := range reqs {
			currentUsage += modelCurrentGPUs(req, k.accType)
		}
		budget := free + currentUsage

		inputs, sumDemandGPUs := rescaleInputsForGroup(reqs, k.accType, budget)
		if sumDemandGPUs <= budget {
			continue // uncontended: leave to the additive path
		}

		targets, overBudget := computeRescaleTargets(inputs, budget)
		if overBudget {
			logger.Info("rescale: minReplicas floors exceed the group budget (Conflict); clamping to floors",
				"accType", k.accType, "scope", k.scope, "budget", budget)
		}

		// Serve highest-priority models first: same-cycle free GPUs are scarce
		// (reclaims free nothing until next cycle), so a lower-priority model earlier
		// in request order must not claim a fill ahead of a higher-priority one.
		// Deterministic tie-break by the full (namespace, ModelID) identity — a bare
		// ModelID ties two same-named models in different namespaces, leaving the winner
		// to randomized request order (flap cycle-to-cycle).
		slices.SortStableFunc(reqs, func(a, b ModelScalingRequest) int {
			return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(modelKey(a), modelKey(b)))
		})

		// GPUs we can physically hand out this cycle. A namespace fill draws from the
		// shared physical pool, so bound it by the cluster's finite free too — filling
		// up to the namespace quota alone would emit scale-ups for GPUs that are not
		// physically free (over-subscription) and drive the cluster budget negative.
		// The water-fill target above intentionally uses the full quota; fills pace
		// toward it over cycles as physical GPUs free up.
		fillable := free
		if k.scope != "" {
			if cur, ok := available[k.accType]; ok && cur != math.MaxInt {
				fillable = min(fillable, cur)
			}
		}

		freeThisCycle := fillable
		for _, req := range reqs {
			d := o.rescaleModelDecisions(ctx, req, k.accType, targets[modelKey(req)], &freeThisCycle)
			decisions = append(decisions, d...)
			handled[modelKey(req)] = true
		}

		if consumed := fillable - freeThisCycle; consumed > 0 {
			if k.scope != "" {
				if availableByNS[k.scope] != nil {
					availableByNS[k.scope][k.accType] -= consumed
				}
				// A namespace fill also draws from the shared physical pool; debit the
				// finite cluster budget too (mirroring allocateForModel). Bounded by
				// fillable above, this can never drive the cluster budget negative.
				if cur, ok := available[k.accType]; ok && cur != math.MaxInt {
					available[k.accType] -= consumed
				}
			} else {
				available[k.accType] -= consumed
			}
		}
	}

	return decisions, handled
}

// rescaleModelDecisions drives one model to targetGPUs on accType: reclaim (shed
// most-expensive-first, respecting minReplicas) or fill (add most-cost-efficient
// first, gated on *freeThisCycle). Reclaim decisions are tagged DecisionReasonRescale.
func (o *GreedyByScoreOptimizer) rescaleModelDecisions(
	ctx context.Context,
	req ModelScalingRequest,
	accType string,
	targetGPUs int,
	freeThisCycle *int,
) []domain.VariantDecision {
	anchor := bindingAnchor(req.AnalyzerResults)
	// applyRescale drops anchor-less requests before grouping, so this is
	// unreachable today. Guard it anyway, as every other bindingAnchor call site
	// does: the anchor is now derived per call rather than read from a stored
	// field, and it returns nil for a stale, non-informative, or ambiguously bound
	// ballot — a much wider set of inputs than the old "entry absent" case. A
	// dereference here would panic the optimize goroutine.
	if anchor == nil {
		return nil
	}
	stateMap := buildStateMap(req.VariantStates)
	vcMap := buildCapacityMap(anchor.VariantCapacities)
	targets := initTargets(req.VariantStates)

	// Split the model's GPU target across its roles by role demand (a P/D model
	// must keep both roles served), then reclaim/fill each role toward its share.
	// A non-disaggregated model has the single synthetic "both" role.
	roles := modelRolesOnType(anchor.VariantCapacities, accType)
	curByRole := make(map[string]int, len(roles))
	demByRole := make(map[string]int, len(roles))
	floorByRole := make(map[string]int, len(roles))
	for _, role := range roles {
		curByRole[role] = roleCurrentGPUs(req, accType, role)
		demByRole[role] = roleDemandGPUs(anchor, stateMap, accType, role)
		floorByRole[role] = roleFloorGPUs(req, accType, role)
	}
	tgtByRole := distributeGPUsByWeight(targetGPUs, roles, demByRole, curByRole, floorByRole)

	for _, role := range roles {
		rt, rc := tgtByRole[role], curByRole[role]
		switch {
		case rt < rc:
			// Combine (RC/SC) math (scale-down score-weighted tie-break) consumes only
			// the voting subset of the ballot; the anchor supplies the variant topology.
			reclaimRole(ctx, votingResults(req.AnalyzerResults), anchor.VariantCapacities, role, stateMap, targets, rc-rt)
		case rt > rc:
			want := rt - rc
			if want > *freeThisCycle {
				want = *freeThisCycle
			}
			*freeThisCycle -= fillRole(anchor.VariantCapacities, role, stateMap, targets, want)
		}
	}

	decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "rescale")
	for i := range decisions {
		if decisions[i].Action == domain.ActionScaleDown {
			decisions[i].SetDecisionReason(domain.ActionScaleDown, domain.DecisionReasonRescale,
				string(domain.DecisionReasonRescale)+" (optimizer: rescale, reclaim)")
		}
	}
	return decisions
}

// reclaimRole sheds up to deltaGPUs worth of a role's replicas, most-expensive-first,
// respecting minReplicas and the cheapest-at-1 protection, via scaleDownVariantSet.
func reclaimRole(
	ctx context.Context,
	s []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	role string,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
	deltaGPUs int,
) {
	remaining := deltaGPUs
	sorted := sortVariantsForScaleDown(s, variantsForRole(variants, role))
	scaleDownVariantSet(ctx, sorted, targets, stateMap,
		func(vc domain.VariantCapacity) int {
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			if remaining <= 0 || g <= 0 {
				return 0
			}
			return remaining / g // whole replicas that fit in the remaining GPU delta
		},
		func(vc domain.VariantCapacity, n int) {
			remaining -= n * gpusPerReplicaFromState(stateMap, vc.VariantName)
		},
	)
}

// fillRole adds up to wantGPUs worth of a role's replicas, most-cost-efficient-first,
// respecting maxReplicas. Returns the GPUs actually consumed.
func fillRole(
	variants []domain.VariantCapacity,
	role string,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
	wantGPUs int,
) int {
	spent := 0
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variants, role)) {
		if wantGPUs-spent <= 0 {
			break
		}
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		g := gpusPerReplicaFromState(stateMap, vc.VariantName)
		if g <= 0 {
			continue
		}
		st := stateMap[vc.VariantName]
		for wantGPUs-spent >= g {
			if st.MaxReplicas != nil && *st.MaxReplicas > 0 && targets[vc.VariantName] >= *st.MaxReplicas {
				break
			}
			targets[vc.VariantName]++
			spent += g
		}
	}
	return spent
}

// singleAccType returns the accelerator type shared by all variants, or false if
// they span more than one (multi-accelerator, deferred) or none is set.
func singleAccType(vcs []domain.VariantCapacity) (string, bool) {
	accType := ""
	for _, vc := range vcs {
		if vc.AcceleratorName == "" {
			continue
		}
		if accType == "" {
			accType = vc.AcceleratorName
		} else if accType != vc.AcceleratorName {
			return "", false
		}
	}
	return accType, accType != ""
}

// modelCurrentGPUs sums CurrentReplicas x GPUsPerReplica over the model's variants
// on accType.
func modelCurrentGPUs(req ModelScalingRequest, accType string) int {
	anchor := bindingAnchor(req.AnalyzerResults)
	if anchor == nil {
		return 0
	}
	stateMap := buildStateMap(req.VariantStates)
	total := 0
	for _, vc := range anchor.VariantCapacities {
		if vc.AcceleratorName != accType {
			continue
		}
		total += stateMap[vc.VariantName].CurrentReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// rescaleInputsForGroup builds the water-filling inputs for a group's models and
// returns them with the summed demand-in-GPUs (for the contention check).
func rescaleInputsForGroup(reqs []ModelScalingRequest, accType string, budget int) ([]rescaleInput, int) {
	inputs := make([]rescaleInput, 0, len(reqs))
	sumDemandGPUs := 0
	for _, req := range reqs {
		anchor := bindingAnchor(req.AnalyzerResults)
		if anchor == nil {
			continue
		}
		stateMap := buildStateMap(req.VariantStates)

		floorGPUs, maxGPUs, maxBounded := 0, 0, true
		for _, vc := range anchor.VariantCapacities {
			if vc.AcceleratorName != accType {
				continue
			}
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			st := stateMap[vc.VariantName]
			if st.MinReplicas != nil && *st.MinReplicas > 0 {
				floorGPUs += *st.MinReplicas * g
			}
			if st.MaxReplicas != nil && *st.MaxReplicas > 0 {
				maxGPUs += *st.MaxReplicas * g
			} else {
				maxBounded = false
			}
		}

		demandGPUs := modelDemandGPUs(anchor, stateMap, accType)
		capGPUs := demandGPUs
		if maxBounded {
			capGPUs = min(capGPUs, maxGPUs)
		} else {
			capGPUs = min(capGPUs, budget) // unbounded max: cap at the group budget
		}
		capGPUs = max(capGPUs, floorGPUs)

		inputs = append(inputs, rescaleInput{
			ID:        modelKey(req),
			Priority:  req.Priority,
			Demand:    anchor.TotalDemand,
			FloorGPUs: floorGPUs,
			CapGPUs:   capGPUs,
		})
		sumDemandGPUs += demandGPUs
	}
	return inputs, sumDemandGPUs
}

// modelDemandGPUs is the model's demand-in-GPUs summed across its roles on accType
// (a P/D model needs GPUs for both prefill and decode).
func modelDemandGPUs(anchor *domain.AnalyzerResult, stateMap map[string]domain.VariantReplicaState, accType string) int {
	total := 0
	for _, role := range modelRolesOnType(anchor.VariantCapacities, accType) {
		total += roleDemandGPUs(anchor, stateMap, accType, role)
	}
	return total
}

// roleDemandGPUs converts a role's token demand to a GPU count via the role's most
// cost-efficient variant's per-replica capacity. The synthetic "both" role uses the
// model-level TotalDemand; a P/D role uses its RoleCapacities demand.
func roleDemandGPUs(anchor *domain.AnalyzerResult, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	demand := anchor.TotalDemand
	if role != domain.RoleBoth {
		if rc, ok := anchor.RoleCapacities[role]; ok {
			demand = rc.TotalDemand
		}
	}
	best := 0.0
	bestGPUs := 1
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variantsOnType(anchor.VariantCapacities, accType), role)) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		best = vc.PerReplicaCapacity
		bestGPUs = gpusPerReplicaFromState(stateMap, vc.VariantName)
		break
	}
	if best <= 0 {
		return 0
	}
	replicas := int(math.Ceil(demand / best))
	if replicas < 0 {
		replicas = 0
	}
	return replicas * bestGPUs
}

// variantsOnType filters variants to those on accType.
func variantsOnType(vcs []domain.VariantCapacity, accType string) []domain.VariantCapacity {
	out := make([]domain.VariantCapacity, 0, len(vcs))
	for _, vc := range vcs {
		if vc.AcceleratorName == accType {
			out = append(out, vc)
		}
	}
	return out
}

// modelRolesOnType returns the distinct roles among a model's variants on accType,
// sorted for determinism. A variant with no role is the synthetic "both".
func modelRolesOnType(vcs []domain.VariantCapacity, accType string) []string {
	return rolesOf(variantsOnType(vcs, accType))
}

// roleCurrentGPUs sums CurrentReplicas x GPUsPerReplica over a role's variants on accType.
func roleCurrentGPUs(req ModelScalingRequest, accType, role string) int {
	anchor := bindingAnchor(req.AnalyzerResults)
	if anchor == nil {
		return 0
	}
	stateMap := buildStateMap(req.VariantStates)
	total := 0
	for _, vc := range variantsForRole(variantsOnType(anchor.VariantCapacities, accType), role) {
		total += stateMap[vc.VariantName].CurrentReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// roleFloorGPUs sums minReplicas x GPUsPerReplica over a role's variants on accType —
// the GPUs that must stay allocated to the role regardless of the weighted split.
func roleFloorGPUs(req ModelScalingRequest, accType, role string) int {
	anchor := bindingAnchor(req.AnalyzerResults)
	if anchor == nil {
		return 0
	}
	stateMap := buildStateMap(req.VariantStates)
	total := 0
	for _, vc := range variantsForRole(variantsOnType(anchor.VariantCapacities, accType), role) {
		st := stateMap[vc.VariantName]
		if st.MinReplicas != nil && *st.MinReplicas > 0 {
			total += *st.MinReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
		}
	}
	return total
}

// distributeGPUsByWeight splits total GPUs across roles: each role is first reserved
// its floor (minReplicas GPUs), then the remainder is split proportional to weight
// (falling back to `fallback` when all weights are zero), rounded by largest remainder
// for determinism. Reserving floors first keeps a role's minReplicas from being
// undercut by the split — e.g. a cold-start P/D model with zero demand and zero current
// on both roles must still keep each role's floor, not dump everything on one role. The
// caller reserves the model-level floor before splitting, so total >= sum of role floors.
func distributeGPUsByWeight(total int, roles []string, weight, fallback, floor map[string]int) map[string]int {
	out := make(map[string]int, len(roles))
	if len(roles) <= 1 {
		if len(roles) == 1 {
			out[roles[0]] = total
		}
		return out
	}

	// Reserve each role's own floor up front.
	sumFloor := 0
	for _, r := range roles {
		out[r] = floor[r]
		sumFloor += floor[r]
	}
	remaining := total - sumFloor
	if remaining <= 0 {
		return out // total only covers the floors; each role keeps exactly its floor
	}

	w, sum := weight, 0
	for _, r := range roles {
		sum += w[r]
	}
	if sum == 0 {
		w, sum = fallback, 0
		for _, r := range roles {
			sum += w[r]
		}
	}
	if sum == 0 {
		out[roles[0]] += remaining // nothing to weight by: deterministic fallback
		return out
	}
	remainder := make(map[string]float64, len(roles))
	assigned := 0
	for _, r := range roles {
		exact := float64(remaining) * float64(w[r]) / float64(sum)
		whole := int(math.Floor(exact))
		out[r] += whole
		assigned += whole
		remainder[r] = exact - float64(whole)
	}
	apportionLeftover(out, remainder, remaining-assigned, nil)
	return out
}
