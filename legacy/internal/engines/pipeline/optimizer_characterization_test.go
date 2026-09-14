package pipeline

// Anchor-refactor characterization goldens.
//
// These tests capture the CURRENT (main) behavior of the sat-v2-only optimizer
// path as literal expected values ("goldens"). They exist to prove design
// invariant #7 of the anchor refactor: when exactly one analyzer votes
// (today's default config), the refactored pipeline must produce the same
// decisions as today. The same file rides unchanged onto the anchor-refactor
// branch as its ship gate.
//
// Every case here is single-analyzer, sat-v2-only (one NamedAnalyzerResult
// named domain.SaturationAnalyzerName). Because the expected values are
// captured from current code, every test in this file passes by construction
// against main — a red test here means the fixture is wrong, not that
// production code is buggy.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// goldenDecision is the subset of domain.VariantDecision fields the anchor
// refactor repoints and this branch freezes: target replica count,
// RequiredCapacity, SpareCapacity, and Utilization.
type goldenDecision struct {
	Replicas         int
	RequiredCapacity float64
	SpareCapacity    float64
	Utilization      float64
}

// expectDecisionSet asserts got matches want as a SET keyed by VariantName,
// never by slice order or slice equality: Optimize's per-decision content is
// deterministic but its output slice order is not (map iteration in
// buildDecisionsWithOptimizer, unstable sort in sortByRemainingDesc).
func expectDecisionSet(got []domain.VariantDecision, want map[string]goldenDecision) {
	gm := make(map[string]domain.VariantDecision, len(got))
	gotNames := make([]string, 0, len(got))
	for _, d := range got {
		gm[d.VariantName] = d
		gotNames = append(gotNames, d.VariantName)
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	Expect(gotNames).To(ConsistOf(wantNames), "decision-set variant names must match the golden")

	for name, w := range want {
		d := gm[name]
		Expect(d.TargetReplicas).To(Equal(w.Replicas), "variant %q: TargetReplicas", name)
		Expect(d.RequiredCapacity).To(BeNumerically("~", w.RequiredCapacity, 1e-9), "variant %q: RequiredCapacity", name)
		Expect(d.SpareCapacity).To(BeNumerically("~", w.SpareCapacity, 1e-9), "variant %q: SpareCapacity", name)
		Expect(d.Utilization).To(BeNumerically("~", w.Utilization, 1e-9), "variant %q: Utilization", name)
	}
}

// unlimitedConstraints emulates "no GPU limit" for GreedyByScoreOptimizer,
// which treats absent/empty constraints as zero (deny), not unlimited —
// mirrors the pattern in optimizer_equivalence_test.go.
func unlimitedConstraints(types ...string) []*ResourceConstraints {
	pools := map[string]ResourcePool{}
	for _, t := range types {
		pools[t] = ResourcePool{Limit: 1_000_000}
	}
	return []*ResourceConstraints{{Pools: pools}}
}

var _ = Describe("Anchor refactor characterization goldens (sat-v2-only)", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	Context("harness smoke test", func() {
		It("freezes a trivial no-op decision (proves the harness itself)", func() {
			// No demand (RequiredCapacity=0) and no spare (SpareCapacity=0): neither
			// optimizer has a reason to change the target. This exercises the
			// harness plumbing (fixture -> Optimize -> expectDecisionSet) without
			// depending on any allocation math.
			build := func() ModelScalingRequest {
				r := &domain.AnalyzerResult{
					ModelID:   "smoke",
					Namespace: "default",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.5},
					},
				}
				return withSatEntry(r, ModelScalingRequest{
					ModelID:   "smoke",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
					},
				})
			}
			want := map[string]goldenDecision{
				"v": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.5},
			}

			expectDecisionSet(NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil), want)
			expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100")), want)
		})
	})
})

// Commit 2 — aggregated (RoleBoth) optimizer goldens: scenarios A1-A4.
var _ = Describe("Commit 2 — aggregated (RoleBoth) optimizer goldens", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("A1: single-variant scale-up — demand exceeds capacity", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:          "a1",
				Namespace:        "default",
				RequiredCapacity: 15000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.71},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID:   "a1",
				Namespace: "default",
				Priority:  1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
				},
			})
		}
		// captured from main@9906dac5: ceil(15000/10000)=2 additional -> 2+2=4.
		want := map[string]goldenDecision{
			"v": {Replicas: 4, RequiredCapacity: 15000, SpareCapacity: 0, Utilization: 0.71},
		}
		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))
		caTargets := make(map[string]int, len(ca))
		for _, d := range ca {
			caTargets[d.VariantName] = d.TargetReplicas
		}
		Expect(caTargets["v"]).To(BeNumerically(">", 2), "non-vacuity: scale-up must actually run")
		expectDecisionSet(ca, want)
		expectDecisionSet(gs, want)
	})

	It("A2: single-variant scale-down — cheapest/only variant protected at 1", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:       "a2",
				Namespace:     "default",
				SpareCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000, Utilization: 0.05},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID:   "a2",
				Namespace: "default",
				Priority:  1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 3, GPUsPerReplica: 1},
				},
			})
		}
		// captured from main@9906dac5: floor(30000/10000)=3 would remove all
		// replicas, but the sole (always-cheapest) variant is protected at 1.
		want := map[string]goldenDecision{
			"v": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 30000, Utilization: 0.05},
		}
		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		caTargets := make(map[string]int, len(ca))
		for _, d := range ca {
			caTargets[d.VariantName] = d.TargetReplicas
		}
		Expect(caTargets["v"]).To(BeNumerically("<", 3), "non-vacuity: scale-down must actually run")
		expectDecisionSet(ca, want)
		expectDecisionSet(gs, want)
	})

	It("A3: no-op / at-target — no demand and no spare, nothing changes", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:   "a3",
				Namespace: "default",
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.4},
					{VariantName: "v2", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000, Utilization: 0.55},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID:   "a3",
				Namespace: "default",
				Priority:  1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 2, GPUsPerReplica: 1},
					{VariantName: "v2", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			})
		}
		// This golden is *supposed* to be vacuous (every target == current) — that
		// is the property being frozen: no spurious churn absent demand or spare.
		want := map[string]goldenDecision{
			"v1": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.4},
			"v2": {Replicas: 1, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.55},
		}
		expectDecisionSet(NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil), want)
		expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100", "H100")), want)
	})

	It("A4: multi-variant cost tie-break — cheapest absorbs demand", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:          "a4",
				Namespace:        "default",
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.6},
					{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000, Utilization: 0.3},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID:   "a4",
				Namespace: "default",
				Priority:  1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "cheap", CurrentReplicas: 2, GPUsPerReplica: 1},
					{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			})
		}
		// captured from main@9906dac5: cost-efficiency cheap=5/10000=0.0005 <
		// expensive=15/20000=0.00075 -> cheap absorbs all demand: ceil(5000/10000)=1.
		want := map[string]goldenDecision{
			"cheap":     {Replicas: 3, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.6},
			"expensive": {Replicas: 1, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.3},
		}
		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100", "H100"))
		expectDecisionSet(ca, want)
		expectDecisionSet(gs, want)
	})
})

// Commit 3 — disaggregated (P/D) optimizer goldens: scenarios B1-B2.
var _ = Describe("Commit 3 — disaggregated (P/D) optimizer goldens", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("B1: paired scale-up — equal prefill/decode demand", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:          "b1",
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
			return withSatEntry(r, ModelScalingRequest{
				ModelID:       "b1",
				Namespace:     "default",
				Priority:      1,
				Disaggregated: true,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "prefill-v", Role: "prefill", CurrentReplicas: 1, GPUsPerReplica: 2},
					{VariantName: "decode-v", Role: "decode", CurrentReplicas: 1, GPUsPerReplica: 2},
				},
			})
		}
		// captured from main@9906dac5: alpha=20000/20000=1; n_P=ceil(20000/10000)=2;
		// n_D=ceil(1x20000/10000)=2 -> both roles committed jointly (1+2 each).
		wantCA := map[string]goldenDecision{
			"prefill-v": {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.65},
			"decode-v":  {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.45},
		}
		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		caTargets := make(map[string]int, len(ca))
		for _, d := range ca {
			caTargets[d.VariantName] = d.TargetReplicas
		}
		Expect(caTargets["prefill-v"]).To(BeNumerically(">", 1), "non-vacuity: prefill scale-up must actually run")
		Expect(caTargets["decode-v"]).To(BeNumerically(">", 1), "non-vacuity: decode scale-up must actually run")
		expectDecisionSet(ca, wantCA)

		// GreedyByScore distributes proportionally to per-role demand rather than
		// jointly committing a paired (n_P, n_D) — a different algorithm, so it is
		// pinned as its own golden rather than asserted equal to CostAware's.
		wantGS := map[string]goldenDecision{
			"prefill-v": {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.65},
			"decode-v":  {Replicas: 3, RequiredCapacity: 20000, SpareCapacity: 0, Utilization: 0.45},
		}
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))
		expectDecisionSet(gs, wantGS)
	})

	It("B2: role-scoped scale-down — expensive prefill fully removed, cheap prefill protected", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:       "b2",
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
			return withSatEntry(r, ModelScalingRequest{
				ModelID:       "b2",
				Namespace:     "default",
				Priority:      1,
				Disaggregated: true,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "cheap-p", Role: "prefill", CurrentReplicas: 2, GPUsPerReplica: 1},
					{VariantName: "expensive-p", Role: "prefill", CurrentReplicas: 2, GPUsPerReplica: 1},
					{VariantName: "decode-v", Role: "decode", CurrentReplicas: 3, GPUsPerReplica: 1},
				},
			})
		}
		// captured from main@9906dac5: prefill sheds cost-desc — expensive-p first,
		// floor(20000/10000)=2 removes it fully; cheap-p is protected as the last
		// prefill variant with replicas. decode sheds floor(10000/10000)=1.
		want := map[string]goldenDecision{
			"cheap-p":     {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.2},
			"expensive-p": {Replicas: 0, RequiredCapacity: 0, SpareCapacity: 20000, Utilization: 0.1},
			"decode-v":    {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 10000, Utilization: 0.3},
		}
		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)

		caTargets := make(map[string]int, len(ca))
		for _, d := range ca {
			caTargets[d.VariantName] = d.TargetReplicas
		}
		Expect(caTargets["expensive-p"]).To(BeNumerically("<", 2), "non-vacuity: prefill scale-down must actually run")
		Expect(caTargets["decode-v"]).To(BeNumerically("<", 3), "non-vacuity: decode scale-down must actually run")

		expectDecisionSet(ca, want)
		expectDecisionSet(gs, want)
	})
})

// Commit 4 — quota-constrained optimizer golden: scenario C1.
//
// CostAwareOptimizer ignores ResourceConstraints entirely (unlimited mode —
// see its doc comment), so quota/GPU-limiting behavior only exists on
// GreedyByScoreOptimizer. C1 is a GreedyByScoreOptimizer-only golden.
var _ = Describe("Commit 4 — quota-constrained optimizer golden", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("C1: namespace quota caps a model's allocation below cluster-unconstrained demand", func() {
		build := func() ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID:          "c1",
				Namespace:        "team-a",
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.9},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID:   "c1",
				Namespace: "team-a",
				Priority:  1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2},
				},
			})
		}
		quotaConstraints := []*ResourceConstraints{
			{
				Pools:          map[string]ResourcePool{"A100": {Limit: 100}},
				NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 4, Used: 2}}},
			},
		}

		// captured from main@9906dac5: team-a has 2 free GPUs (cap 4 - used 2) =
		// room for exactly one more 2-GPU replica, well below cluster-unconstrained
		// demand of ceil(50000/10000)=5 additional replicas.
		want := map[string]goldenDecision{
			"v": {Replicas: 2, RequiredCapacity: 50000, SpareCapacity: 0, Utilization: 0.9},
		}
		constrained := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, quotaConstraints)
		unconstrained := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))

		Expect(constrained[0].TargetReplicas).To(BeNumerically("<", unconstrained[0].TargetReplicas),
			"non-vacuity: the namespace budget must actually bind below unconstrained demand")
		expectDecisionSet(constrained, want)
	})
})
