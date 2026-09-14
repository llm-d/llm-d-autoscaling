package pipeline

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// rolesOf returns the distinct roles among the given variants, sorted for
// determinism. A variant with no role is the synthetic RoleBoth.
func rolesOf(vcs []domain.VariantCapacity) []string {
	set := make(map[string]struct{}, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		set[r] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// Sentinel VariantCapacity.Reason values that indicate a variant carries no
// usable capacity signal (see domain.VariantCapacity.Reason doc). Analyzers
// that skip a variant entirely on failure (e.g. throughput's ITL-model
// resolution) never emit these — the variant is simply absent from
// VariantCapacities, which ResultIsInformative also treats as uninformative.
//
// These are the single source of truth for the no-data/error sentinels:
// producer packages (e.g. saturation_v2) reference them rather than
// re-declaring the literals, so ResultIsInformative and the producers cannot
// drift apart.
const (
	// ReasonNoData marks a variant for which the analyzer had no usable input
	// (no live replicas and no store record).
	ReasonNoData = "no-data"
	// ReasonError marks a variant whose capacity could not be resolved due to
	// an internal analyzer error.
	ReasonError = "error"
)

// ResultIsInformative reports whether nr carries a usable capacity signal:
// a non-nil Result with at least one VariantCapacity whose Reason is not a
// no-data/error sentinel. Used by the engine to decide whether to refresh
// the analyzer's last-good-analysis timestamp for the liveness gate.
func ResultIsInformative(nr NamedAnalyzerResult) bool {
	if nr.Result == nil {
		return false
	}
	for _, vc := range nr.Result.VariantCapacities {
		if vc.Reason != ReasonNoData && vc.Reason != ReasonError {
			return true
		}
	}
	return false
}

// applyAllocation subtracts the capacity provided by n replicas of variant v
// from each analyzer's Remaining counter. Clamps to 0. The slice is the working
// allocation state; Result.RequiredCapacity is never mutated.
//
// Contract: Remaining/Spare are engine-calibrated on entry (via the universal
// threshold post-step). Helpers do not read or mutate PendingReplicas.
func applyAllocation(s []NamedAnalyzerResult, v string, n int) {
	for i := range s {
		if s[i].Result == nil {
			continue
		}
		prc := prcForVariant(s[i].Result, v)
		if prc <= 0 {
			continue
		}
		s[i].Remaining -= float64(n) * prc
		if s[i].Remaining < 0 {
			s[i].Remaining = 0
		}
	}
}

// bindingAnchor derives the per-model anchor on demand from the ballot s. The
// anchor is the topology carrier the optimizer selects variants and accounts
// GPUs against. It merges identity/(a) fields from the saturation entry —
// per variant: AcceleratorName, Cost, Role, ReplicaCount, PendingReplicas; at
// the model level: ModelID, Namespace, AnalyzedAt — with sizing/(b) fields from
// the binding analyzer — per variant: PerReplicaCapacity, Reason, TotalDemand,
// Utilization; at the model level: TotalSupply, TotalDemand, Utilization,
// TotalAnticipatedSupply, RequiredCapacity, SpareCapacity, RoleCapacities —
// keyed by VariantName. TotalCapacity is recomputed, not copied. Returns nil
// when nothing can bind; the optimizer then holds for this model (the nil-guard
// at each call site is that per-model hold).
//
// The binding analyzer (the (b)/sizing source) is:
//   - saturation, when it votes and is live+informative (the default and the
//     saturation+throughput case — merging saturation with itself is the
//     identity, which is why the characterization goldens hold);
//   - otherwise the sole enabled+live+informative non-saturation entry (the
//     throughput-only case);
//   - otherwise none → return nil.
//
// If more than one non-saturation analyzer is enabled+live+informative, this PR
// does not define which binds; rather than guess, the model is treated as
// unbindable and nil is returned.
//
// Per-variant (b)-fallback: where the binding analyzer omits a variant the (a)
// carrier lists, the (demand, PerReplicaCapacity) pair must come from a single
// source. Saturation's own (b) is the fallback ONLY when saturation votes
// (satNR.Enabled) — saturation is then both the demand and the PRC source, so
// the pair stays consistent. Under a throughput-only config (saturation present
// as the (a) carrier but not voting) a variant the binding analyzer omits gets
// PerReplicaCapacity = 0 and is not proactively selectable; genuine cold-starts
// fall to the reactive scale-from-zero engine. The (a) carrier is captured up
// front, before any voting-set prune, so the fallback source is available even
// when saturation is non-voting.
//
// Builds fresh literals throughout — it never mutates the source Results or
// their VariantCapacities slices/elements.
func bindingAnchor(s []NamedAnalyzerResult) *domain.AnalyzerResult {
	// (a) carrier: the saturation entry. It may be present even when it does not
	// vote (throughput-only config), so it is located by name, not by vote.
	var satNR *NamedAnalyzerResult
	for i := range s {
		if s[i].Name == domain.SaturationAnalyzerName && s[i].Result != nil {
			satNR = &s[i]
			break
		}
	}

	// Select the binding (b)/sizing analyzer.
	var binding *NamedAnalyzerResult
	switch {
	case satNR != nil && satNR.Enabled && satNR.Live && ResultIsInformative(*satNR):
		// Saturation binds whenever it votes (default / saturation+throughput).
		binding = satNR
	default:
		// Otherwise the sole enabled+live+informative non-saturation entry binds.
		for i := range s {
			if s[i].Name == domain.SaturationAnalyzerName {
				continue
			}
			if s[i].Enabled && s[i].Live && ResultIsInformative(s[i]) {
				if binding != nil {
					// >1 non-saturation binding candidate is not a config this PR
					// defines; do not guess which binds — hold this model instead.
					return nil
				}
				binding = &s[i]
			}
		}
	}
	if binding == nil {
		return nil
	}

	// Identity/(a) carrier: saturation when present; with no saturation entry at
	// all (not a config this PR defines) fall back to binding so the merge stays
	// well-defined.
	aCarrier := binding
	if satNR != nil {
		aCarrier = satNR
	}
	// Whether saturation votes — gates the per-variant (b)-fallback below.
	satEnabled := satNR != nil && satNR.Enabled

	// Model-level fields: identity from the (a) carrier, sizing from binding.
	anchor := &domain.AnalyzerResult{
		AnalyzerName:           binding.Result.AnalyzerName,
		ModelID:                aCarrier.Result.ModelID,
		Namespace:              aCarrier.Result.Namespace,
		AnalyzedAt:             aCarrier.Result.AnalyzedAt,
		TotalSupply:            binding.Result.TotalSupply,
		TotalDemand:            binding.Result.TotalDemand,
		Utilization:            binding.Result.Utilization,
		TotalAnticipatedSupply: binding.Result.TotalAnticipatedSupply,
		RequiredCapacity:       binding.Result.RequiredCapacity,
		SpareCapacity:          binding.Result.SpareCapacity,
		RoleCapacities:         binding.Result.RoleCapacities,
	}

	// Per-variant merge: iterate the (a) carrier's complete variant list (it emits
	// every configured variant), take (a) from it and (b) from the binding
	// analyzer for the same VariantName.
	bByName := buildCapacityMap(binding.Result.VariantCapacities)
	merged := make([]domain.VariantCapacity, 0, len(aCarrier.Result.VariantCapacities))
	for _, a := range aCarrier.Result.VariantCapacities {
		out := domain.VariantCapacity{
			VariantName:     a.VariantName,
			AcceleratorName: a.AcceleratorName,
			Cost:            a.Cost,
			Role:            a.Role,
			ReplicaCount:    a.ReplicaCount,
			PendingReplicas: a.PendingReplicas,
		}
		if b, ok := bByName[a.VariantName]; ok {
			out.PerReplicaCapacity = b.PerReplicaCapacity
			out.Reason = b.Reason
			out.TotalDemand = b.TotalDemand
			out.Utilization = b.Utilization
		} else if satEnabled {
			// Enablement-gated fallback: saturation votes, so its own (b) is a
			// consistent (demand, PRC) source. aCarrier is saturation here.
			out.PerReplicaCapacity = a.PerReplicaCapacity
			out.Reason = a.Reason
			out.TotalDemand = a.TotalDemand
			out.Utilization = a.Utilization
		}
		// else: throughput-only, binding analyzer omits this variant, no persisted
		// throughput PRC → PerReplicaCapacity stays 0 → not proactively selectable.

		// TotalCapacity is recomputed (not copied) so the invariant
		// TotalCapacity == ReplicaCount × PerReplicaCapacity holds by construction.
		out.TotalCapacity = float64(out.ReplicaCount) * out.PerReplicaCapacity
		merged = append(merged, out)
	}
	anchor.VariantCapacities = merged
	return anchor
}

// votingResults returns the sub-slice of the ballot whose analyzers vote in the
// combine (RC/SC) math this cycle. Non-voting entries (e.g. a saturation entry
// present only as the (a) carrier in a throughput-only config) are excluded.
// The anchor build (bindingAnchor) reads the FULL ballot, not this pruned view.
// In the default and saturation+throughput configs every entry is Enabled, so
// this returns the same combine input set as the raw ballot.
func votingResults(s []NamedAnalyzerResult) []NamedAnalyzerResult {
	out := make([]NamedAnalyzerResult, 0, len(s))
	for _, e := range s {
		if e.Enabled {
			out = append(out, e)
		}
	}
	return out
}

// prcForVariant returns the PerReplicaCapacity for variant v in result r.
// Returns 0 if the variant is not present.
func prcForVariant(r *domain.AnalyzerResult, v string) float64 {
	for _, vc := range r.VariantCapacities {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// =============================================================================
// Paired helpers — disaggregated (P/D) models
// =============================================================================

// initRoleState initialises picker-local role state for one model's allocation pass.
// It unifies disaggregated and non-disaggregated models into one (model, role) view:
//
//   - Disaggregated (RoleCapacities != nil): roles = sorted keys of RoleCapacities;
//     per-role RC → pickerState[i][role]; per-role SC → s[i].RoleSpare[role].
//   - Non-disaggregated (RoleCapacities == nil): one synthetic role "both" using
//     the engine-calibrated model-level RC/SC (Result.RequiredCapacity / SpareCapacity).
//     No re-aggregation — the engine already summed all variants into those scalars.
//
// Returns the list of active roles and the picker-local RolePairedState.
// Remaining/Spare scalars on NamedAnalyzerResult are read-only after this call;
// all dynamic bookkeeping moves to pickerState (scale-up) and RoleSpare (scale-down).
func initRoleState(s []NamedAnalyzerResult) (roles []string, pickerState RolePairedState) {
	pickerState = make(RolePairedState, len(s))
	roleSet := make(map[string]struct{})

	for i, e := range s {
		pickerState[i] = make(map[string]float64)
		if e.Result == nil {
			continue
		}
		if e.Result.RoleCapacities != nil {
			// Disaggregated: per-role RC/SC from engine-calibrated RoleCapacities.
			if s[i].RoleSpare == nil {
				s[i].RoleSpare = make(map[string]float64, len(e.Result.RoleCapacities))
			}
			for role, rc := range e.Result.RoleCapacities {
				pickerState[i][role] = rc.RequiredCapacity
				s[i].RoleSpare[role] = rc.SpareCapacity
				roleSet[role] = struct{}{}
			}
		} else {
			// Non-disaggregated: synthesize a single "both" role from model-level scalars.
			pickerState[i][domain.RoleBoth] = e.Remaining
			if s[i].RoleSpare == nil {
				s[i].RoleSpare = make(map[string]float64, 1)
			}
			s[i].RoleSpare[domain.RoleBoth] = e.Spare
			roleSet[domain.RoleBoth] = struct{}{}
		}
	}

	roles = make([]string, 0, len(roleSet))
	for role := range roleSet {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles, pickerState
}

// =============================================================================
// Paired helpers — role-generic scale-up and scale-down
// =============================================================================
//
// Design § Architecture/D: (model, role) is the unit of allocation math.
// Per-role sizing is independent, scoped to each role's picker-local demand.
// The joint-commit step bounds by the min-util role (the coupling constraint).
//
// RolePairedState holds picker-local per-role demand tracked during one
// model's allocation pass. Indexed as [analyzer-index][role] → remaining demand
// (in that role's own capacity units). Initialized from RoleCapacities[role].RC;
// decremented per joint commit. Lives only inside the allocation loop — not
// stored on NamedAnalyzerResult (per design A10).
type RolePairedState []map[string]float64

// roleBottleneckReplicas computes the cross-analyzer bottleneck replica count
// for variant v in a specific role. Returns max_i ceil(state[i][role] / PRC_i[v]).
func roleBottleneckReplicas(s []NamedAnalyzerResult, state RolePairedState, role, v string) int {
	max := 0
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		prc := prcForVariant(e.Result, v)
		if prc <= 0 {
			continue
		}
		n := int(math.Ceil(state[i][role] / prc))
		if n > max {
			max = n
		}
	}
	return max
}

// roleAggRemaining returns max cross-analyzer remaining demand for role.
func roleAggRemaining(s []NamedAnalyzerResult, state RolePairedState, role string) float64 {
	max := 0.0
	for i := range s {
		if d := state[i][role]; d > max {
			max = d
		}
	}
	return max
}

// anyRoleNeedsScaleUp is the per-role scale-up gate for the unified dispatcher.
// Returns true when any role has aggregate remaining demand > 0.
func anyRoleNeedsScaleUp(state RolePairedState, roles []string) bool {
	for _, role := range roles {
		for _, m := range state {
			if m[role] > 0 {
				return true
			}
		}
	}
	return false
}

// variantsForRole returns the capacities whose role matches role exactly,
// canonicalizing an empty Role to domain.RoleBoth.
func variantsForRole(vcs []domain.VariantCapacity, role string) []domain.VariantCapacity {
	out := make([]domain.VariantCapacity, 0, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		if r == role {
			out = append(out, vc)
		}
	}
	return out
}

// safeRemovalReplicasForRole returns the number of replicas of variant v that
// can safely be removed — the minimum of floor(RoleSpare[role]_i / PRC_i[v])
// across live analyzers that have variant v and a non-zero PRC. Non-live
// analyzers (no metrics, error state, never analyzed, or stale) are skipped
// and do not constrain the minimum. Returns 0 if any contributing analyzer
// has RoleSpare[role] ≤ 0 or RoleSpare is nil.
func safeRemovalReplicasForRole(s []NamedAnalyzerResult, v, role string) int {
	smallest := math.MaxInt
	found := false
	for _, e := range s {
		if !e.Live {
			continue // non-live analyzers do not constrain the safe-removal minimum
		}
		if e.Result == nil || e.RoleSpare == nil {
			continue
		}
		prc := prcForVariant(e.Result, v)
		if prc <= 0 {
			continue
		}
		n := int(math.Floor(e.RoleSpare[role] / prc))
		if n < smallest {
			smallest = n
		}
		found = true
	}
	if !found || smallest < 0 {
		return 0
	}
	return smallest
}

// applyDeallocationForRole decrements each analyzer's RoleSpare[role] by
// n × PRC_i[v]. Clamps to 0. Never mutates Result.
// Intentionally not Live-gated: non-live entries are already excluded from
// the veto (needsScaleDownForRole) and the safe-removal minimum
// (safeRemovalReplicasForRole), so mutating their RoleSpare here is harmless
// — nothing reads it back.
func applyDeallocationForRole(s []NamedAnalyzerResult, v, role string, n int) {
	for i := range s {
		if s[i].Result == nil || s[i].RoleSpare == nil {
			continue
		}
		prc := prcForVariant(s[i].Result, v)
		if prc <= 0 {
			continue
		}
		s[i].RoleSpare[role] -= float64(n) * prc
		if s[i].RoleSpare[role] < 0 {
			s[i].RoleSpare[role] = 0
		}
	}
}

// needsScaleDownForRole reports whether every live analyzer agrees this role
// has spare capacity (all-down gate, scoped to one role). Non-live analyzers
// (no metrics, error state, never analyzed, or stale) do not veto — this
// applies uniformly, including saturation's token-capacity result; there is
// no name-based exemption. Returns false if any live analyzer's
// RoleSpare[role] ≤ 0 or RoleSpare is nil. Safety floor: if no live analyzer
// remains, there is no current basis to scale down, so this returns false.
func needsScaleDownForRole(s []NamedAnalyzerResult, role string) bool {
	liveCount := 0
	for _, e := range s {
		if !e.Live {
			continue // non-live analyzers do not veto (no metrics / error / never analyzed)
		}
		if e.Result == nil || e.RoleSpare == nil || e.RoleSpare[role] <= 0 {
			return false
		}
		liveCount++
	}
	return liveCount > 0
}

// RolePickFn is the role-generic optimizer variant selector for the unified
// allocateForModelPaired loop. Called once per role per iteration; returns the
// chosen variant and its resource cap. Returning ("", 0) signals no variant
// is available for this role.
type RolePickFn func(
	role string,
	s []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
) (variant string, capN int)

// allocateForModelPaired is the Phase-3 role-generic scale-up loop.
// Handles any set of roles (including the arity-1 "both" single-role case).
// Per iteration: pick one variant per role, size independently, compute
// Δ_util = min_role util_role, trim to matched joint commit.
// Arity-1 (roles = ["both"]) reduces to plain per-variant allocation.
func allocateForModelPaired(
	ctx context.Context,
	s []NamedAnalyzerResult,
	variants []domain.VariantCapacity,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
	pick RolePickFn,
	pickerState RolePairedState,
	roles []string,
) {
	logger := ctrl.LoggerFrom(ctx)
	for anyRoleNeedsScaleUp(pickerState, roles) {
		variantByRole := make(map[string]string, len(roles))
		capByRole := make(map[string]int, len(roles))
		prcByRole := make(map[string]float64, len(roles))
		allPicked := true
		for _, role := range roles {
			v, capN := pick(role, s, variants, stateMap, available, targets)
			if v == "" {
				allPicked = false
				break
			}
			variantByRole[role] = v
			capByRole[role] = capN
			prcByRole[role] = prcFromVCs(variants, v)
		}
		if !allPicked {
			break
		}

		nByRole := make(map[string]int, len(roles))
		utilByRole := make(map[string]float64, len(roles))
		for _, role := range roles {
			prc := prcByRole[role]
			n := min(roleBottleneckReplicas(s, pickerState, role, variantByRole[role]), capByRole[role])
			nByRole[role] = n
			demand := roleAggRemaining(s, pickerState, role)
			if demand <= 0 {
				utilByRole[role] = 1.0
			} else {
				utilByRole[role] = float64(n) * prc / demand
			}
		}

		deltaUtil := math.MaxFloat64
		for _, role := range roles {
			if utilByRole[role] < deltaUtil {
				deltaUtil = utilByRole[role]
			}
		}
		if deltaUtil <= 0 {
			break
		}

		kByRole := make(map[string]int, len(roles))
		anyPositive := false
		for _, role := range roles {
			demand := roleAggRemaining(s, pickerState, role)
			prc := prcByRole[role]
			n := nByRole[role]
			k := 0
			if prc > 0 && demand > 0 {
				k = max(int(math.Floor(deltaUtil*demand/prc)), min(1, n))
			}
			kByRole[role] = k
			if k > 0 {
				anyPositive = true
			}
		}
		if !anyPositive {
			break
		}

		for _, role := range roles {
			v := variantByRole[role]
			k := kByRole[role]
			prc := prcByRole[role]
			targets[v] += k
			for i := range pickerState {
				pickerState[i][role] = math.Max(0, pickerState[i][role]-float64(k)*prc)
			}
			if available != nil {
				available[accFromVCs(variants, v)] -= k * gpusPerReplicaFromState(stateMap, v)
			}
		}
		// Update model-level Remaining via the P-anchor role so fairShareValue
		// reflects committed capacity. For "both" (non-disaggregated) use the
		// single role; for P/D prefer "prefill".
		for _, anchor := range []string{"prefill", domain.RoleBoth} {
			if v, ok := variantByRole[anchor]; ok {
				applyAllocation(s, v, kByRole[anchor])
				break
			}
		}
		logger.V(logging.DEBUG).Info("scale-up: joint role commit", "deltaUtil", deltaUtil)
	}
}
