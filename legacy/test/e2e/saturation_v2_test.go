package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// V2 smoke calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage = 0.3 is the operating point that deterministically exercises
// both arcs of the V2 cost-aware optimizer with the suite's chosen thresholds:
//
//   - At 1 replica with scaleUpThreshold = 0.30, replicaDemand crosses the
//     threshold and the optimizer's required-capacity signal becomes positive
//     → scale-up. (Drives the "should recommend scale-up" It below.)
//   - At 2 replicas with the canonical-ordering thresholds scaleUpThreshold =
//     0.95 and scaleDownBoundary = 0.85, the cost-aware optimizer's
//     scale-down rule (cost_aware_optimizer.go: math.Floor(remaining /
//     vc.PerReplicaCapacity)) sees a remaining-capacity ≥ one full per-replica
//     budget → remove 1 replica. (Drives the "should recommend scale-down"
//     It below.)
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values V2 reads. That is the point — the suite
// exercises V2's decision logic against deterministic inputs.
//
// WVA no longer writes a VariantAutoscaling .status; its only output is the
// wva_desired_replicas external metric. The annotated KEDA ScaledObject
// registers the variant with WVA and actuates the recommendation. The V2
// scale-up/scale-down intent is verified through the managed Deployment's
// replica count — the full KEDA pipeline (WVA → Prometheus → KEDA → HPA →
// Deployment) is exercised end-to-end.
//
// --fake-metrics format:
//
//	https://github.com/llm-d/llm-d-inference-sim/blob/main/docs/configuration.md
const v2SmokeFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

// V2 saturation config knobs. The kvCacheThreshold / queueLength* /
// *SpareTrigger fields are V1-specific and have no effect on the V2
// token-based path; they are filled with safe defaults to satisfy
// buildSaturationConfigYAMLWithThresholds.
const (
	v2SmokeKvCacheThreshold     = 0.80
	v2SmokeQueueLengthThreshold = 50
	v2SmokeKvSpareTrigger       = 0.10
	v2SmokeQueueSpareTrigger    = 5

	// Aggressive on scale-up, conservative on scale-down so the path-selection
	// and scale-up tests are stable. The scale-down test raises
	// scaleDownBoundary at runtime to exercise the scale-down arc without
	// disturbing earlier preconditions.
	v2SmokeScaleUpThreshold  = 0.30
	v2SmokeScaleDownBoundary = 0.20
)

var _ = Describe("Saturation V2 engine", Label("smoke", "full"), Ordered, func() {
	const (
		poolName              = "v2-smoke-pool"
		modelSvcName          = "v2-smoke-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"

		// scalerBaseName is the logical base for the annotated scaler; the KEDA
		// ScaledObject name is scalerBaseName+"-so".
		scalerBaseName = "v2-smoke"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		// variantName is the annotated scaler's OBJECT name. WVA uses it as the
		// variant_name label on wva_desired_replicas, and the model-service pod
		// template carries it as the llm-d.ai/variant label so metric attribution
		// lines up on both discovery paths. It is backend-specific, so it is set
		// in BeforeAll.
		variantName string
	)

	BeforeAll(func() {
		// The suite depends on the simulator's --fake-metrics flag to drive
		// deterministic kv-cache-usage values into V2. That flag only exists
		// on llm-d-inference-sim — real vLLM rejects it and the model
		// Deployment fails to start. Skip cleanly on non-simulator runs
		// (e.g., OpenShift-style CI against real vLLM) rather than producing
		// a broken Deployment and timing out on readiness.
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = "default"
		variantName = scalerBaseName + "-so"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for V2 smoke tests")
		// Configure the simulator with --fake-metrics so V2 reads deterministic
		// kv_cache_usage_perc and request-count signals instead of relying on
		// the simulator's runtime emission, which doesn't always reach V2's
		// token-budget magnitude under bounded smoke load. See the
		// v2SmokeFakeMetricsJSON comment for the math.
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, variantName,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", v2SmokeFakeMetricsJSON},
		)).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for the V2 smoke model deployment to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Registering the V2 smoke deployment with WVA via an annotated ScaledObject (min=1, max=10)")
		// The annotated ScaledObject is both the WVA discovery source and the scaler;
		// no VariantAutoscaling CR is created. The 30 s scale-down stabilization window
		// overrides the HPA default (300 s) so the scale-down It completes within the
		// EventuallyExtendedSec budget.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(modelID, "30.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })

		By("Installing V2 saturation config so all subsequent It() blocks share state")
		// Done in BeforeAll (rather than inside the first It) so the suite's
		// behavior does not depend on Ordered execution to set up V2's
		// preconditions for later tests.
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			v2SmokeKvCacheThreshold, v2SmokeQueueLengthThreshold,
			v2SmokeKvSpareTrigger, v2SmokeQueueSpareTrigger,
			v2SmokeScaleUpThreshold, v2SmokeScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up V2 smoke resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// Verifies V2 path selection and that WVA emits wva_desired_replicas for the
	// discovered variant. The V2 saturation config is installed in BeforeAll, so
	// this It body just verifies the engine took the V2 path and that the metric
	// is consumed by the managed scaler.
	It("should select V2 path and emit wva_desired_replicas for the annotated scaler", func() {
		By("Asserting controller logs show V2 path selected for our model")
		expectAnalyzerPathLog("V2", modelID)

		// WVA's observable output is the wva_desired_replicas metric being consumed by KEDA.
		// Verify the KEDA-managed HPA has CurrentMetrics populated (only set after a
		// successful Prometheus query).
		By("Verifying KEDA read wva_desired_replicas for the V2 smoke variant")
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == modelDecodeDeployment {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the V2 smoke deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas")
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies the full KEDA scale-up pipeline: with kv-cache-usage=0.3 from
	// --fake-metrics and scaleUpThreshold=0.30, WVA's V2 optimizer emits
	// wva_desired_replicas=2, KEDA reads the metric and drives the Deployment
	// to 2 ready replicas.
	It("should scale up via KEDA when token utilization crosses scaleUpThreshold", func() {
		By("Asserting KEDA actuates scale-up to ≥ 2 replicas")
		// Chain: WVA (15 s interval) → wva_desired_replicas=2 → KEDA (5 s poll) →
		// HPA → Deployment. Uses ScaleUpTimeout (600 s) to accommodate pod scheduling.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"Deployment should scale up to ≥ 2 replicas with kv-cache-usage=0.3 and scaleUpThreshold=0.30")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies the full KEDA scale-down pipeline: WVA emits wva_desired_replicas=1 after
	// canonical-ordering thresholds raise the scaleDownBoundary, KEDA reads the updated
	// metric, and the managed Deployment's replica count drops back to minReplicas (1).
	//
	// Pre-condition: the scale-up It must have driven the Deployment to ≥ 2 replicas.
	// This It first asserts that pre-condition so a false-positive cannot occur when the
	// Deployment is already at 1 (e.g. if WVA never recommended scale-up).
	//
	// With --fake-metrics kv-cache-usage=0.3 at 2 replicas and canonical-ordering
	// thresholds (scaleUpThreshold=0.95, scaleDownBoundary=0.85), the V2 cost-aware
	// optimizer's remaining-capacity is ≥ one full per-replica budget → NumReplicas=1.
	It("should scale down via KEDA when token utilization falls below scaleDownBoundary", func() {
		By("Confirming Deployment is at ≥ 2 replicas before asserting scale-down")
		// This also catches regressions in the scale-up path — if KEDA never drove the
		// Deployment above minReplicas, the scale-down assertion would be meaningless.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"Deployment should be at 2 replicas before testing scale-down")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Switching saturation config to canonical-ordering thresholds (scaleUp=0.95, scaleDown=0.85)")
		// kv-cache-usage=0.3 << scaleDownBoundary=0.85 at 2 replicas → V2 optimizer
		// decides desiredReplicas=1 → wva_desired_replicas=1 → KEDA drives HPA to 1.
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			v2SmokeKvCacheThreshold, v2SmokeQueueLengthThreshold,
			v2SmokeKvSpareTrigger, v2SmokeQueueSpareTrigger,
			0.95, 0.85,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())

		By("Asserting KEDA actuates scale-down to 1 replica")
		// Chain: WVA (15 s interval) → wva_desired_replicas=1 → KEDA (5 s poll) →
		// HPA (30 s stabilization window set on this ScaledObject) → Deployment.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("<=", 1),
				"Deployment should scale down to minReplicas=1 after scaleDownBoundary raised to 0.85")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

})
