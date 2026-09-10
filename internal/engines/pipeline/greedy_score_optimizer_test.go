package pipeline

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("GreedyByScoreOptimizer", func() {

	var (
		optimizer *GreedyByScoreOptimizer
		ctx       context.Context
	)

	BeforeEach(func() {
		optimizer = NewGreedyByScoreOptimizer()
		ctx = context.Background()
	})

	It("should return 'greedy-by-score' as name", func() {
		Expect(optimizer.Name()).To(Equal("greedy-by-score"))
	})

	Context("Single-Model Scale-Up", func() {

		It("should allocate replicas to cheapest variant within GPU budget", func() {
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 20000,
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
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
					"H100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// cheap is most cost-efficient (5/10000 vs 15/20000)
			// ceil(20000/10000) = 2 replicas, needs 4 A100 GPUs (2 per replica)
			Expect(dm["cheap"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["cheap"].Action).To(Equal(domain.ActionScaleUp))
			Expect(dm["expensive"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("propagates observability fields (utilization/required/spare) from the analyzer result", func() {
			// Regression guard for the greedy-by-score path, which shares
			// buildDecisionsWithOptimizer with cost-aware. Without the copy the V2 gauges
			// (wva_saturation_utilization / wva_required_capacity / wva_spare_capacity) read 0.
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 5000,
				SpareCapacity:    1200,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000, Utilization: 0.42},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 10}}},
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, constraints))
			Expect(dm["v1"].Utilization).To(Equal(0.42))
			Expect(dm["v1"].RequiredCapacity).To(Equal(5000.0))
			Expect(dm["v1"].SpareCapacity).To(Equal(1200.0))
		})

		It("should handle GPU exhaustion with partial allocation", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // Only 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Only 4 GPUs / 2 per replica = 2 replicas max
			Expect(dm["v1"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["v1"].Action).To(Equal(domain.ActionScaleUp))
		})
	})

	Context("Multi-Model Fair-Share", func() {

		It("should give GPUs to most starved model first", func() {
			rA := &domain.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rB := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-A",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID:   "model-B",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8}, // 4 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// A got 3 replicas (1 original + 3 added), B got 2 (1 original + 1 added)
			Expect(dm["a-v1"].TargetReplicas).To(Equal(4)) // 1 + 3
			Expect(dm["b-v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})

		It("should verify 3-model walkthrough from design doc", func() {
			rA := &domain.AnalyzerResult{
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rB := &domain.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			rC := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "c-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-A",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID:   "model-B",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rC, ModelScalingRequest{
					ModelID:   "model-C",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "c-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 12}, // 6 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a-v1"].TargetReplicas).To(Equal(4))
			Expect(dm["b-v1"].TargetReplicas).To(Equal(3))
			Expect(dm["c-v1"].TargetReplicas).To(Equal(2))
		})

		It("should distribute evenly with equal RequiredCapacity", func() {
			rX := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "x-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rY := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "y-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rX, ModelScalingRequest{
					ModelID:   "model-X",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "x-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rY, ModelScalingRequest{
					ModelID:   "model-Y",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "y-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["x-v1"].TargetReplicas).To(Equal(3))
			Expect(dm["y-v1"].TargetReplicas).To(Equal(3))
		})
	})

	Context("GPU Constraints", func() {

		It("should respect per-accelerator-type limits", func() {
			rH := &domain.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			rA := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rH, ModelScalingRequest{
					ModelID:   "model-h100",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
				withSatEntry(rA, ModelScalingRequest{
					ModelID:   "model-a100",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"H100": {Limit: 4},
					"A100": {Limit: 6},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["h100-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			Expect(dm["a100-v"].TargetReplicas).To(Equal(3)) // 1 + 2
		})

		It("should handle mixed accelerator types across variants", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a100-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "h100-v", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-mixed",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a100-v", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "h100-v", CurrentReplicas: 1, GPUsPerReplica: 4},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
					"H100": {Limit: 0},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a100-v"].TargetReplicas).To(Equal(3)) // 1 + 2
			Expect(dm["h100-v"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("should not allocate when zero GPU budget", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 0},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(1))
		})

		It("should not allocate when nil constraints", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(1))
		})

		It("treats a cluster-scope unlimited (sentinel) pool like abundant capacity, not a deny", func() {
			// Regression for the cluster-unlimited-under-V2 bug: a -1
			// (config.QuotaUnlimited) cluster quota is emitted as a sentinel pool
			// (Limit < 0), which mergeConstraints carries through as an unbounded
			// budget. Before the fix the type was absent from the merged budget,
			// so fairShareRolePick read a 0 budget and denied every scale-up —
			// inverting -1 = unlimited into a hard deny.
			newReq := func() []ModelScalingRequest {
				r := &domain.AnalyzerResult{
					ModelID:          "model-1",
					Namespace:        "default",
					AnalyzedAt:       time.Now(),
					RequiredCapacity: 40000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					},
				}
				return []ModelScalingRequest{
					withSatEntry(r, ModelScalingRequest{
						ModelID:   "model-1",
						Namespace: "default",
						VariantStates: []domain.VariantReplicaState{
							{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
						},
					}),
				}
			}

			// -1 is config.QuotaUnlimited; used as a literal here to avoid a config
			// import in this optimizer-level test.
			unlimited := decisionMap(optimizer.Optimize(ctx, newReq(),
				[]*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: -1}}}}))
			abundant := decisionMap(optimizer.Optimize(ctx, newReq(),
				[]*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 1000}}}}))

			Expect(unlimited["v1"].Action).To(Equal(domain.ActionScaleUp), "unlimited cluster quota must not deny scale-up")
			Expect(unlimited["v1"].TargetReplicas).To(BeNumerically(">", 1))
			Expect(unlimited["v1"].TargetReplicas).To(Equal(abundant["v1"].TargetReplicas),
				"unlimited behaves like abundant finite capacity")
		})

		It("scales a finite-type model even when unlimited types were consumed first", func() {
			// Regression for the fairShareScaleUp stop-check overflow: two
			// unlimited (sentinel) budgets are decremented during the round but
			// must stay recognized as unbounded, so the totalGPUs sum cannot wrap
			// to 0 and starve a model on an unrelated finite type.
			mk := func(id, variant, accel string) ModelScalingRequest {
				r := &domain.AnalyzerResult{
					ModelID: id, Namespace: "default", AnalyzedAt: time.Now(),
					RequiredCapacity: 25000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: variant, AcceleratorName: accel, Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					},
				}
				return withSatEntry(r, ModelScalingRequest{
					ModelID: id, Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: variant, CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				})
			}
			requests := []ModelScalingRequest{
				mk("model-A", "a-v1", "A100"),
				mk("model-B", "b-v1", "H100"),
				mk("model-C", "c-v1", "L40S"),
			}
			// -1 is config.QuotaUnlimited for A100/H100; L40S is finite.
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: -1}, "H100": {Limit: -1}, "L40S": {Limit: 4},
				}},
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, constraints))
			Expect(dm["a-v1"].TargetReplicas).To(BeNumerically(">", 1), "unlimited A100 scales up")
			Expect(dm["b-v1"].TargetReplicas).To(BeNumerically(">", 1), "unlimited H100 scales up")
			Expect(dm["c-v1"].TargetReplicas).To(BeNumerically(">", 1), "finite L40S model is not starved by an overflowed stop-check")
		})
	})

	Context("Scale-Down", func() {

		It("should apply role-iterated scale-down for scale-down models", func() {
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

			Expect(dm["expensive"].TargetReplicas).To(Equal(2))
			Expect(dm["cheap"].TargetReplicas).To(Equal(2))
		})

		It("should handle mixed scale-up and scale-down models", func() {
			rUp := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "up-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rDown := &domain.AnalyzerResult{
				SpareCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "down-v1", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rUp, ModelScalingRequest{
					ModelID:   "model-up",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "up-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rDown, ModelScalingRequest{
					ModelID:   "model-down",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "down-v1", CurrentReplicas: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["up-v1"].TargetReplicas).To(Equal(2))
			Expect(dm["up-v1"].Action).To(Equal(domain.ActionScaleUp))

			Expect(dm["down-v1"].TargetReplicas).To(Equal(1))
			Expect(dm["down-v1"].Action).To(Equal(domain.ActionScaleDown))
		})
	})

	Context("Pending Replicas", func() {

		It("should allocate to most cost-efficient variant regardless of pending replicas", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap-pending", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "expensive-ready", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap-pending", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "expensive-ready", CurrentReplicas: 1, PendingReplicas: 0, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["cheap-pending"].TargetReplicas).To(Equal(3))   // +1
			Expect(dm["expensive-ready"].TargetReplicas).To(Equal(1)) // unchanged
		})
	})

	Context("Edge Cases", func() {

		It("should skip requests with nil result", func() {
			requests := []ModelScalingRequest{
				withSatEntry(nil, ModelScalingRequest{ModelID: "model-1", Namespace: "default"}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should skip variants with zero capacity", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "zero-cap", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 0, PerReplicaCapacity: 0},
					{VariantName: "normal", AcceleratorName: "A100", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "zero-cap", CurrentReplicas: 0, GPUsPerReplica: 2},
						{VariantName: "normal", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["zero-cap"].TargetReplicas).To(Equal(0))
			Expect(dm["normal"].TargetReplicas).To(Equal(2))
		})

		It("should handle steady state (no scaling needed)", func() {
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

		It("should handle empty requests", func() {
			decisions := optimizer.Optimize(ctx, nil, nil)
			Expect(decisions).To(BeEmpty())
		})

		It("should default GPUsPerReplica to 1 when not specified", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 0},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 2},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})
	})

	Context("Decision Metadata", func() {

		It("should set correct model ID, namespace, and cost on decisions", func() {
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
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].ModelID).To(Equal("model-1"))
			Expect(decisions[0].Namespace).To(Equal("ns-1"))
			Expect(decisions[0].AcceleratorName).To(Equal("A100"))
			Expect(decisions[0].Cost).To(Equal(5.0))
		})

		It("should contain greedy-by-score in reason strings", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)

			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Reason()).To(ContainSubstring("greedy-by-score"))
		})
	})

	Context("Score-Based Priority", func() {

		It("should give GPUs to higher-score model first", func() {
			rLow := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "low-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rHigh := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "high-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rLow, ModelScalingRequest{
					ModelID:   "low-priority",
					Namespace: "default",
					Priority:  1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "low-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
				withSatEntry(rHigh, ModelScalingRequest{
					ModelID:   "high-priority",
					Namespace: "default",
					Priority:  5.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "high-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // Only 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// High-score model (100000) should get GPU preference over low-score (20000)
			Expect(dm["high-v"].TargetReplicas).To(BeNumerically(">=", 2))
		})

		// T1.3: multi-model fair-share priority integration test.
		// Verifies that fairShareValue = priority × Σ(Remaining × Score) correctly
		// orders models. This test explicitly sets Score on AnalyzerResults, mirroring
		// what the engine populates from config.Analyzers[].Score after the B1 fix.
		// Without B1 (Score=0), fairShareValue falls back to max_i(Remaining) = RC,
		// making both models equal — this test would then produce non-deterministic
		// results and the strict equality assertions would fail intermittently.
		It("T1.3: priority × Score weighting drives fair-share ordering", func() {
			// Model A: RC=20000, Score=1.0, Priority=1.0 → fsv=20000
			// Model B: RC=20000, Score=1.0, Priority=5.0 → fsv=100000
			// With 4 A100 GPUs (2 replicas each, 2 GPUs/replica):
			// B (fsv=100000) should always get served first.
			// Strict assertions require Score to be populated; Score=0 fallback
			// produces equal fsv and non-deterministic ordering.
			rA := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Priority:  1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      domain.SaturationAnalyzerName,
						Result:    rA,
						Score:     1.0, // explicit: mirrors engine-populated value
						Remaining: rA.RequiredCapacity,
						Spare:     rA.SpareCapacity,
						Enabled:   true,
						Live:      true,
					}},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Priority:  5.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      domain.SaturationAnalyzerName,
						Result:    rB,
						Score:     1.0, // explicit: mirrors engine-populated value
						Remaining: rB.RequiredCapacity,
						Spare:     rB.SpareCapacity,
						Enabled:   true,
						Live:      true,
					}},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4}, // 2 replicas worth
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// fsv(A) = 1.0 * 20000 * 1.0 = 20000; fsv(B) = 5.0 * 20000 * 1.0 = 100000
			// B is most starved (highest fsv), gets both available replicas.
			// A gets nothing (GPU budget exhausted by B).
			Expect(dm["b-v1"].TargetReplicas).To(Equal(3)) // 1 + 2 (all GPUs)
			Expect(dm["a-v1"].TargetReplicas).To(Equal(1)) // unchanged
		})

		It("priority orders fair share, and a trusted analyzer does not inflate its model's claim", func() {
			// Two models with identical demand, competing for a single free
			// replica. Model A carries two analyzers, one of them the most
			// trusted entry in the fixture; Model B carries one analyzer and
			// twice the priority.
			//
			// A model's claim is a GPU count — the max over its analyzers, not a
			// score-weighted sum — so A's extra, higher-scored analyzer adds
			// nothing to what A is owed:
			//   claim(A) = max(20000/10000, 20000/10000) × 2 = 4 GPUs
			//   claim(B) =     20000/10000             × 2 = 4 GPUs
			// Equal claims, so priority alone breaks the tie:
			//   fsv(A) = 1.0 × 4 = 4      fsv(B) = 2.0 × 4 = 8
			// B is the more starved model and takes the replica.
			//
			// This fixture is deliberately one that discriminates: a fair-share
			// value built as Σᵢ(demandᵢ × Scoreᵢ) would rank A at 60000 against
			// B's 40000 and hand the replica to A instead. Ranking weights order
			// models; they do not scale what a model is owed.
			rA := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:   "model-A",
					Namespace: "default",
					Priority:  1.0,
					AnalyzerResults: []NamedAnalyzerResult{
						{
							Name:      "saturation",
							Result:    rA,
							Score:     1.0,
							Remaining: rA.RequiredCapacity,
							Enabled:   true,
							Live:      true,
						},
						{
							Name:  "throughput",
							Score: 2.0,
							// A voting analyzer sizes the variants it votes on, so
							// this entry carries a real per-replica capacity for
							// a-v1 rather than an empty capacity list.
							Result: &domain.AnalyzerResult{
								RequiredCapacity: 20000,
								VariantCapacities: []domain.VariantCapacity{
									{VariantName: "a-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
								},
							},
							Remaining: 20000,
							Enabled:   true,
							Live:      true,
						},
					},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "a-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
				{
					ModelID:   "model-B",
					Namespace: "default",
					Priority:  2.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      "saturation",
						Result:    rB,
						Score:     1.0,
						Remaining: rB.RequiredCapacity,
						Enabled:   true,
						Live:      true,
					}},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "b-v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 2}, // exactly one replica, so the winner is unambiguous
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["b-v1"].TargetReplicas).To(Equal(2), "higher priority on an equal claim wins the replica")
			Expect(dm["a-v1"].TargetReplicas).To(Equal(1), "two analyzers and a higher score buy model A nothing")
		})
	})

	Context("Demand-Proportional P/D Distribution", func() {

		It("should distribute replicas proportional to per-role demand", func() {
			// Prefill RequiredCapacity=15000 (75%), Decode RequiredCapacity=5000 (25%)
			// Total model RequiredCapacity=20000, Score=20000
			// With 10 A100 GPUs available, each variant uses 2 GPUs/replica
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 15000, TotalDemand: 15000},
					"decode":  {Role: "decode", RequiredCapacity: 5000, TotalDemand: 5000},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-pd",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// target = 20000 (single model, allocationMean=0)
			// prefill fraction=0.75: roleTarget=15000 → ceil(15000/10000)=2 replicas
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(3)) // 1 + 2
			// decode fraction=0.25: roleTarget=5000 → ceil(5000/10000)=1 replica
			Expect(dm["decode-v"].TargetReplicas).To(Equal(4)) // 3 + 1
		})

		It("should distribute equally when roles have equal demand", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-equal",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 8},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Each role gets 50%: roleTarget=10000 → ceil(10000/10000)=1 replica each
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			Expect(dm["decode-v"].TargetReplicas).To(Equal(2))  // 1 + 1
		})

		It("should only allocate to the role that needs scale-up", func() {
			// Only prefill needs scale-up; decode has 0 RequiredCapacity
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 0, TotalDemand: 0},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-prefill-only",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 3, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 10},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Only prefill fraction=1.0: roleTarget=10000 → 1 replica
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2)) // 1 + 1
			// Decode unchanged (0 RequiredCapacity → not in roleDemands)
			Expect(dm["decode-v"].TargetReplicas).To(Equal(3))
		})

		It("should handle GPU exhaustion for one role without affecting the other", func() {
			// Prefill uses H100s (exhausted), decode uses A100s (available)
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {Role: "decode", RequiredCapacity: 10000, TotalDemand: 10000},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "H100", Cost: 15.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 20000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-mixed-gpu",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 4, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"H100": {Limit: 0}, // No H100s available
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Paired allocation: if P-side (H100) is exhausted, the pair cannot commit.
			// Both prefill and decode stay at their current replicas.
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(1))
			Expect(dm["decode-v"].TargetReplicas).To(Equal(1))
		})

		It("should handle non-disaggregated model with Score", func() {
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					Priority:  2.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 4},
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Score inflates the fair-share ordering priority but not the allocation size.
			// Allocation is demand-driven: RC=10000, PRC=10000 → 1 replica added.
			Expect(dm["v1"].TargetReplicas).To(Equal(2)) // 1 + 1
		})
	})

	Context("Fair-Share Currency — GPUs", func() {

		It("orders models by demand in GPUs, not by demand in tokens per second", func() {
			// One analyzer per model, so nothing here depends on the combine.
			// The two models are built so that the GPU ordering is the ONLY one
			// that puts model Y first:
			//
			//   model X: 20000 / PRC 5000 = 4 replicas × 1 GPU/replica =  4 GPUs
			//   model Y: 12000 / PRC 4000 = 3 replicas × 3 GPU/replica =  9 GPUs
			//
			// In tokens/s X leads (20000 > 12000). In replicas X still leads
			// (4 > 3). Only in GPUs does Y lead (9 > 4) — which is why the two
			// models must differ in GPUsPerReplica for this fixture to mean
			// anything: give them the same value and the factor cancels out of the
			// comparison, leaving a test that passes in either currency.
			//
			// They share one pool sized to a single Y replica, so the model the
			// fair share serves first is the only one that grows. The assertion is
			// the ordering, not the magnitude — a claim's size is not a contract.
			rX := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "x-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 5000},
				},
			}
			rY := &domain.AnalyzerResult{
				RequiredCapacity: 12000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "y-v1", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 4000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rX, ModelScalingRequest{
					ModelID:   "model-X",
					Namespace: "default",
					Priority:  1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "x-v1", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				}),
				withSatEntry(rY, ModelScalingRequest{
					ModelID:   "model-Y",
					Namespace: "default",
					Priority:  1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "y-v1", CurrentReplicas: 1, GPUsPerReplica: 3},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{
					"A100": {Limit: 3}, // one Y replica, or three X replicas
				}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["y-v1"].TargetReplicas).To(Equal(2), "Y claims more GPUs, so the fair share serves it first")
			Expect(dm["x-v1"].TargetReplicas).To(Equal(1), "X leads in tokens/s and in replicas, and neither is the currency")
		})

		It("computeMean averages claims in GPUs, without re-applying any weight", func() {
			// The claims below are the GPU conversions of three different
			// (demand, PRC, GPUs-per-replica) triples, which is the only thing
			// that makes their mean meaningful: 4, 9 and 2 GPUs.
			c1, ok1 := toGPUs(20000, 5000, 1)
			c2, ok2 := toGPUs(12000, 4000, 3)
			c3, ok3 := toGPUs(10000, 10000, 2)
			Expect(ok1 && ok2 && ok3).To(BeTrue())
			Expect([]float64{c1, c2, c3}).To(Equal([]float64{4, 9, 2}))

			active := []*modelWork{{remaining: c1}, {remaining: c2}, {remaining: c3}}

			// A plain arithmetic mean: the water level every model fills toward is
			// common to all of them, so it cannot carry any one model's weight.
			Expect(computeMean(active)).To(Equal(5.0))
		})

		It("clamps each role against its own conversion, truncating neither", func() {
			// Prefill and decode differ in BOTH per-replica capacity and
			// GPUs-per-replica. That is deliberate: with either factor shared, a
			// clamp that converted one role through the other role's numbers would
			// still land on the right answer, and the fixture would read as a
			// guard while guarding nothing.
			//
			//   prefill: 15000 / 5000 = 3 replicas × 1 GPU =  3 GPUs
			//   decode:   8000 / 4000 = 2 replicas × 4 GPU =  8 GPUs
			//                                       claim = 11 GPUs
			//
			// Converting the 11-GPU entitlement back down per role gives each role
			// more headroom than it asked for (55000 for prefill, 11000 for
			// decode), so both roles keep their full demand and each reaches the
			// replica count it needs.
			r := &domain.AnalyzerResult{
				RequiredCapacity: 23000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 15000, TotalDemand: 15000},
					"decode":  {Role: "decode", RequiredCapacity: 8000, TotalDemand: 8000},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:       "model-pd-mixed",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
						{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 4, Role: "decode"},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 20}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["prefill-v"].TargetReplicas).To(Equal(4), "prefill covers its 15000 at 5000 per replica")
			Expect(dm["decode-v"].TargetReplicas).To(Equal(3), "decode covers its 8000 at 4000 per replica")
		})

		It("returns GPUs, not raw demand, when priority cannot scale the claim", func() {
			// Priority 0 is unreachable through config — ApplyDefaults rewrites it
			// to 1.0 — so the request is hand-built. The fallback exists for a
			// priority that cannot scale anything, and it has to answer in the
			// same currency as the path it stands in for: 20000 / 5000 = 4
			// replicas × 3 GPUs = 12 GPUs, not the raw 20000.
			r := &domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 5000},
				},
			}
			req := withSatEntry(r, ModelScalingRequest{
				ModelID:   "model-zero-priority",
				Namespace: "default",
				Priority:  0,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 3},
				},
			})

			s := votingResults(req.AnalyzerResults)
			roles, ps := initRoleState(s)
			fsv := fairShareValue(req.Priority, s, ps, roles, r.VariantCapacities, buildStateMap(req.VariantStates))

			Expect(fsv).To(Equal(12.0))
		})

		It("gives an unpriceable model a zero claim without dropping it from the cycle", func() {
			// The analyzer reports demand but sizes its variant at zero capacity,
			// so nothing can price that demand into GPUs. Both halves matter: the
			// model must not out-rank an actionable one on the strength of a
			// number nobody can act on, and it must still be reported.
			rDead := &domain.AnalyzerResult{
				RequiredCapacity: 90000, // large, and entirely unactionable
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "dead-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 0},
				},
			}
			rLive := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "live-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			deadReq := withSatEntry(rDead, ModelScalingRequest{
				ModelID:   "model-unpriceable",
				Namespace: "default",
				Priority:  1.0,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "dead-v", CurrentReplicas: 1, GPUsPerReplica: 2},
				},
			})

			// The primary path yields nothing to scale, so the value comes back
			// through the fallback — which must also answer 0, not the raw 90000.
			s := votingResults(deadReq.AnalyzerResults)
			roles, ps := initRoleState(s)
			fsv := fairShareValue(deadReq.Priority, s, ps, roles, rDead.VariantCapacities, buildStateMap(deadReq.VariantStates))
			Expect(fsv).To(Equal(0.0), "no conversion factor anywhere means no claim, by either path")

			requests := []ModelScalingRequest{
				deadReq,
				withSatEntry(rLive, ModelScalingRequest{
					ModelID:   "model-actionable",
					Namespace: "default",
					Priority:  1.0,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "live-v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 2}}}, // one replica
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["live-v"].TargetReplicas).To(Equal(2), "the actionable model is served despite the larger raw demand next to it")
			Expect(dm).To(HaveKey("dead-v"), "excluded from the fair-share queue is not excluded from the cycle")
			Expect(dm["dead-v"].TargetReplicas).To(Equal(1), "reported at its current state")
		})

		It("rounds the entitlement up to a whole replica and the pool down", func() {
			// The two terms of the cap round in opposite directions, and only a
			// direct call to the closure can see it: at Optimize() level an
			// understated cap costs iterations rather than replicas, because the
			// allocation total is bounded elsewhere and each iteration re-picks.
			variants := []domain.VariantCapacity{
				{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
			}
			stateMap := map[string]domain.VariantReplicaState{
				"v": {VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2},
			}
			s := []NamedAnalyzerResult{{Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{}, Enabled: true, Live: true}}
			roles := []string{domain.RoleBoth}

			// A 5-GPU entitlement at 2 GPUs per replica is two whole replicas and
			// a half. The half is still owed, and a replica is the smallest thing
			// that can be handed over, so the cap covers it.
			_, capN := fairShareRolePick(5, s, roles)(
				domain.RoleBoth, s, variants, stateMap, map[string]int{"A100": 100}, map[string]int{"v": 1})
			Expect(capN).To(Equal(3))

			// The pool is the opposite case: 5 real GPUs are two replicas and a
			// half, and the half of a replica does not exist to be given.
			_, capN = fairShareRolePick(100, s, roles)(
				domain.RoleBoth, s, variants, stateMap, map[string]int{"A100": 5}, map[string]int{"v": 1})
			Expect(capN).To(Equal(2))
		})
	})

	Context("Fair Share — One Entitlement per Model", func() {

		// The entitlement is granted per MODEL and spent JOINTLY by its roles:
		// Σ_role spend[role] ≤ target, in GPUs, which is the only space that sum
		// is legal in. Handing every role — or worse, every (analyzer, role)
		// pair — the whole target instead is one entitlement drawn several times.
		//
		// Both fixtures below use the same topology, and it is chosen so the bound
		// can be asserted exactly. Prefill and decode differ in BOTH per-replica
		// capacity and GPUs-per-replica, so no clamp can accidentally land on the
		// right answer by borrowing the other role's numbers. Every role's demand
		// is a whole number of replicas, and the shares the sequenced draw hands
		// out are whole multiples of the landing variant's GPUs-per-replica, so
		// replicasToCover rounds nothing up: without that, Σ_role spend exceeds
		// target by the round-up, which is the deferred replicasToCover item and
		// not this one.
		//
		//	prefill: 40000 / 5000 = 8 replicas × 1 GPU  =  8 GPUs
		//	decode:  24000 / 4000 = 6 replicas × 2 GPUs = 12 GPUs
		//	                                     claim  = 20 GPUs
		//
		// A second model is present only to make the mean positive, which is what
		// makes the entitlement smaller than the claim and therefore binding: with
		// one model the entitlement IS the whole claim and nothing can be observed.
		// It claims 6 GPUs (20000 / 10000 = 2 replicas × 3 GPUs), so the mean is
		// 13 and the P/D model's first-round entitlement is 20 − 13 = 7 GPUs. The
		// pool is 7, so the first round is the only round, and the question the
		// fixture asks is how those 7 GPUs are split across the two roles.
		mixedRolePD := func() *domain.AnalyzerResult {
			return &domain.AnalyzerResult{
				RequiredCapacity: 64000,
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {Role: "prefill", RequiredCapacity: 40000, TotalDemand: 40000},
					"decode":  {Role: "decode", RequiredCapacity: 24000, TotalDemand: 24000},
				},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
					{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
				},
			}
		}
		pdStates := func() []domain.VariantReplicaState {
			return []domain.VariantReplicaState{
				{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
				{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
			}
		}
		// The mean-setter. Its claim is what makes the P/D model's entitlement a
		// fraction of its claim; it never allocates, because the P/D model draws
		// first on its larger claim and leaves the pool empty.
		meanSetter := func() ModelScalingRequest {
			return withSatEntry(&domain.AnalyzerResult{
				RequiredCapacity: 20000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "mean-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}, ModelScalingRequest{
				ModelID:   "mean-setter",
				Namespace: "default",
				Priority:  1.0,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "mean-v", CurrentReplicas: 1, GPUsPerReplica: 3},
				},
			})
		}
		sevenGPUs := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 7}}},
		}

		It("spends one shared balance across the roles, never one per role", func() {
			requests := []ModelScalingRequest{
				withSatEntry(mixedRolePD(), ModelScalingRequest{
					ModelID:       "pd-shared-pool",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: pdStates(),
				}),
				meanSetter(),
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, sevenGPUs))

			// Decode draws first and its share is 7 less the one GPU held back for
			// prefill, so it takes 6 / 2 = 3 replicas; prefill draws against the
			// 1 GPU that is left and takes 1. Seven GPUs, spent once.
			Expect(dm["decode-v"].TargetReplicas).To(Equal(4), "decode: 1 + 3 replicas at 2 GPUs = 6 GPUs")
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2), "prefill: 1 + 1 replica at 1 GPU = 1 GPU")

			spent := (dm["decode-v"].TargetReplicas-1)*2 + (dm["prefill-v"].TargetReplicas-1)*1
			Expect(spent).To(Equal(7), "Σ_role spend equals the 7-GPU entitlement exactly, not once per role")

			// One entitlement drawn per role instead would size each role against
			// the whole 7: prefill alone would be allowed 7 replicas and land on 7,
			// for 6 + 6 = 12 GPUs out of a 7-GPU pool.
			Expect(dm["mean-setter-v"].TargetReplicas).To(Equal(0), "the mean-setter has no such variant")
			Expect(dm["mean-v"].TargetReplicas).To(Equal(1), "the pool is empty by the time the mean-setter draws")
		})

		It("does not multiply the entitlement by the number of analyzers", func() {
			// The clamp is applied per (analyzer, role) pair, so with two analyzers
			// and two roles a per-pair budget is one entitlement drawn four times.
			// The second voter here is deliberately the less demanding one and
			// prices every variant exactly as saturation does, so it changes no
			// sizing and no claim: the model's answer must be the one above,
			// digit for digit. Any difference is the grid being spent.
			satResult := mixedRolePD()
			taResult := mixedRolePD()
			taResult.RequiredCapacity = 32000
			taResult.RoleCapacities = map[string]domain.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: 20000, TotalDemand: 20000},
				"decode":  {Role: "decode", RequiredCapacity: 12000, TotalDemand: 12000},
			}

			requests := []ModelScalingRequest{
				{
					ModelID:       "pd-shared-pool-two-voters",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					VariantStates: pdStates(),
					AnalyzerResults: []NamedAnalyzerResult{
						{Name: domain.SaturationAnalyzerName, Result: satResult, Remaining: satResult.RequiredCapacity, Enabled: true, Live: true},
						{Name: "throughput", Result: taResult, Remaining: taResult.RequiredCapacity, Enabled: true, Live: true},
					},
				},
				meanSetter(),
			}

			dm := decisionMap(optimizer.Optimize(ctx, requests, sevenGPUs))

			Expect(dm["decode-v"].TargetReplicas).To(Equal(4))
			Expect(dm["prefill-v"].TargetReplicas).To(Equal(2))

			spent := (dm["decode-v"].TargetReplicas-1)*2 + (dm["prefill-v"].TargetReplicas-1)*1
			Expect(spent).To(Equal(7), "two voters, still one 7-GPU entitlement")
		})

		// The two Optimize()-level specs above are genuine discriminators for the
		// per-(analyzer, role) CLAMP, but not for the picker's own ledger: in both,
		// each role's demand already exceeds the whole-model entitlement, so
		// bounding each role by `target` independently lands on the same numbers.
		// Discriminating the ledger needs the other shape — roles that would EACH
		// individually fit inside the entitlement and only jointly overrun it. That shape is what
		// separates one shared balance from one budget per role, and at
		// Optimize() level it is not observable: the round-up in replicasToCover
		// and the downstream pool both move the totals, so a direct call to the
		// returned closure is the only place the balance itself is visible. Same
		// technique, and same reason, as the round-the-entitlement-up spec above.
		//
		// Entitlement 6 GPUs. Either role alone fits: decode would take 3 replicas
		// at 2 GPUs, prefill 6 at 1 GPU — 6 GPUs each. Together they want 12.
		balanceVariants := []domain.VariantCapacity{
			{VariantName: "decode-v", AcceleratorName: "A100", Cost: 5.0, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
			{VariantName: "prefill-v", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
		}
		balanceStates := map[string]domain.VariantReplicaState{
			"decode-v":  {VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 2, Role: "decode"},
			"prefill-v": {VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
		}
		// Deliberately far larger than the entitlement: a pool that could bind
		// would mask the balance, which is exactly the masking this spec exists
		// to avoid.
		balancePool := map[string]int{"A100": 100}
		balanceRoles := []string{"decode", "prefill"}
		balanceBallot := []NamedAnalyzerResult{
			{Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{}, Enabled: true, Live: true},
		}

		It("hands the second role the remainder, not the entitlement over again", func() {
			targets := map[string]int{"decode-v": 1, "prefill-v": 1}
			pick := fairShareRolePick(6, balanceBallot, balanceRoles)

			// Decode sorts first, so it draws against the balance less the one GPU
			// held back for prefill: 5 GPUs at 2 per replica, rounded up.
			v, capN := pick("decode", balanceBallot, balanceVariants, balanceStates, balancePool, targets)
			Expect(v).To(Equal("decode-v"))
			Expect(capN).To(Equal(3))

			// Prefill draws second. One budget per role would size it against the
			// full 6 GPUs again and hand it 6 replicas — each role fitting on its
			// own, the pair overrunning by 100%. One shared balance leaves it what
			// decode did not reserve.
			v, capN = pick("prefill", balanceBallot, balanceVariants, balanceStates, balancePool, targets)
			Expect(v).To(Equal("prefill-v"))
			Expect(capN).To(Equal(1), "the remainder of one balance, not a second copy of the entitlement")
		})

		It("does not hand back the whole entitlement on the next iteration", func() {
			// committed0 is snapshotted at the first draw and the spend is measured
			// against it, so a later iteration is bounded by what the earlier one
			// actually committed. Without that the model draws a full entitlement
			// every time round and buys the same GPUs again — which no
			// single-iteration fixture can see.
			targets := map[string]int{"decode-v": 1, "prefill-v": 1}
			pick := fairShareRolePick(6, balanceBallot, balanceRoles)

			_, capN := pick("decode", balanceBallot, balanceVariants, balanceStates, balancePool, targets)
			Expect(capN).To(Equal(3))
			_, capN = pick("prefill", balanceBallot, balanceVariants, balanceStates, balancePool, targets)
			Expect(capN).To(Equal(1))

			// The caller commits both grants — 6 GPUs for decode, 1 for prefill —
			// and the loop comes round again.
			targets["decode-v"] = 4
			targets["prefill-v"] = 2

			v, capN := pick("decode", balanceBallot, balanceVariants, balanceStates, balancePool, targets)
			Expect(v).To(BeEmpty(), "the entitlement is spent; the second iteration buys nothing")
			Expect(capN).To(Equal(0))
		})

		It("draws the roles in a deterministic order", func() {
			// The draw is sequenced, so which role is sized against the full
			// balance and which against the remainder is decided by the order the
			// roles come back in — and that order is initRoleState's, sorted. The
			// split asserted above is only reproducible because of this.
			s := votingResults(withSatEntry(mixedRolePD(), ModelScalingRequest{
				ModelID:       "pd-order",
				Disaggregated: true,
				VariantStates: pdStates(),
			}).AnalyzerResults)

			roles, _ := initRoleState(s)
			Expect(roles).To(Equal([]string{"decode", "prefill"}))
		})
	})

	Context("Abstain Is Not Exempt", func() {

		// The abstention rule, named here because the comments below refer back to
		// it: an analyzer with no per-replica capacity for the variant it is being
		// clamped against abstains — it contributes no claim and it spends
		// nothing. It is NOT budget-exempt. The `continue` at the clamp in
		// allocateForModel is the abstention.
		//
		// The assertion is an EQUALITY, not a magnitude: adding a voter that
		// cannot price the reference variant must not change the allocation at
		// all. Asserting a number would pin today's arithmetic instead of the
		// property. The companion is the 3-analyzer non-participant fixture in
		// analyzer_helpers_test.go, which pins the same property inside
		// combineVotes; this one pins it at the spend sites.
		//
		// SCOPE — READ BEFORE TRUSTING THIS AS A GATE ON THAT RULE. The property holds here
		// and does NOT hold universally. The clamp keys on the role's REFERENCE
		// variant while the vote keys on the PICKED variant, and
		// referenceVariantForRole's own doc comment says divergence is expected
		// when the cheaper variant is at its replica ceiling. When those two
		// variants also disagree on GPUsPerReplica, the claim is priced through
		// the reference variant's value and spent through the picked one's, so the
		// entitlement is inflated by their ratio and an abstaining voter can spend
		// past the claiming voter's bottleneck to fill it. Measured, single role,
		// pool 100, sat demand 30000, TA demand 100000 pricing only pricey-v:
		//
		//	cheap-v  (reference) PRC 10000, 3 GPUs/replica, MaxReplicas 1
		//	pricey-v (picked)    PRC 10000, 1 GPU/replica
		//	  [sat]     -> pricey-v 4      (+3, and 3 GPUs is the true need)
		//	  [sat,TA]  -> pricey-v 10     (+9, the whole inflated 9-GPU claim)
		//
		// That inflation is upstream of the abstention rule and reachable with ONE analyzer: it
		// also shifts share between models in a multi-model pass with no TA
		// involved. Whether a claim may be priced through a variant the picker
		// cannot buy is an open design question, not a settled contract. The
		// fixtures below therefore cover the aligned regime deliberately, and the
		// rule is NOT fully gated by them.
		w4Sat := func() *domain.AnalyzerResult {
			return &domain.AnalyzerResult{
				RequiredCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "pricey-v", AcceleratorName: "A100", Cost: 20.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
		}
		// Prices ONLY pricey-v, so prcForVariant(ta, "cheap-v") is zero and this
		// entry cannot price the role's reference variant. Its demand is more than
		// three times sat's, so a voter that escaped the budget would be obvious.
		w4TA := func() *domain.AnalyzerResult {
			return &domain.AnalyzerResult{
				RequiredCapacity: 100000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "pricey-v", AcceleratorName: "A100", Cost: 20.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
		}
		// capReference pins cheap-v at its current replica count, which is what
		// pushes the picker onto pricey-v and makes reference != picked.
		w4States := func(capReference bool) []domain.VariantReplicaState {
			one := 1
			st := []domain.VariantReplicaState{
				{VariantName: "cheap-v", CurrentReplicas: 1, GPUsPerReplica: 1},
				{VariantName: "pricey-v", CurrentReplicas: 1, GPUsPerReplica: 1},
			}
			if capReference {
				st[0].MaxReplicas = &one
			}
			return st
		}
		// Both ballots use the same model identity, so the comparison below covers
		// the whole decision — action, cost, replica counts — and not just the
		// numbers a hand-picked field accessor would have looked at.
		w4Request := func(withTA, capReference bool) ModelScalingRequest {
			sat := w4Sat()
			req := withSatEntry(sat, ModelScalingRequest{
				ModelID:       "w4",
				Namespace:     "default",
				Priority:      1.0,
				VariantStates: w4States(capReference),
			})
			if withTA {
				ta := w4TA()
				req.AnalyzerResults = append(req.AnalyzerResults, NamedAnalyzerResult{
					Name:      "throughput",
					Result:    ta,
					Remaining: ta.RequiredCapacity,
					Enabled:   true,
					Live:      true,
				})
			}
			return req
		}
		// The pool is far larger than anything claimed here, so it is never what
		// makes the two ballots agree — the entitlement is.
		roomyPool := []*ResourceConstraints{
			{Pools: map[string]ResourcePool{"A100": {Limit: 100}}},
		}

		It("allocates the same with an unpriced voter present as with it absent", func() {
			// Here the picker lands on the reference variant itself, so the entry
			// that cannot price it is also excluded from the vote for it by
			// votesFromPickerState. Both halves of the rule are exercised: no claim
			// (claimGPUs passes over the entry) and no spend.
			withTA := decisionMap(optimizer.Optimize(ctx,
				[]ModelScalingRequest{w4Request(true, false)}, roomyPool))
			without := decisionMap(optimizer.Optimize(ctx,
				[]ModelScalingRequest{w4Request(false, false)}, roomyPool))

			Expect(withTA).To(Equal(without),
				"a voter that cannot price the reference variant must not change the allocation")
		})

		It("allocates the same when the picker lands off the reference variant", func() {
			// cheap-v is at its ceiling, so the picker lands on pricey-v — which
			// the abstaining entry CAN price, and does vote for. The equality
			// still holds because the entitlement binds: the claim is priced
			// through cheap-v and spent through pricey-v, and the two share a
			// GPUsPerReplica, so capN lands exactly on the replica count sat alone
			// would ask for and bounds n whatever the second voter votes.
			//
			// This is the regime boundary described in the Context comment. Make
			// the two GPUsPerReplica values differ and this equality fails; that
			// is the open claim-pricing question, not a defect in this fixture.
			withTA := decisionMap(optimizer.Optimize(ctx,
				[]ModelScalingRequest{w4Request(true, true)}, roomyPool))
			without := decisionMap(optimizer.Optimize(ctx,
				[]ModelScalingRequest{w4Request(false, true)}, roomyPool))

			Expect(withTA).To(Equal(without),
				"reference != picked must not by itself hand the unpriced voter a draw")
		})
	})

	Context("Claim Pricing", func() {

		// DORMANT AND PROVISIONAL — READ BEFORE ACTING ON THIS.
		//
		// This spec is PENDING, so it does not run and gates nothing. It records
		// a measured defect that no golden can catch, and it asserts the answer
		// the defect implies rather than the behaviour in the tree. Whether that
		// answer is the right one is an open design question, not a settled
		// contract. If the disposition is that today's pricing is correct as
		// designed, delete this spec — it encodes a premise, not a decision.
		//
		// It is written as a pending spec asserting the HONEST split rather than
		// as a characterization fixture pinning the current numbers on purpose. A
		// characterization fixture would freeze the distortion and make the
		// eventual fix look like a regression; this goes green when a fix lands.
		//
		// The defect: claimGPUs prices a role's claim through
		// referenceVariantForRole using THAT variant's GPUsPerReplica, while
		// fairShareRolePick spends the entitlement through whichever candidate it
		// lands on, using THAT variant's GPUsPerReplica. Reference selection
		// filters only on PerReplicaCapacity > 0 and never checks headroom, so it
		// can price a whole role through a variant the picker provably cannot buy.
		// When the unbuyable reference is the more GPU-hungry one, the claim — and
		// therefore the model's entitlement and its ranking position — is inflated
		// by the ratio between the two GPUsPerReplica values.
		//
		// Why this fixture and not a simpler one: the inflation needs no second
		// analyzer and no abstention escape. Both models below are sat-only. The pool is
		// honoured whichever way it goes (4 GPUs spent either way), so this is a
		// pure redistribution BETWEEN models — which is why no pool check catches
		// it and why every single-model-per-pass golden is blind to it.
		//
		// Measured on this exact fixture, against the claim pricing as it stands:
		//	cheap-x at 3 GPUs/replica -> pricey-x 4, y-v 2   (X takes 3 of 4)
		//	cheap-x at 1 GPU/replica  -> pricey-x 3, y-v 3   (even, and honest)
		// The ONLY difference is the GPUsPerReplica of a variant X cannot buy.
		PIt("does not price a claim through a variant the model cannot buy", func() {
			one := 1
			// Both models truly need 3 GPUs: 30000 demand at PRC 10000, served at
			// 1 GPU per replica. Neither is entitled to more than the other.
			model := func(id, unbuyable, buyable string, unbuyableGPUs int) ModelScalingRequest {
				vcs := []domain.VariantCapacity{
					{VariantName: buyable, AcceleratorName: "A100", Cost: 20.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				}
				states := []domain.VariantReplicaState{
					{VariantName: buyable, CurrentReplicas: 1, GPUsPerReplica: 1},
				}
				if unbuyable != "" {
					// Cheaper per unit capacity, so it wins cost-efficiency and
					// becomes the reference variant — but it is pinned at its
					// ceiling, so the picker can never land on it.
					vcs = append(vcs, domain.VariantCapacity{
						VariantName: unbuyable, AcceleratorName: "A100", Cost: 5.0,
						ReplicaCount: 1, PerReplicaCapacity: 10000,
					})
					states = append(states, domain.VariantReplicaState{
						VariantName: unbuyable, CurrentReplicas: 1,
						GPUsPerReplica: unbuyableGPUs, MaxReplicas: &one,
					})
				}
				return withSatEntry(&domain.AnalyzerResult{
					RequiredCapacity: 30000, VariantCapacities: vcs,
				}, ModelScalingRequest{
					ModelID: id, Namespace: "default", Priority: 1.0, VariantStates: states,
				})
			}

			dm := decisionMap(optimizer.Optimize(ctx, []ModelScalingRequest{
				model("x", "cheap-x", "pricey-x", 3),
				model("y", "", "y-v", 1),
			}, []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 4}}},
			}))

			// Two equal claims and four GPUs to add, so two each: both models start
			// at one replica and should reach three. Asserting the split rather
			// than X's number alone is the point — the failure mode is that X wins
			// a GPU Y should have had, and only Y's count shows that.
			Expect(dm["pricey-x"].TargetReplicas).To(Equal(3), "X takes 2 of the 4 added GPUs, not 3")
			Expect(dm["y-v"].TargetReplicas).To(Equal(3), "Y is a bystander and must not be starved")
			Expect(dm["cheap-x"].TargetReplicas).To(Equal(1), "still unbuyable")
		})
	})

	Context("Helper Functions", func() {

		It("filterActive should return only models with remaining > 0", func() {
			work := []*modelWork{
				{remaining: 100},
				{remaining: -1},
				{remaining: 50},
				{remaining: 0},
			}

			active := filterActive(work)
			Expect(active).To(HaveLen(2))
			Expect(active[0].remaining).To(Equal(100.0))
			Expect(active[1].remaining).To(Equal(50.0))
		})

		It("computeMean should return average of remaining", func() {
			active := []*modelWork{
				{remaining: 100},
				{remaining: 200},
				{remaining: 300},
			}

			mean := computeMean(active)
			Expect(mean).To(Equal(200.0))
		})

		It("computeMean should return 0 for empty slice", func() {
			mean := computeMean(nil)
			Expect(mean).To(Equal(0.0))
		})

		It("allocateForModel should respect maxReplicas", func() {
			intPtr := func(n int) *int { return &n }
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 20}}},
			}

			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				AnalyzedAt:       time.Now(),
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
					{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "cheap", CurrentReplicas: 1, GPUsPerReplica: 1, MaxReplicas: intPtr(3)},
						{VariantName: "expensive", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// cheap: capped at max=3 (starts at 1, can add 2)
			// expensive: gets remaining capacity
			Expect(dm["cheap"].TargetReplicas).To(BeNumerically("<=", 3))
		})

		It("scale-down should respect minReplicas via scaleDownRoleIterated", func() {
			intPtr := func(n int) *int { return &n }

			r := &domain.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				AnalyzedAt:    time.Now(),
				SpareCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "expensive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 3, PerReplicaCapacity: 20000},
					{VariantName: "cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "expensive", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(2)},
						{VariantName: "cheap", CurrentReplicas: 3, GPUsPerReplica: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			// expensive: minReplicas=2, so can only remove 1
			Expect(dm["expensive"].TargetReplicas).To(BeNumerically(">=", 2))
		})

		It("scale-down should zero minReplicas=0 variant while keeping minReplicas>0 sibling", func() {
			intPtr := func(n int) *int { return &n }

			r := &domain.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				AnalyzedAt:    time.Now(),
				SpareCapacity: 80000, // enough to remove all
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "keep-alive", AcceleratorName: "A100", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
					{VariantName: "expendable", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "keep-alive", CurrentReplicas: 2, GPUsPerReplica: 1, MinReplicas: intPtr(1)},
						{VariantName: "expendable", CurrentReplicas: 3, GPUsPerReplica: 1, MinReplicas: intPtr(0)},
					},
				}),
			}

			decisions := optimizer.Optimize(ctx, requests, nil)
			dm := decisionMap(decisions)

			Expect(dm["keep-alive"].TargetReplicas).To(Equal(1))
			Expect(dm["expendable"].TargetReplicas).To(Equal(0))
		})

		It("sortByRemainingDesc should sort descending", func() {
			active := []*modelWork{
				{remaining: 100, req: ModelScalingRequest{ModelID: "low"}},
				{remaining: 300, req: ModelScalingRequest{ModelID: "high"}},
				{remaining: 200, req: ModelScalingRequest{ModelID: "mid"}},
			}

			sortByRemainingDesc(active)

			Expect(active[0].req.ModelID).To(Equal("high"))
			Expect(active[1].req.ModelID).To(Equal("mid"))
			Expect(active[2].req.ModelID).To(Equal("low"))
		})

		// filterVariantCapacitiesByRole removed (duplicate of variantsForRole in analyzer_helpers.go; N2 cleanup)
	})

	// Phase 3 test: D-only scale-up via the per-role gate.
	Context("Disaggregated D-only scale-up (Phase 3)", func() {
		It("should scale up only decode when RC_P=0 and RC_D>0", func() {
			// Pre-Phase-3 the model-level gate (Remaining=0 from P-anchor) would
			// route the model to scale-down. anyRoleNeedsScaleUp fires on D demand.
			r := &domain.AnalyzerResult{
				RequiredCapacity: 0,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "pf", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "dc", AcceleratorName: "A100", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {RequiredCapacity: 0, TotalDemand: 0},
					"decode":  {RequiredCapacity: 10000, TotalDemand: 10000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:       "d-only",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      domain.SaturationAnalyzerName,
						Result:    r,
						Score:     1.0,
						Remaining: r.RequiredCapacity,
						Spare:     r.SpareCapacity,
						Enabled:   true,
						Live:      true,
					}},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "pf", CurrentReplicas: 2, GPUsPerReplica: 2},
						{VariantName: "dc", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 4}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["pf"].TargetReplicas).To(Equal(2)) // no P demand
			Expect(dm["dc"].TargetReplicas).To(Equal(2)) // +1 decode
		})
	})

	// Phase 3 test: min-util coupling without α.
	Context("Disaggregated min-util coupling (Phase 3)", func() {
		It("should advance P and D by matched util (not fixed α ratio)", func() {
			// P-demand=10000, D-demand=30000, PRC=10000 each.
			// Without α: P and D are sized independently and joint-committed by Δ_util.
			// n_P=1 (ceil(10000/10000)), n_D=3 (ceil(30000/10000)).
			// util_P=1.0, util_D=3.0 → Δ_util=1.0 → k_P=1, k_D=3.
			// Result: prefill+1, decode+3 — same Δ_util=1.0 for both.
			r := &domain.AnalyzerResult{
				RequiredCapacity: 10000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "pf", AcceleratorName: "A100", Cost: 5.0, Role: "prefill", PerReplicaCapacity: 10000},
					{VariantName: "dc", AcceleratorName: "A100", Cost: 5.0, Role: "decode", PerReplicaCapacity: 10000},
				},
				RoleCapacities: map[string]domain.RoleCapacity{
					"prefill": {RequiredCapacity: 10000, TotalDemand: 10000},
					"decode":  {RequiredCapacity: 30000, TotalDemand: 30000},
				},
			}
			requests := []ModelScalingRequest{
				{
					ModelID:       "pd-min-util",
					Namespace:     "default",
					Disaggregated: true,
					Priority:      1.0,
					AnalyzerResults: []NamedAnalyzerResult{{
						Name:      domain.SaturationAnalyzerName,
						Result:    r,
						Score:     1.0,
						Remaining: r.RequiredCapacity,
						Spare:     r.SpareCapacity,
						Enabled:   true,
						Live:      true,
					}},
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "pf", CurrentReplicas: 1, GPUsPerReplica: 2},
						{VariantName: "dc", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				},
			}
			constraints := []*ResourceConstraints{
				{Pools: map[string]ResourcePool{"A100": {Limit: 12}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			// Both roles committed by the same Δ_util=1.0.
			Expect(dm["pf"].TargetReplicas).To(Equal(2)) // 1+1
			Expect(dm["dc"].TargetReplicas).To(Equal(4)) // 1+3
		})
	})

	Context("Namespace-Scoped Quota", func() {

		It("caps a model at its namespace budget even when cluster GPUs remain", func() {
			r := &domain.AnalyzerResult{
				ModelID:          "m",
				Namespace:        "team-a",
				RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID:   "m",
					Namespace: "team-a",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2},
					},
				}),
			}
			// Cluster has plenty of A100, but team-a's quota leaves only 2 GPUs
			// (cap 4 − 2 in use) = room for exactly one more 2-GPU replica.
			constraints := []*ResourceConstraints{
				{
					Pools: map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{
						"team-a": {"A100": {Limit: 4, Used: 2}},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v"].TargetReplicas).To(Equal(2), "bounded by team-a quota (2 GPUs), not cluster (100)")
		})

		It("enforces independent per-namespace budgets across models", func() {
			rA := &domain.AnalyzerResult{
				ModelID: "mA", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &domain.AnalyzerResult{
				ModelID: "mB", Namespace: "team-b", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID: "mA", Namespace: "team-a",
					VariantStates: []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID: "mB", Namespace: "team-b",
					VariantStates: []domain.VariantReplicaState{{VariantName: "b", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			// Cluster is unconstrained relative to the quotas; team-a is capped
			// to +1 replica (2 GPUs free) and team-b to +3 (6 GPUs free).
			constraints := []*ResourceConstraints{
				{
					Pools: map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{
						"team-a": {"A100": {Limit: 4, Used: 2}},
						"team-b": {"A100": {Limit: 6, Used: 0}},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a"].TargetReplicas).To(Equal(2), "team-a bounded to +1 by its 2-GPU quota")
			Expect(dm["b"].TargetReplicas).To(BeNumerically(">", 2), "team-b has a larger quota, so it scales further")
			Expect(dm["b"].TargetReplicas).To(BeNumerically("<=", 4), "but no further than its 6-GPU quota (+3)")
		})

		It("shares one namespace budget across same-namespace models, higher priority first", func() {
			rHi := &domain.AnalyzerResult{
				ModelID: "hi", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "hi-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rLo := &domain.AnalyzerResult{
				ModelID: "lo", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "lo-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rHi, ModelScalingRequest{
					ModelID: "hi", Namespace: "team-a", Priority: 10,
					VariantStates: []domain.VariantReplicaState{{VariantName: "hi-v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
				withSatEntry(rLo, ModelScalingRequest{
					ModelID: "lo", Namespace: "team-a", Priority: 1,
					VariantStates: []domain.VariantReplicaState{{VariantName: "lo-v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			// Both models are in team-a, which has 4 free GPUs (cap 6 − 2 used) =
			// 2 replicas to share. The cluster has far more, so the namespace cap
			// is the binding constraint and the two models draw from one budget.
			constraints := []*ResourceConstraints{
				{
					Pools: map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{
						"team-a": {"A100": {Limit: 6, Used: 2}},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)
			hiAdded := dm["hi-v"].TargetReplicas - 1
			loAdded := dm["lo-v"].TargetReplicas - 1

			Expect(hiAdded+loAdded).To(Equal(2), "the shared team-a budget (4 GPUs) bounds the sum across both models")
			Expect(hiAdded).To(BeNumerically(">=", loAdded), "the higher-priority model gets at least as much of the shared budget")
		})

		It("gives a scarce shared namespace budget to the higher-priority model first", func() {
			// Only 2 free GPUs in team-a (cap 4 − 2 used) = room for exactly ONE
			// 2-GPU replica. With a 100x priority gap, the winner is deterministic:
			// hi takes the single replica, lo gets nothing. A weaker >= assertion
			// would not catch a priority inversion here.
			rHi := &domain.AnalyzerResult{
				ModelID: "hi", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "hi-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rLo := &domain.AnalyzerResult{
				ModelID: "lo", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "lo-v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rHi, ModelScalingRequest{
					ModelID: "hi", Namespace: "team-a", Priority: 100,
					VariantStates: []domain.VariantReplicaState{{VariantName: "hi-v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
				withSatEntry(rLo, ModelScalingRequest{
					ModelID: "lo", Namespace: "team-a", Priority: 1,
					VariantStates: []domain.VariantReplicaState{{VariantName: "lo-v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			constraints := []*ResourceConstraints{
				{
					Pools: map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{
						"team-a": {"A100": {Limit: 4, Used: 2}},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["hi-v"].TargetReplicas).To(Equal(2), "hi (priority 100) wins the single shared replica")
			Expect(dm["lo-v"].TargetReplicas).To(Equal(1), "lo (priority 1) gets nothing from the exhausted budget")
		})

		It("denies a type the namespace does not list instead of leaking another namespace's quota", func() {
			// Heterogeneous config: team-a caps H100 only, team-b caps A100 only.
			// A team-a model whose variant runs on A100 must be DENIED (A100 is
			// not in team-a's allowlist) — it must NOT draw on team-b's A100
			// quota via the cluster aggregate. This is the cross-namespace
			// isolation breach the closed-allowlist model closes.
			rA := &domain.AnalyzerResult{
				ModelID: "mA", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			rB := &domain.AnalyzerResult{
				ModelID: "mB", Namespace: "team-b", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "b", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(rA, ModelScalingRequest{
					ModelID: "mA", Namespace: "team-a",
					VariantStates: []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
				withSatEntry(rB, ModelScalingRequest{
					ModelID: "mB", Namespace: "team-b",
					VariantStates: []domain.VariantReplicaState{{VariantName: "b", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			constraints := []*ResourceConstraints{
				{
					Pools: map[string]ResourcePool{"H100": {Limit: 100}, "A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{
						"team-a": {"H100": {Limit: 10}}, // A100 unlisted → denied for team-a
						"team-b": {"A100": {Limit: 6}},
					},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["a"].TargetReplicas).To(Equal(1), "team-a's A100 model is denied — A100 is not in team-a's allowlist")
			Expect(dm["b"].TargetReplicas).To(BeNumerically(">", 1), "team-b scales on its own A100 quota, unaffected")
		})

		It("denies all scale-up for a closed namespace with no listed types (deny-all)", func() {
			r := &domain.AnalyzerResult{
				ModelID: "m", Namespace: "team-x", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID: "m", Namespace: "team-x",
					VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			// team-x is present (closed) but lists no types — a real deny-all.
			constraints := []*ResourceConstraints{
				{
					Pools:          map[string]ResourcePool{"A100": {Limit: 100}},
					NamespacePools: map[string]map[string]ResourcePool{"team-x": {}},
				},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v"].TargetReplicas).To(Equal(1), "deny-all namespace allocates nothing despite ample cluster GPUs")
		})

		It("honors an unlimited (-1) per-namespace cap, bounding only by the cluster budget", func() {
			r := &domain.AnalyzerResult{
				ModelID: "m", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID: "m", Namespace: "team-a",
					VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			// team-a holds an unlimited A100 cap (sentinel Limit == -1) from a
			// namespace-scope provider, composed with a separate cluster-scope
			// provider supplying the only finite bound (100 A100). This mirrors
			// production: a finite cluster Pools alongside an unlimited ns cap
			// only arises from a distinct provider, never from
			// aggregateNamespacePools (which skips unlimited). The model should
			// scale to meet demand, not be denied (the bug would drop the
			// unlimited entry and deny A100 as "unlisted").
			constraints := []*ResourceConstraints{
				{ProviderName: "ns-quota", NamespacePools: map[string]map[string]ResourcePool{
					"team-a": {"A100": {Limit: -1}},
				}},
				{ProviderName: "cluster-quota", Pools: map[string]ResourcePool{"A100": {Limit: 100}}},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v"].TargetReplicas).To(BeNumerically(">=", 4), "unlimited ns cap scales to demand, bounded only by the cluster budget")
		})

		It("does not scale a purely-unlimited namespace config with no finite cluster cap (documented V2 limitation)", func() {
			r := &domain.AnalyzerResult{
				ModelID: "m", Namespace: "team-a", RequiredCapacity: 50000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []ModelScalingRequest{
				withSatEntry(r, ModelScalingRequest{
					ModelID: "m", Namespace: "team-a",
					VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 2}},
				}),
			}
			// All-unlimited ns config: aggregateNamespacePools yields an empty
			// cluster Pools, so available is empty and fairShareScaleUp's
			// totalGPUs==0 guard stops immediately. This is the documented
			// under-provision boundary (not an isolation breach) — pinned here so
			// it can't silently change. Pools is built via aggregateNamespacePools
			// to stay faithful to what ComputeConstraints would emit.
			nsPools := map[string]map[string]ResourcePool{"team-a": {"A100": {Limit: -1}}}
			constraints := []*ResourceConstraints{
				{Pools: aggregateNamespacePools(nsPools), NamespacePools: nsPools},
			}

			decisions := optimizer.Optimize(ctx, requests, constraints)
			dm := decisionMap(decisions)

			Expect(dm["v"].TargetReplicas).To(Equal(1), "no finite cluster budget -> no V2 scaling (documented limitation)")
		})
	})
})

var _ = Describe("effectiveAvailable", func() {
	It("returns a copy of the cluster budget when the namespace is open (nil nsBudget)", func() {
		available := map[string]int{"A100": 8, "H100": 4}
		eff := effectiveAvailable(available, nil)
		Expect(eff).To(Equal(available))
		eff["A100"] = 0 // mutate the copy
		Expect(available["A100"]).To(Equal(8), "must be a copy, not an alias")
	})

	It("binds a listed finite type at min(cluster, namespace cap)", func() {
		eff := effectiveAvailable(map[string]int{"A100": 8}, map[string]int{"A100": 3})
		Expect(eff).To(HaveKeyWithValue("A100", 3))
	})

	It("denies a type the closed namespace does not list (absent => optimizer sees 0)", func() {
		eff := effectiveAvailable(map[string]int{"A100": 8, "H100": 8}, map[string]int{"A100": 3})
		Expect(eff).To(HaveKey("A100"))
		Expect(eff).NotTo(HaveKey("H100"), "unlisted type is omitted so gpusAvail==0 denies it")
	})

	It("bounds an unlimited listed type by the cluster budget when the cluster caps it", func() {
		eff := effectiveAvailable(map[string]int{"A100": 8}, map[string]int{"A100": -1})
		Expect(eff).To(HaveKeyWithValue("A100", 8))
	})

	It("treats an unlimited listed type as unbounded when the cluster does not cap it", func() {
		eff := effectiveAvailable(map[string]int{}, map[string]int{"A100": -1})
		Expect(eff).To(HaveKeyWithValue("A100", math.MaxInt))
	})

	It("denies everything for a closed deny-all namespace (empty nsBudget)", func() {
		eff := effectiveAvailable(map[string]int{"A100": 8}, map[string]int{})
		Expect(eff).To(BeEmpty())
	})
})
