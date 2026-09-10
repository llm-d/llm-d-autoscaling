package pipeline

// Invariant 7, asserted directly: with saturation as the only voting analyzer,
// the anchor is the saturation entry field-for-field, and the per-iteration
// sizing refresh does not run at all.
//
// The decision goldens assert this only transitively — they would stay green if
// the anchor were subtly different but the decisions happened to coincide. The
// invariant is the load-bearing backward-compatibility guard of the whole
// refactor, so it gets its own direct test: equality before, during and after
// allocation, plus the refresh guard pinned on its own.
//
// On "the refresh is not invoked": refreshAnchorSizing is a plain function, so
// non-invocation is not observable from outside without turning it into a
// package variable purely for test instrumentation. What is observable is the
// property the frozen design actually names in its parenthetical — "the anchor
// is not mutated after initial population" — so the guard test below asserts
// the refresh has no effect on a one-analyzer ballot *even when handed a slice
// it would otherwise rewrite*, and proves that fixture is not inert by showing
// the same call does rewrite it with a second voting entry.
//
// Verified by mutation rather than assumed: deleting the len(s) <= 1 early
// return from refreshAnchorSizing fails exactly one spec in the package — the
// third one below. The two equality specs stay green, which is the reason that
// third spec exists: with one voter the refresh is a value-level no-op, so
// running it and not running it are indistinguishable from the anchor's
// contents alone.

import (
	"context"
	"fmt"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// expectAnchorEqualsSatEntry asserts the derived anchor matches the saturation
// Result it was derived from, field by field, at model level and per variant.
//
// TotalCapacity is the one field bindingAnchor recomputes rather than copies
// (ReplicaCount x PerReplicaCapacity, so the invariant holds by construction).
// The fixtures therefore state it consistently and the assertion stays exact,
// rather than the test excusing the field.
func expectAnchorEqualsSatEntry(anchor, sat *domain.AnalyzerResult, phase string) {
	Expect(anchor).NotTo(BeNil(), "%s: anchor must not be nil", phase)

	// AnalyzerName comes from the *binding* entry's Result, not from the ballot
	// entry's Name, so the fixtures set it explicitly and this assertion is not
	// two empty strings agreeing.
	Expect(anchor.AnalyzerName).To(Equal(sat.AnalyzerName), "%s: AnalyzerName", phase)
	Expect(anchor.AnalyzerName).NotTo(BeEmpty(), "%s: the fixture must set AnalyzerName for the check above to mean anything", phase)

	Expect(anchor.ModelID).To(Equal(sat.ModelID), "%s: ModelID", phase)
	Expect(anchor.Namespace).To(Equal(sat.Namespace), "%s: Namespace", phase)
	Expect(anchor.AnalyzedAt).To(Equal(sat.AnalyzedAt), "%s: AnalyzedAt", phase)
	Expect(anchor.TotalSupply).To(Equal(sat.TotalSupply), "%s: TotalSupply", phase)
	Expect(anchor.TotalDemand).To(Equal(sat.TotalDemand), "%s: TotalDemand", phase)
	Expect(anchor.Utilization).To(Equal(sat.Utilization), "%s: Utilization", phase)
	Expect(anchor.TotalAnticipatedSupply).To(Equal(sat.TotalAnticipatedSupply), "%s: TotalAnticipatedSupply", phase)
	Expect(anchor.RequiredCapacity).To(Equal(sat.RequiredCapacity), "%s: RequiredCapacity", phase)
	Expect(anchor.SpareCapacity).To(Equal(sat.SpareCapacity), "%s: SpareCapacity", phase)
	Expect(anchor.RoleCapacities).To(Equal(sat.RoleCapacities), "%s: RoleCapacities", phase)

	expectVariantsEqualSat(anchor.VariantCapacities, sat, phase)
}

// expectVariantsEqualSat is the per-variant half, split out so the allocation
// loop can assert it mid-flight: the slice the pick closure receives IS the
// anchor's own VariantCapacities.
func expectVariantsEqualSat(got []domain.VariantCapacity, sat *domain.AnalyzerResult, phase string) {
	want := make(map[string]domain.VariantCapacity, len(sat.VariantCapacities))
	wantNames := make([]string, 0, len(sat.VariantCapacities))
	for _, vc := range sat.VariantCapacities {
		want[vc.VariantName] = vc
		wantNames = append(wantNames, vc.VariantName)
	}
	gotNames := make([]string, 0, len(got))
	for _, vc := range got {
		gotNames = append(gotNames, vc.VariantName)
	}
	Expect(gotNames).To(ConsistOf(wantNames), "%s: anchor variant set (topology)", phase)

	for _, g := range got {
		w := want[g.VariantName]
		Expect(g).To(Equal(w), "%s: variant %q must equal saturation's entry field-for-field", phase, g.VariantName)
	}
}

var _ = Describe("Invariant 7 — saturation-only ⇒ the anchor IS the saturation entry", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// satOnly builds a one-entry ballot: the default config, saturation voting
	// alone. Deliberately not withSatEntry — that helper is shared with the
	// #1513 goldens, and a test whose subject is the anchor must not be able to
	// go green by pinning the helper instead.
	satOnly := func(sat *domain.AnalyzerResult, req ModelScalingRequest) ModelScalingRequest {
		req.AnalyzerResults = []NamedAnalyzerResult{{
			Name:      domain.SaturationAnalyzerName,
			Result:    sat,
			Score:     1.0,
			Remaining: sat.RequiredCapacity,
			Spare:     sat.SpareCapacity,
			Enabled:   true,
			Live:      true,
		}}
		return req
	}

	It("holds before, during and after allocation (aggregated)", func() {
		sat := &domain.AnalyzerResult{
			AnalyzerName:     domain.SaturationAnalyzerName,
			ModelID:          "inv7-agg",
			Namespace:        "default",
			RequiredCapacity: 35000,
			TotalDemand:      35000,
			TotalSupply:      20000,
			Utilization:      0.71,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000,
					TotalCapacity: 20000, TotalDemand: 35000, Utilization: 0.71, Reason: "P1-obs"},
			},
		}
		req := satOnly(sat, ModelScalingRequest{
			ModelID:   "inv7-agg",
			Namespace: "default",
			Priority:  1,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
			},
		})

		s := votingResults(req.AnalyzerResults)
		Expect(s).To(HaveLen(1), "the fixture must be a genuine one-vote ballot")

		anchor := bindingAnchor(req.AnalyzerResults)
		expectAnchorEqualsSatEntry(anchor, sat, "before allocation")

		stateMap := buildStateMap(req.VariantStates)
		targets := initTargets(req.VariantStates)
		roles, ps := initRoleState(s)

		iterations := 0
		probe := func(role string, bs []NamedAnalyzerResult, variants []domain.VariantCapacity,
			sm map[string]domain.VariantReplicaState, available, tg map[string]int) (string, int) {
			iterations++
			expectVariantsEqualSat(variants, sat, fmt.Sprintf("during allocation, iteration %d", iterations))
			return costGreedyRolePick(role, bs, variants, sm, available, tg)
		}
		allocateForModelPaired(ctx, s, anchor.VariantCapacities, stateMap, nil, targets, probe, ps, roles)

		Expect(iterations).To(BeNumerically(">=", 1), "non-vacuity: the allocation loop must actually iterate")
		Expect(targets["v"]).To(BeNumerically(">", 2), "non-vacuity: allocation must actually commit replicas")

		expectAnchorEqualsSatEntry(anchor, sat, "after allocation")
	})

	It("holds before, during and after allocation (disaggregated P/D)", func() {
		sat := &domain.AnalyzerResult{
			AnalyzerName:     domain.SaturationAnalyzerName,
			ModelID:          "inv7-pd",
			Namespace:        "default",
			RequiredCapacity: 30000,
			TotalDemand:      30000,
			Utilization:      0.6,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p", Role: "prefill", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000,
					TotalCapacity: 10000, TotalDemand: 30000, Utilization: 0.65, Reason: "P1-obs"},
				{VariantName: "d", Role: "decode", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000,
					TotalCapacity: 10000, TotalDemand: 10000, Utilization: 0.45, Reason: "P1-obs"},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: 30000, TotalDemand: 30000},
				"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
			},
		}
		req := satOnly(sat, ModelScalingRequest{
			ModelID:       "inv7-pd",
			Namespace:     "default",
			Priority:      1,
			Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "p", Role: "prefill", CurrentReplicas: 1, GPUsPerReplica: 1},
				{VariantName: "d", Role: "decode", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		})

		s := votingResults(req.AnalyzerResults)
		Expect(s).To(HaveLen(1), "the fixture must be a genuine one-vote ballot")

		anchor := bindingAnchor(req.AnalyzerResults)
		expectAnchorEqualsSatEntry(anchor, sat, "before allocation")

		stateMap := buildStateMap(req.VariantStates)
		targets := initTargets(req.VariantStates)
		roles, ps := initRoleState(s)
		Expect(roles).To(ConsistOf("prefill", "decode"), "the fixture must be genuinely disaggregated")

		iterations := 0
		probe := func(role string, bs []NamedAnalyzerResult, variants []domain.VariantCapacity,
			sm map[string]domain.VariantReplicaState, available, tg map[string]int) (string, int) {
			iterations++
			expectVariantsEqualSat(variants, sat, fmt.Sprintf("during allocation, pick %d", iterations))
			return costGreedyRolePick(role, bs, variants, sm, available, tg)
		}
		allocateForModelPaired(ctx, s, anchor.VariantCapacities, stateMap, nil, targets, probe, ps, roles)

		Expect(iterations).To(BeNumerically(">=", 2), "non-vacuity: both roles must be picked at least once")
		Expect(targets["p"]).To(BeNumerically(">", 1), "non-vacuity: prefill must actually commit replicas")

		expectAnchorEqualsSatEntry(anchor, sat, "after allocation")
	})

	It("does not execute the per-iteration sizing refresh at all on a one-vote ballot", func() {
		sat := &domain.AnalyzerResult{
			ModelID:          "inv7-refresh",
			Namespace:        "default",
			RequiredCapacity: 15000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000,
					TotalCapacity: 20000, TotalDemand: 15000, Utilization: 0.71, Reason: "P1-obs"},
			},
		}
		satEntry := NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Result: sat, Score: 1.0,
			Remaining: sat.RequiredCapacity, Spare: sat.SpareCapacity, Enabled: true, Live: true,
		}

		// Deliberately out of step with the ballot: every sizing field below
		// differs from saturation's, so a refresh that ran would rewrite all of
		// them. Nothing builds an anchor like this -- that is the point. The
		// subject is the guard, not a realistic anchor.
		variants := []domain.VariantCapacity{{
			VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2,
			PerReplicaCapacity: 1, TotalCapacity: 2, TotalDemand: 7, Utilization: 0.01,
			Reason: "must-not-be-touched",
		}}
		before := slices.Clone(variants)

		_, ps := initRoleState([]NamedAnalyzerResult{satEntry})
		refreshAnchorSizing(variants, []NamedAnalyzerResult{satEntry}, ps)
		Expect(variants).To(Equal(before), "one vote ⇒ the refresh must not execute at all")

		// Non-vacuity, and the mutation guard: with a second voting entry the
		// very same call DOES rewrite the slice. So the assertion above pins the
		// len(s) <= 1 early return rather than an inert fixture -- remove that
		// guard and this test fails on the line above.
		ta := &domain.AnalyzerResult{
			ModelID:          "inv7-refresh",
			Namespace:        "default",
			RequiredCapacity: 25000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", PerReplicaCapacity: 10000, TotalDemand: 25000, Reason: "T1-ols"},
			},
		}
		twoVotes := []NamedAnalyzerResult{satEntry, {
			Name: "throughput", Result: ta, Score: 1.0,
			Remaining: ta.RequiredCapacity, Spare: ta.SpareCapacity, Enabled: true, Live: true,
		}}
		moved := slices.Clone(before)
		_, ps2 := initRoleState(twoVotes)
		refreshAnchorSizing(moved, twoVotes, ps2)
		Expect(moved).NotTo(Equal(before), "non-vacuity: two votes must actually refresh the sizing fields")
	})
})
