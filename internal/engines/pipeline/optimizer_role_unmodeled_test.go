package pipeline

// C12 (AD8 option (b)): a role a voter has no demand model for must abstain
// from the ballot, not vote a structural zero/whole-fleet-spare as if it were
// measured. Covers the three ballot-construction guards (votesFromPickerState,
// votesFromRoleSpare, votesFromTotalDemand) and the observability fallback in
// buildDecisionsWithOptimizer.
//
// This closes only the drain (regime (ii), decode RC==0 with spare -- the
// scale-down arm): abstaining removes TA's whole-fleet prefill "spare" from
// the safe-removal ballot entirely, so there is no vote left to drain it. It
// does NOT close the freeze (regime (i), decode RC>0 -- the scale-up arm):
// with TA as the only voter, an abstention and a 0-vote both leave
// roleBottleneckReplicas at 0 (combineVotes returns binder<0 either way), so
// prefill stays frozen at its current count exactly as before. That freeze
// non-regression spec exists to catch a future change that closes this gap by
// accident, not to exercise new behavior.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// taOnlyPD builds a single-voter throughput-only ballot for a disaggregated
// model: prefill tagged ReasonRoleUnmodeled with a whole-fleet SpareCapacity
// (mirroring aggregateRoleCapacities' structural zero-demand entry), decode
// carrying real, analyzer-supplied RequiredCapacity (decodeRC selects the
// regime: 0 is the drain/scale-down arm, >0 the freeze/scale-up arm). Decode's
// own SpareCapacity is irrelevant to prefill's behavior and left at 0.
const taOnlyPDPRC = 10000.0 // per-replica capacity shared by both roles' variants

func taOnlyPD(prefillReplicas int, decodeRC float64) NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name: "throughput",
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "pf", Role: domain.RolePrefill, PerReplicaCapacity: taOnlyPDPRC, Reason: "T1-ols"},
				{VariantName: "dc", Role: domain.RoleDecode, PerReplicaCapacity: taOnlyPDPRC, Reason: "T1-ols"},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RolePrefill: {
					Role:             domain.RolePrefill,
					TotalDemand:      0,
					RequiredCapacity: 0,
					SpareCapacity:    float64(prefillReplicas) * taOnlyPDPRC, // whole fleet -- the structural bug this closes
					Reason:           ReasonRoleUnmodeled,
				},
				domain.RoleDecode: {
					Role:             domain.RoleDecode,
					TotalDemand:      decodeRC,
					RequiredCapacity: decodeRC,
				},
			},
		},
		Enabled: true,
		Live:    true,
	}
}

// nonLiveSatPD builds a non-live saturation entry with real per-role figures.
// VG-up prunes it from votingResults, and votesFromRoleSpare/votesFromPickerState
// skip non-live entries independently of Reason -- present here only to mirror
// the "[sat, TA] with saturation forced non-live" shape the design doc asks for.
func nonLiveSatPD(prefillReplicas int, prefillPRC float64) NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "pf", Role: domain.RolePrefill, PerReplicaCapacity: prefillPRC, Reason: "P1-obs"},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RolePrefill: {Role: domain.RolePrefill, RequiredCapacity: 0, SpareCapacity: float64(prefillReplicas) * prefillPRC},
			},
		},
		Enabled: true,
		Live:    false, // stale: excluded from votingResults (VG-up) and from every Live-gated vote collector
	}
}

var _ = Describe("C12: a structurally unmodeled role abstains rather than votes", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	pdStates := func(prefillReplicas, decodeReplicas int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: "pf", Role: domain.RolePrefill, CurrentReplicas: prefillReplicas, GPUsPerReplica: 1},
			{VariantName: "dc", Role: domain.RoleDecode, CurrentReplicas: decodeReplicas, GPUsPerReplica: 1},
		}
	}

	Describe("regime (ii), the drain: closes", func() {
		It("stops draining prefill to its floor when TA is the sole voter", func() {
			// decode RC==0 with spare -> both roles' RequiredCapacity are 0 ->
			// anyRoleNeedsScaleUp is false -> the scale-down arm.
			req := ModelScalingRequest{
				ModelID:         "m1",
				Namespace:       "default",
				AnalyzerResults: []NamedAnalyzerResult{taOnlyPD(3, 0)},
				VariantStates:   pdStates(3, 3),
			}
			decisions := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
			dm := decisionMap(decisions)
			Expect(dm["pf"].TargetReplicas).To(Equal(3), "prefill must not be shed: TA has no demand model for it and must abstain")
			Expect(dm["pf"].Action).To(Equal(domain.ActionNoChange))
		})

		It("mirrors with saturation present but non-live", func() {
			req := ModelScalingRequest{
				ModelID:   "m1",
				Namespace: "default",
				AnalyzerResults: []NamedAnalyzerResult{
					nonLiveSatPD(3, taOnlyPDPRC),
					taOnlyPD(3, 0),
				},
				VariantStates: pdStates(3, 3),
			}
			decisions := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
			dm := decisionMap(decisions)
			Expect(dm["pf"].TargetReplicas).To(Equal(3), "a non-live saturation entry must not change the outcome: it is excluded by VG-up before the ballot forms")
		})
	})

	Describe("regime (i), the freeze: does not close, and must not silently start closing", func() {
		It("still leaves prefill frozen at its current count when decode needs scale-up", func() {
			// decode RC>0 -> anyRoleNeedsScaleUp is true -> the scale-up arm.
			req := ModelScalingRequest{
				ModelID:         "m1",
				Namespace:       "default",
				AnalyzerResults: []NamedAnalyzerResult{taOnlyPD(2, 20000)},
				VariantStates:   pdStates(2, 2),
			}
			decisions := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
			dm := decisionMap(decisions)
			Expect(dm["pf"].TargetReplicas).To(Equal(2), "regime (i) has no second voter to un-suppress and cannot close with this mechanism")
		})

		It("still leaves prefill frozen at zero from a cold start", func() {
			req := ModelScalingRequest{
				ModelID:         "m1",
				Namespace:       "default",
				AnalyzerResults: []NamedAnalyzerResult{taOnlyPD(0, 20000)},
				VariantStates:   pdStates(0, 2),
			}
			decisions := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
			dm := decisionMap(decisions)
			Expect(dm["pf"].TargetReplicas).To(Equal(0))
		})
	})

	Describe("observability: a decision built from an unmodeled role falls back to model-level totals", func() {
		It("does not publish the structurally-wrong per-role RequiredCapacity/SpareCapacity", func() {
			req := ModelScalingRequest{
				ModelID:         "m1",
				Namespace:       "default",
				AnalyzerResults: []NamedAnalyzerResult{taOnlyPD(3, 0)},
				VariantStates:   pdStates(3, 3),
			}
			// Model-level scalars, distinct from the per-role prefill figures,
			// so a fallback to them is distinguishable from the per-role ones.
			req.AnalyzerResults[0].Result.RequiredCapacity = 111
			req.AnalyzerResults[0].Result.SpareCapacity = 222

			decisions := NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{req}, nil)
			dm := decisionMap(decisions)
			Expect(dm["pf"].RequiredCapacity).To(Equal(111.0), "must fall back to the model-level total, not RoleCapacities[prefill].RequiredCapacity (0)")
			Expect(dm["pf"].SpareCapacity).To(Equal(222.0), "must fall back to the model-level total, not RoleCapacities[prefill].SpareCapacity (30000, the whole fleet)")
		})
	})
})

var _ = Describe("ReasonRoleUnmodeled abstention at the ballot-collector level", func() {
	Describe("votesFromPickerState", func() {
		It("abstains an entry whose RoleCapacity for role carries ReasonRoleUnmodeled", func() {
			s := []NamedAnalyzerResult{{
				Result: &domain.AnalyzerResult{
					RoleCapacities: map[string]domain.RoleCapacity{"prefill": {Reason: ReasonRoleUnmodeled}},
				},
			}}
			state := RolePairedState{{"prefill": 500}}
			votes := votesFromPickerState(s, state, "prefill", "v")
			Expect(votes).To(BeEmpty())
		})

		It("still counts a sibling entry with a real (untagged) RoleCapacity for the same role", func() {
			s := []NamedAnalyzerResult{
				{Result: &domain.AnalyzerResult{
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {Reason: ReasonRoleUnmodeled}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				}},
				{Result: &domain.AnalyzerResult{
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				}},
			}
			state := RolePairedState{{"prefill": 500}, {"prefill": 200}}
			votes := votesFromPickerState(s, state, "prefill", "v")
			Expect(votes).To(HaveLen(1))
			Expect(votes[0].Index).To(Equal(1))
		})
	})

	Describe("votesFromRoleSpare", func() {
		It("abstains a live entry whose RoleCapacity for role carries ReasonRoleUnmodeled", func() {
			s := []NamedAnalyzerResult{{
				Live: true,
				Result: &domain.AnalyzerResult{
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {Reason: ReasonRoleUnmodeled}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				},
				RoleSpare: map[string]float64{"prefill": 30000},
			}}
			votes := votesFromRoleSpare(s, "prefill", "v")
			Expect(votes).To(BeEmpty())
		})
	})

	Describe("votesFromTotalDemand", func() {
		It("abstains an entry whose RoleCapacity for role carries ReasonRoleUnmodeled", func() {
			s := []NamedAnalyzerResult{{
				Result: &domain.AnalyzerResult{
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {TotalDemand: 999, Reason: ReasonRoleUnmodeled}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				},
			}}
			votes := votesFromTotalDemand(s, "prefill", "v")
			Expect(votes).To(BeEmpty())
		})

		It("still prices a role from an entry that DOES model it, unaffected by a sibling's abstention", func() {
			// The rescale-path mirror of the "[sat, TA]" case: saturation still
			// prices prefill's demand; TA's abstention on the same role must not
			// suppress or dilute that vote.
			s := []NamedAnalyzerResult{
				{Result: &domain.AnalyzerResult{ // TA: no demand model for prefill
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {TotalDemand: 0, Reason: ReasonRoleUnmodeled}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				}},
				{Result: &domain.AnalyzerResult{ // saturation: real prefill demand
					RoleCapacities:    map[string]domain.RoleCapacity{"prefill": {TotalDemand: 5000}},
					VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
				}},
			}
			votes := votesFromTotalDemand(s, "prefill", "v")
			Expect(votes).To(HaveLen(1))
			Expect(votes[0].Index).To(Equal(1))
			Expect(votes[0].Value).To(Equal(50.0)) // 5000/100
		})
	})
})
