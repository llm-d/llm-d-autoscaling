package pipeline

// Liveness fixes for the multi-vote combine:
//
//   - Scale-up gating: votingResults prunes on Enabled && Live, not Enabled
//     alone. Were it Enabled alone, an enabled analyzer that had gone stale
//     would still seed initRoleState/roleBottleneckReplicas with its last
//     (possibly huge) Result and force a spurious scale-up. Scale-down is
//     Live-gated at point of use; this makes scale-up equally robust.
//   - Abstention: needsScaleDownForRole abstains a live voter that simply
//     doesn't decompose a given role (map-miss), rather than reading the miss
//     as "spare == 0" and vetoing every role a coarser voter never sized.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("liveness gates on the multi-vote combine", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("a stale-but-enabled analyzer's demand no longer forces scale-up (VG-up)", func() {
		// saturation: live, no demand (RC=0) -- alone, no scale-up is needed.
		// throughput: enabled but stale (Live=false) with a huge RC=100000 that
		// would force a massive scale-up if votingResults still pruned on
		// Enabled alone (initRoleState/roleBottleneckReplicas would seed and
		// combine its stale Result). With the VG-up fix, throughput is excluded
		// from the voting slice entirely, so its stale demand cannot contribute.
		sat := &domain.AnalyzerResult{
			ModelID:          "stale",
			Namespace:        "default",
			RequiredCapacity: 0,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 1, PerReplicaCapacity: 100, Reason: "P1-obs"},
			},
		}
		ta := &domain.AnalyzerResult{
			ModelID:          "stale",
			Namespace:        "default",
			RequiredCapacity: 100000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v", PerReplicaCapacity: 100, Reason: "T1-ols"},
			},
		}
		req := ModelScalingRequest{
			ModelID:   "stale",
			Namespace: "default",
			Priority:  1,
			AnalyzerResults: []NamedAnalyzerResult{
				{Name: domain.SaturationAnalyzerName, Result: sat, Score: 1.0, Remaining: 0, Enabled: true, Live: true},
				{Name: "throughput", Result: ta, Score: 1.0, Remaining: 100000, Enabled: true, Live: false}, // stale
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		}

		ca := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
		var target int
		for _, d := range ca {
			if d.VariantName == "v" {
				target = d.TargetReplicas
			}
		}
		Expect(target).To(Equal(1), "a stale-but-enabled analyzer's demand must not force scale-up")
	})

	Describe("needsScaleDownForRole", func() {
		It("abstains a live voter that doesn't decompose this role, rather than vetoing", func() {
			// saturation: disaggregated, RoleSpare[prefill]=RoleSpare[decode]=20000
			// (positive -- would allow scale-down on either role).
			// throughput: live, non-disaggregated -- initRoleState seeds it with
			// only RoleSpare[RoleBoth]; it has no opinion on "prefill" at all.
			sat := makeNamedPD("saturation", 0, 0, 20000, 20000, 0, 0, 10000, 10000)
			ta := makeNamed("throughput", 0, 5000, "v", 100.0)
			s := []NamedAnalyzerResult{sat, ta}
			_, _ = initRoleState(s) // populates ta.RoleSpare[RoleBoth]=5000, no "prefill" key

			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue(),
				"throughput has no opinion on prefill (only RoleBoth) and must abstain, not veto")
		})

		It("still vetoes when a live voter DOES decompose the role and reports no spare", func() {
			sat := makeNamedPD("saturation", 0, 0, 20000, 20000, 0, 0, 10000, 10000)
			ta := makeNamedPD("throughput", 0, 0, 0, 20000, 0, 0, 10000, 10000) // prefill spare = 0
			s := []NamedAnalyzerResult{sat, ta}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse(),
				"a live voter that DOES size this role and reports no spare still vetoes")
		})
	})
})
