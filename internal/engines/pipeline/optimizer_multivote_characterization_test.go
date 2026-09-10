package pipeline

// Multi-vote optimizer goldens: the same seven scenarios #1513 froze for a
// one-analyzer ballot, asserted across all three ballot shapes the combined
// design defines.
//
// Why this file exists: a sat-only freeze suite used to live in
// optimizer_characterization_test.go as the anchor refactor's ship gate, and it
// was removed once this suite covered it. Removal was only permissible because
// the coverage survives, so every scenario here carries a `[sat]`-only shape
// whose expectations are transcribed verbatim from the golden it replaced --
// never re-derived, never "improved".
//
// Scenario names. Each table below is named for the removed golden it subsumes,
// and those names are recorded here so the references resolve without reaching
// for deleted code:
//
//	A1 -- aggregated scale-up, one variant, demand over capacity
//	A2 -- aggregated scale-down, the sole/cheapest variant protected at one
//	A3 -- idle model, two variants, no demand and no spare, nothing changes
//	A4 -- two variants, cost tie-break, the cheapest absorbs the demand
//	B1 -- disaggregated prefill/decode paired scale-up, equal per-role demand
//	B2 -- disaggregated role-scoped scale-down, expensive prefill fully removed
//	C1 -- namespace quota caps a model below its unconstrained demand
//
// Ballot construction, and why the [sat]-only shape is genuinely the same test.
// All three shapes start from withSatEntry, the identical helper the #1513
// goldens call, and the saturation Result is the identical literal. The
// [sat]-only shape then adds nothing at all, so it is not merely named after
// the golden it replaces -- it presents a bit-identical ballot to a bit-identical
// optimizer call. The other two shapes are that same ballot plus one throughput
// entry (and, for [TA]-only, saturation's Enabled cleared). Nothing here reaches
// through the helper, so a change to withSatEntry moves this file and the
// goldens together rather than silently sparing one of them.
//
// The three shapes, and what each is for:
//
//   - [sat] only          -- one voter. The anchor is saturation's entry and the
//                            per-iteration sizing refresh does not run
//                            (Invariant 7, asserted directly in
//                            optimizer_invariant7_test.go).
//   - [TA] only           -- saturation present but not voting. It still carries
//                            the anchor's identity, located by name; throughput
//                            binds the sizing and the model-level values.
//   - [sat, TA] both vote -- saturation binds the anchor (so model-level
//                            RequiredCapacity/SpareCapacity stay saturation's),
//                            while the per-(role, variant) sizing binder is
//                            whichever entry demands more replicas. When that is
//                            throughput, the decision's Utilization moves and its
//                            RequiredCapacity does not. That split is the whole
//                            point of the refactor and it is what these goldens
//                            pin.
//
// How the throughput numbers were chosen. Saturation's side is transcribed and
// therefore fixed. Throughput's side is chosen so that every demand is an exact
// integer multiple of the per-replica capacity it is divided by. The allocator
// computes k = floor(deltaUtil * demand / prc) where deltaUtil is itself
// n*prc/demand, so a non-integer ratio makes the expected k depend on whether
// that round trip lands on 4.0 or 3.9999999999999996. Integer ratios keep these
// expectations hand-derivable instead of merely captured.
//
// Fair-share exposure, stated rather than left implicit: replicasToCover rounds
// a GPU entitlement UP, and whether it should instead take a whole-replica floor
// is an open question in this tree. No golden here can freeze
// either side -- the aggregated and P/D scenarios run on 1e6-GPU pools where the
// entitlement never binds, and the quota scenario leaves exactly 2 free GPUs at
// 2 GPUs per replica, so ceil and floor agree at 1. If the fork later resolves to
// floor, nothing in this file has to move.
//
// One coverage boundary, named because a silent one would read as coverage:
// GreedyByScore is asserted on the multi-vote shapes for the aggregated and
// quota scenarios only. Its disaggregated path distributes demand proportionally
// instead of committing a paired (n_P, n_D), which is why B1 pinned it as its own
// golden rather than asserting equality with CostAware; hand-deriving a
// multi-vote fair-share distribution is not something this file can do honestly,
// so the P/D multi-vote shapes assert CostAware and the [sat]-only shape carries
// B1/B2's GreedyByScore expectations forward unchanged.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// ballotShape selects which analyzers vote in a scenario's ballot.
type ballotShape int

const (
	shapeSatOnly ballotShape = iota // one voter: saturation
	shapeTAOnly                     // saturation present as identity carrier only
	shapeBoth                       // both vote
)

// throughputEntryName is the ballot name of the throughput analyzer. There is no
// exported constant for it (unlike domain.SaturationAnalyzerName, which the
// carrier lookup needs), and the literal is what the pipeline's other multi-vote
// tests use.
const throughputEntryName = "throughput"

// applyShape turns a request already carrying withSatEntry's one-entry ballot
// into one of the three shapes. shapeSatOnly deliberately touches nothing.
func applyShape(req ModelScalingRequest, shape ballotShape, ta *domain.AnalyzerResult) ModelScalingRequest {
	taEntry := NamedAnalyzerResult{
		Name:      throughputEntryName,
		Result:    ta,
		Remaining: ta.RequiredCapacity,
		Spare:     ta.SpareCapacity,
		Enabled:   true,
		Live:      true,
	}
	switch shape {
	case shapeSatOnly:
		return req
	case shapeTAOnly:
		// Saturation stops voting but stays on the ballot: bindingAnchor finds the
		// identity carrier by name, with no Enabled test, so the anchor keeps
		// saturation's topology while throughput supplies every sizing field.
		req.AnalyzerResults[0].Enabled = false
		req.AnalyzerResults = append(req.AnalyzerResults, taEntry)
	case shapeBoth:
		req.AnalyzerResults = append(req.AnalyzerResults, taEntry)
	}
	return req
}

// targetsOf indexes decisions by variant name for non-vacuity checks.
func targetsOf(decisions []domain.VariantDecision) map[string]int {
	out := make(map[string]int, len(decisions))
	for _, d := range decisions {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

var _ = Describe("Multi-vote optimizer goldens — [sat] / [TA] / [sat, TA]", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// -------------------------------------------------------------------------
	// M1 mirrors A1: aggregated scale-up, single variant.
	// -------------------------------------------------------------------------
	DescribeTable("M1 (mirrors A1) — aggregated scale-up, single variant",
		func(shape ballotShape, want map[string]goldenDecision) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:          "m1",
					Namespace:        "default",
					RequiredCapacity: 15000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.71},
					},
				}
				ta := &domain.AnalyzerResult{
					ModelID:          "m1",
					Namespace:        "default",
					RequiredCapacity: 40000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 10000, TotalDemand: 40000, Utilization: 0.9, Reason: "T1-ols"},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:   "m1",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
					},
				}), shape, ta)
			}
			ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))
			Expect(targetsOf(ca)["v"]).To(BeNumerically(">", 2), "non-vacuity: scale-up must actually run")
			expectDecisionSet(ca, want)
			expectDecisionSet(gs, want)
		},
		// Transcribed verbatim from A1 (captured main@9906dac5): ceil(15000/10000)=2
		// additional -> 4.
		Entry("[sat] only — A1's ballot and A1's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"v": {Replicas: 4, RequiredCapacity: 15000, SpareCapacity: 0, Utilization: 0.71},
			}),
		// Throughput binds everything: model-level RequiredCapacity is its 40000,
		// and 40000/10000 = 4 additional replicas -> 6.
		Entry("[TA] only — throughput sizes and supplies the model-level values", shapeTAOnly,
			map[string]goldenDecision{
				"v": {Replicas: 6, RequiredCapacity: 40000, SpareCapacity: 0, Utilization: 0.9},
			}),
		// The split: saturation binds the anchor, so RequiredCapacity stays 15000,
		// while the sizing refresh hands the variant to throughput (4 replicas of
		// demand beats 1.5) -- so the target is 6 and Utilization is throughput's.
		Entry("[sat, TA] — anchor from saturation, sizing from throughput", shapeBoth,
			map[string]goldenDecision{
				"v": {Replicas: 6, RequiredCapacity: 15000, SpareCapacity: 0, Utilization: 0.9},
			}),
	)

	// -------------------------------------------------------------------------
	// M2 mirrors A2: aggregated scale-down, and the scale-down veto.
	// -------------------------------------------------------------------------
	DescribeTable("M2 (mirrors A2) — aggregated scale-down, and one voter's veto",
		func(shape ballotShape, want map[string]goldenDecision, wantRemoval bool) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:       "m2",
					Namespace:     "default",
					SpareCapacity: 30000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000, Utilization: 0.05},
					},
				}
				// No spare at all: an explicit, live "there is nothing to give back"
				// on the synthetic both role, which is a veto rather than a vote.
				ta := &domain.AnalyzerResult{
					ModelID:       "m2",
					Namespace:     "default",
					SpareCapacity: 0,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 10000, Utilization: 0.4, Reason: "T1-ols"},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:   "m2",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 3, GPUsPerReplica: 1},
					},
				}), shape, ta)
			}
			ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			if wantRemoval {
				Expect(targetsOf(ca)["v"]).To(BeNumerically("<", 3), "non-vacuity: scale-down must actually run")
			} else {
				Expect(targetsOf(ca)["v"]).To(Equal(3), "non-vacuity: the veto must actually block removal")
			}
			expectDecisionSet(ca, want)
			expectDecisionSet(gs, want)
		},
		// Transcribed verbatim from A2: floor(30000/10000)=3 would remove every
		// replica, but the sole (always-cheapest) variant is protected at 1.
		Entry("[sat] only — A2's ballot and A2's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"v": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 30000, Utilization: 0.05},
			}, true),
		// Throughput binds: its own zero spare is the published SpareCapacity, and
		// its zero spare on the both role also vetoes the removal it would size.
		Entry("[TA] only — throughput reports no spare, so nothing is removed", shapeTAOnly,
			map[string]goldenDecision{
				"v": {Replicas: 3, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.4},
			}, false),
		// The veto: saturation offers 30000 of spare and would shed to the floor of
		// 1, but one live voter with an explicit zero spare blocks the whole role.
		// The published SpareCapacity is still the anchor's -- saturation's 30000 --
		// so the decision reports spare it is not permitted to spend, and
		// Utilization stays saturation's because the scale-down path runs no
		// sizing refresh.
		Entry("[sat, TA] — a single explicit zero spare vetoes the role", shapeBoth,
			map[string]goldenDecision{
				"v": {Replicas: 3, RequiredCapacity: 0, SpareCapacity: 30000, Utilization: 0.05},
			}, false),
	)

	// -------------------------------------------------------------------------
	// M3 mirrors A3: no demand, no spare, two variants -- no churn.
	// -------------------------------------------------------------------------
	DescribeTable("M3 (mirrors A3) — idle model, no churn under any ballot",
		func(shape ballotShape, want map[string]goldenDecision) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:   "m3",
					Namespace: "default",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.4},
						{VariantName: "v2", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000, Utilization: 0.55},
					},
				}
				ta := &domain.AnalyzerResult{
					ModelID:   "m3",
					Namespace: "default",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 10000, Utilization: 0.45, Reason: "T1-ols"},
						{VariantName: "v2", PerReplicaCapacity: 20000, Utilization: 0.5, Reason: "T1-ols"},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:   "m3",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2, GPUsPerReplica: 1},
						{VariantName: "v2", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				}), shape, ta)
			}
			// Deliberately vacuous in every shape (target == current everywhere):
			// that IS the property, and adding a voter must not create churn. Same
			// reasoning A3 records for itself.
			expectDecisionSet(NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil), want)
			expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100", "H100")), want)
		},
		// Transcribed verbatim from A3.
		Entry("[sat] only — A3's ballot and A3's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"v1": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.4},
				"v2": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.55},
			}),
		// Only the reported Utilization moves, because it is the one decision field
		// the binder supplies per variant.
		Entry("[TA] only — throughput's utilization is reported, targets unmoved", shapeTAOnly,
			map[string]goldenDecision{
				"v1": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.45},
				"v2": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.5},
			}),
		// Saturation binds and no scale-up runs, so nothing refreshes: identical to
		// the one-voter shape.
		Entry("[sat, TA] — a second idle voter changes nothing", shapeBoth,
			map[string]goldenDecision{
				"v1": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.4},
				"v2": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.55},
			}),
	)

	// -------------------------------------------------------------------------
	// M4 mirrors A4: two variants, cost tie-break, cheapest absorbs demand.
	// -------------------------------------------------------------------------
	DescribeTable("M4 (mirrors A4) — cheapest absorbs the demand, whoever sized it",
		func(shape ballotShape, want map[string]goldenDecision) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:          "m4",
					Namespace:        "default",
					RequiredCapacity: 5000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.6},
						{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000, Utilization: 0.3},
					},
				}
				ta := &domain.AnalyzerResult{
					ModelID:          "m4",
					Namespace:        "default",
					RequiredCapacity: 20000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "cheap", PerReplicaCapacity: 10000, TotalDemand: 20000, Utilization: 0.8, Reason: "T1-ols"},
						{VariantName: "expensive", PerReplicaCapacity: 20000, TotalDemand: 20000, Utilization: 0.35, Reason: "T1-ols"},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:   "m4",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 2, GPUsPerReplica: 1},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				}), shape, ta)
			}
			ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100", "H100"))
			Expect(targetsOf(ca)["expensive"]).To(Equal(1),
				"the expensive variant must stay put -- cost-efficiency ordering comes from the carrier's Cost, which no ballot shape changes")
			expectDecisionSet(ca, want)
			expectDecisionSet(gs, want)
		},
		// Transcribed verbatim from A4: cost-efficiency cheap=0.0005 <
		// expensive=0.00075, so cheap absorbs all of it: ceil(5000/10000)=1.
		Entry("[sat] only — A4's ballot and A4's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"cheap":     {Replicas: 3, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.6},
				"expensive": {Replicas: 1, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.3},
			}),
		// 20000/10000 = 2 more replicas of cheap; the ordering is unchanged because
		// Cost is an identity field and comes from saturation either way.
		Entry("[TA] only — throughput's larger demand, same cheapest variant", shapeTAOnly,
			map[string]goldenDecision{
				"cheap":     {Replicas: 4, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.8},
				"expensive": {Replicas: 1, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.35},
			}),
		// Throughput outbids saturation on both variants, so both utilizations are
		// refreshed, while the published RequiredCapacity stays saturation's 5000.
		Entry("[sat, TA] — sizing refreshed per variant, model-level unchanged", shapeBoth,
			map[string]goldenDecision{
				"cheap":     {Replicas: 4, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.8},
				"expensive": {Replicas: 1, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.35},
			}),
	)

	// -------------------------------------------------------------------------
	// M5 mirrors B1: disaggregated paired scale-up.
	// -------------------------------------------------------------------------
	DescribeTable("M5 (mirrors B1) — disaggregated paired scale-up",
		func(shape ballotShape, want map[string]goldenDecision, alsoGreedyByScore bool) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:          "m5",
					Namespace:        "default",
					RequiredCapacity: 20000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "prefill-v", Role: "prefill", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.65},
						{VariantName: "decode-v", Role: "decode", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.45},
					},
					RoleCapacities: map[string]domain.RoleCapacity{
						"prefill": {Role: "prefill", RequiredCapacity: 20000, TotalDemand: 20000},
						"decode":  {Role: "decode", RequiredCapacity: 20000, TotalDemand: 20000},
					},
				}
				ta := &domain.AnalyzerResult{
					ModelID:          "m5",
					Namespace:        "default",
					RequiredCapacity: 40000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "prefill-v", Role: "prefill", PerReplicaCapacity: 10000, TotalDemand: 40000, Utilization: 0.7, Reason: "T1-ols"},
						{VariantName: "decode-v", Role: "decode", PerReplicaCapacity: 10000, TotalDemand: 40000, Utilization: 0.5, Reason: "T1-ols"},
					},
					RoleCapacities: map[string]domain.RoleCapacity{
						"prefill": {Role: "prefill", RequiredCapacity: 40000, TotalDemand: 40000},
						"decode":  {Role: "decode", RequiredCapacity: 40000, TotalDemand: 40000},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:       "m5",
					Namespace:     "default",
					Priority:      1,
					Disaggregated: true,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", Role: "prefill", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "decode-v", Role: "decode", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}), shape, ta)
			}
			ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			t := targetsOf(ca)
			Expect(t["prefill-v"]).To(BeNumerically(">", 1), "non-vacuity: prefill scale-up must actually run")
			Expect(t["decode-v"]).To(BeNumerically(">", 1), "non-vacuity: decode scale-up must actually run")
			Expect(t["prefill-v"]).To(Equal(t["decode-v"]), "the joint commit must move both roles together")
			expectDecisionSet(ca, want)
			if alsoGreedyByScore {
				expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100")), want)
			}
		},
		// Transcribed verbatim from B1 (both optimizers, which B1 pinned
		// separately and found to agree here): alpha=1, n_P=n_D=2 -> 3 each.
		Entry("[sat] only — B1's ballot and B1's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"prefill-v": {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.65},
				"decode-v":  {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.45},
			}, true),
		// Throughput binds, so the published per-role RequiredCapacity is its own
		// 40000: 4 more replicas per role, still jointly committed.
		Entry("[TA] only — throughput's per-role demand drives the pair", shapeTAOnly,
			map[string]goldenDecision{
				"prefill-v": {Replicas: 5, RequiredCapacity: 40000, SpareCapacity: 0, Utilization: 0.7},
				"decode-v":  {Replicas: 5, RequiredCapacity: 40000, SpareCapacity: 0, Utilization: 0.5},
			}, false),
		// The split again, per role: targets and utilizations are throughput's,
		// while the published per-role RequiredCapacity is the anchor's 20000.
		Entry("[sat, TA] — per-role sizing refreshed, per-role RC unchanged", shapeBoth,
			map[string]goldenDecision{
				"prefill-v": {Replicas: 5, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.7},
				"decode-v":  {Replicas: 5, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.5},
			}, false),
	)

	// -------------------------------------------------------------------------
	// M6 mirrors B2: disaggregated scale-down, and a veto scoped to one role.
	// -------------------------------------------------------------------------
	DescribeTable("M6 (mirrors B2) — disaggregated scale-down, veto scoped to one role",
		func(shape ballotShape, want map[string]goldenDecision, alsoGreedyByScore bool) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:       "m6",
					Namespace:     "default",
					SpareCapacity: 20000, // model-level; unused in the disaggregated path
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "cheap-p", Role: "prefill", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.2},
						{VariantName: "expensive-p", Role: "prefill", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.1},
						{VariantName: "decode-v", Role: "decode", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000, Utilization: 0.3},
					},
					RoleCapacities: map[string]domain.RoleCapacity{
						"prefill": {Role: "prefill", SpareCapacity: 20000, TotalDemand: 10000},
						"decode":  {Role: "decode", SpareCapacity: 10000, TotalDemand: 10000},
					},
				}
				// Agrees on prefill's spare, reports none on decode. The veto is
				// per-role, so prefill still sheds while decode is frozen.
				ta := &domain.AnalyzerResult{
					ModelID:   "m6",
					Namespace: "default",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "cheap-p", Role: "prefill", PerReplicaCapacity: 10000, Utilization: 0.25, Reason: "T1-ols"},
						{VariantName: "expensive-p", Role: "prefill", PerReplicaCapacity: 10000, Utilization: 0.15, Reason: "T1-ols"},
						{VariantName: "decode-v", Role: "decode", PerReplicaCapacity: 10000, Utilization: 0.35, Reason: "T1-ols"},
					},
					RoleCapacities: map[string]domain.RoleCapacity{
						"prefill": {Role: "prefill", SpareCapacity: 20000, TotalDemand: 10000},
						"decode":  {Role: "decode", SpareCapacity: 0, TotalDemand: 10000},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:       "m6",
					Namespace:     "default",
					Priority:      1,
					Disaggregated: true,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap-p", Role: "prefill", CurrentReplicas: 2, GPUsPerReplica: 1},
						{VariantName: "expensive-p", Role: "prefill", CurrentReplicas: 2, GPUsPerReplica: 1},
						{VariantName: "decode-v", Role: "decode", CurrentReplicas: 3, GPUsPerReplica: 1},
					},
				}), shape, ta)
			}
			ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
			Expect(targetsOf(ca)["expensive-p"]).To(BeNumerically("<", 2),
				"non-vacuity: prefill scale-down must actually run in every shape")
			expectDecisionSet(ca, want)
			if alsoGreedyByScore {
				expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil), want)
			}
		},
		// Transcribed verbatim from B2: prefill sheds cost-desc, expensive-p goes
		// fully at floor(20000/10000)=2, cheap-p is protected as the last prefill
		// variant holding replicas; decode sheds floor(10000/10000)=1.
		Entry("[sat] only — B2's ballot and B2's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"cheap-p":     {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.2},
				"expensive-p": {Replicas: 0, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.1},
				"decode-v":    {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 10000, Utilization: 0.3},
			}, true),
		// Throughput binds, so decode's published SpareCapacity is its own 0 -- and
		// that same zero blocks decode's removal while prefill proceeds.
		Entry("[TA] only — throughput's zero decode spare, prefill unaffected", shapeTAOnly,
			map[string]goldenDecision{
				"cheap-p":     {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.25},
				"expensive-p": {Replicas: 0, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.15},
				"decode-v":    {Replicas: 3, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.35},
			}, false),
		// The role-scoped veto: decode is frozen at 3 by throughput's explicit zero
		// while prefill sheds exactly as it does under one voter. The published
		// decode SpareCapacity is the anchor's 10000, so again the decision reports
		// spare it may not spend.
		Entry("[sat, TA] — decode vetoed, prefill sheds unchanged", shapeBoth,
			map[string]goldenDecision{
				"cheap-p":     {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.2},
				"expensive-p": {Replicas: 0, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.1},
				"decode-v":    {Replicas: 3, RequiredCapacity: 0, SpareCapacity: 10000, Utilization: 0.3},
			}, false),
	)

	// -------------------------------------------------------------------------
	// M7 mirrors C1: namespace quota caps the allocation. GreedyByScore only --
	// CostAware ignores ResourceConstraints entirely (see its doc comment).
	// -------------------------------------------------------------------------
	DescribeTable("M7 (mirrors C1) — namespace quota caps the allocation",
		func(shape ballotShape, want map[string]goldenDecision) {
			build := func() ModelScalingRequest {
				sat := &domain.AnalyzerResult{
					ModelID:          "m7",
					Namespace:        "team-a",
					RequiredCapacity: 50000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.9},
					},
				}
				// Deliberately the weaker voter: 20000/10000 = 2 replicas of demand
				// against saturation's 5, so saturation keeps the sizing bind and the
				// [sat, TA] shape must reproduce the one-voter numbers exactly.
				ta := &domain.AnalyzerResult{
					ModelID:          "m7",
					Namespace:        "team-a",
					RequiredCapacity: 20000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", PerReplicaCapacity: 10000, TotalDemand: 20000, Utilization: 0.95, Reason: "T1-ols"},
					},
				}
				return applyShape(withSatEntry(sat, ModelScalingRequest{
					ModelID:   "m7",
					Namespace: "team-a",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}), shape, ta)
			}
			// team-a has 2 free GPUs (limit 4 - used 2) at 2 GPUs per replica: room
			// for exactly one more replica. An exact whole-replica entitlement, so
			// the unresolved ceil/floor fork in replicasToCover cannot be frozen by
			// this golden either way.
			quotaConstraints := []*ResourceConstraints{
				{
					Pools:          map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 4, Used: 2}}},
				},
			}
			constrained := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, quotaConstraints)
			unconstrained := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))
			Expect(targetsOf(constrained)["v"]).To(BeNumerically("<", targetsOf(unconstrained)["v"]),
				"non-vacuity: the namespace budget must actually bind below unconstrained demand")
			expectDecisionSet(constrained, want)
		},
		// Transcribed verbatim from C1: capped at +1 replica, far below the
		// unconstrained ceil(50000/10000)=5.
		Entry("[sat] only — C1's ballot and C1's numbers, unchanged", shapeSatOnly,
			map[string]goldenDecision{
				"v": {Replicas: 2, RequiredCapacity: 50000, SpareCapacity: 0, Utilization: 0.9},
			}),
		// Throughput binds: unconstrained it would want 2 more replicas, and the
		// quota still allows exactly one.
		Entry("[TA] only — smaller demand, same binding quota", shapeTAOnly,
			map[string]goldenDecision{
				"v": {Replicas: 2, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.95},
			}),
		// A weaker second voter changes nothing: saturation keeps both the anchor
		// and the sizing bind, so this reproduces the [sat]-only decision exactly,
		// Utilization included.
		Entry("[sat, TA] — a weaker voter leaves the quota-capped result alone", shapeBoth,
			map[string]goldenDecision{
				"v": {Replicas: 2, RequiredCapacity: 50000, SpareCapacity: 0, Utilization: 0.9},
			}),
	)
})
