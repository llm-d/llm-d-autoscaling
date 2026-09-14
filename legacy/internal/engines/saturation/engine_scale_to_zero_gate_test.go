package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// scaleToZeroSupportedForEngines is the safety gate that prevents an active
// non-vLLM model from being scaled to zero by the vLLM-hardcoded request counter
// (see CollectModelRequestCount). These specs lock in its behavior: only an
// all-vLLM (or empty/unknown) target set may be scaled to zero.
var _ = Describe("scaleToZeroSupportedForEngines", func() {
	target := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "server", Image: image}},
					},
				},
			},
		})
	}
	vllm := target("vllm/vllm-openai:latest")
	sglang := target("lmsysorg/sglang:latest")

	It("supports scale-to-zero for an all-vLLM model", func() {
		Expect(scaleToZeroSupportedForEngines(
			map[string]scaletarget.ScaleTargetAccessor{"a": vllm, "b": vllm})).To(BeTrue())
	})

	It("supports scale-to-zero for an empty or nil target set (defaults to vLLM)", func() {
		Expect(scaleToZeroSupportedForEngines(nil)).To(BeTrue())
		Expect(scaleToZeroSupportedForEngines(
			map[string]scaletarget.ScaleTargetAccessor{})).To(BeTrue())
	})

	It("gates scale-to-zero off when any variant runs SGLang", func() {
		Expect(scaleToZeroSupportedForEngines(
			map[string]scaletarget.ScaleTargetAccessor{"a": sglang})).To(BeFalse())
	})

	It("gates scale-to-zero off for a mixed vLLM+SGLang model", func() {
		Expect(scaleToZeroSupportedForEngines(
			map[string]scaletarget.ScaleTargetAccessor{"a": vllm, "b": sglang})).To(BeFalse())
	})
})
