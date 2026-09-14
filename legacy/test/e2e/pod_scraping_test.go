package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// PodScrapingSource tests validate that the PodScrapingSource can discover and scrape
// metrics from EPP (Endpoint Picker) pods. These tests work with existing EPP infrastructure
// that should be deployed as part of infra-only mode.
//
// Note: On Kind clusters, pod IPs are not routable from outside the cluster, so direct
// scraping tests are skipped. In-cluster scraping tests still run to verify functionality.
var _ = Describe("PodScrapingSource", Label("full"), Ordered, func() {
	var (
		poolName         = "pod-scraping-pool"
		modelServiceName = "pod-scraping-ms"
		eppServiceName   string
	)

	BeforeAll(func() {
		By("Creating model service to ensure EPP pods exist")
		// EPP pods are created when a model service is deployed to an InferencePool
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, poolName, cfg.ModelID, "", cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		By("Creating service to expose model server")
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace,
			modelServiceName, modelServiceName+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx,
				modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
				"Model service should have at least 1 ready replica")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Discovering EPP service")
		// Discover existing EPP services dynamically (like legacy tests)
		// EPP service name follows pattern: {poolName}-epp
		// First try the expected pool name, then discover any existing EPP service
		expectedEPPName := poolName + "-epp"

		// Verify EPP service exists (either the expected one or discover an existing one)
		Eventually(func(g Gomega) {
			// Try expected EPP service first
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx,
				expectedEPPName, metav1.GetOptions{})
			if err == nil {
				eppServiceName = expectedEPPName
				return
			}

			// If expected service doesn't exist, discover existing EPP services
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			// Find first EPP service (service name ends with "-epp")
			for _, svc := range serviceList.Items {
				if len(svc.Name) > 4 && svc.Name[len(svc.Name)-4:] == "-epp" {
					eppServiceName = svc.Name
					GinkgoWriter.Printf("Discovered EPP service: %s\n", eppServiceName)
					return
				}
			}

			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}).Should(Succeed())

		Expect(eppServiceName).NotTo(BeEmpty(), "EPP service name should be set")

		By("Verifying EPP pods are Ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, eppServiceName)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to find EPP pods")

			readyCount := 0
			for _, pod := range pods {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						break
					}
				}
			}
			g.Expect(readyCount).To(BeNumerically(">=", 1),
				"Should have at least one Ready EPP pod")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

	})

	AfterAll(func() {
		By("Cleaning up test resources")
		// Service and deployment cleanup
		serviceName := modelServiceName + "-service"
		deploymentName := modelServiceName + "-decode"
		cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
			func() error {
				return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
		cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentName,
			func() error {
				return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
			},
			func() bool {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				return errors.IsNotFound(err)
			})
	})

	// Use shared PodScrapingSource tests with environment-agnostic configuration
	utils.DescribePodScrapingSourceTests(func() utils.PodScrapingTestConfig {
		return utils.PodScrapingTestConfig{
			Environment:      cfg.Environment,
			ServiceName:      eppServiceName,
			ServiceNamespace: cfg.LLMDNamespace,
			MetricsPort:      9090,
			MetricsPath:      "/metrics",
			MetricsScheme:    "http",
			K8sClient:        k8sClient,
			CRClient:         crClient,
			Ctx:              ctx,
		}
	})
})
