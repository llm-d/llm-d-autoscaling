package pipeline

// Two-analyzer combine characterization golden (saturation + throughput).
//
// The single-analyzer goldens (optimizer_characterization_test.go) freeze the
// single-analyzer, sat-v2-only decision set. This file adds the companion
// two-voting-entry case: a ballot with BOTH saturation and throughput enabled,
// exercised through the full combine (RC/SC) path. It freezes the resulting
// decision SET (keyed by VariantName, same shape as the single-analyzer
// goldens) so the combine arithmetic stays identical to the current
// (pre-refactor) main behavior when two analyzers vote.
//
// Expected values are captured from main@9906dac5 by hand-tracing the combine
// path (the same method used for the single-analyzer goldens). A red test here
// means the refactor changed multi-analyzer combine behavior — which the
// single-vote-equivalence invariant forbids from extending to the two-vote case.
//
// Fixture constraint: every variant is live (>=1 current replica). This keeps
// the later throughput-side proactive-capacity complement (which only emits for
// previously-live-now-zero variants) a no-op for this fixture, so the golden
// stays valid across that change too.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Anchor refactor combine characterization goldens (saturation + throughput)", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("freezes the two-analyzer scale-up decision set (throughput demand dominates)", func() {
		// A [saturation, throughput] ballot, both voting (Enabled) and live.
		// Saturation binds the anchor (identity + sizing); throughput contributes
		// only to the combine's cross-analyzer bottleneck. Single non-disaggregated
		// variant "v" with 2 live replicas (satisfies the all-live constraint).
		//
		//   saturation: RC=5000  -> ceil(5000/10000)  = 1 additional replica
		//   throughput: RC=25000 -> ceil(25000/10000) = 3 additional replicas
		//
		// The scale-up bottleneck is the cross-analyzer MAX, so throughput drives
		// the joint commit to +3 -> target 5. RequiredCapacity/SpareCapacity/
		// Utilization come from the saturation-bound anchor (RC=5000, SC=0, u=0.8).
		build := func() ModelScalingRequest {
			sat := &domain.AnalyzerResult{
				ModelID:          "combine",
				Namespace:        "default",
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.8, Reason: "P1-obs"},
				},
			}
			ta := &domain.AnalyzerResult{
				ModelID:          "combine",
				Namespace:        "default",
				RequiredCapacity: 25000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", PerReplicaCapacity: 10000, Reason: "T1-ols"},
				},
			}
			return ModelScalingRequest{
				ModelID:   "combine",
				Namespace: "default",
				Priority:  1,
				AnalyzerResults: []NamedAnalyzerResult{
					{
						Name:      domain.SaturationAnalyzerName,
						Result:    sat,
						Score:     1.0,
						Remaining: sat.RequiredCapacity,
						Spare:     sat.SpareCapacity,
						Enabled:   true,
						Live:      true,
					},
					{
						Name:      "throughput",
						Result:    ta,
						Score:     1.0,
						Remaining: ta.RequiredCapacity,
						Spare:     ta.SpareCapacity,
						Enabled:   true,
						Live:      true,
					},
				},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
				},
			}
		}
		// captured from main@9906dac5: cross-analyzer bottleneck MAX(1, 3)=3
		// additional -> 2+3=5; anchor (saturation-bound) RC=5000, SC=0, u=0.8.
		want := map[string]goldenDecision{
			"v": {Replicas: 5, RequiredCapacity: 5000, SpareCapacity: 0, Utilization: 0.8},
		}

		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil)
		gs := NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100"))

		// Non-vacuity: saturation alone would scale to 3 (ceil(5000/10000)=1
		// additional); the throughput vote must actually drive the target higher.
		caTargets := make(map[string]int, len(ca))
		for _, d := range ca {
			caTargets[d.VariantName] = d.TargetReplicas
		}
		Expect(caTargets["v"]).To(BeNumerically(">", 3), "non-vacuity: the throughput vote must lift the target above saturation-alone")

		expectDecisionSet(ca, want)
		expectDecisionSet(gs, want)
	})
})
