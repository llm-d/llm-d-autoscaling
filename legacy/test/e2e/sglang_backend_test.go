package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// SGLang backend e2e: deploys a synthetic SGLang model server (CPU-only emitter of
// sglang:* metrics, launched with a faithful `sglang.launch_server` command),
// wires Prometheus scraping, and registers it with WVA via an annotated scaler
// (annotation-based discovery — no VariantAutoscaling CRD). It asserts that WVA —
// with no engine configuration — detects SGLang, collects the sglang:* metrics,
// and emits wva_desired_replicas driving a scale-up.
//
// This exercises the same code path as vLLM, only with SGLang metric names and
// flags. It runs in the kind-emulator environment (cfg.UseSimulator); a real
// SGLang server requires a GPU.
var _ = Describe("SGLang backend", Label("full"), Ordered, func() {
	const (
		baseName = "e2e-sglang"
		// variantName MUST equal the annotated scaler's object name, which is
		// baseName+"-so" for a KEDA ScaledObject (see fixtures.EnsureScaledObject).
		// WVA uses that object name as the variant identity: it attributes the decode
		// pods' sglang:* metrics by matching their llm-d.ai/variant label to it, and
		// emits wva_desired_replicas{variant_name=<object name>}. A mismatch leaves the
		// pods unattributed, so WVA falls back to a safety-net desiredReplicas=current
		// and never scales up. (Was the stale "e2e-sglang-hpa" from the HPA era.)
		variantName = baseName + "-so"
		// decodeSuffix mirrors the fixtures' "<base>-decode" naming convention.
		decodeSuffix = "-decode"
		// sglangEmulatorPort is the container/Service port the emitter serves on.
		sglangEmulatorPort = 8000
	)
	var (
		ctx      = context.Background()
		appLabel = baseName + decodeSuffix
		modelID  = "e2ewva/sglang-model"

		// Saturation ConfigMap snapshot for the V1 guard (see BeforeAll).
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("SGLang e2e uses a CPU-only metrics emitter; it runs in the kind-emulator environment (UseSimulator=true)")
		}

		// V1 guard (v0.9.0 V2-default flip): this suite relies on the shipped
		// saturation ConfigMap, and its scale-up assertion is calibrated to the V1
		// (percentage-based) analyzer. The fixture emits a fixed operating point
		// (token_usage=0.85, num_queue_reqs=3) that deterministically saturates V1's
		// default thresholds, but sits exactly at V2's scaleUpThreshold=0.85 (V2 must
		// *exceed* it) — so under the new V2 default the scale-up is no longer
		// deterministic. Pin this suite to V1 by overwriting the `default` entry with
		// a V1 config, and restore the original in AfterAll. Analyzer selection is
		// re-resolved from the watched ConfigMap each cycle, so no controller restart
		// is needed.
		// TODO(v2): recalibrate the SGLang fixture metrics + assertion for the V2
		// (token/capacity-based) analyzer and drop this guard.
		cmNamespace = cfg.WVANamespace
		cmName = saturationConfigMapName()
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			// Fail fast on a real API error: a false "did not exist" here would make
			// AfterAll delete the shared ConfigMap without recreating it.
			Expect(err).NotTo(HaveOccurred(), "reading existing saturation configmap")
		}
		By("Pinning the saturation analyzer to V1 for this suite (V2 is the default since v0.9.0)")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey, buildSaturationConfigYAML(""))).To(Succeed())

		By("Deploying the synthetic SGLang model server")
		Expect(fixtures.CreateSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName, modelID, variantName)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName) })

		By("Exposing it via a Service and ServiceMonitor")
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, baseName, appLabel, sglangEmulatorPort)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteService(ctx, k8sClient, cfg.LLMDNamespace, baseName) })
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, baseName, appLabel)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteServiceMonitor(ctx, crClient, cfg.MonitoringNS, baseName) })

		By("Registering the SGLang deployment with WVA via an annotated scaler")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName, appLabel, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName) })
	})

	AfterAll(func() {
		By("Restoring the saturation ConfigMap (V1 guard teardown)")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)
	})

	It("detects SGLang and emits wva_desired_replicas from sglang:* metrics", func() {
		// A correctly routed SGLang collection must produce wva_desired_replicas for
		// the variant; vLLM queries would return nothing here, so a scale-up proves
		// the SGLang path collected sglang:* metrics. The fixture emits a saturated
		// operating point (token_usage=0.85, num_queue_reqs=3), so WVA recommends
		// scale-up and KEDA drives the Deployment above a single replica. Assert the
		// observable Deployment replica count — the ground truth — rather than the
		// KEDA HPA CurrentMetrics surface, which only proves the metric was consumed.
		By("Asserting KEDA actuates a scale-up above a single replica for the SGLang variant")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, appLabel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(2)),
				"saturated SGLang metrics (token_usage=0.85, num_queue_reqs=3) should drive the Deployment above a single replica")
		}).WithTimeout(time.Duration(cfg.ScaleUpTimeout) * time.Second).
			WithPolling(time.Duration(cfg.PollIntervalSlowSec) * time.Second).
			Should(Succeed())
	})
})
