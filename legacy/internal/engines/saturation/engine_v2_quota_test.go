package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// satReq builds a ModelScalingRequest with a saturation analyzer entry whose
// variant capacities and replica states drive computeCurrentGPUUsageByNamespace.
func satReq(namespace string, vcs []domain.VariantCapacity, states []domain.VariantReplicaState) pipeline.ModelScalingRequest {
	return pipeline.ModelScalingRequest{
		Namespace: namespace,
		AnalyzerResults: []pipeline.NamedAnalyzerResult{{
			Name:   domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{Namespace: namespace, VariantCapacities: vcs},
		}},
		VariantStates: states,
	}
}

var _ = Describe("computeCurrentGPUUsageByNamespace", func() {
	It("sums currentReplicas*gpusPerReplica per (namespace, accelerator type)", func() {
		usage := computeCurrentGPUUsageByNamespace([]pipeline.ModelScalingRequest{
			satReq("team-a",
				[]domain.VariantCapacity{{VariantName: "v", AcceleratorName: "A100"}},
				[]domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 2}}),
			satReq("team-b",
				[]domain.VariantCapacity{{VariantName: "w", AcceleratorName: "H100"}},
				[]domain.VariantReplicaState{{VariantName: "w", CurrentReplicas: 1, GPUsPerReplica: 4}}),
		})
		Expect(usage["team-a"]).To(HaveKeyWithValue("A100", 4))
		Expect(usage["team-b"]).To(HaveKeyWithValue("H100", 4))
	})

	It("defaults GPUsPerReplica <= 0 to 1", func() {
		usage := computeCurrentGPUUsageByNamespace([]pipeline.ModelScalingRequest{
			satReq("team-a",
				[]domain.VariantCapacity{{VariantName: "v", AcceleratorName: "A100"}},
				[]domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 3, GPUsPerReplica: 0}}),
		})
		Expect(usage["team-a"]).To(HaveKeyWithValue("A100", 3), "GPUsPerReplica defaulted to 1")
	})

	It("marks a namespace active (empty map) even when the request has no saturation entry", func() {
		usage := computeCurrentGPUUsageByNamespace([]pipeline.ModelScalingRequest{{Namespace: "team-z"}})
		Expect(usage).To(HaveKey("team-z"), "zero-replica / no-saturation namespace must still be constrained")
		Expect(usage["team-z"]).To(BeEmpty())
	})
})

var _ = Describe("gpuConstraintProviders", func() {
	clusterQuota := func(name string) *pipeline.DefaultLimiter {
		inv := pipeline.NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4},
		})
		return pipeline.NewDefaultLimiter(name, inv, pipeline.NewGreedyBySaturation())
	}

	It("returns a DefaultLimiter (ConstraintProvider) as its own single provider", func() {
		dl := clusterQuota("q")
		got := gpuConstraintProviders(dl)
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(BeIdenticalTo(dl))
	})

	It("returns each ConstraintProvider constituent of a CompositeLimiter", func() {
		comp := pipeline.NewCompositeLimiter("c", []pipeline.Limiter{clusterQuota("a"), clusterQuota("b")})
		Expect(gpuConstraintProviders(comp)).To(HaveLen(2))
	})

	It("returns nil for a limiter that is not a ConstraintProvider (NoOpLimiter)", func() {
		Expect(gpuConstraintProviders(pipeline.NewNoOpLimiter("noop"))).To(BeNil())
	})
})
