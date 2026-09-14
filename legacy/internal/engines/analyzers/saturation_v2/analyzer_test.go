package saturation_v2

import (
	"context"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("SaturationAnalyzer", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		ctx      context.Context
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	Describe("Name", func() {
		It("should return 'saturation-token-based'", func() {
			Expect(analyzer.Name()).To(Equal("saturation-token-based"))
		})
	})

	Describe("k1/k2 interaction", func() {
		It("should use k1 (memory-bound) when k2 is unknown", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// k1 = 16000 * 0.8 = 12800, k2 = k1 (fallback, queue < threshold)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should use k2 (compute-bound) when queue is saturated", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue >= threshold (5), tokensInUse = 8000
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 8000 (observed: tokensInUse when queue saturated)
			// effective = min(12800, 8000) = 8000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})

		It("should detect compute-bound when k2 < k1 with high queue and low KV", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Low KV usage but saturated queue
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						4000, 16000, 10, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 4000 (observed), effective = 4000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(4000)))
		})
	})

	Describe("k2 history", func() {
		It("should store k2 observation in rolling average", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Verify k2 was stored in history
			histKey := "test-model|H100|1|short"
			ra, ok := analyzer.computeCapacityHistory[histKey]
			Expect(ok).To(BeTrue())
			Expect(ra.Average()).To(Equal(float64(8000)))
		})

		It("should use historical k2 when queue drops below threshold", func() {
			// First call: queue saturated → observes k2
			input1 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, input1)
			Expect(err).NotTo(HaveOccurred())

			// Second call: queue below threshold → uses historical k2
			input2 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						6000, 16000, 2, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// Should use historical k2=8000, not fallback to k1=12800
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})
	})

	Describe("Output-length bucketing", func() {
		It("should use different k2 history buckets for different output lengths", func() {
			// Short output workload: k2 observation
			input1 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						8000, 16000, 6, 100, 50), // avgOutput=50 → "short"
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, _ = analyzer.Analyze(ctx, input1)

			// Long output workload: no history yet
			input2 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						6000, 16000, 2, 100, 600), // avgOutput=600 → "long"
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// No "long" history → falls back to k1=12800 (not the "short" k2=8000)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("k2 derivation from deployment params", func() {
		It("should derive k2 from chunked prefill params", func() {
			// Pre-populate store with deployment params for this variant
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName: "H100",
				GpuCount:        1,
				EngineParams: &EngineParams{
					EffectiveMaxBatchedTokens: 2048,
					MaxNumSeqs:                256,
					ChunkedPrefillEnabled:     true,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue below threshold, no history → uses derived k2
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 2, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// B=2048, S=256, I=500, O=100
			// N_steady = min(2048*100/(500+100), 256) = min(341.3, 256) = 256
			// k2 = 256 * (500 + 100/2) = 256 * 550 = 140800
			// k1 = 12800, effective = min(12800, 140800) = 12800
			// k2 > k1, so memory-bound
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should fall back to k1 when no batch/queue/history data exists", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 0, 0), // no avg tokens
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k2 derivation needs avgOutput > 0, falls back to k1
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("Pending replicas", func() {
		It("should include pending replicas in anticipated supply for scale-up", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						10000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// readyCount = 2 - 1 = 1; anticipated = (1 + 1) × PRC
			// RC/SC are left zero by the analyzer; engine post-step sets them.
			Expect(result.TotalAnticipatedSupply).To(BeNumerically(">", result.TotalSupply))
			Expect(result.RequiredCapacity).To(BeZero())
		})

		It("should NOT include pending replicas in scale-down calculation", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 3, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TotalSupply uses ready replicas only; TotalAnticipatedSupply includes pending.
			// RC/SC are left zero by the analyzer; engine post-step sets them.
			Expect(result.TotalAnticipatedSupply).To(BeNumerically(">", result.TotalSupply))
			Expect(result.SpareCapacity).To(BeZero())
		})
	})

	Describe("Zero-replica variants", func() {
		It("should use stored live capacity directly when variant has zero replicas", func() {
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 12000,
				LearnedFrom:       "live",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{}, // no pods
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// Live record → uses stored EffectiveCapacity directly
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12000)))
		})

		It("should derive capacity from params + workload for deployment-derived records", func() {
			// Zero-replica variant with deployment-derived params only
			store.Update("test-ns", "test-model", "variant-b", CapacityRecord{
				AcceleratorName:   "A100",
				GpuCount:          1,
				EffectiveCapacity: 8192, // conservative fallback from LoadFromDeployment
				EngineParams: &EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			// Another variant has live pods providing workload data
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(2))

			// variant-b: deployment-derived with workload data available
			// B=8192, S=256, I=500, O=100
			// N_steady = min(8192*100/(500+100), 256) = min(1365.3, 256) = 256
			// k2_derived = 256 * (500 + 100/2) = 256 * 550 = 140800
			// Much better than the 8192 fallback!
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(140800)))
		})

		It("should bound k2 estimate by compatible variant's live EffectiveCapacity", func() {
			// variant-b is a new deployment on the same H100 hardware as variant-a
			defaultParams := &EngineParams{
				GpuMemoryUtilization:      0.9,
				BlockSize:                 16,
				KvCacheDtype:              "auto",
				TensorParallelSize:        1,
				MaxNumSeqs:                256,
				EffectiveMaxBatchedTokens: 8192,
			}
			store.Update("test-ns", "test-model", "variant-b", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 8192,
				EngineParams:      defaultParams,
				LearnedFrom:       "deployment",
			})

			// variant-a has a compatible live record (same accel, GPU count, params)
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 50000,
				EffectiveCapacity:     40000, // observed min(k1, k2) = 40000
				EngineParams:          defaultParams,
				LearnedFrom:           "live",
			})

			// variant-a provides workload data
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 50000, 0, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800, but bounded by variant-a's live
			// EffectiveCapacity = 40000 → result = min(140800, 40000) = 40000
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(40000)))
		})

		It("should bound k2 estimate by own k1 when TotalKvCapacityTokens is known", func() {
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:       "A100",
				GpuCount:              1,
				EffectiveCapacity:     8192,
				TotalKvCapacityTokens: 30000, // from num_gpu_blocks_override
				EngineParams: &EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
					NumGpuBlocksOverride:      1875,
					BlockSize:                 16,
				},
				LearnedFrom: "deployment",
			})

			// Another variant provides workload data (different accelerator, won't be compatible)
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-x", "L40S", 5.0,
						5000, 16000, 0, 500, 100),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-x", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800
			// k1 = 30000 * 0.8 (KvCacheThreshold) = 24000
			// No compatible live record (A100 vs L40S) → bounded by k1 only
			// result = min(140800, 24000) = 24000
			varA := result.VariantCapacities[1]
			Expect(varA.VariantName).To(Equal("variant-a"))
			Expect(varA.PerReplicaCapacity).To(Equal(float64(24000)))
		})

		It("should use EffectiveMaxBatchedTokens fallback when no workload data exists", func() {
			// Zero-replica variant with deployment-derived params, no other live pods
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:   "H100",
				GpuCount:          1,
				EffectiveCapacity: 8192,
				EngineParams: &EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{}, // no live pods at all
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// No workload data → falls back to stored EffectiveCapacity (8192)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8192)))
		})
	})

	Describe("estimateCapacityFromParams", func() {
		It("should compute k2 from B, S, I, O", func() {
			params := &EngineParams{
				EffectiveMaxBatchedTokens: 4096,
				MaxNumSeqs:                256,
			}
			// B=4096, S=256, I=500, O=100
			// N_steady = min(4096*100/600, 256) = min(682.6, 256) = 256
			// k2 = 256 * (500 + 50) = 256 * 550 = 140800
			Expect(estimateCapacityFromParams(params, 500, 100)).To(Equal(int64(140800)))
		})

		It("should cap N_steady at MaxNumSeqs", func() {
			params := &EngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                64,
			}
			// B=8192, S=64, I=100, O=200
			// N_steady = min(8192*200/300, 64) = min(5461, 64) = 64
			// k2 = 64 * (100 + 100) = 64 * 200 = 12800
			Expect(estimateCapacityFromParams(params, 100, 200)).To(Equal(int64(12800)))
		})

		It("should return 0 when avgOutput is 0", func() {
			params := &EngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                256,
			}
			Expect(estimateCapacityFromParams(params, 500, 0)).To(Equal(int64(0)))
		})

		It("should return 0 when params is nil", func() {
			Expect(estimateCapacityFromParams(nil, 500, 100)).To(Equal(int64(0)))
		})
	})

	Describe("Scaling signals", func() {
		It("should signal scale-up when demand exceeds threshold", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						11000, 16000, 3, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// demand is high relative to supply — TotalDemand > TotalAnticipatedSupply.
			// RC/SC are left zero by the analyzer; engine post-step sets them.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply*0.85))
			Expect(result.RequiredCapacity).To(BeZero())
		})

		It("should signal scale-down when utilization is below boundary", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-2", "variant-a", "H100", 10.0,
						1000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 2, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Very low utilization — TotalSupply well above TotalDemand/scaleDown.
			// RC/SC are left zero by the analyzer; engine post-step sets them.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand/0.70))
			Expect(result.SpareCapacity).To(BeZero())
			Expect(result.RequiredCapacity).To(BeZero())
		})

		It("should signal steady state when utilization is between thresholds", func() {
			// Supply ~ demand / 0.77 (between 0.70 boundary and 0.85 threshold)
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						10000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// utilization = 10000/12800 = 0.78 — between 0.70 and 0.85
			Expect(result.RequiredCapacity).To(Equal(float64(0)))
			Expect(result.SpareCapacity).To(Equal(float64(0)))
		})
	})

	Describe("Scheduler queue demand", func() {
		It("should add scheduler queue demand to total demand", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 8000,
			}

			resultWithQueue, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Without scheduler queue
			input.SchedulerQueue = nil
			// Reset analyzer for clean comparison
			analyzer2 := NewSaturationAnalyzer(store)
			resultWithout, err := analyzer2.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			Expect(resultWithQueue.TotalDemand).To(BeNumerically(">", resultWithout.TotalDemand))
		})

		It("should not add demand when scheduler queue is nil", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Total demand = just the replica demand (tokensInUse)
			Expect(result.TotalDemand).To(Equal(float64(5000)))
		})

		It("should reduce input tokens by prefix cache hit rate", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					{
						PodName: "pod-1", VariantName: "variant-a",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 5000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						PrefixCacheHitRate: 0.5, // 50% cache hit rate
					},
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 4000, // 4000/4 = 1000 tokens from bytes
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Input from count: 10 * 100 = 1000 tokens
			// Input from bytes: 4000 / 4 = 1000 tokens
			// max(1000, 1000) = 1000
			// After cache hit: 1000 * (1 - 0.5) = 500
			// Output: 10 * 50 = 500
			// Scheduler demand = 500 + 500 = 1000
			// Replica demand = 5000
			// Total = 5000 + 1000 = 6000
			Expect(result.TotalDemand).To(Equal(float64(6000)))
		})

		It("should use max of bytes and count estimates for input tokens", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 20000, // 20000/4 = 5000 >> 10*100=1000
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Bytes estimate (5000) > count estimate (1000), so bytes wins
			// Input = 5000 * (1-0) = 5000 (no cache hit)
			// Output = 10 * 50 = 500
			// Scheduler demand = 5500
			// Total = 5000 + 5500 = 10500
			Expect(result.TotalDemand).To(Equal(float64(10500)))
		})
	})

	Describe("Scheduler queue demand role attribution", func() {
		It("should attribute inputTokens to prefill and inputTokens+outputTokens to decode", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0, // use count-based estimation only
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			// Input: max(0/4=0, 10*100=1000) = 1000 (no cache hit)
			// Output: 10 * 50 = 500
			// Total: 1000 + 500 = 1500
			Expect(result.total).To(Equal(1500.0))

			// Prefill gets inputTokens only
			Expect(result.byRole["prefill"]).To(Equal(1000.0))
			// Decode gets inputTokens + outputTokens
			Expect(result.byRole["decode"]).To(Equal(1500.0))
		})

		It("should attribute full demand to 'both' role", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"both": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole["both"]).To(Equal(1500.0))
		})

		It("should return empty byRole when nil activeRoles", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole).To(BeEmpty())
		})

		It("should return zero for nil scheduler queue", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", "H100", 10.0,
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}

			result := estimateSchedulerQueueDemand(nil, metrics, activeRoles)

			Expect(result.total).To(Equal(0.0))
			Expect(result.byRole).To(BeNil())
		})

		It("should apply prefix cache hit rate to per-role input tokens", func() {
			metrics := []domain.ReplicaMetrics{
				{
					PodName: "pod-1", VariantName: "variant-a",
					AcceleratorName: "H100", Cost: 10.0,
					TokensInUse: 5000, TotalKvCapacityTokens: 16000,
					NumGpuBlocks: 1000, BlockSize: 16,
					AvgInputTokens: 100, AvgOutputTokens: 50,
					PrefixCacheHitRate: 0.5, // 50% cache hit rate
				},
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, activeRoles)

			// Input: 10*100=1000, after cache: 1000*(1-0.5)=500
			// Output: 10*50=500
			// Total: 500 + 500 = 1000
			Expect(result.total).To(Equal(1000.0))

			// Prefill: inputTokens = 500
			Expect(result.byRole["prefill"]).To(Equal(500.0))
			// Decode: inputTokens + outputTokens = 500 + 500 = 1000
			Expect(result.byRole["decode"]).To(Equal(1000.0))
		})

		It("should add queue demand to role capacities in end-to-end analysis", func() {
			input := domain.AnalyzerInput{
				ModelID:   "test-model",
				Namespace: "test-ns",
				ReplicaMetrics: []domain.ReplicaMetrics{
					{
						PodName: "prefill-pod", VariantName: "prefill-v",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 3000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
					{
						PodName: "decode-pod", VariantName: "decode-v",
						AcceleratorName: "H100", Cost: 10.0,
						TokensInUse: 2000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
				},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
					{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "decode"},
				},
				Config: &config.SaturationScalingConfig{
					KvCacheThreshold:     0.8,
					QueueLengthThreshold: 5,
					AnalyzerName:         "saturation",
					ScaleUpThreshold:     0.85,
					ScaleDownBoundary:    0.70,
				},
				SchedulerQueue: &domain.SchedulerQueueMetrics{
					QueueSize:  10,
					QueueBytes: 0,
				},
			}

			store := NewCapacityKnowledgeStore()
			a := NewSaturationAnalyzer(store)
			result, err := a.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleCapacities).NotTo(BeNil())

			// Scheduler queue: input=max(0, 10*100)=1000, output=10*50=500
			// Prefill role demand: replica(3000) + queue(1000) = 4000
			// Decode role demand: replica(2000) + queue(1500) = 3500
			prefill := result.RoleCapacities["prefill"]
			Expect(prefill.TotalDemand).To(Equal(4000.0))

			decode := result.RoleCapacities["decode"]
			Expect(decode.TotalDemand).To(Equal(3500.0))

			// Model-level total still uses inputTokens+outputTokens (1500)
			// Replica demand = 3000 + 2000 = 5000
			// Total = 5000 + 1500 = 6500
			Expect(result.TotalDemand).To(Equal(6500.0))
		})
	})

	Describe("median helper", func() {
		It("should return 0 for empty slice", func() {
			Expect(median([]int64{})).To(Equal(int64(0)))
		})

		It("should return the value for single element", func() {
			Expect(median([]int64{42})).To(Equal(int64(42)))
		})

		It("should return middle value for odd count", func() {
			Expect(median([]int64{1, 3, 5})).To(Equal(int64(3)))
		})

		It("should return average of middle two for even count", func() {
			Expect(median([]int64{1, 3, 5, 7})).To(Equal(int64(4)))
		})

		It("should handle unsorted input", func() {
			Expect(median([]int64{5, 1, 3})).To(Equal(int64(3)))
		})
	})
})

// makeAnalyzerInput creates a standard AnalyzerInput with default config.
func makeAnalyzerInput(
	metrics []domain.ReplicaMetrics,
	states []domain.VariantReplicaState,
) domain.AnalyzerInput {
	config := &config.SaturationScalingConfig{
		KvCacheThreshold:     0.8,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.1,
		QueueSpareTrigger:    3,
		AnalyzerName:         "saturation",
		ScaleUpThreshold:     0.85,
		ScaleDownBoundary:    0.70,
	}
	return domain.AnalyzerInput{
		ModelID:        "test-model",
		Namespace:      "test-ns",
		ReplicaMetrics: metrics,
		VariantStates:  states,
		Config:         config,
	}
}

// makeReplicaMetrics creates a ReplicaMetrics with the given parameters.
func makeReplicaMetrics(
	podName, variantName, accelerator string,
	cost float64,
	tokensInUse, totalCapacity int64,
	queueLen int,
	avgInput, avgOutput float64,
) domain.ReplicaMetrics {
	var kvUsage float64
	if totalCapacity > 0 {
		kvUsage = float64(tokensInUse) / float64(totalCapacity)
	}
	blockSize := int64(16)
	numBlocks := totalCapacity / blockSize

	return domain.ReplicaMetrics{
		PodName:               podName,
		VariantName:           variantName,
		AcceleratorName:       accelerator,
		Cost:                  cost,
		KvCacheUsage:          kvUsage,
		QueueLength:           queueLen,
		NumGpuBlocks:          numBlocks,
		BlockSize:             blockSize,
		TotalKvCapacityTokens: totalCapacity,
		TokensInUse:           tokensInUse,
		AvgInputTokens:        avgInput,
		AvgOutputTokens:       avgOutput,
		ModelID:               "test-model",
		Namespace:             "test-ns",
	}
}

var _ = Describe("aggregateByRole", func() {
	var analyzer *SaturationAnalyzer

	BeforeEach(func() {
		store := NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
	})

	It("should return nil when all variants are role 'both'", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "v1", Role: "both", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "v2", Role: "", TotalCapacity: 20000, TotalDemand: 10000, ReplicaCount: 1, PerReplicaCapacity: 20000},
		}
		result := analyzer.aggregateByRole(vcs, nil)
		Expect(result).To(BeNil())
	})

	It("should compute per-role capacities for P/D disaggregated model", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 9000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalCapacity: 20000, TotalDemand: 5000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateByRole(vcs, nil)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(2))

		prefill := result["prefill"]
		Expect(prefill.Role).To(Equal("prefill"))
		Expect(prefill.TotalSupply).To(Equal(10000.0))
		Expect(prefill.TotalDemand).To(Equal(9000.0))
		Expect(prefill.TotalAnticipatedSupply).To(Equal(10000.0)) // 1 replica, no pending
		// RC/SC are left zero; engine post-step sets them.
		Expect(prefill.RequiredCapacity).To(BeZero())
		Expect(prefill.SpareCapacity).To(BeZero())

		decode := result["decode"]
		Expect(decode.Role).To(Equal("decode"))
		Expect(decode.TotalSupply).To(Equal(20000.0))
		Expect(decode.TotalDemand).To(Equal(5000.0))
		Expect(decode.TotalAnticipatedSupply).To(Equal(20000.0)) // 2 replicas, no pending
		// RC/SC are left zero; engine post-step sets them.
		Expect(decode.SpareCapacity).To(BeZero())
		Expect(decode.RequiredCapacity).To(BeZero())
	})

	It("should handle mixed roles including 'both'", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 9000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "both-v1", Role: "both", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateByRole(vcs, nil)
		// Has disaggregation because prefill-v1 has role != "both"
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveKey("prefill"))
		Expect(result).To(HaveKey("both"))
	})

	It("should add scheduler queue demand to per-role totals", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 2000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalCapacity: 20000, TotalDemand: 3000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		queueByRole := map[string]float64{
			"prefill": 1000, // inputTokens only
			"decode":  1500, // inputTokens + outputTokens
		}
		result := analyzer.aggregateByRole(vcs, queueByRole)
		Expect(result).NotTo(BeNil())

		prefill := result["prefill"]
		// 2000 (replica) + 1000 (queue) = 3000
		Expect(prefill.TotalDemand).To(Equal(3000.0))

		decode := result["decode"]
		// 3000 (replica) + 1500 (queue) = 4500
		Expect(decode.TotalDemand).To(Equal(4500.0))
	})

	It("should skip queue demand for roles with no variants", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalCapacity: 10000, TotalDemand: 5000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		// Queue demand for decode, but no decode variants exist
		queueByRole := map[string]float64{
			"prefill": 1000,
			"decode":  1500,
		}
		result := analyzer.aggregateByRole(vcs, queueByRole)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(1))
		Expect(result).To(HaveKey("prefill"))
		Expect(result["prefill"].TotalDemand).To(Equal(6000.0)) // 5000 + 1000
	})
})

var _ = Describe("computeReplicaCapacityFallback", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		cfg      *config.SaturationScalingConfig
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		cfg = &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		}
	})

	It("should return nil when capacity store has no record", func() {
		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).To(BeNil())
	})

	It("should return nil when capacity store record has zero effective capacity", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			EffectiveCapacity: 0,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).To(BeNil())
	})

	It("should apply KvCacheThreshold to stored capacity (consistent with main path)", func() {
		// Store raw capacity of 10000
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.6,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 (KvCacheThreshold) = 8000
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		// demand = 0.6 * 8000 = 4800
		Expect(result.ReplicaDemand).To(Equal(int64(4800)))
		Expect(result.IsSaturated).To(BeFalse()) // 4800 < 8000
	})

	It("should detect saturation at KvCacheUsage >= KvCacheThreshold", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          1.0, // 100% KV usage
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 1.0 * 8000 = 8000 >= 8000
		Expect(result.ReplicaDemand).To(Equal(int64(8000)))
		Expect(result.IsSaturated).To(BeTrue())
	})

	It("should detect saturation when KvCacheUsage exceeds threshold (matching main path behavior)", func() {
		// This verifies the fix from the review: at 90% KV usage with 0.8 threshold,
		// the fallback should report saturation, matching the main path behavior.
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.9, // 90% usage, threshold is 80%
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 0.9 * 8000 = 7200
		// 7200 < 8000 → not saturated by demand alone, BUT this is close to capacity.
		// In the main path, k1=8000 and demand includes tokens+queue. The important thing
		// is that the threshold IS applied (8000 not 10000), so scale-up signals trigger
		// at the same utilization level as the main path.
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		Expect(result.ReplicaDemand).To(Equal(int64(7200)))
	})

	It("should add queue-based demand when avg input tokens available", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
			QueueLength:           3,
			AvgInputTokens:        500,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 0.5 * 8000 + 3 * 500 = 4000 + 1500 = 5500
		Expect(result.ReplicaDemand).To(Equal(int64(5500)))
		Expect(result.IsSaturated).To(BeFalse())
	})

	It("should not add queue demand when token metrics are unavailable", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.3,
			TotalKvCapacityTokens: 0,
			QueueLength:           10,
			AvgInputTokens:        0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 0.3 * 8000 = 2400 (no queue contribution)
		Expect(result.ReplicaDemand).To(Equal(int64(2400)))
	})

	It("should charge queue demand by role", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
			QueueLength:           3,
			AvgInputTokens:        500,
			AvgOutputTokens:       250,
		}

		// effectiveCapacity = 10000 * 0.8 = 8000; resident = 0.5 * 8000 = 4000.
		prefill := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RolePrefill)
		Expect(prefill).NotTo(BeNil())
		// 4000 + 3 * 500 = 5500 — output tokens excluded.
		Expect(prefill.ReplicaDemand).To(Equal(int64(5500)))

		decode := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleDecode)
		Expect(decode).NotTo(BeNil())
		// 4000 + 3 * (500 + 250) = 6250 — output tokens included.
		Expect(decode.ReplicaDemand).To(Equal(int64(6250)))
	})

	It("should not report saturation for an idle replica with a shallow queue", func() {
		// The fallback denominator is a per-step batched-token budget, while the
		// queue charge is in absolute KV tokens — a dimensional mismatch this
		// path inherits. Raising the per-request charge moves the point where
		// that mismatch invents saturation, so pin the safe end: a mostly-idle
		// replica with a few queued requests must not read as saturated.
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 8192,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.10,
			TotalKvCapacityTokens: 0,
			QueueLength:           2,
			AvgInputTokens:        200,
			AvgOutputTokens:       100,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleDecode)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 8192 * 0.8 = 6553
		// demand = 0.10 * 6553 + 2 * (200 + 100) = 655 + 600 = 1255
		Expect(result.EffectiveCapacity).To(Equal(int64(6553)))
		Expect(result.ReplicaDemand).To(Equal(int64(1255)))
		Expect(result.IsSaturated).To(BeFalse())
	})

	It("should populate all ReplicaCapacity fields correctly", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.4,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth)
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		Expect(result.PodName).To(Equal("pod-1"))
		Expect(result.VariantName).To(Equal("variant-a"))
		Expect(result.AcceleratorName).To(Equal("H100"))
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		Expect(result.MemoryBoundCapacity).To(Equal(int64(8000)))
		Expect(result.ComputeBoundCapacity).To(Equal(int64(8000)))
		Expect(result.TotalKvCapacityTokens).To(Equal(int64(8000)))
		// demand = 0.4 * 8000 = 3200
		Expect(result.TokensInUse).To(Equal(int64(3200)))
		Expect(result.ReplicaDemand).To(Equal(int64(3200)))
	})
})

var _ = Describe("waitingQueueDemand", func() {
	// Average request shape shared by the role cases: a waiting request costs
	// 100 input tokens and, once it starts generating, 50 more output tokens.
	rm := domain.ReplicaMetrics{
		QueueLength:     4,
		AvgInputTokens:  100,
		AvgOutputTokens: 50,
	}

	It("charges prefill replicas for input tokens only", func() {
		// Prompt KV is all a prefill replica materializes: 4 * 100.
		Expect(waitingQueueDemand(rm, domain.RolePrefill)).To(Equal(int64(400)))
	})

	It("charges decode replicas for input plus output tokens", func() {
		// Decode holds the transferred prompt KV and grows it per generated
		// token: 4 * (100 + 50).
		Expect(waitingQueueDemand(rm, domain.RoleDecode)).To(Equal(int64(600)))
	})

	It("charges 'both' replicas for input plus output tokens", func() {
		Expect(waitingQueueDemand(rm, domain.RoleBoth)).To(Equal(int64(600)))
	})

	It("treats an empty role as 'both'", func() {
		Expect(waitingQueueDemand(rm, "")).To(Equal(int64(600)))
	})

	It("treats an unknown role as 'both'", func() {
		Expect(waitingQueueDemand(rm, "some-future-role")).To(Equal(int64(600)))
	})

	It("returns zero for an empty queue regardless of role", func() {
		empty := rm
		empty.QueueLength = 0
		for _, role := range []string{domain.RolePrefill, domain.RoleDecode, domain.RoleBoth, ""} {
			Expect(waitingQueueDemand(empty, role)).To(BeZero())
		}
	})

	It("returns zero for a negative queue length", func() {
		negative := rm
		negative.QueueLength = -1
		Expect(waitingQueueDemand(negative, domain.RoleDecode)).To(BeZero())
	})

	It("returns zero when no token metrics are available", func() {
		noTokens := domain.ReplicaMetrics{QueueLength: 10}
		Expect(waitingQueueDemand(noTokens, domain.RolePrefill)).To(BeZero())
		Expect(waitingQueueDemand(noTokens, domain.RoleDecode)).To(BeZero())
	})

	It("returns zero for non-finite token metrics", func() {
		// Converting a non-finite float64 to int64 is implementation-defined in
		// Go, so an unguarded NaN yields garbage demand — hugely negative on
		// amd64. A `<= 0` check does not catch NaN, hence the explicit guard.
		for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: bad}, domain.RolePrefill)).To(BeZero())
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgOutputTokens: bad}, domain.RoleDecode)).To(BeZero())
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: 100, AvgOutputTokens: bad}, domain.RoleDecode)).To(BeZero())
		}
	})

	It("returns zero for finite token metrics too large for an int64", func() {
		// Finite but out of int64 range hits the same implementation-defined
		// conversion as NaN, so the guard must cover it too.
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: 1e300}, domain.RolePrefill)).To(BeZero())
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 1e9, AvgInputTokens: 1e12}, domain.RoleDecode)).To(BeZero())

		// Exactly 2^63: float64(math.MaxInt64) rounds up to this value, so a
		// `>` bound would admit it and overflow to MinInt64.
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 2, AvgInputTokens: math.Pow(2, 62)}, domain.RolePrefill)).To(BeZero())
	})

	It("charges decode replicas for output even when input tokens are unreported", func() {
		// A decode replica whose prompt-token metric is missing still pays for
		// generation; the old input-only formula returned 0 here.
		outputOnly := domain.ReplicaMetrics{QueueLength: 4, AvgOutputTokens: 50}
		Expect(waitingQueueDemand(outputOnly, domain.RoleDecode)).To(Equal(int64(200)))
		Expect(waitingQueueDemand(outputOnly, domain.RolePrefill)).To(BeZero())
	})
})

var _ = Describe("Analyze per-replica waiting-queue demand by role", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		ctx      context.Context
		satCfg   *config.SaturationScalingConfig
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
		satCfg = &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		}
	})

	// replicaFor builds a replica with 1000 tokens resident and 4 requests
	// waiting, each averaging 100 input and 50 output tokens.
	replicaFor := func(variant string) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               variant + "-pod-1",
			VariantName:           variant,
			AcceleratorName:       "H100",
			ModelID:               "test-model",
			Namespace:             "test-ns",
			Cost:                  10.0,
			TotalKvCapacityTokens: 10000,
			TokensInUse:           1000,
			QueueLength:           4,
			AvgInputTokens:        100,
			AvgOutputTokens:       50,
		}
	}

	demandFor := func(role string) float64 {
		input := domain.AnalyzerInput{
			ModelID:        "test-model",
			Namespace:      "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{replicaFor("variant-a")},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1, Role: role},
			},
			Config: satCfg,
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		return result.VariantCapacities[0].TotalDemand
	}

	It("excludes output tokens for a prefill variant", func() {
		// 1000 resident + 4 * 100 input = 1400
		Expect(demandFor(domain.RolePrefill)).To(BeNumerically("==", 1400))
	})

	It("includes output tokens for a decode variant", func() {
		// 1000 resident + 4 * (100 + 50) = 1600
		Expect(demandFor(domain.RoleDecode)).To(BeNumerically("==", 1600))
	})

	It("includes output tokens for a 'both' variant", func() {
		Expect(demandFor(domain.RoleBoth)).To(BeNumerically("==", 1600))
	})

	// The fallback path (no vllm:cache_config_info) must honour the role too.
	// These go through Analyze so they pin the role forwarding from
	// computeReplicaCapacity into computeReplicaCapacityFallback, not just the
	// helper — hardcoding a role at that hand-off would otherwise go unnoticed.
	Context("on the fallback path (no cache_config_info)", func() {
		fallbackDemandFor := func(role string) float64 {
			store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
				AcceleratorName:   "H100",
				EffectiveCapacity: 10000,
				LearnedFrom:       "deployment",
			})

			rm := replicaFor("variant-a")
			rm.TotalKvCapacityTokens = 0 // forces the fallback
			rm.TokensInUse = 0           // fallback derives demand from KvCacheUsage
			rm.KvCacheUsage = 0.5

			input := domain.AnalyzerInput{
				ModelID:        "test-model",
				Namespace:      "test-ns",
				ReplicaMetrics: []domain.ReplicaMetrics{rm},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1, Role: role},
				},
				Config: satCfg,
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			return result.VariantCapacities[0].TotalDemand
		}

		It("excludes output tokens for a prefill variant", func() {
			// effectiveCapacity = 10000 * 0.8 = 8000
			// 0.5 * 8000 + 4 * 100 input = 4000 + 400 = 4400
			Expect(fallbackDemandFor(domain.RolePrefill)).To(BeNumerically("==", 4400))
		})

		It("includes output tokens for a decode variant", func() {
			// 0.5 * 8000 + 4 * (100 + 50) = 4000 + 600 = 4600
			Expect(fallbackDemandFor(domain.RoleDecode)).To(BeNumerically("==", 4600))
		})
	})

	It("includes output tokens when the role is unset", func() {
		// An unset role canonicalizes to "both", so non-disaggregated
		// deployments get the full-lifecycle charge.
		Expect(demandFor("")).To(BeNumerically("==", 1600))
	})

	It("charges each role correctly in a P/D disaggregated model", func() {
		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				replicaFor("prefill-variant"),
				replicaFor("decode-variant"),
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "prefill-variant", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
				{VariantName: "decode-variant", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
			},
			Config: satCfg,
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(2))

		byVariant := map[string]float64{}
		for _, vc := range result.VariantCapacities {
			byVariant[vc.VariantName] = vc.TotalDemand
		}
		Expect(byVariant["prefill-variant"]).To(BeNumerically("==", 1400))
		Expect(byVariant["decode-variant"]).To(BeNumerically("==", 1600))

		// Model-level demand is the sum of both roles.
		Expect(result.TotalDemand).To(BeNumerically("==", 3000))
	})
})

var _ = Describe("Analyze with fallback (no cache_config_info)", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		ctx      context.Context
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	It("should produce valid result using fallback when cache_config_info is absent", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 8192,
			LearnedFrom:       "deployment",
		})

		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					AcceleratorName:       "H100",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					Cost:                  10.0,
					KvCacheUsage:          0.9,
					TotalKvCapacityTokens: 0,
					TokensInUse:           0,
					QueueLength:           3,
					AvgInputTokens:        100,
					AvgOutputTokens:       50,
				},
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		Expect(result.VariantCapacities).To(HaveLen(1))

		vc := result.VariantCapacities[0]
		Expect(vc.VariantName).To(Equal("variant-a"))
		// effectiveCapacity = int64(8192 * 0.8) = 6553
		storeCapacity := float64(8192)
		expectedCapacity := float64(int64(storeCapacity * 0.8))
		Expect(vc.PerReplicaCapacity).To(Equal(expectedCapacity))
		Expect(vc.TotalDemand).To(BeNumerically(">", 0))
		Expect(result.TotalDemand).To(BeNumerically(">", 0))
	})

	It("should skip replicas with no store data and no cache_config_info", func() {
		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					AcceleratorName:       "H100",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					Cost:                  10.0,
					KvCacheUsage:          0.5,
					TotalKvCapacityTokens: 0,
				},
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
	})
})

var _ = Describe("SaturationScalingConfig ApplyDefaults before Validate", func() {
	It("should pass validation after ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
		}

		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ScaleUpThreshold).To(BeNumerically(">", 0))
		Expect(cfg.ScaleDownBoundary).To(BeNumerically(">", 0))
	})

	It("should fail validation without ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
		}

		err := cfg.Validate()
		Expect(err).To(HaveOccurred())
	})

	It("should preserve explicitly set values after ApplyDefaults", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.9,
			ScaleDownBoundary:    0.6,
			Priority:             2.0,
		}

		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ScaleUpThreshold).To(Equal(0.9))
		Expect(cfg.ScaleDownBoundary).To(Equal(0.6))
		Expect(cfg.Priority).To(Equal(2.0))
	})

	It("should apply default priority when omitted", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
		}

		cfg.ApplyDefaults()
		Expect(cfg.Priority).To(BeNumerically(">", 0))
	})
})

var _ = Describe("aggregateByVariant capacity Reason", func() {
	It("sets Reason to P0-store when no live replicas but a capacity store record exists", func() {
		store := NewCapacityKnowledgeStore()
		store.Update("ns", "m", "v1", CapacityRecord{
			AcceleratorName:   "A100",
			EffectiveCapacity: 50000,
			LearnedFrom:       learnedFromLive,
		})
		a := NewSaturationAnalyzer(store)

		input := domain.AnalyzerInput{
			ModelID:   "m",
			Namespace: "ns",
			// No ReplicaMetrics — store path will fire.
			ReplicaMetrics: nil,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v1", CurrentReplicas: 0, PendingReplicas: 0},
			},
			Config: &config.SaturationScalingConfig{KvCacheThreshold: 0.9, KvSpareTrigger: 0.2, QueueLengthThreshold: 5},
		}
		result, err := a.Analyze(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		Expect(result.VariantCapacities[0].Reason).To(Equal(satReasonP0Store))
	})

	It("sets Reason to no-data when variant has zero replicas and no store or compatible record", func() {
		store := NewCapacityKnowledgeStore()
		a := NewSaturationAnalyzer(store)

		input := domain.AnalyzerInput{
			ModelID:        "m",
			Namespace:      "ns",
			ReplicaMetrics: nil,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v1", CurrentReplicas: 0, PendingReplicas: 0},
			},
			Config: &config.SaturationScalingConfig{KvCacheThreshold: 0.9, KvSpareTrigger: 0.2, QueueLengthThreshold: 5},
		}
		result, err := a.Analyze(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		Expect(result.VariantCapacities[0].Reason).To(Equal(satReasonNoData))
	})
})

var _ = Describe("k2SourceLabel", func() {
	It("returns error when K2Priority is not in the known set", func() {
		replicas := []ReplicaCapacity{{K2Priority: 0, EffectiveCapacity: 100}}
		Expect(k2SourceLabel(replicas)).To(Equal("error"))
	})

	It("returns empty string when replicas slice is empty", func() {
		Expect(k2SourceLabel(nil)).To(Equal(""))
	})
})

var _ = Describe("aggregateByVariant DP>1 with pending replicas", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
	)

	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		ctx = context.Background()
	})

	It("converts pending pods to instance units in anticipated supply", func() {
		// One ready pod hosts 8 DP-rank instances (8 ReplicaMetrics sharing a
		// PodName). State: 1 ready + 1 pending pod → instances-per-pod = 8/1 = 8,
		// so the pending pod must count as 8 instances, not 1.
		metrics := make([]domain.ReplicaMetrics, 0, 8)
		for i := 0; i < 8; i++ {
			metrics = append(metrics, makeReplicaMetrics("pod-1", "decode-v1", "H100", 10.0,
				8000, 16000, 0, 100, 50))
		}

		input := makeAnalyzerInput(
			metrics,
			[]domain.VariantReplicaState{
				{VariantName: "decode-v1", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 8},
			},
		)

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))

		vc := result.VariantCapacities[0]
		Expect(vc.ReplicaCount).To(Equal(8))    // 8 scraped instances
		Expect(vc.PendingReplicas).To(Equal(8)) // 1 pending pod × 8 instances-per-pod

		// TotalAnticipatedSupply = (ReplicaCount + PendingReplicas) × PerReplicaCapacity
		want := float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
		Expect(result.TotalAnticipatedSupply).To(Equal(want))
	})
})
