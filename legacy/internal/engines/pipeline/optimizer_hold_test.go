package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Test 4 (optimizer facet) — a request whose ballot yields no anchor is held
// gracefully. bindingAnchor returns nil for that request, so each optimizer's
// nil-anchor guard skips the model: it produces no decision and never indexes
// into the empty or unbindable ballot (the empty-ballot regression). The
// engine's unconditional applySaturationDecisions then re-affirms the model's
// last-good replicas and emits the scaling metric — that hold-and-emit path is
// exercised end-to-end in the saturation engine's queueing-model refusal test.
var _ = Describe("optimizer hold on an unbindable ballot", func() {
	ctx := context.Background()

	// unbindableRequest builds a request with real variant state but a ballot
	// that cannot bind: a lone throughput entry that is enabled and informative
	// but not live, so bindingAnchor returns nil. VariantStates is populated so
	// the request is non-trivial — the guard must skip on the nil anchor, not on
	// an empty request set.
	unbindableRequest := func() ModelScalingRequest {
		return ModelScalingRequest{
			ModelID:   "m1",
			Namespace: "ns1",
			AnalyzerResults: []NamedAnalyzerResult{{
				Name:    "throughput",
				Enabled: true,
				Live:    false, // not live → no binder → nil anchor
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v1", CurrentReplicas: 2, DesiredReplicas: 2, GPUsPerReplica: 1, Role: domain.RoleBoth},
			},
		}
	}

	It("CostAwareOptimizer produces no decision and does not panic", func() {
		var decisions []domain.VariantDecision
		Expect(func() {
			decisions = NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{unbindableRequest()}, nil)
		}).NotTo(Panic())
		Expect(decisions).To(BeEmpty())
	})

	It("GreedyByScoreOptimizer produces no decision and does not panic", func() {
		var decisions []domain.VariantDecision
		Expect(func() {
			decisions = NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{unbindableRequest()}, nil)
		}).NotTo(Panic())
		Expect(decisions).To(BeEmpty())
	})
})
