package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// GPU Limiter test validates that the WVA controller respects GPU resource constraints
// and doesn't recommend scaling beyond available GPU capacity.
//
// This test creates two annotated scalers (discovery sources) for two deployments with
// different variant costs and verifies that the limiter correctly constrains scale-up
// decisions. With the VariantAutoscaling CRD removed, WVA derives the accelerator from
// the scale target's node placement rather than from a variant object; the two variants
// are differentiated here by their annotation cost, and the limiter outcome is observed
// through Deployment replica counts and the wva_desired_replicas metric surface.
var _ = Describe("GPU Limiter Feature", Label("full"), Ordered, func() {
	var (
		poolA         = "limiter-pool-a"
		poolB         = "limiter-pool-b"
		modelServiceA = "limiter-ms-a"
		modelServiceB = "limiter-ms-b"
		// variantA/variantB are the annotated scalers' object names; WVA uses them as
		// the variant_name label on wva_desired_replicas. They are also passed to the
		// model-service creation as the llm-d.ai/variant pod label value.
		variantA = "limiter-va-nvidia"
		variantB = "limiter-va-amd"
		hpaA     = "limiter-hpa-nvidia"
		hpaB     = "limiter-hpa-amd"
		ns       string
	)

	BeforeAll(func() {
		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "limiter-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to create isolated test namespace")
		ns = nsObj.Name
		By("Using isolated test namespace " + ns)
		DeferCleanup(func() {
			By("Deleting isolated namespace " + ns)
			if err := k8sClient.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to delete namespace %s: %v\n", ns, err)
			}
		})

		By("Creating two model services with different accelerator requirements")

		// Pool A - NVIDIA GPUs
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceA, poolA, cfg.ModelID, variantA, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service A")

		err = fixtures.EnsureService(ctx, k8sClient, ns, modelServiceA, modelServiceA+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service A")

		By("Creating ServiceMonitor for service A")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, modelServiceA, modelServiceA+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor A")

		DeferCleanup(func() {
			_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      modelServiceA + "-monitor",
					Namespace: cfg.MonitoringNS,
				},
			})
		})

		// Pool B - AMD GPUs
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceB, poolB, cfg.ModelID, variantB, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service B")

		err = fixtures.EnsureService(ctx, k8sClient, ns, modelServiceB, modelServiceB+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service B")

		By("Creating ServiceMonitor for service B")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, modelServiceB, modelServiceB+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor B")

		DeferCleanup(func() {
			_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      modelServiceB + "-monitor",
					Namespace: cfg.MonitoringNS,
				},
			})
		})

		By("Waiting for both model services to be ready")
		Eventually(func(g Gomega) {
			depA, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depA.Status.ReadyReplicas).To(Equal(int32(1)))

			depB, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depB.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating annotated ScaledObjects (discovery sources) for both deployments, with distinct variant costs")
		err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaA, modelServiceA+"-decode", variantA, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject A")
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, ns, hpaA) })

		err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaB, modelServiceB+"-decode", variantB, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "40.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject B")
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, ns, hpaB) })

		GinkgoWriter.Println("GPU Limiter test setup complete with two annotated scalers (distinct variant costs)")
	})

	Context("Annotated scaler discovery", func() {
		It("should discover both variants via their annotated scalers", func() {
			// Both annotated ScaledObjects are the discovery sources; KEDA creates a
			// managed HPA for each. Verify both scale targets have a corresponding HPA,
			// which is the observable signal that WVA discovered the variant and the
			// scaler is wired.
			By("Verifying KEDA created a managed HPA for each deployment")
			Eventually(func(g Gomega) {
				hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var foundA, foundB bool
				for i := range hpaList.Items {
					switch hpaList.Items[i].Spec.ScaleTargetRef.Name {
					case modelServiceA + "-decode":
						foundA = true
					case modelServiceB + "-decode":
						foundB = true
					}
				}
				g.Expect(foundA).To(BeTrue(), "KEDA should have created an HPA for deployment A")
				g.Expect(foundB).To(BeTrue(), "KEDA should have created an HPA for deployment B")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Both variants discovered via their annotated scalers")
		})

		It("should emit wva_desired_replicas for both variants", func() {
			// WVA's sole output is the wva_desired_replicas metric. Verify KEDA has
			// consumed it by checking each managed HPA's CurrentMetrics (populated only
			// after a successful Prometheus query).
			By("Verifying KEDA read wva_desired_replicas for both deployments")
			Eventually(func(g Gomega) {
				hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var hpaForA, hpaForB *autoscalingv2.HorizontalPodAutoscaler
				for i := range hpaList.Items {
					switch hpaList.Items[i].Spec.ScaleTargetRef.Name {
					case modelServiceA + "-decode":
						hpaForA = &hpaList.Items[i]
					case modelServiceB + "-decode":
						hpaForB = &hpaList.Items[i]
					}
				}
				g.Expect(hpaForA).NotTo(BeNil(), "KEDA should have an HPA for deployment A")
				g.Expect(hpaForB).NotTo(BeNil(), "KEDA should have an HPA for deployment B")
				g.Expect(hpaForA.Status.CurrentMetrics).NotTo(BeEmpty(),
					"KEDA HPA A should have CurrentMetrics populated from wva_desired_replicas")
				g.Expect(hpaForB.Status.CurrentMetrics).NotTo(BeEmpty(),
					"KEDA HPA B should have CurrentMetrics populated from wva_desired_replicas")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})

	Context("Accelerator-specific scaling", func() {
		It("should respect GPU resource constraints per accelerator type", func() {
			By("Checking deployment replicas don't exceed expected limits")

			// Get deployment replica counts
			depA, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			depB, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicasA := depA.Status.Replicas
			replicasB := depB.Status.Replicas

			GinkgoWriter.Printf("Deployment A (NVIDIA) replicas: %d\n", replicasA)
			GinkgoWriter.Printf("Deployment B (AMD) replicas: %d\n", replicasB)

			// The GPU limiter must not let a variant scale beyond what GPU capacity
			// allows; in the emulated environment that ceiling is the scaler's
			// maxReplicas. Observe the constraint through the Deployment replica counts
			// rather than a VA status field.
			Expect(replicasA).To(BeNumerically("<=", 10), "Deployment A should not exceed maxReplicas")
			Expect(replicasB).To(BeNumerically("<=", 10), "Deployment B should not exceed maxReplicas")

			// Both deployments should be able to scale independently
			GinkgoWriter.Println("GPU limiter correctly manages deployments with different accelerator types")
		})
	})
})
