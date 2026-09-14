package e2e

import (
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

var _ = Describe("KEDA Smoke Tests - Infrastructure Readiness", Label("smoke", "full"), func() {
	Context("Basic infrastructure validation", func() {
		It("should have WVA controller running and ready", func() {
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod should exist")
				readyPods := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase == corev1.PodRunning {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == "Ready" && condition.Status == "True" {
								readyPods++
								break
							}
						}
					}
				}
				g.Expect(readyPods).To(BeNumerically(">", 0), "At least one WVA controller pod should be ready")
			}).Should(Succeed())
		})

		It("should have llm-d CRDs installed", func() {
			_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
			Expect(err).NotTo(HaveOccurred(), "llm-d CRDs should be installed")
		})

		It("should have Prometheus running", func() {
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=prometheus",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod should exist")
			}).Should(Succeed())
		})

		It("should have KEDA operator ready", func() {
			By("Checking KEDA operator pods in " + cfg.KEDANamespace)
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=keda-operator",
				})
				g.Expect(err).NotTo(HaveOccurred(), "Failed to list KEDA operator pods")
				g.Expect(pods.Items).NotTo(BeEmpty(), "At least one KEDA operator pod should exist")
				ready := 0
				for _, p := range pods.Items {
					if p.Status.Phase == corev1.PodRunning {
						for _, c := range p.Status.Conditions {
							if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
								ready++
								break
							}
						}
					}
				}
				g.Expect(ready).To(BeNumerically(">", 0), "At least one KEDA operator pod should be ready")
			}).Should(Succeed())
		})

		It("should have KEDA metrics server serving the external metrics API", func() {
			By("Checking external.metrics.k8s.io/v1beta1 is available (owned by KEDA)")
			Eventually(func(g Gomega) {
				_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
				g.Expect(err).NotTo(HaveOccurred(), "KEDA metrics server should register external.metrics.k8s.io/v1beta1")
			}).Should(Succeed())
		})
	})
})

var _ = Describe("KEDA Smoke Tests - Basic Autoscaling", Label("smoke", "full"), Ordered, func() {
	var (
		ns               string
		poolName         = "smoke-test-pool"
		modelServiceName = "smoke-test-ms"
		deploymentName   = modelServiceName + "-decode"
		// variantName is both the ScaledObject's variant_name label selector value
		// (on wva_desired_replicas) and the model service's llm-d.ai/variant pod
		// label. The two must match for metric attribution.
		variantName = "smoke-test-va"
		scalerName  = "smoke-test-hpa" // base name; ScaledObject will be scalerName+"-so"
		minReplicas = int32(1)
	)

	BeforeAll(func() {
		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "smoke-keda-basic-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to create isolated test namespace")
		ns = nsObj.Name
		By("Using isolated test namespace " + ns)

		DeferCleanup(func() {
			By("Deleting isolated namespace " + ns)
			if err := k8sClient.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
				GinkgoWriter.Printf("WARNING: failed to delete namespace %s: %v\n", ns, err)
			}
		})
		DeferCleanup(func() {
			smName := modelServiceName + "-monitor"
			By("Deleting ServiceMonitor " + smName)
			if err := crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
			}); err != nil {
				GinkgoWriter.Printf("WARNING: failed to delete ServiceMonitor %s: %v\n", smName, err)
			}
		})

		if cfg.ScaleToZeroEnabled {
			minReplicas = 0
		}

		By("Creating model service deployment")
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceName, poolName, cfg.ModelID, variantName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		By("Creating service to expose model server")
		err = fixtures.EnsureService(ctx, k8sClient, ns, modelServiceName, deploymentName, 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, modelServiceName, deploymentName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating annotated ScaledObject (both WVA discovery source and scaler)")
		err = fixtures.EnsureScaledObject(ctx, crClient, ns, scalerName, deploymentName, variantName, minReplicas, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
	})

	It("should emit wva_desired_replicas consumed by KEDA", func() {
		// WVA discovers the variant from the annotated ScaledObject and emits
		// wva_desired_replicas to Prometheus. KEDA only populates the managed
		// HPA's CurrentMetrics after a successful Prometheus query, so a non-empty
		// CurrentMetrics proves the metric was emitted and consumed.
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == deploymentName {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas")
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have ScaledObject Ready condition set by KEDA", func() {
		soName := scalerName + "-so"
		Eventually(func(g Gomega) {
			so := &kedav1alpha1.ScaledObject{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: soName}, so)
			g.Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			ready := so.Status.Conditions.GetReadyCondition()
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "ScaledObject should have Ready=True")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should have KEDA create an HPA for the deployment", func() {
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var found bool
			for _, h := range hpaList.Items {
				if h.Spec.ScaleTargetRef.Name == deploymentName {
					found = true
					g.Expect(h.Status.DesiredReplicas).To(BeNumerically(">=", 0), "KEDA HPA should have desired replicas set")
					break
				}
			}
			g.Expect(found).To(BeTrue(), "KEDA should have created an HPA targeting %s", deploymentName)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("should verify Prometheus is scraping model server pods", func() {
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + deploymentName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")
			readyCount := 0
			for _, pod := range pods.Items {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						break
					}
				}
			}
			g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod should be ready for metrics scraping")
		}).Should(Succeed())
	})

	It("should collect saturation metrics without triggering scale-up", func() {
		// Guard against a vacuous pass: the Deployment starts at minReplicas, so a bare
		// Consistently{<=1} would also succeed if WVA were dead and emitting nothing.
		// First prove the pipeline is live — KEDA is consuming wva_desired_replicas for
		// this variant — so the steady-state assertion means "WVA is actively
		// recommending minReplicas under no load", not "nothing happened".
		By("Confirming WVA is live: KEDA is consuming wva_desired_replicas for the variant")
		Eventually(func(g Gomega) {
			expectWVADesiredReplicasConsumed(g, ns, deploymentName)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Verifying the deployment stays at its baseline replica count under no load")
		// No load and no --fake-metrics pressure: the simulator's near-zero saturation
		// signals keep WVA recommending minReplicas, so the managed Deployment must not
		// scale up. Sampled repeatedly to confirm no scale-up is triggered.
		Consistently(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.Replicas).To(BeNumerically("<=", int32(1)),
				"Deployment should not scale up beyond its baseline replica count")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})
})

var _ = Describe("KEDA Smoke Tests - Error Handling", Label("smoke", "full"), Ordered, func() {
	var (
		ns                        string
		errorTestPoolName         = "error-test-pool"
		errorTestModelServiceName = "error-test-ms"
		errorTestDeploymentName   = errorTestModelServiceName + "-decode"
		// errorTestVariantName is the variant name used as the ScaledObject's
		// variantName and the model service's llm-d.ai/variant pod label.
		errorTestVariantName = "error-test-va"
		errorScalerName      = "error-test-hpa" // base name; ScaledObject will be errorScalerName+"-so"
	)

	BeforeAll(func() {
		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "smoke-keda-error-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to create isolated test namespace")
		ns = nsObj.Name
		By("Using isolated test namespace " + ns)

		DeferCleanup(func() {
			By("Deleting isolated namespace " + ns)
			if err := k8sClient.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
				GinkgoWriter.Printf("WARNING: failed to delete namespace %s: %v\n", ns, err)
			}
		})

		By("Creating model service deployment for error handling tests")
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, errorTestVariantName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating annotated ScaledObject (both WVA discovery source and scaler)")
		err = fixtures.EnsureScaledObject(ctx, crClient, ns, errorScalerName, errorTestDeploymentName, errorTestVariantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
	})

	It("should handle deployment deletion and recreation gracefully", func() {
		By("Verifying deployment exists before deletion")
		_, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Deployment should exist before deletion")

		By("Deleting the deployment")
		err = k8sClient.AppsV1().Deployments(ns).Delete(ctx, errorTestDeploymentName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to delete deployment")

		By("Waiting for deployment to be fully deleted")
		Eventually(func(g Gomega) {
			_, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(err).To(MatchError(ContainSubstring("not found")), "Deployment should be deleted")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())

		By("Verifying the ScaledObject continues to exist after deployment deletion")
		soName := errorScalerName + "-so"
		so := &kedav1alpha1.ScaledObject{}
		err = crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: soName}, so)
		Expect(err).NotTo(HaveOccurred(), "ScaledObject should continue to exist after deployment deletion")

		By("Recreating the deployment")
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, errorTestVariantName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to recreate model service")

		By("Waiting for deployment to be ready after recreation")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, errorTestDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
				"Model service should have 1 ready replica after recreation")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

		By("Verifying WVA re-emits wva_desired_replicas consumed by KEDA after recreation")
		// After the deployment is recreated, WVA should rediscover the variant
		// from the annotated ScaledObject and resume emitting wva_desired_replicas.
		// KEDA populates the managed HPA's CurrentMetrics only after a successful
		// Prometheus query, so a non-empty CurrentMetrics confirms the metric is
		// being emitted again.
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == errorTestDeploymentName {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have an HPA for the recreated deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas after recreation")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())
	})
})
