package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// withQMEntry builds a ModelScalingRequest whose single AnalyzerResults entry
// mirrors the shape optimizeQueueingModel constructs in engine_queueing_model.go
// (Name: SaturationAnalyzerName, Enabled: true and Live: true statically), without
// going through the full prepareModelData collection pipeline.
func withQMEntry(r *domain.AnalyzerResult, req pipeline.ModelScalingRequest) pipeline.ModelScalingRequest {
	req.AnalyzerResults = []pipeline.NamedAnalyzerResult{{
		Name:      domain.SaturationAnalyzerName,
		Result:    r,
		Score:     1.0,
		Remaining: r.RequiredCapacity,
		Spare:     r.SpareCapacity,
		Enabled:   true,
		Live:      true,
	}}
	return req
}

// This mirrors the QM path's request shape rather than calling optimizeQueueingModel
// end-to-end (which requires a full prepareModelData collection fixture — a fake
// k8s client and metrics source). It documents and pins the optimizer-side invariant
// ("a QM-shaped Live:true result scales down") but does not, on its own, catch a
// future regression where the Live: true statement is removed from
// engine_queueing_model.go — that would require the heavier end-to-end fixture.
var _ = Describe("Queueing model path stays live under the liveness gate", func() {

	It("still scales down a QM result with spare capacity (regression guard for static Live: true)", func() {
		optimizer := pipeline.NewCostAwareOptimizer()
		r := &domain.AnalyzerResult{
			ModelID:       "model-1",
			Namespace:     "default",
			SpareCapacity: 25000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
			},
		}
		requests := []pipeline.ModelScalingRequest{
			withQMEntry(r, pipeline.ModelScalingRequest{
				ModelID:   "model-1",
				Namespace: "default",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 3},
				},
			}),
		}

		decisions := optimizer.Optimize(context.Background(), requests, nil)
		dm := decisionsByVariant(decisions)

		// Spare=25000, PRC=10000 → floor(25000/10000)=2 removable; would be 0
		// (no scale-down at all) if the QM entry were left at the Enabled/Live
		// zero-values, since the anchor would then fail to bind.
		Expect(dm["v1"].TargetReplicas).To(Equal(1))
	})
})
