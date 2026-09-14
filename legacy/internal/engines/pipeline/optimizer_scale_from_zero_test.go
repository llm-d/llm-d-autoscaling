package pipeline

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Test 10 — scale-from-zero selection under a throughput-only ([TA]) ballot.
//
// End-to-end companion to the throughput analyzer's scale-from-zero complement:
// the analyzer re-emits a PRC-only VariantCapacity for a previously-live variant
// now at zero replicas, and the pipeline must consume it. The saturation entry is
// present only as the (a)/identity carrier (not voting), so bindingAnchor merges
// its per-variant identity (Cost/AcceleratorName/Role/ReplicaCount) with the
// throughput analyzer's per-replica capacity. A previously-live-now-zero variant
// then has a positive merged PerReplicaCapacity and is a viable scale-up target,
// while a never-seen variant the throughput analyzer did not emit stays at
// PerReplicaCapacity 0 and is skipped — the picker selects by viability, not by
// cost ranking (the never-seen decoy here is strictly cheaper).
var _ = Describe("scale-from-zero selection under a throughput-only ballot", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// taOnlyBallot builds a [TA]-only ballot for one model:
	//   - saturation entry present as the (a) carrier only (Enabled:false): it emits
	//     both variants' identity (cost/accelerator/role/replica-count) but does not
	//     vote or bind.
	//   - throughput entry as the sole voting+binding analyzer: model-level demand
	//     (Remaining) drives scale-up; VariantCapacities is the PRC-only emission for
	//     the previously-live variant only.
	taOnlyBallot := func(demand float64) ModelScalingRequest {
		satIdentity := &domain.AnalyzerResult{
			ModelID:    "m",
			Namespace:  "default",
			AnalyzedAt: time.Now(),
			VariantCapacities: []domain.VariantCapacity{
				// previously-live-now-zero: identity carrier, no PRC of its own.
				{VariantName: "revived", AcceleratorName: "H100", Cost: 10.0, Role: "", ReplicaCount: 0},
				// never-seen-now-zero: strictly cheaper, but TA emits no PRC for it.
				{VariantName: "cold", AcceleratorName: "A100", Cost: 5.0, Role: "", ReplicaCount: 0},
			},
		}
		taResult := &domain.AnalyzerResult{
			AnalyzerName:     "throughput",
			ModelID:          "m",
			Namespace:        "default",
			AnalyzedAt:       time.Now(),
			RequiredCapacity: demand,
			VariantCapacities: []domain.VariantCapacity{
				// PRC-only scale-from-zero emission (mirrors the analyzer's T-sfz row).
				{VariantName: "revived", PerReplicaCapacity: 12000, Reason: "T-sfz"},
			},
		}
		return ModelScalingRequest{
			ModelID:   "m",
			Namespace: "default",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "revived", CurrentReplicas: 0},
				{VariantName: "cold", CurrentReplicas: 0},
			},
			AnalyzerResults: []NamedAnalyzerResult{
				{
					Name:    domain.SaturationAnalyzerName,
					Result:  satIdentity,
					Enabled: false, // (a) carrier only — does not vote or bind
					Live:    false,
				},
				{
					Name:      "throughput",
					Result:    taResult,
					Remaining: demand,
					Enabled:   true,
					Live:      true,
				},
			},
		}
	}

	It("raises the previously-live variant above zero and skips the never-seen one", func() {
		requests := []ModelScalingRequest{taOnlyBallot(12000)}

		dm := decisionMap(NewCostAwareOptimizer().Optimize(ctx, requests, nil))

		Expect(dm).To(HaveKey("revived"))
		// ceil(12000 / 12000) = 1 replica added from zero — selected as the viable
		// scale-from-zero source despite being the more expensive variant.
		Expect(dm["revived"].TargetReplicas).To(BeNumerically(">", 0))
		// The cheaper never-seen variant has no per-replica capacity → not selectable.
		Expect(dm["cold"].TargetReplicas).To(Equal(0))
	})
})
