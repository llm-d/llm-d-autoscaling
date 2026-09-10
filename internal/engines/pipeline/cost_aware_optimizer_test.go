package pipeline

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// withSatEntry adds a single-saturation AnalyzerResults to req, initialised from r.
// Used by test fixtures so the optimizer's binding anchor resolves to the
// saturation entry. Enabled marks it as a voting member of the ballot; Live
// lets it bind as the anchor (the engine populates both in production).
func withSatEntry(r *domain.AnalyzerResult, req ModelScalingRequest) ModelScalingRequest {
	if r != nil {
		req.AnalyzerResults = []NamedAnalyzerResult{{
			Name:      domain.SaturationAnalyzerName,
			Result:    r,
			Remaining: r.RequiredCapacity,
			Spare:     r.SpareCapacity,
			Enabled:   true,
			Live:      true,
		}}
	}
	return req
}

var _ = Describe("CostAwareOptimizer", func() {

	var (
		optimizer *CostAwareOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewCostAwareOptimizer()
		ctx = context.Background()
	})

	It("should return 'cost-aware' as name", func() {
		Expect(optimizer.Name()).To(Equal("cost-aware"))
	})

	Context("Scale-Up", func() {

		It("should add replicas to most cost-efficient variant", func() {
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 2},
						{VariantName: "expensive", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cost-efficiency: cheap=5/10000=0.0005, expensive=15/20000=0.00075
			// cheap is more efficient → ceil(5000/10000) = 1 replica added
			Expect(dm["cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["expensive"].TargetReplicas).To(Equal(1))
		})

		It("populates decision observability fields (utilization/required/spare) from the analyzer result", func() {
			// Regression: these three feed wva_saturation_utilization / wva_required_capacity /
			// wva_spare_capacity. If buildDecisionsWithOptimizer stops copying them, the gauges read 0.
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				RequiredCapacity: 5000,
				SpareCapacity:    1200,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.42},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2},
					},
				}),
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, nil))
			Expect(dm["v1"].Utilization).To(Equal(0.42))
			Expect(dm["v1"].RequiredCapacity).To(Equal(5000.0)) // model-level (no RoleCapacities)
			Expect(dm["v1"].SpareCapacity).To(Equal(1200.0))    // tokens, companion to required
		})

		It("uses per-role required/spare capacity for P/D-disaggregated models", func() {
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				RequiredCapacity: 9999, // model-level; must NOT be used for role-scoped variants
				SpareCapacity:    8888,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 100, SpareCapacity: 10},
					"decode":  {Role: "decode", RequiredCapacity: 200, SpareCapacity: 20},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p", Role: "prefill", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.3},
					{VariantName: "d", Role: "decode", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.6},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "p", Role: "prefill", CurrentReplicas: 1},
						{VariantName: "d", Role: "decode", CurrentReplicas: 1},
					},
				}),
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, nil))
			Expect(dm["p"].RequiredCapacity).To(Equal(100.0))
			Expect(dm["p"].SpareCapacity).To(Equal(10.0))
			Expect(dm["d"].RequiredCapacity).To(Equal(200.0))
			Expect(dm["d"].SpareCapacity).To(Equal(20.0))
		})

		It("maps an empty-role variant to the \"both\" RoleCapacities entry", func() {
			// Role "" normalizes to RoleBoth, so the "both" entry (not the model-level
			// totals) must be used. Model-level 9999/8888 are decoys.
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				RequiredCapacity: 9999,
				SpareCapacity:    8888,
				RoleCapacities: map[string]domain.RoleCapacity{
					"both": {Role: "both", RequiredCapacity: 300, SpareCapacity: 30},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", Role: "", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.5},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", Role: "", CurrentReplicas: 1},
					},
				}),
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, nil))
			Expect(dm["v"].RequiredCapacity).To(Equal(300.0)) // "both" entry, not model-level 9999
			Expect(dm["v"].SpareCapacity).To(Equal(30.0))     // "both" entry, not model-level 8888
		})

		It("should not skip variants with pending replicas", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 2, PendingReplicas: 1},
						{VariantName: "mid", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cheap has pending but is more cost-efficient → still gets the allocation
			// (analyzer already accounts for pending capacity in supply)
			Expect(dm["cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["mid"].TargetReplicas).To(Equal(1))
		})

		It("should skip variants with zero capacity", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "zero-cap", Cost: 1.0, ReplicaCount: 0, PerReplicaCapacity: 0},
					{VariantName: "normal", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "zero-cap", CurrentReplicas: 0},
						{VariantName: "normal", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["normal"].TargetReplicas).To(Equal(2))
			Expect(dm["zero-cap"].TargetReplicas).To(Equal(0))
		})

		It("should spread across multiple variants when needed", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 25000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1},
						{VariantName: "mid", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cheap is more efficient (5/10000=0.0005 vs 10/15000=0.00067)
			// cheap gets ceil(25000/10000)=3 replicas → adds 3, remaining=25000-30000<0
			Expect(dm["cheap"].TargetReplicas).To(Equal(4))
		})
	})

	Context("Scale-Down", func() {

		It("should remove from most expensive variant first", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 15000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 3},
						{VariantName: "expensive", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: floor(15000/20000)=0 → can't remove full replica
			// cheap: expensive still has replicas → not protected, floor(15000/10000)=1 → remove 1
			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(2))
		})

		It("should protect cheapest variant at 1 replica", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 1},
						{VariantName: "cheap", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive (not cheapest) → can go to 0: floor(30000/20000)=1
			// cheap (cheapest) → protected at 1: minReplicas=1, removable=0
			Expect(dm["expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["cheap"].TargetReplicas).To(Equal(1))
		})

		It("should remove cheap variant when expensive replica capacity exceeds spare", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 15000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 1},
						{VariantName: "cheap", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: floor(15000/20000)=0 → can't remove
			// cheap: expensive still has replicas → not protected, removable=1, floor(15000/10000)=1 → remove 1
			Expect(dm["expensive"].TargetReplicas).To(Equal(1))
			Expect(dm["cheap"].TargetReplicas).To(Equal(0))
		})

		It("should cascade scale-down across variants", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
					{VariantName: "mid", Cost: 10.0, ReplicaCount: 2, PerReplicaCapacity: 15000},
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 2},
						{VariantName: "mid", CurrentReplicas: 2},
						{VariantName: "cheap", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// sorted by cost DESC: expensive(15), mid(10), cheap(5)
			// expensive: floor(50000/20000)=2, removable=2 → remove 2. spare=10000
			// mid: floor(10000/15000)=0 → skip
			// cheap: mid still has replicas → not protected, floor(10000/10000)=1 → remove 1. spare=0
			Expect(dm["expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["mid"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(1))
		})

		It("should shed only the role with spare for disaggregated models", func() {
			// Prefill is saturated (zero role spare) and is the more expensive role;
			// decode has spare. Model-level SpareCapacity aggregates both roles, so a
			// role-blind scale-down would trim the expensive prefill. Role-aware
			// scale-down must leave the saturated prefill untouched and shed decode.
			r := &domain.AnalyzerResult{
				SpareCapacity: 20000, // aggregate (decode's spare) — gates scale-down
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill", Role: "prefill", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "decode", Role: "decode", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", SpareCapacity: 0},
					"decode":  {Role: "decode", SpareCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill", Role: "prefill", CurrentReplicas: 2},
						{VariantName: "decode", Role: "decode", CurrentReplicas: 3},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// Prefill (saturated role) is never trimmed despite being most expensive.
			Expect(dm["prefill"].TargetReplicas).To(Equal(2))
			Expect(dm["prefill"].Action).To(Equal(domain.ActionNoChange))
			// Decode sheds its own spare: floor(20000/10000)=2 → 3 - 2 = 1.
			Expect(dm["decode"].TargetReplicas).To(Equal(1))
			Expect(dm["decode"].Action).To(Equal(domain.ActionScaleDown))
		})

		It("should shed each role by its own spare when both have slack", func() {
			// Both roles have spare, but different amounts. Each role must shed by
			// its own per-role spare, not the aggregate.
			r := &domain.AnalyzerResult{
				SpareCapacity: 30000, // aggregate — gates scale-down
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill", Role: "prefill", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
					{VariantName: "decode", Role: "decode", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", SpareCapacity: 10000}, // 1 replica
					"decode":  {Role: "decode", SpareCapacity: 20000},  // 2 replicas
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill", Role: "prefill", CurrentReplicas: 3},
						{VariantName: "decode", Role: "decode", CurrentReplicas: 3},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// prefill sheds floor(10000/10000)=1 → 3 - 1 = 2.
			Expect(dm["prefill"].TargetReplicas).To(Equal(2))
			// decode sheds floor(20000/10000)=2 → 3 - 2 = 1.
			Expect(dm["decode"].TargetReplicas).To(Equal(1))
		})

		It("sheds within a single role when only one role has capacity", func() {
			// Only decode has a RoleCapacities entry (e.g. a prefill data-collection
			// gap). The one-iteration path must shed decode against its own spare and
			// keep the cheapest decode variant at one replica.
			r := &domain.AnalyzerResult{
				SpareCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode-exp", Role: "decode", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-cheap", Role: "decode", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"decode": {Role: "decode", SpareCapacity: 20000}, // 2 replicas
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "decode-exp", Role: "decode", CurrentReplicas: 1},
						{VariantName: "decode-cheap", Role: "decode", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// Most-expensive-first removes decode-exp (1→0); decode-cheap is then the
			// last with replicas in the role → protected at one.
			Expect(dm["decode-exp"].TargetReplicas).To(Equal(0))
			Expect(dm["decode-cheap"].TargetReplicas).To(Equal(1))
		})

		It("leaves an empty-role variant untouched when RoleCapacities is per-role", func() {
			// A variant with Role "" canonicalizes to RoleBoth, which has no entry in a
			// prefill/decode RoleCapacities map — so the per-role sheds never include it.
			// The design assumes one role per variant; this pins the defensive behavior
			// if that assumption is violated: the orphan is left as-is.
			r := &domain.AnalyzerResult{
				SpareCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill", Role: "prefill", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "decode", Role: "decode", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
					{VariantName: "orphan", Role: "", Cost: 20.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", SpareCapacity: 0},
					"decode":  {Role: "decode", SpareCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill", Role: "prefill", CurrentReplicas: 2},
						{VariantName: "decode", Role: "decode", CurrentReplicas: 3},
						{VariantName: "orphan", Role: "", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// orphan (role "") matches no per-role set → never trimmed, despite being
			// the most expensive variant.
			Expect(dm["orphan"].TargetReplicas).To(Equal(2))
			Expect(dm["orphan"].Action).To(Equal(domain.ActionNoChange))
			// prefill saturated → untouched; decode sheds its own spare → 3 - 2 = 1.
			Expect(dm["prefill"].TargetReplicas).To(Equal(2))
			Expect(dm["decode"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Steady State", func() {

		It("should return no-change when no scaling signal", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 0,
				SpareCapacity:    0,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Action).To(Equal(domain.ActionNoChange))
			Expect(decisions[0].TargetReplicas).To(Equal(2))
		})

		It("should skip requests with no saturation entry", func() {
			requests := []ModelScalingRequest{
				{ModelID: "model-1", Namespace: "default"},
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			Expect(decisions).To(BeEmpty())
		})
	})

	Context("Multi-Model", func() {

		It("should process models independently", func() {
			r1 := &domain.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "m1-v1", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			r2 := &domain.AnalyzerResult{
				SpareCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "m2-v1", Cost: 10.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r1, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "m1-v1", CurrentReplicas: 1},
					},
				}),
				withSatEntry(r2, ModelScalingRequest{
					ModelID:   "model-2",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "m2-v1", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["m1-v1"].Action).To(Equal(domain.ActionScaleUp))
			Expect(dm["m1-v1"].TargetReplicas).To(Equal(2))
			Expect(dm["m2-v1"].Action).To(Equal(domain.ActionScaleDown))
			Expect(dm["m2-v1"].TargetReplicas).To(Equal(1))
		})
	})

	Context("Decision Metadata", func() {

		It("should set model ID, namespace, and cost on decisions", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "ns-1",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].ModelID).To(Equal("model-1"))
			Expect(decisions[0].Namespace).To(Equal("ns-1"))
			Expect(decisions[0].AcceleratorName).To(Equal("A100"))
			Expect(decisions[0].Cost).To(Equal(5.0))
		})
	})

	Context("MinReplicas/MaxReplicas Bounds", func() {
		intPtr := func(n int) *int { return &n }

		It("should respect maxReplicas during scale-up (spillover to next variant)", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, MaxReplicas: intPtr(3)},
						{VariantName: "expensive", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// cheap: ceil(30000/10000)=3, headroom=3-1=2 → add 2; remaining=10000
			// expensive: ceil(10000/20000)=1 → add 1
			Expect(dm["cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
		})

		It("should respect minReplicas during scale-down", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "expensive", Cost: 15.0, ReplicaCount: 3, PerReplicaCapacity: 20000},
					{VariantName: "cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 3, MinReplicas: intPtr(2)},
						{VariantName: "cheap", CurrentReplicas: 3},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: min=2, removable=1. floor(50000/20000)=2 → capped to 1. spare=30000
			// cheap: not last variant → min=0. removable=3. floor(30000/10000)=3 → remove 3
			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(0))
		})

		It("should scale minReplicas=0 variant to zero while keeping minReplicas>0 sibling", func() {
			r := &domain.AnalyzerResult{
				SpareCapacity: 80000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "keep-alive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
					{VariantName: "expendable", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "keep-alive", CurrentReplicas: 2, MinReplicas: intPtr(1)},
						{VariantName: "expendable", CurrentReplicas: 3, MinReplicas: intPtr(0)},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["keep-alive"].TargetReplicas).To(Equal(1))
			Expect(dm["expendable"].TargetReplicas).To(Equal(0))
		})

		It("should propagate MinReplicas/MaxReplicas to VariantDecision", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 0,
				SpareCapacity:    0,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 2, MinReplicas: intPtr(1), MaxReplicas: intPtr(10)},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].MinReplicas).To(Equal(intPtr(1)))
			Expect(decisions[0].MaxReplicas).To(Equal(intPtr(10)))
		})
	})

	Context("Disaggregated (P/D) Scale-Up", func() {

		// withSatEntryPD builds a request with a disaggregated saturation result.
		withSatEntryPD := func(r *domain.AnalyzerResult, req ModelScalingRequest) ModelScalingRequest {
			if r != nil {
				req.Disaggregated = true
				req.AnalyzerResults = []NamedAnalyzerResult{{
					Name:      domain.SaturationAnalyzerName,
					Result:    r,
					Remaining: r.RequiredCapacity,
					Spare:     r.SpareCapacity,
					Enabled:   true,
					Live:      true,
				}}
			}
			return req
		}

		It("should allocate paired (n_P, n_D) replicas for cheapest prefill + decode pair", func() {
			// P-Remaining=20000, PRC_P=10000 → n_P=2
			// D=α×P=20000 (α=1), PRC_D=10000 → n_D=2
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {RequiredCapacity: 20000, TotalDemand: 20000},
					"decode":  {RequiredCapacity: 20000, TotalDemand: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntryPD(r, ModelScalingRequest{
					ModelID:   "model-pd",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1},
						{VariantName: "decode-v", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// α=20000/20000=1; n_P=ceil(20000/10000)=2; n_D=ceil(1×20000/10000)=2
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["decode-v"].TargetReplicas).To(Equal(3))  // 1 + 2
		})

		It("should pick cheapest prefill and decode variants independently", func() {
			// Two prefill variants: cheap-p (cost 5) and expensive-p (cost 15)
			// Two decode variants: cheap-d (cost 5) and expensive-d (cost 15)
			// α=1; P-RC=10000, PRC=10000 → n_P=1 on cheap-p; n_D=1 on cheap-d
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap-p", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "expensive-p", Cost: 15.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "cheap-d", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
					{VariantName: "expensive-d", Cost: 15.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {RequiredCapacity: 10000, TotalDemand: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntryPD(r, ModelScalingRequest{
					ModelID:   "model-pd",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap-p", CurrentReplicas: 1},
						{VariantName: "expensive-p", CurrentReplicas: 1},
						{VariantName: "cheap-d", CurrentReplicas: 1},
						{VariantName: "expensive-d", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["cheap-p"].TargetReplicas).To(Equal(2))     // cheapest P gets +1
			Expect(dm["expensive-p"].TargetReplicas).To(Equal(1)) // unchanged
			Expect(dm["cheap-d"].TargetReplicas).To(Equal(2))     // cheapest D gets +1
			Expect(dm["expensive-d"].TargetReplicas).To(Equal(1)) // unchanged
		})
	})

	// Phase 3 test: D-only scale-up (RC_P=0, RC_D>0).
	// Before Phase 3, the model-level gate read Remaining (P-anchor=0) and routed
	// the model to scale-down instead of allocating decode. The per-role gate
	// (anyRoleNeedsScaleUp) correctly fires on decode demand.
	Context("Disaggregated (P/D) D-only scale-up", func() {

		withSatEntryPD := func(r *domain.AnalyzerResult, req ModelScalingRequest) ModelScalingRequest {
			if r != nil {
				req.Disaggregated = true
				req.AnalyzerResults = []NamedAnalyzerResult{{
					Name:      domain.SaturationAnalyzerName,
					Result:    r,
					Remaining: r.RequiredCapacity,
					Spare:     r.SpareCapacity,
					Enabled:   true,
					Live:      true,
				}}
			}
			return req
		}

		It("should scale up only decode when only D has demand (RC_P=0, RC_D>0)", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "decode-v", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {RequiredCapacity: 0, TotalDemand: 0},
					"decode":  {RequiredCapacity: 10000, TotalDemand: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntryPD(r, ModelScalingRequest{
					ModelID:   "model-d-only",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 2},
						{VariantName: "decode-v", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// prefill has no demand: unchanged.
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2))
			// decode has demand 10000 / PRC 10000 = 1 replica added.
			Expect(dm["decode-v"].TargetReplicas).To(Equal(2))
		})
	})

	Context("Disaggregated (P/D) Scale-Down", func() {

		withSatEntryPD := func(r *domain.AnalyzerResult, req ModelScalingRequest) ModelScalingRequest {
			if r != nil {
				req.Disaggregated = true
				req.AnalyzerResults = []NamedAnalyzerResult{{
					Name:      domain.SaturationAnalyzerName,
					Result:    r,
					Remaining: r.RequiredCapacity,
					Spare:     r.SpareCapacity,
					Enabled:   true,
					Live:      true,
				}}
			}
			return req
		}

		It("should remove from most expensive prefill and decode variants", func() {
			// P-Spare=20000, PRC_P=10000 → n_P=2; D-Spare=10000, PRC_D=10000 → n_D=1
			r := &domain.AnalyzerResult{
				SpareCapacity: 20000, // model-level (unused in disaggregated path)
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap-p", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "expensive-p", Cost: 15.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "decode-v", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {SpareCapacity: 20000, TotalDemand: 10000},
					"decode":  {SpareCapacity: 10000, TotalDemand: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntryPD(r, ModelScalingRequest{
					ModelID:   "model-pd",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap-p", CurrentReplicas: 2},
						{VariantName: "expensive-p", CurrentReplicas: 2},
						{VariantName: "decode-v", CurrentReplicas: 3},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive-p removed first (cost 15 > 5); n_P=floor(20000/10000)=2 but capped by removable
			Expect(dm["expensive-p"].TargetReplicas).To(Equal(0)) // -2
			Expect(dm["cheap-p"].TargetReplicas).To(Equal(2))     // unchanged (protected as last P)
			// decode-v: n_D=floor(10000/10000)=1
			Expect(dm["decode-v"].TargetReplicas).To(Equal(2)) // -1
		})
	})

	Context("Helper Functions", func() {

		It("sortByCostEfficiencyAsc should order by cost/capacity", func() {
			capacities := []domain.VariantCapacity{
				{VariantName: "expensive", Cost: 15.0, PerReplicaCapacity: 10000},
				{VariantName: "cheap", Cost: 5.0, PerReplicaCapacity: 10000},
				{VariantName: "mid", Cost: 10.0, PerReplicaCapacity: 10000},
			}

			sorted := sortByCostEfficiencyAsc(capacities)

			Expect(sorted[0].VariantName).To(Equal("cheap"))
			Expect(sorted[1].VariantName).To(Equal("mid"))
			Expect(sorted[2].VariantName).To(Equal("expensive"))
		})

		It("mergeConstraints should take minimum available per type", func() {
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 10}, "H100": {Limit: 4}}},
				{Pools: map[string]ResourcePool{"A100": {Limit: 6}}},
			}

			merged := mergeConstraints(constraints)

			Expect(merged["A100"]).To(Equal(6))
			Expect(merged["H100"]).To(Equal(4))
		})

		It("mergeConstraints carries an unlimited (negative) pool through as an unbounded budget", func() {
			merged := mergeConstraints([]*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: -1}, "H100": {Limit: 4}}},
			})

			// Unlimited must be present as an unbounded budget, NOT absent: an
			// absent type reads as 0 in fairShareRolePick and is silently denied,
			// which would invert the -1 = unlimited semantic.
			Expect(merged["A100"]).To(Equal(math.MaxInt), "unlimited => unbounded budget")
			Expect(merged["H100"]).To(Equal(4))
		})

		It("mergeConstraints lets a finite pool win over an unlimited sentinel regardless of provider order", func() {
			// This pins the min()-across-providers ordering property (the
			// sentinel-carry regression itself is guarded by the math.MaxInt
			// assertion above); a finite cap must beat unlimited either way.
			// finite before sentinel
			m1 := mergeConstraints([]*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 5}}},
				{Pools: map[string]ResourcePool{"A100": {Limit: -1}}},
			})
			Expect(m1["A100"]).To(Equal(5), "finite cap is more restrictive than unlimited")

			// sentinel before finite
			m2 := mergeConstraints([]*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: -1}}},
				{Pools: map[string]ResourcePool{"A100": {Limit: 5}}},
			})
			Expect(m2["A100"]).To(Equal(5), "finite cap wins even when the sentinel is seen first")
		})
	})

	Context("The From-Zero Admission Ceiling", func() {

		// The ceiling a from-zero-admitted variant is held to. The
		// admitting write site is deferred (see ReasonFromZeroAdmission), so the tag
		// is set directly here rather than obtained from bindingAnchor -- these
		// assert the ceiling mechanism, which is what landed.
		//
		// costGreedyRolePick is where it is observable. fairShareRolePick also
		// grants, but gates on available[vc.AcceleratorName] first, and a
		// never-measured variant's AcceleratorName is empty for the same reason its
		// Cost is zero -- both come from saturation's zero-replica lookup -- so it is
		// skipped there regardless. Separate pre-existing gap, and not this
		// ceiling's to close.
		admittedVariants := []domain.VariantCapacity{
			{VariantName: "measured", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 8000, Reason: "P1-obs"},
			{VariantName: "newcomer", PerReplicaCapacity: 1, Reason: ReasonFromZeroAdmission},
		}
		// No MaxReplicas on either: the configuration the ceiling has to survive.
		// Folded into the existing headroom branch it would not exist here, because
		// that branch is behind a nil-guard and costGreedyRolePick returns
		// math.MaxInt outside it.
		admittedStates := buildStateMap([]domain.VariantReplicaState{
			{VariantName: "measured", CurrentReplicas: 2, GPUsPerReplica: 1},
			{VariantName: "newcomer", CurrentReplicas: 0, GPUsPerReplica: 1},
		})

		It("caps the admitted variant at one replica with no MaxReplicas set", func() {
			// It is picked first, which is worth stating plainly because the
			// intuitive expectation is last: PRC = 1 makes cost efficiency degenerate to Cost, and a
			// never-measured variant's Cost arrives as 0, so the ratio is 0/1 = 0 and
			// it sorts ahead of every priced variant. No sentinel value repairs that --
			// any positive PRC divides a zero numerator -- and the ordering recovers on
			// its own once the zero-replica cost lookup is fixed, which is out of
			// scope here. Until then the ceiling is not one guard among several, it is
			// the only one.
			targets := map[string]int{"measured": 2, "newcomer": 0}

			v, capN := costGreedyRolePick(domain.RoleBoth, nil, admittedVariants, admittedStates, nil, targets)
			Expect(v).To(Equal("newcomer"), "cost 0 over a positive PRC sorts ahead of any priced variant")
			Expect(capN).To(Equal(1), "and the ceiling is the only thing bounding what it may take")
		})

		It("skips the variant once the ceiling is met, rather than capping it at zero", func() {
			// The bound is on the target, not on one iteration, so an allocator coming
			// round again buys nothing more. It must skip and move on: a picker that
			// *returns* cap 0 makes the caller compute deltaUtil == 0 and break out of
			// the whole model's allocation, taking every variant behind it down too.
			// Hence the assertion is about the OTHER variant.
			targets := map[string]int{"measured": 2, "newcomer": 1}

			v, capN := costGreedyRolePick(domain.RoleBoth, nil, admittedVariants, admittedStates, nil, targets)
			Expect(v).To(Equal("measured"), "an exhausted ceiling skips the variant, it does not zero-cap it")
			Expect(capN).To(BeNumerically(">", 0))
		})

		It("leaves an untagged variant's headroom exactly as it was", func() {
			// The ceiling reads through a shared helper that every grant site now uses,
			// so this pins the other branch of it: without the tag, behaviour is the
			// MaxReplicas check verbatim, MaxInt and all.
			plain := []domain.VariantCapacity{{VariantName: "measured", Cost: 5.0, PerReplicaCapacity: 8000}}
			targets := map[string]int{"measured": 2}

			v, capN := costGreedyRolePick(domain.RoleBoth, nil, plain, admittedStates, nil, targets)
			Expect(v).To(Equal("measured"))
			Expect(capN).To(Equal(math.MaxInt), "no MaxReplicas and no tag means unbounded, as before")
		})
	})
})

func decisionMap(decisions []domain.VariantDecision) map[string]domain.VariantDecision {
	m := make(map[string]domain.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}

var _ = Describe("namespace constraint merge helpers", func() {
	Describe("nsPoolBudget", func() {
		It("preserves the unlimited sentinel as -1", func() {
			Expect(nsPoolBudget(ResourcePool{Limit: -1})).To(Equal(-1))
		})
		It("returns the available count for a finite pool", func() {
			Expect(nsPoolBudget(ResourcePool{Limit: 6, Used: 2})).To(Equal(4))
		})
		It("clamps a finite over-used pool to 0", func() {
			Expect(nsPoolBudget(ResourcePool{Limit: 2, Used: 5})).To(Equal(0))
		})
	})

	Describe("tighterBudget", func() {
		It("returns the smaller of two finite budgets", func() {
			Expect(tighterBudget(4, 2)).To(Equal(2))
			Expect(tighterBudget(2, 4)).To(Equal(2))
		})
		It("treats a negative (unlimited) input as +infinity", func() {
			Expect(tighterBudget(-1, 4)).To(Equal(4), "unlimited vs finite -> finite")
			Expect(tighterBudget(4, -1)).To(Equal(4), "finite vs unlimited -> finite")
		})
		It("stays unlimited only when both inputs are unlimited", func() {
			Expect(tighterBudget(-1, -1)).To(Equal(-1))
		})
	})

	Describe("mergeNamespaceConstraints", func() {
		It("returns nil when no provider carries namespace pools", func() {
			Expect(mergeNamespaceConstraints([]*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 4}}}})).To(BeNil())
		})

		It("materializes a present-but-empty namespace as a closed (deny-all) marker", func() {
			merged := mergeNamespaceConstraints([]*ResourceConstraints{
				{NamespacePools: map[string]map[string]ResourcePool{"team-x": {}}},
			})
			Expect(merged).To(HaveKey("team-x"))
			Expect(merged["team-x"]).To(BeEmpty())
			Expect(merged["team-x"]).NotTo(BeNil(), "non-nil empty map signals a closed namespace, not 'open'")
		})

		It("carries the unlimited sentinel through as -1", func() {
			merged := mergeNamespaceConstraints([]*ResourceConstraints{
				{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: -1}}}},
			})
			Expect(merged["team-a"]).To(HaveKeyWithValue("A100", -1))
		})

		It("takes the tighter budget across two providers for the same (ns,type)", func() {
			merged := mergeNamespaceConstraints([]*ResourceConstraints{
				{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 8, Used: 1}}}}, // avail 7
				{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 4}}}},          // avail 4
			})
			Expect(merged["team-a"]).To(HaveKeyWithValue("A100", 4), "min(7,4)")
		})

		It("lets a finite provider win over an unlimited one for the same (ns,type)", func() {
			merged := mergeNamespaceConstraints([]*ResourceConstraints{
				{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: -1}}}},
				{NamespacePools: map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: 5}}}},
			})
			Expect(merged["team-a"]).To(HaveKeyWithValue("A100", 5))
		})
	})

	Describe("aggregateNamespacePools", func() {
		It("sums only finite per-(ns,type) pools across namespaces", func() {
			agg := aggregateNamespacePools(map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 4, Used: 1}},
				"team-b": {"A100": {Limit: 2}, "H100": {Limit: 3}},
			})
			Expect(agg["A100"]).To(Equal(ResourcePool{Limit: 6, Used: 1}))
			Expect(agg["H100"]).To(Equal(ResourcePool{Limit: 3}))
		})

		It("skips unlimited (negative) pools so an all-unlimited config yields an empty map", func() {
			agg := aggregateNamespacePools(map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: -1}},
				"team-b": {"H100": {Limit: -1}},
			})
			Expect(agg).To(BeEmpty(), "unlimited types impose no finite cluster cap")
		})

		It("includes finite types but drops unlimited ones in a mixed namespace", func() {
			agg := aggregateNamespacePools(map[string]map[string]ResourcePool{
				"team-a": {"A100": {Limit: 4}, "H100": {Limit: -1}},
			})
			Expect(agg).To(HaveKey("A100"))
			Expect(agg).NotTo(HaveKey("H100"))
		})
	})

	Describe("poolTotals", func() {
		It("sums limit/used and returns available", func() {
			limit, used, avail := poolTotals(map[string]ResourcePool{
				"A100": {Limit: 8, Used: 3},
				"H100": {Limit: 4, Used: 1},
			})
			Expect(limit).To(Equal(12))
			Expect(used).To(Equal(4))
			Expect(avail).To(Equal(8))
		})

		It("clamps available to 0 when usage exceeds limit", func() {
			_, _, avail := poolTotals(map[string]ResourcePool{"A100": {Limit: 2, Used: 5}})
			Expect(avail).To(Equal(0))
		})
	})
})

// shedEntry builds a live ballot entry for the scale-down loop: the role-spare
// balance the loop draws down, the belief weight the combine gives its votes, and
// the per-variant capacities it is willing to price. A variant absent from prcs is
// one this analyzer does not size — that absence is itself under test.
func shedEntry(name string, score float64, spare map[string]float64, prcs ...any) NamedAnalyzerResult {
	var caps []domain.VariantCapacity
	for i := 0; i+1 < len(prcs); i += 2 {
		caps = append(caps, domain.VariantCapacity{
			VariantName:        prcs[i].(string),
			PerReplicaCapacity: prcs[i+1].(float64),
		})
	}
	return NamedAnalyzerResult{
		Name:      name,
		Result:    &domain.AnalyzerResult{VariantCapacities: caps},
		RoleSpare: spare,
		Score:     score,
		Live:      true,
		Enabled:   true,
	}
}

// Every fixture here drives scaleDownRoleIterated end-to-end, on purpose. The
// defect these pin is not reachable through a direct safeRemovalReplicasForRole
// call: a hand-built RoleSpare[role] = 0 is a state the pipeline never presents at
// role entry, because needsScaleDownForRole skips the whole role first. Such a
// test is green before and after the fix, for the wrong reason. What is reachable
// is mid-loop — applyDeallocationForRole drives a spare to 0 after one variant has
// shed, and the role gate is never re-checked — so the loop is the unit.
var _ = Describe("the scale-down loop's role-level veto and shed ordering", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	intPtr := func(n int) *int { return &n }

	Context("a live analyzer's exhausted role spare vetoes the rest of the role", func() {

		It("holds the next variant when the objector cannot price it (PRC absence)", func() {
			// v1 (cost 15) sheds before v2 (cost 5). The objector sizes v1 only, so
			// removing one v1 replica takes its role spare to exactly 0 — and on v2
			// votesFromRoleSpare would drop it for having no per-variant capacity,
			// silently discarding an explicit "nothing left in this role".
			anchor := []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 15, PerReplicaCapacity: 100},
				{VariantName: "v2", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 200},
			}
			states := map[string]domain.VariantReplicaState{
				"v1": {VariantName: "v1", CurrentReplicas: 3, GPUsPerReplica: 1},
				"v2": {VariantName: "v2", CurrentReplicas: 3, GPUsPerReplica: 1},
			}
			targets := map[string]int{"v1": 3, "v2": 3}
			s := []NamedAnalyzerResult{
				shedEntry("objector", 1, map[string]float64{domain.RoleBoth: 100}, "v1", 100.0),
				shedEntry("wide", 1, map[string]float64{domain.RoleBoth: 1000}, "v1", 100.0, "v2", 200.0),
			}

			scaleDownRoleIterated(ctx, s, anchor, targets, states)

			Expect(targets["v1"]).To(Equal(2), "one replica of the expensive variant comes off first")
			Expect(s[0].RoleSpare[domain.RoleBoth]).To(Equal(0.0),
				"the objector's spare is exhausted mid-loop: this is the state the entry gate cannot see")
			Expect(targets["v2"]).To(Equal(3),
				"v2 is held: an exhausted role spare blocks the whole role, not only the variants its owner prices")
		})

		It("holds the next variant when the objector is outscored", func() {
			// The objector sizes BOTH variants here, so PRC absence is not in play.
			// Its 0 vote is still not a veto after dominance weighting: with e = 0 and
			// a higher-scored voter reporting spare, the correction pulls the combined
			// value positive and floor() returns a removable count. Only a veto holds.
			anchor := []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 15, PerReplicaCapacity: 500},
				{VariantName: "v2", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 100},
			}
			states := map[string]domain.VariantReplicaState{
				// minReplicas on v1 keeps it holding replicas after its shed, so the
				// cheapest-at-1 protection stays out of v2's outcome.
				"v1": {VariantName: "v1", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(2)},
				"v2": {VariantName: "v2", CurrentReplicas: 3, GPUsPerReplica: 1},
			}
			targets := map[string]int{"v1": 3, "v2": 3}
			s := []NamedAnalyzerResult{
				shedEntry("objector", 1, map[string]float64{domain.RoleBoth: 500}, "v1", 500.0, "v2", 100.0),
				shedEntry("trusted", 3, map[string]float64{domain.RoleBoth: 1000}, "v1", 100.0, "v2", 100.0),
			}

			scaleDownRoleIterated(ctx, s, anchor, targets, states)

			Expect(targets["v1"]).To(Equal(2))
			Expect(s[0].RoleSpare[domain.RoleBoth]).To(Equal(0.0))
			Expect(targets["v2"]).To(Equal(3),
				"a vote cannot encode a veto: the objector is outscored 3:1 and would be overruled as a voter")
		})

		It("still lets removal proceed when the role key is missing (abstain)", func() {
			// Same numbers as the outscored fixture, one difference: the objector's
			// RoleSpare does not carry this role at all. A missing key is an ABSTAIN —
			// the analyzer never sized the role, so it has no basis to block it — where
			// a present, non-positive key is a veto. This is the pair that pins the
			// present-and-exhausted veto and the missing-key abstain as different
			// statements rather than two spellings of one.
			anchor := []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 15, PerReplicaCapacity: 500},
				{VariantName: "v2", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 100},
			}
			states := map[string]domain.VariantReplicaState{
				"v1": {VariantName: "v1", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(2)},
				"v2": {VariantName: "v2", CurrentReplicas: 3, GPUsPerReplica: 1},
			}
			targets := map[string]int{"v1": 3, "v2": 3}
			s := []NamedAnalyzerResult{
				shedEntry("abstainer", 1, map[string]float64{"prefill": 1234}, "v1", 500.0, "v2", 100.0),
				shedEntry("trusted", 3, map[string]float64{domain.RoleBoth: 1000}, "v1", 100.0, "v2", 100.0),
			}

			scaleDownRoleIterated(ctx, s, anchor, targets, states)

			Expect(targets["v1"]).To(Equal(2))
			Expect(targets["v2"]).To(Equal(0), "an abstention does not block removal")
			// The abstain has to survive the loop, not just its first iteration: a bare
			// `m[role] -= x` inside applyDeallocationForRole would materialize the key
			// at 0 and the fabricated entry would veto v2.
			Expect(s[0].RoleSpare).NotTo(HaveKey(domain.RoleBoth),
				"an abstained role must not be materialized by the deallocation bookkeeping")
			Expect(s[0].RoleSpare["prefill"]).To(Equal(1234.0), "an unrelated role's balance is untouched")
		})
	})

	Context("the scale-down path never refreshes the anchor's sizing", func() {

		It("leaves every anchor sizing field alone across a multi-variant, multi-iteration shed", func() {
			// Multi-variant and multi-iteration are both required: a single removal is
			// green whether or not the property holds. The voters' sizing deliberately
			// disagrees with the anchor's on all five fields refreshAnchorSizing writes,
			// so an invocation could not be silent. len(s) == 2 keeps the helper past its
			// own single-voter early-out, so nothing but the absence of the call is
			// keeping this green.
			voterCaps := func(prc1, prc2 float64) []domain.VariantCapacity {
				return []domain.VariantCapacity{
					{VariantName: "v1", PerReplicaCapacity: prc1, Reason: "voter-sizing", TotalDemand: 7, Utilization: 0.9},
					{VariantName: "v2", PerReplicaCapacity: prc2, Reason: "voter-sizing", TotalDemand: 7, Utilization: 0.9},
				}
			}
			anchor := []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 15, ReplicaCount: 3,
					PerReplicaCapacity: 111, Reason: "anchor-sizing", TotalDemand: 999, Utilization: 0.5, TotalCapacity: 333},
				{VariantName: "v2", AcceleratorName: "A100", Cost: 5, ReplicaCount: 3,
					PerReplicaCapacity: 222, Reason: "anchor-sizing", TotalDemand: 888, Utilization: 0.25, TotalCapacity: 666},
			}
			before := append([]domain.VariantCapacity(nil), anchor...)
			states := map[string]domain.VariantReplicaState{
				"v1": {VariantName: "v1", CurrentReplicas: 3, GPUsPerReplica: 1},
				"v2": {VariantName: "v2", CurrentReplicas: 3, GPUsPerReplica: 1},
			}
			targets := map[string]int{"v1": 3, "v2": 3}
			s := []NamedAnalyzerResult{
				{Name: "a", Live: true, Enabled: true, Score: 1,
					Result:    &domain.AnalyzerResult{VariantCapacities: voterCaps(300, 400)},
					RoleSpare: map[string]float64{domain.RoleBoth: 100000}},
				{Name: "b", Live: true, Enabled: true, Score: 1,
					Result:    &domain.AnalyzerResult{VariantCapacities: voterCaps(350, 450)},
					RoleSpare: map[string]float64{domain.RoleBoth: 100000}},
			}

			scaleDownRoleIterated(ctx, s, anchor, targets, states)

			Expect(targets["v1"]).To(Equal(0), "first iteration sheds the expensive variant")
			Expect(targets["v2"]).To(Equal(1), "second iteration sheds the cheap one down to its floor")
			Expect(anchor).To(Equal(before), "scale-down must not re-size the anchor: populate once, never refresh")
		})
	})

	Context("shed ordering — coverage per GPU freed", func() {

		It("orders same-cost variants by coverage per GPU, not by raw capacity", func() {
			// b's replicas are four times the size of a's. Raw capacity says shed a
			// first (100 < 200); per GPU returned, a gives up 100 and b gives up 50, so
			// b is the cheaper thing to shed and goes first.
			roleVCs := []domain.VariantCapacity{
				{VariantName: "a", Cost: 10, PerReplicaCapacity: 100},
				{VariantName: "b", Cost: 10, PerReplicaCapacity: 200},
			}
			states := map[string]domain.VariantReplicaState{
				"a": {VariantName: "a", GPUsPerReplica: 1},
				"b": {VariantName: "b", GPUsPerReplica: 4},
			}
			s := []NamedAnalyzerResult{
				shedEntry("x", 1, nil, "a", 100.0, "b", 200.0),
				shedEntry("y", 1, nil, "a", 80.0, "b", 150.0),
			}

			sorted := sortVariantsForScaleDown(s, roleVCs, states)

			Expect([]string{sorted[0].VariantName, sorted[1].VariantName}).To(Equal([]string{"b", "a"}))
		})

		It("combines the analyzers' estimates with a maximum, not a sum", func() {
			// Equal GPUsPerReplica isolates the combine. Maxima: a = 100, b = 150, so
			// a sheds first. Sums: a = 200, b = 160, which would shed b first. The sum
			// also grows with the number of configured analyzers, so under it a new
			// voter could reorder the shed with no observation having changed.
			roleVCs := []domain.VariantCapacity{
				{VariantName: "a", Cost: 10, PerReplicaCapacity: 100},
				{VariantName: "b", Cost: 10, PerReplicaCapacity: 150},
			}
			states := map[string]domain.VariantReplicaState{
				"a": {VariantName: "a", GPUsPerReplica: 1},
				"b": {VariantName: "b", GPUsPerReplica: 1},
			}
			s := []NamedAnalyzerResult{
				shedEntry("x", 1, nil, "a", 100.0, "b", 150.0),
				shedEntry("y", 1, nil, "a", 100.0, "b", 10.0),
			}

			sorted := sortVariantsForScaleDown(s, roleVCs, states)

			Expect([]string{sorted[0].VariantName, sorted[1].VariantName}).To(Equal([]string{"a", "b"}))
		})

		It("ignores Score, which is a sizing belief weight and not an ordering key", func() {
			// The maximum fixture's ballot, with the scores spread 100:1 the other way.
			// Unweighted the maxima are a = 100 and b = 150, so a still sheds first.
			// Weighting by Score — as either a max or a sum — puts a above b and would
			// flip the shed, so this fixture fails against any score-weighted key.
			roleVCs := []domain.VariantCapacity{
				{VariantName: "a", Cost: 10, PerReplicaCapacity: 100},
				{VariantName: "b", Cost: 10, PerReplicaCapacity: 150},
			}
			states := map[string]domain.VariantReplicaState{
				"a": {VariantName: "a", GPUsPerReplica: 1},
				"b": {VariantName: "b", GPUsPerReplica: 1},
			}
			s := []NamedAnalyzerResult{
				shedEntry("x", 0.1, nil, "a", 100.0, "b", 150.0),
				shedEntry("y", 10, nil, "a", 100.0, "b", 10.0),
			}

			sorted := sortVariantsForScaleDown(s, roleVCs, states)

			Expect([]string{sorted[0].VariantName, sorted[1].VariantName}).To(Equal([]string{"a", "b"}))
		})

		It("drives the shed order end-to-end: the larger-replica variant goes first", func() {
			// Same shapes as the ordering fixture, now through the loop. Both variants
			// have the same cost, so the tie-break alone decides who sheds — and the
			// first shed exhausts x's role spare, so whoever goes first is the only one
			// that moves. Coverage per GPU sheds b (a is held at 2); raw capacity or a
			// score-weighted sum would shed a to 0 and hold b.
			anchor := []domain.VariantCapacity{
				{VariantName: "a", AcceleratorName: "A100", Cost: 10, PerReplicaCapacity: 100},
				{VariantName: "b", AcceleratorName: "A100", Cost: 10, PerReplicaCapacity: 200},
			}
			states := map[string]domain.VariantReplicaState{
				"a": {VariantName: "a", CurrentReplicas: 2, GPUsPerReplica: 1},
				"b": {VariantName: "b", CurrentReplicas: 2, GPUsPerReplica: 4},
			}
			targets := map[string]int{"a": 2, "b": 2}
			s := []NamedAnalyzerResult{
				shedEntry("x", 1, map[string]float64{domain.RoleBoth: 200}, "a", 100.0, "b", 200.0),
				shedEntry("y", 1, map[string]float64{domain.RoleBoth: 200}, "a", 80.0, "b", 150.0),
			}

			scaleDownRoleIterated(ctx, s, anchor, targets, states)

			Expect(targets["b"]).To(Equal(1), "b sheds first: it gives up the least coverage per GPU freed")
			Expect(targets["a"]).To(Equal(2), "whoever sheds second finds x's spare spent and gets nothing")
		})
	})
})
