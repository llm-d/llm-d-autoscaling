package saturation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// applyScaleToZeroEnforcement is the single seam every optimize path (V1, V2,
// queueing-model) routes its enforcement through. These specs drive that seam with
// a real, idle (request count 0) Enforcer and assert the gate's outcome end-to-end:
// an SGLang model is left untouched while an otherwise-identical vLLM model is zeroed.
// The vLLM case is the canary — it proves the SGLang case isn't passing simply because
// the enforcer never ran; inverting the engine gate would flip both and fail here.
var _ = Describe("applyScaleToZeroEnforcement", func() {
	const (
		modelID   = "test-model"
		namespace = "test-ns"
	)

	var ctx = context.Background()

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

	// engineWithIdleEnforcer builds an Engine whose enforcer always reads zero
	// traffic and whose config enables scale-to-zero for the test model — so the
	// enforcer will zero any model it is actually allowed to act on.
	engineWithIdleEnforcer := func() *Engine {
		cfg := config.NewTestConfig()
		cfg.UpdateScaleToZeroConfigForNamespace(namespace, config.ScaleToZeroConfigData{
			modelID: {EnableScaleToZero: ptrTo(true), RetentionPeriod: "10m"},
		})
		return &Engine{
			Config: cfg,
			ScaleToZeroEnforcer: pipeline.NewEnforcer(
				func(context.Context, string, string, time.Duration) (float64, error) {
					return 0, nil // idle: enforcer would scale to zero unless gated off
				},
			),
		}
	}

	decisions := func() []domain.VariantDecision {
		return []domain.VariantDecision{
			{VariantName: "v1", ModelID: modelID, Namespace: namespace, Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
		}
	}

	It("zeroes an idle all-vLLM model (canary: the enforcer does run when ungated)", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v1-saturation",
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeTrue())
		Expect(d[0].TargetReplicas).To(Equal(0))
	})

	It("does NOT zero an idle SGLang model (engine gate skips the enforcer)", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("lmsysorg/sglang:latest")}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2), "SGLang model must not be scaled to zero by the vLLM-hardcoded counter")
	})

	It("does NOT zero a mixed vLLM+SGLang model", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "queueing-model",
			d, map[string]scaletarget.ScaleTargetAccessor{
				"a": target("vllm/vllm-openai:latest"),
				"b": target("lmsysorg/sglang:latest"),
			}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2))
	})

	It("does NOT zero a vLLM model whose variant declares minReplicas > 0", func() {
		e := engineWithIdleEnforcer()
		d := decisions()
		min := 1
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v1-saturation",
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")},
			[]domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}})
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2))
	})

	It("does NOT zero an idle vLLM model when the inline scaleToZero override disables it", func() {
		// The scale-to-zero ConfigMap (via engineWithIdleEnforcer) enables scale-to-zero
		// for the model; an inline scaleToZero:{enabled:false} on the resolved saturation
		// entry must win, proving the resolveSaturationConfig -> EnforcePolicyOnDecisions
		// -> ResolveScaleToZeroEnabled wiring is honored end-to-end.
		e := engineWithIdleEnforcer()
		e.Config.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: ptrTo(false)}},
		})
		d := decisions()
		scaledToZero := e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			d, map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}, nil)
		Expect(scaledToZero).To(BeFalse())
		Expect(d[0].TargetReplicas).To(Equal(2), "inline scaleToZero:false must override the enabled ConfigMap")
	})
})

// ptrTo returns a pointer to v. Local helper so this file stays self-contained.
func ptrTo[T any](v T) *T { return &v }
