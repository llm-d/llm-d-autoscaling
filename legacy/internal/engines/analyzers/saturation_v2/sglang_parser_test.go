package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

var _ = Describe("ParseSGLangArgs", func() {

	Describe("Default values", func() {
		It("should return SGLang defaults when no args are provided", func() {
			deploy := makeTestDeployment() // no args
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))

			Expect(params.GpuMemoryUtilization).To(Equal(0.9))
			Expect(params.BlockSize).To(Equal(int64(1))) // page-size default
			Expect(params.KvCacheDtype).To(Equal("auto"))
			Expect(params.TensorParallelSize).To(Equal(1))
			Expect(params.MaxNumSeqs).To(Equal(int64(256)))
			Expect(params.TotalKvTokensOverride).To(Equal(int64(0)))
			Expect(params.EnforceEager).To(BeFalse())
			Expect(params.IsV1Engine).To(BeTrue())
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
		})
	})

	Describe("SGLang flag mapping", func() {
		It("should parse all known SGLang capacity flags", func() {
			deploy := makeTestDeployment(
				"--mem-fraction-static=0.85",
				"--page-size=16",
				"--kv-cache-dtype=fp8_e4m3",
				"--tp-size=4",
				"--max-running-requests=512",
				"--max-total-tokens=131072",
				"--context-length=8192",
			)
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))

			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
			Expect(params.BlockSize).To(Equal(int64(16)))
			Expect(params.KvCacheDtype).To(Equal("fp8_e4m3"))
			Expect(params.TensorParallelSize).To(Equal(4))
			Expect(params.MaxNumSeqs).To(Equal(int64(512)))
			Expect(params.TotalKvTokensOverride).To(Equal(int64(131072)))
			Expect(params.MaxModelLen).To(Equal(int64(8192)))
		})

		It("should accept --tp and --tensor-parallel-size aliases", func() {
			Expect(ParseSGLangArgs(scaletarget.NewDeploymentAccessor(
				makeTestDeployment("--tp", "8"))).TensorParallelSize).To(Equal(8))
			Expect(ParseSGLangArgs(scaletarget.NewDeploymentAccessor(
				makeTestDeployment("--tensor-parallel-size=2"))).TensorParallelSize).To(Equal(2))
		})

		It("should treat --disable-cuda-graph as enforce-eager (boolean flag)", func() {
			deploy := makeTestDeployment("--disable-cuda-graph")
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.EnforceEager).To(BeTrue())
		})

		It("should map --max-prefill-tokens to the per-step token budget", func() {
			deploy := makeTestDeployment("--max-prefill-tokens=16384")
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(16384)))
			// With an explicit batched-token budget, it is used verbatim.
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(16384)))
		})

		It("should set the per-step token budget from --chunked-prefill-size", func() {
			deploy := makeTestDeployment("--chunked-prefill-size=4096")
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(4096)))
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(4096)))
		})

		It("should disable chunked prefill when --chunked-prefill-size=-1", func() {
			deploy := makeTestDeployment("--chunked-prefill-size=-1", "--context-length=4096")
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.ChunkedPrefillEnabled).To(BeFalse())
			// Unchunked prefill falls back to max(MaxModelLen, 2048).
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(4096)))
		})
	})

	Describe("Shell command parsing", func() {
		It("should parse SGLang args from a shell command string", func() {
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "sglang",
									Command: []string{"/bin/sh", "-c", "python3 -m sglang.launch_server --model-path m --mem-fraction-static=0.8 --max-running-requests 128"},
								},
							},
						},
					},
				},
			}
			params := ParseSGLangArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.GpuMemoryUtilization).To(Equal(0.8))
			Expect(params.MaxNumSeqs).To(Equal(int64(128)))
		})
	})

	Describe("Nil scale target", func() {
		It("should return defaults for a nil scale target", func() {
			params := ParseSGLangArgs(nil)
			Expect(params.GpuMemoryUtilization).To(Equal(0.9))
			Expect(params.MaxNumSeqs).To(Equal(int64(256)))
		})
	})
})

var _ = Describe("ParseEngineArgs", func() {
	It("should dispatch to the SGLang parser for EngineSGLang", func() {
		deploy := makeTestDeployment("--max-running-requests=64")
		params := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(deploy))
		Expect(params.MaxNumSeqs).To(Equal(int64(64)))
		Expect(params.BlockSize).To(Equal(int64(1))) // SGLang page-size default
	})

	It("should dispatch to the vLLM parser for EngineVLLM", func() {
		deploy := makeTestDeployment("--max-num-seqs=64")
		params := ParseEngineArgs(inferenceengine.EngineVLLM, scaletarget.NewDeploymentAccessor(deploy))
		Expect(params.MaxNumSeqs).To(Equal(int64(64)))
		Expect(params.BlockSize).To(Equal(int64(16))) // vLLM block-size default
	})
})
