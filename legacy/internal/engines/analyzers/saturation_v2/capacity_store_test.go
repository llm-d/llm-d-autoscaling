package saturation_v2

import (
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("CapacityKnowledgeStore", func() {

	var store *CapacityKnowledgeStore

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
	})

	Describe("Store and retrieve", func() {
		It("should store and retrieve a capacity record by namespace/model/variant", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				NumGpuBlocks:          1000,
				BlockSize:             16,
				TotalKvCapacityTokens: 16000,
				EffectiveCapacity:     14000,
				LearnedFrom:           "live",
			})

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)))
			Expect(got.AcceleratorName).To(Equal("H100"))
			Expect(got.GpuCount).To(Equal(1))
			Expect(got.LearnedFrom).To(Equal("live"))
		})

		It("should keep records separate across namespaces", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			})
			store.Update("ns-2", "model-a", "variant-h100", CapacityRecord{
				TotalKvCapacityTokens: 32000,
				LearnedFrom:           "live",
			})

			Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(16000)))
			Expect(store.Get("ns-2", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(32000)))
		})
	})

	Describe("Get missing key", func() {
		It("should return nil for a key that does not exist", func() {
			Expect(store.Get("ns-1", "nonexistent", "variant")).To(BeNil())
		})
	})

	Describe("LoadFromScaleTarget", func() {
		It("should populate EngineParams from deployment args", func() {
			deploy := makeTestDeployment("--gpu-memory-utilization=0.85", "--max-num-batched-tokens=4096")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-a100", "A100", 2, scaletarget.NewDeploymentAccessor(deploy))

			got := store.Get("ns-1", "model-a", "variant-a100")
			Expect(got).NotTo(BeNil())
			Expect(got.EngineParams).NotTo(BeNil())
			Expect(got.EngineParams.GpuMemoryUtilization).To(Equal(0.85))
			Expect(got.EngineParams.MaxNumBatchedTokens).To(Equal(int64(4096)))
			Expect(got.AcceleratorName).To(Equal("A100"))
			Expect(got.GpuCount).To(Equal(2))
			Expect(got.LearnedFrom).To(Equal("deployment"))
		})

		It("should populate SGLang EngineParams and TotalKvCapacityTokens from --max-total-tokens", func() {
			// SGLang is detected from the launch command; --max-total-tokens maps to
			// TotalKvTokensOverride and is used as the total KV capacity directly
			// (vLLM's num_gpu_blocks path must not be taken).
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:    "sglang",
								Image:   "lmsysorg/sglang:latest",
								Command: []string{"python3", "-m", "sglang.launch_server"},
								Args:    []string{"--mem-fraction-static=0.85", "--max-total-tokens=100000"},
							}},
						},
					},
				},
			}
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-sglang", "H100", 1, scaletarget.NewDeploymentAccessor(deploy))

			got := store.Get("ns-1", "model-a", "variant-sglang")
			Expect(got).NotTo(BeNil())
			Expect(got.EngineParams).NotTo(BeNil())
			Expect(got.EngineParams.Engine).To(Equal(inferenceengine.EngineSGLang))
			Expect(got.EngineParams.TotalKvTokensOverride).To(Equal(int64(100000)))
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(100000)))
			Expect(got.NumGpuBlocks).To(Equal(int64(0)))
		})

		It("should not overwrite live data with deployment data", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			})

			deploy := makeTestDeployment("--gpu-memory-utilization=0.85")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy))

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got.LearnedFrom).To(Equal("live"))
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)))
		})

		It("should handle nil scale target gracefully", func() {
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, nil)
			Expect(store.Get("ns-1", "model-a", "variant-h100")).To(BeNil())
		})

		It("should set conservative EffectiveCapacity from EffectiveMaxBatchedTokens", func() {
			// Default vLLM V1 deployment (no overrides)
			deploy := makeTestDeployment()
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy))

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			// V1 engine default EffectiveMaxBatchedTokens = 8192
			Expect(got.EffectiveCapacity).To(Equal(int64(8192)))
			Expect(got.LearnedFrom).To(Equal("deployment"))
		})

		It("should use num_gpu_blocks_override for k1 without overriding EffectiveCapacity", func() {
			deploy := makeTestDeployment("--num-gpu-blocks-override=5000", "--block-size=16")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy))

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(80000))) // 5000 * 16
			// EffectiveCapacity should NOT be overwritten since TotalKvCapacityTokens is set
			// but LoadFromScaleTarget doesn't compute k1 from TotalKvCapacityTokens directly,
			// it only sets EffectiveCapacity from EffectiveMaxBatchedTokens as a fallback
			// Since num_gpu_blocks_override doesn't set EffectiveCapacity,
			// the fallback EffectiveMaxBatchedTokens (8192) is used
			Expect(got.EffectiveCapacity).To(Equal(int64(8192)))
		})
	})

	Describe("Staleness", func() {
		It("should report missing keys as stale", func() {
			Expect(store.IsStale("ns-1", "x", "y")).To(BeTrue())
		})

		It("should report fresh records as not stale", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{LearnedFrom: "live"})
			Expect(store.IsStale("ns-1", "model-a", "variant-h100")).To(BeFalse())
		})

		It("should report old records as stale", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{LearnedFrom: "live"})

			// Manually age the record
			store.mu.Lock()
			rec := store.records[storeKey("ns-1", "model-a", "variant-h100")]
			rec.LearnedAt = time.Now().Add(-CapacityStalenessTimeout - time.Minute)
			store.mu.Unlock()

			Expect(store.IsStale("ns-1", "model-a", "variant-h100")).To(BeTrue())
		})
	})

	Describe("Concurrent access", func() {
		It("should handle concurrent reads and writes without panicking", func() {
			var wg sync.WaitGroup

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
						TotalKvCapacityTokens: int64(i * 1000),
						LearnedFrom:           "live",
					})
				}(i)
			}

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = store.Get("ns-1", "model-a", "variant-h100")
					_ = store.IsStale("ns-1", "model-a", "variant-h100")
				}()
			}

			wg.Wait()

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
		})
	})

	Describe("Cross-variant lookup", func() {
		It("should keep separate records for the same model on different variants", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				TotalKvCapacityTokens: 32000,
				LearnedFrom:           "live",
			})
			store.Update("ns-1", "model-a", "variant-a100", CapacityRecord{
				AcceleratorName:       "A100",
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			})

			Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(32000)))
			Expect(store.Get("ns-1", "model-a", "variant-a100").TotalKvCapacityTokens).To(Equal(int64(16000)))
		})
	})

	Describe("FindCompatible", func() {
		defaultParams := func() *EngineParams {
			p := defaultEngineParams()
			resolveEffectiveMaxBatchedTokens(&p)
			return &p
		}

		It("should find a compatible record from another variant", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-1", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			// Search for a compatible record for a different H100 variant
			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should not match across different accelerator types", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			// Search for A100 — should not match H100 record
			found := store.FindCompatible("model-a", "A100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should not match across different GPU counts", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-1gpu", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			// Search for 2-GPU H100 — should not match 1-GPU record
			found := store.FindCompatible("model-a", "H100", 2, params)
			Expect(found).To(BeNil())
		})

		It("should not match across different engine parameters", func() {
			params1 := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-high-util", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params1,
				LearnedFrom:           "live",
			})

			// Search with different gpu_memory_utilization
			params2 := defaultParams()
			params2.GpuMemoryUtilization = 0.5
			found := store.FindCompatible("model-a", "H100", 1, params2)
			Expect(found).To(BeNil())
		})

		It("should match across different namespaces (capacity is hardware-dependent)", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			// Record in ns-1 should be found when searching for model-a
			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should not match across different models", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			// Different model should not match
			found := store.FindCompatible("model-b", "H100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should prefer live records over deployment-derived records", func() {
			params := defaultParams()

			store.Update("ns-1", "model-a", "variant-h100-deploy", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 30000,
				EffectiveCapacity:     25000,
				EngineParams:          params,
				LearnedFrom:           "deployment",
			})
			store.Update("ns-1", "model-a", "variant-h100-live", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			})

			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.LearnedFrom).To(Equal("live"))
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should skip records with no useful capacity data", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-empty", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				EngineParams:          params,
				EffectiveCapacity:     0,
				TotalKvCapacityTokens: 0,
				LearnedFrom:           "deployment",
			})

			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should return nil when nil params are provided", func() {
			store.Update("ns-1", "model-a", "variant-h100", CapacityRecord{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          defaultParams(),
				LearnedFrom:           "live",
			})

			found := store.FindCompatible("model-a", "H100", 1, nil)
			Expect(found).To(BeNil())
		})
	})
})

// makeTestDeployment creates a minimal Deployment with the given vLLM args.
func makeTestDeployment(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "vllm",
							Command: []string{"vllm", "serve", "model-name"},
							Args:    args,
						},
					},
				},
			},
		},
	}
}
