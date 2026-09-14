package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Annotation-based discovery e2e tests verify that WVA discovers variants from
// annotated HPA / ScaledObject resources. Annotation-based discovery is the only
// discovery path (the VariantAutoscaling CRD has been removed).
var _ = Describe("Annotation-based variant discovery", Serial, func() {

	// ── Scenario A: Basic lifecycle (without llm-d.ai/variant label) ────────────
	// No VA CR and no llm-d.ai/variant pod label. WVA discovers the variant via
	// PodLocator owner-walk (Pod → ReplicaSet → Deployment → managed HPA/ScaledObject).
	Context("basic lifecycle - without variant label (PodLocator owner-walk)", Ordered, func() {
		var (
			poolName         = "ann-disc-pool"
			modelServiceName = "ann-disc-basic"
			deploymentName   = modelServiceName + "-decode"
			// hpaBaseName is the logical base; the HPA object name will be hpaBaseName+"-hpa".
			// WVA discovers that HPA and uses its object name as variant_name in wva_desired_replicas.
			hpaBaseName = "ann-disc-basic"
			hpaName     = hpaBaseName + "-hpa"
			ns          string
		)

		BeforeAll(func() {
			nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "ann-disc-a-"},
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

			By("Creating model service deployment")
			err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceName, poolName, cfg.ModelID, "", cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

			By("Waiting for deployment to be ready")
			Eventually(func(g Gomega) {
				d, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(d.Status.ReadyReplicas).To(Equal(int32(1)))
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating annotated ScaledObject")
			err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaBaseName, deploymentName, hpaName, 1, 10, cfg.MonitoringNS,
				fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
			Expect(err).NotTo(HaveOccurred(), "Failed to create annotated ScaledObject")
		})

		It("should expose wva_desired_replicas for the annotated scaler", func() {
			By("Verifying KEDA has read wva_desired_replicas from Prometheus")
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
					"KEDA HPA should have CurrentMetrics populated, indicating wva_desired_replicas was read from Prometheus")
				GinkgoWriter.Printf("KEDA HPA CurrentMetrics populated (%d entries) — wva_desired_replicas is being consumed\n",
					len(kedaHPA.Status.CurrentMetrics))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

	})

	// ── Scenario B: Basic lifecycle (with llm-d.ai/variant label) ────────────────
	// Same as Scenario A but the pod template carries the llm-d.ai/variant label.
	// WVA can take the label fast path (llm_d_ai_variant metric label) instead of
	// walking ownerReferences. Both paths must produce the same wva_desired_replicas.
	Context("basic lifecycle - with variant label (label fast path)", Ordered, func() {
		var (
			poolName         = "ann-disc-label-pool"
			modelServiceName = "ann-disc-label"
			deploymentName   = modelServiceName + "-decode"
			hpaBaseName      = "ann-disc-label"
			hpaName          = hpaBaseName + "-hpa"
			ns               string
		)

		BeforeAll(func() {
			nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "ann-disc-b-"},
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

			By("Creating model service deployment WITH llm-d.ai/variant label")
			// Pass hpaName as variantName so the pod template carries llm-d.ai/variant=<hpaName>.
			err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceName, poolName, cfg.ModelID, hpaName, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

			By("Verifying pod template carries llm-d.ai/variant label")
			Eventually(func(g Gomega) {
				d, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(d.Spec.Template.Labels).To(HaveKeyWithValue("llm-d.ai/variant", hpaName),
					"Pod template must carry llm-d.ai/variant=%s", hpaName)
			}).Should(Succeed())

			By("Waiting for deployment to be ready")
			Eventually(func(g Gomega) {
				d, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(d.Status.ReadyReplicas).To(Equal(int32(1)))
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating annotated ScaledObject")
			err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaBaseName, deploymentName, hpaName, 1, 10, cfg.MonitoringNS,
				fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
			Expect(err).NotTo(HaveOccurred(), "Failed to create annotated ScaledObject")
		})

		It("should expose wva_desired_replicas for the annotated scaler", func() {
			By("Verifying KEDA has read wva_desired_replicas from Prometheus")
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
					"KEDA HPA should have CurrentMetrics populated")
				GinkgoWriter.Printf("KEDA HPA CurrentMetrics populated (%d entries)\n", len(kedaHPA.Status.CurrentMetrics))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

	})

})
