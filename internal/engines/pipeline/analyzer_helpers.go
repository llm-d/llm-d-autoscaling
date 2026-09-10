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

// ReasonFromZeroAdmission marks a variant the anchor admitted on the from-zero
// sentinel: it holds no replicas and no analyzer this anchor sizes from prices
// it, so there is no measurement to build a capacity out of. That is a claim
// about the ballot, not about the world -- the variant may well have been
// measured before and the record since lapsed (the throughput analyzer evicts
// per-variant state on an idle window), or be measured right now by an analyzer
// whose sizing the merge below deliberately does not borrow. Its
// PerReplicaCapacity is a declared minimum in the binder's own currency, not a
// measurement, and the one-replica ceiling in maxTargetReplicas keys on this tag.
// That ceiling is dormant -- see DEFERRED below before reading any of this as
// something the running system does.
//
// Deliberately NOT a member of the no-data/error family above. Those mean "no
// usable signal"; this one means the opposite — the anchor is asserting that the
// variant may be tried. Adding it to ResultIsInformative's set would be a
// category error, and ResultIsInformative never sees it in any case: it runs on
// ballot entries, and this tag is written only on the anchor the ballot builds.
//
// DEFERRED: nothing writes this tag yet. The ceiling below and its three grant
// sites are complete, but the admitting write site in bindingAnchor is held: an
// anchor-only sentinel makes a variant *selectable* without making it *sizable*.
// Selection reads the anchor; the replica count comes from the ballot, via
// votesFromPickerState -> combineVotes -> roleBottleneckReplicas, which abstains
// for a variant no voting entry prices and so yields 0. allocateForModelPaired
// then computes deltaUtil == 0 and breaks, costing the model every variant behind
// this one -- a regression, not a missed feature. Compare the previously-live
// variant in optimizer_scale_from_zero_test.go, which works precisely because the
// throughput analyzer emits its PRC into the *ballot* rather than the anchor.
// Whether the sentinel may enter the voting set at all is a question about the
// combine's admission rules, and is not settled here.
const ReasonFromZeroAdmission = "from-zero-admission"

// ReasonRoleUnmodeled marks a RoleCapacity whose TotalDemand/RequiredCapacity/
// SpareCapacity is structurally, not measurably, zero: the analyzer has no
// demand model for this role at all, so the map key was never written rather
// than computed and landing on zero (e.g. throughput's distributeDemandByRole
// excludes prefill by construction). A ballot function seeing this Reason
// abstains rather than voting the value, the same idiom ReasonNoData/
// ReasonFromZeroAdmission already use one level down on VariantCapacity.
// Analyzer-owned pipeline vocabulary — see satReasonNoData =
// pipeline.ReasonNoData (saturation_v2/types.go) for the existing
// cross-package alias pattern this follows.
const ReasonRoleUnmodeled = "role-unmodeled"

// admissionCeilingReplicas is how many replicas a variant admitted on the
// from-zero sentinel may hold. One bite, then measure: the sentinel does not
// price the variant's capacity, so the spend is what has to be bounded instead.
// Once a real measurement arrives the tag goes with it and the ceiling lifts.
const admissionCeilingReplicas = 1

// maxTargetReplicas reports the ceiling on vc's target, in replicas, and whether
// any ceiling applies. It merges the two sources: the variant's configured
// MaxReplicas, and the from-zero admission ceiling.
//
// The second is why this is a function rather than the MaxReplicas check written
// out at each grant site. Two of the three sites treat "no MaxReplicas" as
// unbounded -- costGreedyRolePick returns math.MaxInt, fillRole's loop is bounded
// only inside the MaxReplicas condition -- so a ceiling folded into the existing
// headroom branch is skipped entirely on exactly the configurations that do not
// set MaxReplicas, which is most of them. That failure is silent: the variant is
// admitted, no gate objects, and it absorbs whatever the allocator had to spend.
//
// For a variant without the tag the result is the MaxReplicas check verbatim, so
// callers keep their existing behaviour byte for byte.
func maxTargetReplicas(vc domain.VariantCapacity, state domain.VariantReplicaState) (int, bool) {
	bound, bounded := 0, false
	if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
		bound, bounded = *state.MaxReplicas, true
	}
	if vc.Reason == ReasonFromZeroAdmission && (!bounded || admissionCeilingReplicas < bound) {
		bound, bounded = admissionCeilingReplicas, true
	}
	return bound, bounded
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
			// This entry cannot price v, so it abstains on v: it made no claim
			// against v and it is charged nothing for v. Debiting it at some
			// stand-in rate would bill an analyzer for capacity it never asked
			// for; leaving Remaining untouched is the abstention, not an
			// oversight.
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
// GPUs against. It merges identity fields from the saturation entry —
// per variant: AcceleratorName, Cost, Role, ReplicaCount, PendingReplicas; at
// the model level: ModelID, Namespace, AnalyzedAt — with sizing fields from
// the binding analyzer — per variant: PerReplicaCapacity, Reason, TotalDemand,
// Utilization; at the model level: TotalSupply, TotalDemand, Utilization,
// TotalAnticipatedSupply, RequiredCapacity, SpareCapacity, RoleCapacities —
// keyed by VariantName. TotalCapacity is recomputed, not copied. Returns nil
// when nothing can bind; the optimizer then holds for this model (the nil-guard
// at each call site is that per-model hold).
//
// The binding analyzer (the sizing source) is:
//   - saturation, when it votes and is live+informative (the default and the
//     saturation+throughput case — merging saturation with itself is the
//     identity, which is why the characterization goldens hold);
//   - otherwise the lowest-ballot-index enabled+live+informative non-saturation
//     entry (the throughput-only case, and the deterministic tie-break once
//     more than one non-saturation entry qualifies);
//   - otherwise none → return nil.
//
// Binder tie-break (deterministic, not a guess): saturation binds whenever it
// qualifies; otherwise the first (lowest ballot index) qualifying
// non-saturation entry binds. A later qualifying non-saturation entry still
// votes in the quantity combine (votingResults) but does not become the
// binder.
//
// Per-variant completeness: where the binding analyzer omits a variant the
// identity carrier lists, the variant keeps its identity and abstains on capacity
// — PerReplicaCapacity stays 0 and it is not proactively selectable, because its
// sizing must not be invented. That holds whether or not the variant is running.
// Proactively admitting the zero-replica case is deferred; see
// ReasonFromZeroAdmission for why an anchor-side sentinel alone does not achieve
// it.
//
// This does not fall back to saturation's own sizing for an omitted variant, even
// when saturation votes. Two independent reasons, and the first is structural:
// when saturation binds, the omitted case cannot arise at all — the sizing map is
// built from the binder's own capacities while the merge iterates the identity
// carrier's, and with saturation in both roles those are the same slice, so every
// lookup hits. A fallback could therefore only ever fire with saturation present
// as identity carrier but *not* binding — !(Enabled && Live && Informative) —
// which is precisely the condition under which saturation's own sizing is the
// least trustworthy thing to borrow: stale, no-data, or not even asked for.
// Second, borrowing it
// would mix metric scales across variants within one anchor: the binder's sized
// variants carry the binder's PRC scale, a borrowed one would carry saturation's.
// When the binder binds, every sized variant is the binder's — uniformly, no
// name-based exception.
//
// Builds fresh literals throughout — it never mutates the source Results or
// their VariantCapacities slices/elements.
func bindingAnchor(s []NamedAnalyzerResult) *domain.AnalyzerResult {
	// Identity carrier: the saturation entry. It may be present even when it
	// does not vote (throughput-only config), so it is located by name, not by
	// vote.
	var satNR *NamedAnalyzerResult
	for i := range s {
		if s[i].Name == domain.SaturationAnalyzerName && s[i].Result != nil {
			satNR = &s[i]
			break
		}
	}

	// Select the binding (sizing) analyzer.
	var binding *NamedAnalyzerResult
	switch {
	case satNR != nil && satNR.Enabled && satNR.Live && ResultIsInformative(*satNR):
		// Saturation binds whenever it votes (default / saturation+throughput).
		binding = satNR
	default:
		// Otherwise the lowest-ballot-index enabled+live+informative
		// non-saturation entry binds (deterministic tie-break). More than one
		// non-saturation entry can qualify here — votingResults caps neither
		// the count nor the kind — and when several do, a later one does not
		// overwrite the earlier: it votes without binding.
		for i := range s {
			if s[i].Name == domain.SaturationAnalyzerName {
				continue
			}
			if s[i].Enabled && s[i].Live && ResultIsInformative(s[i]) {
				if binding == nil {
					binding = &s[i]
				}
			}
		}
	}
	if binding == nil {
		return nil
	}

	// Identity carrier: saturation when present; with no saturation entry at
	// all (not a config this PR defines) fall back to binding so the merge stays
	// well-defined.
	aCarrier := binding
	if satNR != nil {
		aCarrier = satNR
	}

	// Model-level fields: identity from the identity carrier, sizing from binding.
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

	// Per-variant merge: iterate the identity carrier's complete variant list
	// (it emits every configured variant), take identity from it and sizing
	// from the binding analyzer for the same VariantName.
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
		}
		// else: the binder omits this variant, so it abstains -- PerReplicaCapacity
		// stays 0 -- uniformly, regardless of whether saturation votes. Its sizing
		// must not be fabricated. Reachable only when a saturation entry exists but
		// does not bind: when saturation binds it is both carrier and binder, so
		// bByName was built from the very slice being iterated and every lookup
		// above hits. Previously-live variants now at zero are usually covered by
		// the throughput analyzer's own scale-from-zero complement from persisted
		// supply, so what reaches this branch at zero replicas is a variant the
		// *binder* cannot price -- which is weaker than "never measured", since
		// that persisted supply expires on an idle window and saturation may hold a
		// stored capacity this merge deliberately does not borrow. Admitting it
		// anyway is the deferred work described at ReasonFromZeroAdmission.

		// TotalCapacity is recomputed (not copied) so the invariant
		// TotalCapacity == ReplicaCount × PerReplicaCapacity holds by construction.
		out.TotalCapacity = float64(out.ReplicaCount) * out.PerReplicaCapacity
		merged = append(merged, out)
	}
	anchor.VariantCapacities = merged
	return anchor
}

// votingResults returns the sub-slice of the ballot whose analyzers vote in the
// combine (RC/SC) math this cycle: Enabled AND Live (VG-up). Non-voting entries
// (e.g. a saturation entry present only as the identity carrier in a throughput-only
// config) are excluded; so is a stale Enabled entry that has gone dead, which
// would otherwise still seed initRoleState/roleBottleneckReplicas with a stale
// Result and force a spurious scale-up. Scale-down was already Live-gated at
// point of use (needsScaleDownForRole, safeRemovalReplicasForRole); this makes
// scale-up equally robust rather than relying on the external invariant "dead
// analyzer implies RC=0". Establishes the invariant non-nil anchor implies
// non-empty voting set: bindingAnchor's own binder gate (Enabled && Live &&
// Informative) is strictly stronger, so the binder itself always satisfies
// this prune; an empty voting set forces anchor == nil upstream, which is
// already the existing per-model hold path — there is no "empty voters,
// non-nil anchor" case to handle separately.
// The anchor build (bindingAnchor) reads the FULL ballot, not this pruned view
// — it needs a non-voting saturation's identity even when Live is false.
// In the default and saturation+throughput configs every entry is Enabled and
// Live, so this returns the same combine input set as the raw ballot.
func votingResults(s []NamedAnalyzerResult) []NamedAnalyzerResult {
	out := make([]NamedAnalyzerResult, 0, len(s))
	for _, e := range s {
		if e.Enabled && e.Live {
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

// =============================================================================
// The cross-analyzer combine — one core, three collectors
// =============================================================================

// replicaVote is one analyzer's opinion in a single (variant, role) combine,
// already converted to replica space. Value is real-valued — rounding happens
// once, at the caller, after the weighting.
type replicaVote struct {
	Index int     // ballot index — binder identity and deterministic tie-break
	Value float64 // replicas: demand/PRC (scale-up) or spare/PRC (scale-down)
	Score float64 // belief weight; > 0 (config coerces 0 -> 1.0)
}

// combineVotes reduces one (variant, role) ballot to a single real-valued
// replica count plus the ballot index of the binding analyzer. up=true takes
// the max (scale-up demand), up=false the min (scale-down safe removal).
//
// Higher-scored analyzers pull the result toward their own vote without it ever
// leaving [min, max]:
//
//	e  = max v_i (up) | min v_i (down)             -- the binder's own vote
//	v* = e - sum_i (e-v_i)*(s_i - s_e)+ / sum_j s_j   ((x)+ = max(x, 0))
//
// One expression serves both directions: for scale-down e is the min, so
// (e - v_i) <= 0 and the subtraction adds. Uniform scores zero every
// (s_i - s_e)+ term, collapsing v* to the plain extremum — which is what makes
// retrofitting the per-site loops onto this core behavior-preserving.
//
// Ties keep the lowest ballot index. Returns (0, -1) when no vote participates,
// which callers read as "no basis to act".
//
// Callers round once, after the weighting: ceil for scale-up, floor for
// scale-down. Rounding per element and then taking the extremum agrees only
// while all scores are uniform.
func combineVotes(votes []replicaVote, up bool) (float64, int) {
	if len(votes) == 0 {
		return 0, -1
	}

	b := 0
	for i := 1; i < len(votes); i++ {
		switch {
		case up && votes[i].Value > votes[b].Value:
			b = i
		case !up && votes[i].Value < votes[b].Value:
			b = i
		case votes[i].Value == votes[b].Value && votes[i].Index < votes[b].Index:
			b = i
		}
	}

	e := votes[b].Value
	sumScore := 0.0
	for _, vt := range votes {
		sumScore += vt.Score
	}
	if sumScore <= 0 {
		// No usable belief weight anywhere: take the plain extremum rather than
		// dividing by zero. Reachable only from hand-built ballots — the config
		// layer coerces a zero score to 1.0.
		return e, votes[b].Index
	}

	correction := 0.0
	for _, vt := range votes {
		excess := vt.Score - votes[b].Score
		if excess <= 0 {
			continue // no more trusted than the binder, so no pull
		}
		correction += (e - vt.Value) * excess
	}
	return e - correction/sumScore, votes[b].Index
}

// The collectors below are the only places that decide who participates in a
// combine. Keeping the filter here rather than in combineVotes is what makes
// sum_j s_j run over participating votes alone: an analyzer that says nothing
// about a (variant, role) cannot dilute the correction and so cannot make the
// system trust the binder more by staying silent.
//
// Every collector weighs its votes with voteScore, so an analyzer's configured
// score reaches the combine and nowhere else. All shipped configs leave it at
// the 1.0 default, which zeroes every correction term and yields the plain
// extremum.

// voteScore is the belief weight the combine gives one analyzer's vote. Zero or
// negative means "unset" and reads as the 1.0 default, matching the config
// layer's own coercion: an entry built without a score must not silently
// out-weigh or under-weigh a configured one, and a mixed ballot must never see a
// 0 in the (s_i - s_e)+ term.
func voteScore(e NamedAnalyzerResult) float64 {
	if e.Score > 0 {
		return e.Score
	}
	return 1.0
}

// votesFromPickerState collects the scale-up ballot for (role, variant) from
// picker-local remaining demand: state[i][role] / PRC_i[variant] replicas per
// participating entry. An entry whose RoleCapacity for role carries
// ReasonRoleUnmodeled abstains: it has no demand model for the role, so its
// value is structural, not a measurement of "nothing needed".
func votesFromPickerState(s []NamedAnalyzerResult, state RolePairedState, role, variant string) []replicaVote {
	out := make([]replicaVote, 0, len(s))
	for i, e := range s {
		if e.Result == nil || i >= len(state) || state[i] == nil {
			continue
		}
		if rc, ok := e.Result.RoleCapacities[role]; ok && rc.Reason == ReasonRoleUnmodeled {
			// This entry has no demand model for role at all -- its zero is
			// structural, not a measurement of "nothing needed" -- so it
			// abstains rather than voting 0 into the scale-up combine.
			continue
		}
		prc := prcForVariant(e.Result, variant)
		if prc <= 0 {
			// Cannot price this variant, so it abstains rather than voting zero:
			// a zero vote is an opinion that no replicas are needed, which is
			// the opposite of having no opinion. Casting no vote leaves the
			// bottleneck to the entries that can actually price the variant.
			continue
		}
		out = append(out, replicaVote{Index: i, Value: state[i][role] / prc, Score: voteScore(e)})
	}
	return out
}

// votesFromTotalDemand collects the rescale ballot for (role, variant) from
// each entry's own reported demand: the synthetic "both" role reads model-level
// TotalDemand, a P/D role reads that entry's own RoleCapacities demand. An
// entry that does not decompose the role contributes nothing, and one whose
// RoleCapacity for role carries ReasonRoleUnmodeled abstains for the same
// reason: it has no demand model for the role, so there is no real demand
// here to convert into replicas.
func votesFromTotalDemand(s []NamedAnalyzerResult, role, variant string) []replicaVote {
	out := make([]replicaVote, 0, len(s))
	for i, e := range s {
		if e.Result == nil {
			continue
		}
		demand := e.Result.TotalDemand
		if role != domain.RoleBoth {
			rc, ok := e.Result.RoleCapacities[role]
			if !ok {
				continue
			}
			if rc.Reason == ReasonRoleUnmodeled {
				// No demand model for role at all -- the zero is structural,
				// not a measurement, so it must not enter the rescale ballot.
				continue
			}
			demand = rc.TotalDemand
		}
		prc := prcForVariant(e.Result, variant)
		if prc <= 0 {
			// Cannot price this variant, so it abstains: its demand is real but
			// there is no factor to convert it into replicas of this variant,
			// and demand that cannot be priced must not inflate the model's
			// claim. See the participation filter in the developer guide.
			continue
		}
		out = append(out, replicaVote{Index: i, Value: demand / prc, Score: voteScore(e)})
	}
	return out
}

// votesFromRoleSpare collects the scale-down ballot for (role, variant) from
// each entry's per-role spare: RoleSpare[role] / PRC_i[variant] removable
// replicas per participating entry. An entry whose RoleCapacity for role
// carries ReasonRoleUnmodeled abstains: with no demand model for the role its
// reported spare is the whole fleet by construction, not a measurement of what
// is safe to remove.
//
// Live-gated — a non-live entry (no metrics, error state, never analyzed, or
// stale) does not constrain safe removal. An entry with no per-variant capacity
// for variant is dropped: with no conversion factor there is no removable count
// to offer. That drop is a magnitude-level silence only; the same entry can still
// object at role level through roleSpareVetoed, which is PRC-blind precisely
// because dropping it here would otherwise discard the objection.
//
// A live entry whose RoleSpare map exists but carries no key for role still
// votes, reading the map-miss as 0.0. This is the pre-existing shape, preserved
// deliberately: whether a role-level silence should abstain in the ballot too is
// a behavioral question, not part of hoisting the arithmetic into one place.
// Two things about that 0 are worth stating exactly, because neither is what it
// looks like. It does not hold removal at zero on its own — under dominance
// weighting a higher-scored voter reporting spare pulls the combined value
// positive, so a 0 vote is absolute only when nothing outscores it (which is the
// shipped uniform-score case). And it is not the role-level abstain: that is
// decided by needsScaleDownForRole and roleSpareVetoed, both of which read a
// map-miss as ABSTAIN and never as "spare == 0".
func votesFromRoleSpare(s []NamedAnalyzerResult, role, variant string) []replicaVote {
	out := make([]replicaVote, 0, len(s))
	for i, e := range s {
		if !e.Live {
			continue // non-live analyzers do not constrain the safe-removal minimum
		}
		if e.Result == nil || e.RoleSpare == nil {
			continue
		}
		if rc, ok := e.Result.RoleCapacities[role]; ok && rc.Reason == ReasonRoleUnmodeled {
			// No demand model for role at all, so its "spare" is the whole
			// fleet by construction, not a measurement. Left unguarded, this
			// entry would vote its entire fleet as removable simply because
			// it never priced this role's demand in the first place.
			continue
		}
		prc := prcForVariant(e.Result, variant)
		if prc <= 0 {
			// Cannot price this variant, so it abstains from the safe-removal
			// minimum. Abstaining is not the same as reporting no spare: an
			// entry that cannot price the variant has no view on how many of its
			// replicas are removable, and a zero here would veto every removal.
			continue
		}
		out = append(out, replicaVote{Index: i, Value: e.RoleSpare[role] / prc, Score: voteScore(e)})
	}
	return out
}

// roleBottleneckReplicas computes the cross-analyzer bottleneck replica count
// for variant v in a specific role: the combined scale-up vote over
// picker-local demand (votesFromPickerState, whose doc comment carries the
// participation rules), rounded up once. It does not run its own loop over
// analyzers — combineVotes is the only combine.
//
// Clamped at zero. Picker-local demand can overshoot negative on a joint
// commit, and a negative bottleneck is not a replica count.
func roleBottleneckReplicas(s []NamedAnalyzerResult, state RolePairedState, role, v string) int {
	value, binder := combineVotes(votesFromPickerState(s, state, role, v), true)
	if binder < 0 {
		return 0
	}
	if n := int(math.Ceil(value)); n > 0 {
		return n
	}
	return 0
}

// variantCapacityByName returns the VariantCapacity for v in vcs, and whether
// it was found. Linear scan, mirroring prcForVariant's style.
func variantCapacityByName(vcs []domain.VariantCapacity, v string) (domain.VariantCapacity, bool) {
	for _, vc := range vcs {
		if vc.VariantName == v {
			return vc, true
		}
	}
	return domain.VariantCapacity{}, false
}

// refreshAnchorSizing overwrites each entry in variants (the anchor's own
// VariantCapacities) with the voting entry currently binding it — per (role,
// variant), the binder combineVotes returns over the same votesFromPickerState
// ballot (same participation rules) that sizes the role. Identity fields
// (AcceleratorName, Cost, Role, replica counts) are untouched; only the
// sizing fields (PerReplicaCapacity, Reason, TotalDemand, Utilization) move,
// plus the recomputed TotalCapacity. Mutates variants in place — the anchor's
// own freshly-built slice, never the source ballot Results.
//
// No-op when len(s) <= 1: with a single voter the anchor's sizing already
// equals that voter's, so refreshing would reproduce the same values. The
// single-vote invariant ("populate once, never refresh") is upheld by not
// running this at all rather than running it to a no-op — see
// docs/developer-guide/multi-analyzer-pipeline.md, "Scale-up path", under
// "Per-iteration anchor refresh".
func refreshAnchorSizing(variants []domain.VariantCapacity, s []NamedAnalyzerResult, state RolePairedState) {
	if len(s) <= 1 {
		return
	}
	for i := range variants {
		vc := &variants[i]
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		_, idx := combineVotes(votesFromPickerState(s, state, role, vc.VariantName), true)
		if idx < 0 || s[idx].Result == nil {
			continue // no voting entry currently sizes this variant; leave as-is
		}
		b, ok := variantCapacityByName(s[idx].Result.VariantCapacities, vc.VariantName)
		if !ok {
			continue // the current binder doesn't carry this variant; leave as-is
		}
		vc.PerReplicaCapacity = b.PerReplicaCapacity
		vc.Reason = b.Reason
		vc.TotalDemand = b.TotalDemand
		vc.Utilization = b.Utilization
		vc.TotalCapacity = float64(vc.ReplicaCount) * vc.PerReplicaCapacity
	}
}

// roleAggRemaining returns the raw remaining demand of the entry currently
// bottlenecking variant v in role — the binder combineVotes selects over the
// same votesFromPickerState ballot (same participation rules) that
// roleBottleneckReplicas sizes from, not a raw cross-analyzer max, and not a
// second private loop that could disagree with it. Comparing raw
// RequiredCapacity directly is meaningless
// once analyzers' units differ (saturation = tokens, throughput = req/s —
// Bug #2); combining in replica space and returning that entry's own raw
// value keeps the result commensurable with prc (that same entry's
// PerReplicaCapacity) in the caller's n*prc/demand and k formulas. With a
// single voter this is always that voter's own state[0][role], byte-identical
// to the previous raw max.
func roleAggRemaining(s []NamedAnalyzerResult, state RolePairedState, role, v string) float64 {
	_, idx := combineVotes(votesFromPickerState(s, state, role, v), true)
	if idx < 0 {
		return 0
	}
	return state[idx][role]
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

// roleSpareVetoed reports whether any live analyzer holds an explicit,
// non-positive role-level spare — an objection to removing anything from this
// role, whichever variant is under consideration.
//
// "Explicit" carries the whole distinction. The entry must be live, must have a
// Result and a RoleSpare map, and the map must carry a key for role. A live
// analyzer whose RoleSpare does not decompose this role ABSTAINS: it never
// sized the role, so it has no basis to block it. A key that is present and
// non-positive is a different statement — that analyzer did size the role and
// reports there is nothing left to give back.
//
// PRC-blind and score-blind, both load-bearing rather than incidental. A veto is
// not a magnitude: there is no quantity to convert, so a per-variant capacity is
// irrelevant to it, and there is nothing to weigh, so a score cannot dilute it.
// Both are ways an objection would otherwise evaporate on the way to being
// counted — see safeRemovalReplicasForRole for the two reachable paths.
//
// Shared with needsScaleDownForRole on purpose, and the duplication is the
// point: the same predicate makes that call a cheap early-out for the whole
// role, and this one the actual enforcement point, because a spare that was
// positive at role entry can be driven to zero mid-loop by
// applyDeallocationForRole. Neither caller is redundant.
func roleSpareVetoed(s []NamedAnalyzerResult, role string) bool {
	for _, e := range s {
		if !e.Live {
			continue // non-live analyzers do not veto (no metrics / error / never analyzed)
		}
		if e.Result == nil || e.RoleSpare == nil {
			continue // no data at all this cycle; abstain, not veto
		}
		if spare, ok := e.RoleSpare[role]; ok && spare <= 0 {
			return true
		}
	}
	return false
}

// safeRemovalReplicasForRole returns the number of replicas of variant v that
// can safely be removed — the combined scale-down vote over each live
// analyzer's per-role spare (votesFromRoleSpare, whose doc comment carries the
// participation rules), rounded down once. Clamps a negative combine to 0: an
// over-committed spare is not a removable count.
//
// Returns 0 in two distinct cases. First, when no live analyzer sizes v at all,
// there is no ballot to combine. Second, and independently of v, when any live
// analyzer holds an explicit non-positive role-level spare (roleSpareVetoed):
// that objection blocks removal from the whole role, whether or not the
// objecting analyzer can price this particular variant and whatever its score.
//
// The veto is checked BEFORE combining, not expressed as a vote inside the
// ballot, because after dominance weighting a vote is no longer a veto. Two
// reachable paths would otherwise discard a live objection, both of them opened
// by the role-entry gate running once and never being re-checked while
// applyDeallocationForRole drives spares down variant by variant:
//
//   - The objector does not size v. votesFromRoleSpare drops any entry with no
//     per-variant capacity, so its explicit "no spare left in this role" never
//     reaches the combine and the remaining spares decide. A variant with no
//     observed metrics yet is absent from that analyzer's VariantCapacities
//     while still present in the anchor, so this is ordinary, not exotic.
//   - The objector is outscored. Even when it does size v and therefore votes 0,
//     the dominance correction pulls the combined value positive whenever a
//     higher-scored voter reports spare, so floor() can still return a positive
//     removable count. A zero vote is only absolute when no voter outscores it.
func safeRemovalReplicasForRole(s []NamedAnalyzerResult, v, role string) int {
	if roleSpareVetoed(s, role) {
		return 0
	}
	value, binder := combineVotes(votesFromRoleSpare(s, role, v), false)
	if binder < 0 {
		return 0
	}
	if n := int(math.Floor(value)); n > 0 {
		return n
	}
	return 0
}

// applyDeallocationForRole decrements each analyzer's RoleSpare[role] by
// n × PRC_i[v]. Clamps to 0. Never mutates Result.
//
// Intentionally not Live-gated, and the reason survives the per-variant veto
// unchanged because it turns on liveness alone: every reader of RoleSpare on this
// path — the role gate, the per-variant veto, and the safe-removal ballot — skips
// non-live entries first, so a non-live entry's RoleSpare is written here and
// never read back. What the veto does change is that a LIVE entry's decremented
// spare is now read by two things rather than one: it can reach 0 here and then
// block every remaining variant in the role, which is precisely the mid-loop
// path roleSpareVetoed exists to catch.
//
// The clamp at 0 matters to that path. A spare driven negative would still be
// non-positive and so would still veto, but clamping keeps "exhausted" a single
// value rather than a range, which is what makes the veto's ≤ 0 test read as an
// exact state and not as a tolerance.
//
// Only an already-present key is decremented, and that guard is what keeps an
// abstain an abstain for the whole loop rather than only until the first removal.
// A bare `m[role] -= x` on a map with no such key reads the zero value, writes
// the clamped result, and so MATERIALIZES the key at 0 — turning an analyzer that
// never sized this role into one that appears to report no spare left in it, and
// from the next variant onward that fabricated entry would veto. You can only
// spend spare you reported: an analyzer with no opinion on the role has no
// balance here to draw down. Observationally inert before the per-variant veto
// existed (votesFromRoleSpare reads an absent key as 0 either way), which is why
// the pre-existing form was harmless and is not any longer.
func applyDeallocationForRole(s []NamedAnalyzerResult, v, role string, n int) {
	for i := range s {
		if s[i].Result == nil || s[i].RoleSpare == nil {
			continue
		}
		spare, ok := s[i].RoleSpare[role]
		if !ok {
			continue // abstained on this role: no reported balance to spend
		}
		prc := prcForVariant(s[i].Result, v)
		if prc <= 0 {
			// Cannot price v, so it abstained on v above and is credited nothing
			// back for it here. The two must agree: an entry that did not
			// constrain the removal must not have its spare adjusted by it.
			continue
		}
		spare -= float64(n) * prc
		if spare < 0 {
			spare = 0
		}
		s[i].RoleSpare[role] = spare
	}
}

// needsScaleDownForRole reports whether every live analyzer that has an
// opinion on role agrees it has spare capacity (all-agree gate, scoped to one
// role). Non-live analyzers (no metrics, error state, never analyzed, or
// stale) do not veto — this applies uniformly, including saturation's
// token-capacity result; there is no name-based exemption. A live analyzer
// with no RoleSpare data at all, or whose RoleSpare simply does not decompose
// this role, ABSTAINS rather than vetoing: a coarser voter (e.g. a
// non-disaggregated analyzer's single RoleBoth entry, seeded by initRoleState)
// has no basis to veto a role it never sized, so a map-miss must not read as
// "spare == 0". Returns false only when a live analyzer that DOES have an
// opinion on role reports RoleSpare[role] ≤ 0. Safety floor: if no live
// analyzer has an opinion on this role at all, there is no current basis to
// scale down, so this returns false.
//
// Deliberately not expressed through combineVotes: this is a veto, not a
// magnitude, and its participation rules are stricter than votesFromRoleSpare's
// — a live entry whose RoleSpare carries no key for role abstains here but
// still votes 0.0 on the safe-removal ballot. The abstain holds for the whole
// role, not only until the first removal: applyDeallocationForRole draws down
// reported balances only, so the loop cannot materialize a key for an analyzer
// that abstained and thereby manufacture a veto out of a silence.
//
// The veto half is roleSpareVetoed, shared with safeRemovalReplicasForRole. This
// call is an early-out that skips the role in full before any variant is
// considered; the shared predicate is re-checked per variant there, because
// deallocating one variant can exhaust a spare this gate already passed.
func needsScaleDownForRole(s []NamedAnalyzerResult, role string) bool {
	if roleSpareVetoed(s, role) {
		return false
	}
	liveCount := 0
	for _, e := range s {
		if !e.Live {
			continue // non-live analyzers do not veto (no metrics / error / never analyzed)
		}
		if e.Result == nil || e.RoleSpare == nil {
			continue // no data at all this cycle; abstain, not veto
		}
		if _, ok := e.RoleSpare[role]; !ok {
			continue // this analyzer doesn't decompose this role; abstain, not veto
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
// Per iteration: refresh the anchor's per-variant sizing to the current
// binding analyzer (multi-vote only — see refreshAnchorSizing), pick one
// variant per role, size independently, compute Δ_util = min_role util_role,
// trim to matched joint commit. Arity-1 (roles = ["both"]) reduces to plain
// per-variant allocation.
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
		refreshAnchorSizing(variants, s, pickerState)
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
			demand := roleAggRemaining(s, pickerState, role, variantByRole[role])
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
			demand := roleAggRemaining(s, pickerState, role, variantByRole[role])
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
			targets[v] += k
			// Decrement each analyzer's own remaining by k*PRC_i[v] -- the
			// picked variant's PRC differs per analyzer, so a single uniform
			// PRC (the anchor's binder) would mix units for every other
			// analyzer (Bug #1). The combine collects each vote as
			// state[i][role]/PRC_i, so the decrement must match.
			for i := range pickerState {
				if s[i].Result == nil {
					continue
				}
				prcI := prcForVariant(s[i].Result, v)
				if prcI <= 0 {
					continue
				}
				pickerState[i][role] = math.Max(0, pickerState[i][role]-float64(k)*prcI)
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
