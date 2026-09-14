package pipeline

import (
	"context"
	"maps"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// These tests answer: does the fair-share optimizer (GreedyByScore) produce the
// same per-variant targets as the cost-aware optimizer when GPUs are NOT
// constrained? That is the premise behind falling back to "unlimited" when the
// GPU inventory cannot be discovered.
//
// CostAware is run with nil constraints (its unlimited mode). GreedyByScore is
// run with a non-binding huge per-type budget (effectively unlimited), since
// GreedyByScore treats absent/empty constraints as zero (deny), not unlimited.
//
// NOTE: the optimizers decrement AnalyzerResult.Remaining in place, so each
// optimizer must be given a FRESHLY built request set (build() is called twice).
//
// Scope: these cases cover the non-disaggregated scale-up path (the common
// case). Equivalence on scale-down and on P/D-disaggregated (per-role) requests
// is not asserted here — the fallback's correctness does not depend on it, since
// the cost-aware optimizer is itself a valid unlimited optimizer for those paths
// (it is the optimizer the engine already runs when enableLimiter is false).
var _ = Describe("GreedyByScore vs CostAware when GPUs are unconstrained", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	hugeConstraints := func(types ...string) []*ResourceConstraints {
		pools := map[string]ResourcePool{}
		for _, t := range types {
			pools[t] = ResourcePool{Limit: 1_000_000}
		}
		return []*ResourceConstraints{{Pools: pools}}
	}

	targets := func(decisions []domain.VariantDecision) map[string]int {
		m := make(map[string]int, len(decisions))
		for _, d := range decisions {
			m[d.VariantName] = d.TargetReplicas
		}
		return m
	}

	keys := func(m map[string]int) []string {
		return slices.Sorted(maps.Keys(m))
	}

	// compare builds two independent request sets and reports both optimizers'
	// targets. Returns (costAware, greedyByScore) for the caller to assert on.
	run := func(build func() []ModelScalingRequest, types ...string) (map[string]int, map[string]int) {
		ca := targets(NewCostAwareOptimizer().Optimize(ctx, build(), nil))
		gs := targets(NewGreedyByScoreOptimizer().Optimize(ctx, build(), hugeConstraints(types...)))
		return ca, gs
	}

	It("single model, single variant: identical", func() {
		build := func() []ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID: "m", Namespace: "ns", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			return []ModelScalingRequest{withSatEntry(r, ModelScalingRequest{
				ModelID: "m", Namespace: "ns", Priority: 1,
				VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2}},
			})}
		}
		ca, gs := run(build, "A100")
		Expect(keys(gs)).To(Equal(keys(ca)))
		// Anchor the absolute target so the equivalence check can't pass vacuously
		// (both staying at 1): ceil(50000/10000)=5 additional -> 1+5=6.
		Expect(ca["v"]).To(Equal(6))
		Expect(gs["v"]).To(Equal(ca["v"]), "ca=%d gs=%d", ca["v"], gs["v"])
	})

	It("single model, multiple variants (cost-efficiency selection): identical", func() {
		build := func() []ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID: "m", Namespace: "ns", RequiredCapacity: 25000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "pricey", AcceleratorName: "H100", Cost: 15, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			return []ModelScalingRequest{withSatEntry(r, ModelScalingRequest{
				ModelID: "m", Namespace: "ns", Priority: 1,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "pricey", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			})}
		}
		ca, gs := run(build, "A100", "H100")
		Expect(keys(gs)).To(Equal(keys(ca)))
		// Anchor absolute targets: cheap is most cost-efficient (5/10000 < 15/20000)
		// so it absorbs demand — ceil(25000/10000)=3 -> 1+3=4; pricey stays at 1.
		Expect(ca["cheap"]).To(Equal(4))
		Expect(ca["pricey"]).To(Equal(1))
		Expect(gs["cheap"]).To(Equal(ca["cheap"]), "cheap ca=%d gs=%d", ca["cheap"], gs["cheap"])
		Expect(gs["pricey"]).To(Equal(ca["pricey"]), "pricey ca=%d gs=%d", ca["pricey"], gs["pricey"])
	})

	It("multiple models scaling up at once: report both", func() {
		mk := func(id, variant, accel string, req float64, prio float64) ModelScalingRequest {
			r := &domain.AnalyzerResult{
				ModelID: id, Namespace: "ns", RequiredCapacity: req,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: variant, AcceleratorName: accel, Cost: 5, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			return withSatEntry(r, ModelScalingRequest{
				ModelID: id, Namespace: "ns", Priority: prio,
				VariantStates: []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: 1, GPUsPerReplica: 1}},
			})
		}
		build := func() []ModelScalingRequest {
			return []ModelScalingRequest{
				mk("a", "av", "A100", 30000, 1),
				mk("b", "bv", "A100", 50000, 1),
			}
		}
		ca, gs := run(build, "A100")
		GinkgoWriter.Printf("multi-model unconstrained: cost-aware=%v  greedy-by-score=%v\n", ca, gs)
		// Concrete targets prove real scale-up (not both trivially staying at 1):
		// a: ceil(30000/10000)=3 -> 1+3=4 ; b: ceil(50000/10000)=5 -> 1+5=6.
		Expect(ca["av"]).To(Equal(4))
		Expect(ca["bv"]).To(Equal(6))
		Expect(gs["av"]).To(Equal(ca["av"]), "av ca=%d gs=%d", ca["av"], gs["av"])
		Expect(gs["bv"]).To(Equal(ca["bv"]), "bv ca=%d gs=%d", ca["bv"], gs["bv"])
	})
})
