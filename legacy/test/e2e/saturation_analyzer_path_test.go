package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// TODO(cleanup): Unify analyzer-path configuration across algorithms
// (saturation config fields vs queueing-model config presence), then simplify
// this spec to a single explicit analyzer selector contract.
//
// NOTE (v0.9.0): V2 is the default analyzer since v0.9.0. This suite is
// self-guarded — it snapshots the base ConfigMap and writes its own explicit
// config for each arc (V1 cases set analyzerName:"" to select the legacy V1
// path), so the default flip does not affect it. The V1 arcs below deliberately
// exercise the still-selectable legacy analyzer.
// TODO(v1-removal): drop the V1 arcs (analyzerName:"") when V1 is removed.

// V1 saturation calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage=0.3 and waiting-requests=2 are chosen so both threshold arcs
// are deterministically exercisable by config alone — no load required:
//
//   - Scale-up path (aggressive thresholds: kvCache=0.05, queue=1):
//     0.3 > 0.05 and 2 >= 1 → V1 sees saturation → recommends scale-up.
//   - No-scale path (conservative thresholds: kvCache=1.00, queue=100):
//     0.3 < 1.00 and 2 < 100 → V1 sees no saturation → no scale-up.
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values V1 reads.
const v1FakeMetricsJSON = `{"kv-cache-usage":0.3,"waiting-requests":2,"running-requests":1}`

const (
	saturationConfigTemplate = `
model_id: ""
namespace: ""
kvCacheThreshold: %.2f
queueLengthThreshold: %d
kvSpareTrigger: %.2f
queueSpareTrigger: %d
scaleUpThreshold: %.2f
scaleDownBoundary: %.2f
analyzerName: %q
`

	// Aggressive V1 thresholds: fake metrics (kv=0.3, queue=2) exceed these → scale-up.
	saturationV1KVCacheThreshold     = 0.05
	saturationV1QueueLengthThreshold = 1
	saturationV1KVSpareTrigger       = 0.01
	saturationV1QueueSpareTrigger    = 1
	saturationV1ScaleUpThreshold     = 0.85
	saturationV1ScaleDownBoundary    = 0.70

	// Conservative V1 thresholds: fake metrics (kv=0.3, queue=2) stay below these → no scale-up.
	saturationV1NoScaleKVCacheThreshold     = 1.00
	saturationV1NoScaleQueueLengthThreshold = 100
	saturationV1NoScaleKVSpareTrigger       = 0.00
	saturationV1NoScaleQueueSpareTrigger    = 0
)

// buildSaturationConfigYAML builds a valid saturation config entry for the requested analyzer mode.
func buildSaturationConfigYAML(analyzerName string) string {
	return fmt.Sprintf(saturationConfigTemplate, 0.80, 1, 0.20, 1, 0.85, 0.70, analyzerName)
}

// buildSaturationConfigYAMLWithThresholds builds a valid saturation config entry with explicit thresholds.
func buildSaturationConfigYAMLWithThresholds(analyzerName string, kvCacheThreshold float64, queueLengthThreshold int, kvSpareTrigger float64, queueSpareTrigger int, scaleUpThreshold float64, scaleDownBoundary float64) string {
	return fmt.Sprintf(
		saturationConfigTemplate,
		kvCacheThreshold,
		queueLengthThreshold,
		kvSpareTrigger,
		queueSpareTrigger,
		scaleUpThreshold,
		scaleDownBoundary,
		analyzerName,
	)
}

// saturationConfigMapName resolves the active saturation ConfigMap name from controller runtime env.
func saturationConfigMapName() string {
	// Match the controller's runtime config map name; discover by label first
	// since the deployment name can vary across overlays.
	deps, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "control-plane=controller-manager",
	})
	if err != nil || len(deps.Items) == 0 {
		return config.SaturationConfigMapName()
	}
	return saturationConfigMapNameFromDeployment(&deps.Items[0])
}

// saturationConfigMapNameFromDeployment extracts SATURATION_CONFIG_MAP_NAME from manager container env.
func saturationConfigMapNameFromDeployment(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "SATURATION_CONFIG_MAP_NAME" && e.Value != "" {
				return e.Value
			}
		}
	}
	return config.SaturationConfigMapName()
}

// expectAnalyzerPathLog is a Ginkgo helper: it Eventually-waits until WVA
// controller-manager logs contain both the analyzer path marker for mode and
// modelID. It uses testutils.PodLogsLabelSelectorContain for log collection.
func expectAnalyzerPathLog(mode, modelID string) {
	GinkgoHelper()
	const controllerManagerLabel = "control-plane=controller-manager"
	pattern := fmt.Sprintf("Processing model (%s)", mode)
	Eventually(func(g Gomega) {
		ok, logs, logErr := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace, controllerManagerLabel, pattern, 120)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok && strings.Contains(logs, modelID)).To(BeTrue())
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

var _ = Describe("Saturation analyzer path and status propagation", Label("full"), Ordered, func() {
	const (
		poolName     = "saturation-path-pool"
		modelSvcName = "saturation-path-ms"
		// modelDecodeDeployment is the Deployment name fixtures.CreateModelService creates
		// (name + "-decode"), matching llm-d model-service decode pods / labels.
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		// scalerBaseName is the annotated scaler's logical base. WVA discovers the
		// scaler and uses its OBJECT name as the variant_name label on
		// wva_desired_replicas — that is base+"-so" for a KEDA ScaledObject and
		// base+"-hpa" for an HPA. The decode pods must carry
		// llm-d.ai/variant=<scaler object name> for metric attribution, so variantName is
		// derived from the backend below.
		scalerBaseName = "saturation-path"
		hpaObjectName  = scalerBaseName + "-hpa"
		soObjectName   = scalerBaseName + "-so"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the scaler's object name — stamped as the
		// decode pods' llm-d.ai/variant label so the collector attributes their
		// metrics to the variant. Set from the backend in BeforeAll.
		variantName string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		variantName = soObjectName

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		// Use global saturation config for deterministic engine-path selection.
		// Namespace-local ConfigMap watch is opt-in/tracked and can race in e2e.
		cmNamespace = cfg.WVANamespace
		cmKey = "default"

		// Snapshot existing saturation config so the test can restore it.
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for saturation path test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		err = fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, variantName,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", v1FakeMetricsJSON})
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, depErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(depErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the saturation-path deployment with WVA via an annotated ScaledObject")
		// The ScaledObject's variantName matches the model service's variantName so the
		// decode pods' llm-d.ai/variant label and wva_desired_replicas variant_name align.
		// The 30 s scale-down stabilization window overrides the HPA default (300 s) so
		// the "does not scale up" It can wait for minReplicas within EventuallyLongSec.
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(modelID, "30.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			// Replace the object in two steps (delete + create) instead of updating in place.
			// That avoids resourceVersion conflict retries; a brief gap without the ConfigMap
			// during suite teardown is acceptable for e2e.
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

		By("Cleaning up saturation analyzer path resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("uses V2 path when analyzerName is saturation", func() {
		By("Writing model-specific saturation config with analyzerName=saturation")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML("saturation"))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V2 processing for this model")
		expectAnalyzerPathLog("V2", modelID)
	})

	It("switches to V1 path when analyzerName is unset", func() {
		By("Updating model-specific saturation config with analyzerName unset")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML(""))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V1 processing for this model")
		expectAnalyzerPathLog("V1", modelID)
	})

	It("propagates saturation results into wva_desired_replicas for the variant", func() {
		// WVA no longer writes VA .status; its sole output is wva_desired_replicas.
		// expectWVADesiredReplicasConsumed observes that through the KEDA-managed
		// HPA's CurrentMetrics, which KEDA populates only after reading the metric
		// from Prometheus.
		By("Verifying wva_desired_replicas was emitted and consumed for the saturation-path variant")
		Eventually(func(g Gomega) {
			expectWVADesiredReplicasConsumed(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("does not scale the target deployment up for bounded below-threshold V1 traffic", func() {
		By("Configuring conservative V1 thresholds to avoid scale-up")
		// Set thresholds before capturing the baseline so WVA has time to reconcile
		// while we wait for KEDA to settle. With the KEDA Prometheus query now using
		// exported_namespace (fixed), KEDA can act on wva_desired_replicas from the
		// prior It (which may have left a scale-up recommendation in Prometheus). We
		// must wait for WVA to re-evaluate and KEDA to read the new value before the
		// Consistently window starts — otherwise the HPA would fire a scale-up.
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationV1NoScaleKVCacheThreshold,
				saturationV1NoScaleQueueLengthThreshold,
				saturationV1NoScaleKVSpareTrigger,
				saturationV1NoScaleQueueSpareTrigger,
				saturationV1ScaleUpThreshold,
				saturationV1ScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is using V1 analyzer path")
		expectAnalyzerPathLog("V1", modelID)

		By("Waiting for the pipeline to converge to a sustained minReplicas (drain any in-flight scale-up)")
		// A single Spec.Replicas <= 1 reading is NOT proof of convergence: the deployment
		// starts at minReplicas, so that check passes on the pre-existing state while a
		// scale-up recommendation left in flight by the prior It (default config:
		// queueLengthThreshold=1 vs faked queue=2 → V1 scale-up) is still working through
		// WVA (≤15 s reconcile) → Prometheus → KEDA (5 s poll) → HPA. That stale
		// recommendation actuates a scale-up mid-assertion unless it has fully drained.
		//
		// Require the deployment to HOLD at <=1 continuously for stableWindow before
		// trusting it: any bump resets the stability clock, and stableWindow outlasts the
		// full pipeline latency (reconcile + poll + the 30 s scale-down stabilization set
		// on this ScaledObject) so the old recommendation is guaranteed drained.
		const stableWindow = 45 * time.Second
		var stableSince *time.Time
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			now := time.Now()
			if *dep.Spec.Replicas > 1 {
				stableSince = nil // reset the stability clock on any in-flight scale-up
				GinkgoWriter.Printf("  Convergence (%s): replicas=%d (>1) — resetting stability clock\n", modelDecodeDeployment, *dep.Spec.Replicas)
				g.Expect(*dep.Spec.Replicas).To(BeNumerically("<=", int32(1)),
					"waiting for the in-flight scale-up recommendation to drain")
				return
			}
			if stableSince == nil {
				stableSince = &now
			}
			held := now.Sub(*stableSince)
			GinkgoWriter.Printf("  Convergence (%s): replicas<=1 held for %s (need %s)\n", modelDecodeDeployment, held.Round(time.Second), stableWindow)
			g.Expect(held).To(BeNumerically(">=", stableWindow),
				"waiting for minReplicas to hold long enough to confirm the pipeline converged")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Capturing baseline target deployment replicas before steady-state assertion")
		var baseline int32
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		baseline = *dep.Spec.Replicas
		GinkgoWriter.Printf("  Negative-path baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)

		By("Verifying the target deployment does not scale above baseline")
		Consistently(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			current := int32(0)
			if dep.Spec.Replicas != nil {
				current = *dep.Spec.Replicas
			}
			GinkgoWriter.Printf("  Negative-path progress (%s): replicas=%d baseline=%d\n", modelDecodeDeployment, current, baseline)
			g.Expect(current).To(BeNumerically("<=", baseline),
				"V1 bounded below-threshold traffic should not scale the target deployment above baseline")
		}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("crosses V1 threshold with bounded requests and raises wva_desired_replicas", func() {
		var baseline int32

		By("Capturing baseline target deployment replicas before scale-up trigger")
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			baseline = *dep.Spec.Replicas
			GinkgoWriter.Printf("  Scale-up baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Configuring aggressive V1 thresholds and unsetting analyzerName")
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationV1KVCacheThreshold,
				saturationV1QueueLengthThreshold,
				saturationV1KVSpareTrigger,
				saturationV1QueueSpareTrigger,
				saturationV1ScaleUpThreshold,
				saturationV1ScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is using V1 analyzer path")
		expectAnalyzerPathLog("V1", modelID)

		By("Asserting KEDA actuates scale-up above baseline")
		// Aggressive V1 thresholds (kvCache=0.05, queue=1) against faked metrics
		// (kv=0.3, queue=2) deterministically drive a scale-up; KEDA consumes
		// wva_desired_replicas and drives the Deployment above its baseline. Assert the
		// observable Deployment replica count — the ground truth — rather than the KEDA
		// HPA CurrentMetrics surface, which only proves the metric was consumed.
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">", baseline),
				"V1 above-threshold traffic should scale the target Deployment above baseline")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

})

// upsertSaturationConfigEntry creates or updates a saturation ConfigMap data entry.
func upsertSaturationConfigEntry(ctx context.Context, cmNamespace, cmName, key, value string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(cmNamespace)
	cm, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: cmNamespace,
				},
				Data: map[string]string{key: value},
			}
			_, createErr := cmClient.Create(ctx, newCM, metav1.CreateOptions{})
			return createErr
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// saturationConfigMapForRecreate returns a copy of orig suitable for Create after Delete,
// with apiserver-owned fields cleared so admission succeeds.
func saturationConfigMapForRecreate(orig *corev1.ConfigMap) *corev1.ConfigMap {
	cm := orig.DeepCopy()
	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.DeletionTimestamp = nil
	cm.DeletionGracePeriodSeconds = nil
	cm.ManagedFields = nil
	cm.Finalizers = nil
	return cm
}
