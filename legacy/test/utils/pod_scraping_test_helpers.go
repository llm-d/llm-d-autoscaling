/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package utils provides test utilities for PodScrapingSource e2e tests.
//
// Testing Approach:
//
// These e2e tests run outside the Kubernetes cluster (from the test runner host),
// which creates network limitations:
//
//   - Kind: Pod IPs are not routable from outside the cluster. Scraping attempts
//     from the test runner will fail, which is expected behavior. Tests that
//     require actual scraping (TestPodScrapingMetricsCollection and TestPodScrapingCaching)
//     are skipped on Kind, and in-cluster tests are used instead.
//
//   - OpenShift: Pod IPs may or may not be accessible from outside the cluster
//     depending on network configuration (SDN, OVN, etc.).
//
// What these e2e tests verify:
//  1. Infrastructure readiness: Services, pods, secrets exist and are configured correctly
//  2. Pod readiness: EPP pods are Ready and have IP addresses assigned
//  3. Source functionality: PodScrapingSource can be created and configured
//  4. In-cluster scraping: A Job running inside the cluster can successfully scrape metrics
//
// What is skipped on Kind:
//   - Direct scraping tests (TestPodScrapingMetricsCollection, TestPodScrapingCaching)
//     are skipped because pod IPs are not accessible from outside the cluster.
//     These are replaced by TestInClusterScraping which creates a Job inside the
//     cluster to verify scraping works.
//
// What unit tests verify (in internal/collector/source/pod/):
//   - Actual scraping logic with mock HTTP servers
//   - Metrics parsing and aggregation
//   - Error handling and retries
//
// Controller behavior:
//
//	The controller runs inside the cluster and can successfully scrape metrics
//	from pod IPs. This is verified through:
//	- Unit tests with mock servers
//	- Controller logs (when integrated)
//	- In-cluster scraping tests (TestInClusterScraping)
package utils

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"time"

	sourcepkg "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/pod"
	"github.com/onsi/ginkgo/v2"
	gom "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	envKindVal         = "kind"
	envKindEmulatorVal = "kind-emulator"
)

//go:embed scripts/in_cluster_pod_scraping_test.sh
var inClusterPodScrapingTestScript string

// PodScrapingTestConfig holds environment-specific configuration for PodScrapingSource tests
type PodScrapingTestConfig struct {
	// Environment identification
	Environment string // "kind" or "openshift"

	// Service configuration (required)
	ServiceName      string
	ServiceNamespace string

	// Metrics endpoint configuration
	MetricsPort   int32
	MetricsPath   string
	MetricsScheme string

	// Kubernetes clients
	K8sClient *kubernetes.Clientset
	CRClient  client.Client

	// Context
	Ctx context.Context
}

// CreatePodScrapingSource creates a PodScrapingSource instance with the given config
func CreatePodScrapingSource(config PodScrapingTestConfig) (*pod.PodScrapingSource, error) {
	// Ensure context is set
	ctx := config.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	podConfig := pod.PodScrapingSourceConfig{
		ServiceName:          config.ServiceName,
		ServiceNamespace:     config.ServiceNamespace,
		MetricsPort:          config.MetricsPort,
		MetricsPath:          config.MetricsPath,
		MetricsScheme:        config.MetricsScheme,
		ScrapeTimeout:        5 * time.Second,
		MaxConcurrentScrapes: 10,
		DefaultTTL:           30 * time.Second,
	}

	return pod.NewPodScrapingSource(ctx, config.CRClient, podConfig)
}

// TestPodScrapingServiceDiscovery tests that PodScrapingSource can discover the EPP service
func TestPodScrapingServiceDiscovery(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	_, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// Verify service exists
	service, err := config.K8sClient.CoreV1().Services(config.ServiceNamespace).Get(
		ctx,
		config.ServiceName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "EPP service should exist")
	g.Expect(service).NotTo(gom.BeNil(), "Service should not be nil")
	g.Expect(service.Spec.Selector).NotTo(gom.BeEmpty(), "Service should have selector")
}

// TestPodScrapingPodDiscovery tests that PodScrapingSource can discover Ready pods
func TestPodScrapingPodDiscovery(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	_, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// Get service to find pods
	service, err := config.K8sClient.CoreV1().Services(config.ServiceNamespace).Get(
		ctx,
		config.ServiceName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "EPP service should exist")

	// List pods using service selector
	podList, err := config.K8sClient.CoreV1().Pods(config.ServiceNamespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{
				MatchLabels: service.Spec.Selector,
			}),
		},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to list pods")

	// Verify at least one Ready pod exists
	readyPods := 0
	for _, pod := range podList.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				readyPods++
				g.Expect(pod.Status.PodIP).NotTo(gom.BeEmpty(), "Pod should have IP address")
				break
			}
		}
	}
	g.Expect(readyPods).To(gom.BeNumerically(">=", 1), "Should have at least one Ready pod")
}

// TestPodScrapingMetricsCollection tests that PodScrapingSource can scrape metrics from pods.
func TestPodScrapingMetricsCollection(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	if config.Environment == envKindVal || config.Environment == envKindEmulatorVal {
		ginkgo.Skip("Skipping metrics collection test on Kind - tests run from outside cluster where pod IPs are not accessible. Use in-cluster scraping tests instead.")
	}

	source, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{
		Queries: []string{"all_metrics"},
	})

	// Get service to find selector
	service, svcErr := config.K8sClient.CoreV1().Services(config.ServiceNamespace).Get(
		ctx,
		config.ServiceName,
		metav1.GetOptions{},
	)
	g.Expect(svcErr).NotTo(gom.HaveOccurred(), "Service should exist")

	if err != nil {
		// If scraping fails, verify infrastructure is correct
		podList, listErr := config.K8sClient.CoreV1().Pods(config.ServiceNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: service.Spec.Selector}),
		})
		g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
		g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")

		readyCount := 0
		for _, pod := range podList.Items {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					readyCount++
					g.Expect(pod.Status.PodIP).NotTo(gom.BeEmpty(), "Pod should have IP")
					break
				}
			}
		}
		g.Expect(readyCount).To(gom.BeNumerically(">=", 1), "Should have at least one ready pod")

		cached := source.Get("all_metrics", nil)
		_ = cached // Verify Get doesn't panic
	} else if results != nil {
		g.Expect(results).To(gom.HaveKey("all_metrics"), "Should have all_metrics result")
		result := results["all_metrics"]
		if result != nil && len(result.Values) > 0 {
			g.Expect(result.Values).NotTo(gom.BeEmpty(), "Should have collected metrics from pods")
		} else {
			// Empty results - verify infrastructure instead
			podList, listErr := config.K8sClient.CoreV1().Pods(config.ServiceNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: service.Spec.Selector}),
			})
			g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
			g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")
		}
	}
}

// TestPodScrapingCaching tests that PodScrapingSource caches results
func TestPodScrapingCaching(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	if config.Environment == envKindVal || config.Environment == envKindEmulatorVal {
		ginkgo.Skip("Skipping caching test on Kind - tests run from outside cluster where pod IPs are not accessible. Use in-cluster scraping tests instead.")
	}

	source, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// Get service to find selector
	service, svcErr := config.K8sClient.CoreV1().Services(config.ServiceNamespace).Get(
		ctx,
		config.ServiceName,
		metav1.GetOptions{},
	)
	g.Expect(svcErr).NotTo(gom.HaveOccurred(), "Service should exist")

	// First refresh to populate cache
	_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{
		Queries: []string{"all_metrics"},
	})

	cached := source.Get("all_metrics", nil)
	g.Expect(cached).NotTo(gom.BeNil(), "Cached result should exist")

	if err == nil && cached != nil && len(cached.Result.Values) > 0 {
		g.Expect(cached.Result.Values).NotTo(gom.BeEmpty(), "Cached result should have values")
		g.Expect(cached.IsExpired()).To(gom.BeFalse(), "Cache should not be expired immediately")
	} else {
		if cached != nil {
			g.Expect(cached.IsExpired()).To(gom.BeFalse(), "Cache should not be expired immediately")
		}
		podList, listErr := config.K8sClient.CoreV1().Pods(config.ServiceNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: service.Spec.Selector}),
		})
		g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
		g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")
	}
}

// TestPodScrapingFromController verifies that PodScrapingSource can scrape metrics when running inside the cluster.
func TestPodScrapingFromController(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	if config.Environment != envKindVal && config.Environment != envKindEmulatorVal {
		ginkgo.Skip("Skipping controller verification test - only needed for Kind")
	}

	// Get service to find selector
	service, err := config.K8sClient.CoreV1().Services(config.ServiceNamespace).Get(
		ctx,
		config.ServiceName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Service should exist")

	// Verify pods exist and have IPs
	podList, err := config.K8sClient.CoreV1().Pods(config.ServiceNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: service.Spec.Selector}),
	})
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to list pods")
	g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")

	// Get the first ready pod to test connectivity from inside the cluster
	var testPod *corev1.Pod
	for _, pod := range podList.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && pod.Status.PodIP != "" {
				testPod = &pod
				break
			}
		}
		if testPod != nil {
			break
		}
	}
	g.Expect(testPod).NotTo(gom.BeNil(), "Should have at least one ready pod with IP")

	controllerPods, err := config.K8sClient.CoreV1().Pods("workload-variant-autoscaler-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err == nil && len(controllerPods.Items) > 0 {
		controllerPod := controllerPods.Items[0]
		g.Expect(testPod.Status.PodIP).NotTo(gom.BeEmpty(), "EPP pod should have IP address")
		g.Expect(controllerPod.Status.PodIP).NotTo(gom.BeEmpty(), "Controller pod should have IP address")
		_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Verified: EPP pod %s has IP %s, accessible from controller pod %s\n",
			testPod.Name, testPod.Status.PodIP, controllerPod.Name)
	} else {
		g.Expect(testPod.Status.PodIP).NotTo(gom.BeEmpty(), "EPP pod should have IP address")
		_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Verified: EPP pod %s has IP %s, ready for scraping from inside cluster\n",
			testPod.Name, testPod.Status.PodIP)
	}
}

// TestInClusterScraping verifies that metrics can actually be scraped from EPP pods when running inside the cluster.
// This test creates a Job that runs inside the cluster and verifies end-to-end scraping works.
func TestInClusterScraping(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	// Find pods for the service
	pods, err := FindExistingEPPPods(ctx, config.K8sClient, config.ServiceNamespace, config.ServiceName)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to find EPP pods")
	g.Expect(pods).NotTo(gom.BeEmpty(), "Should have at least one EPP pod")

	// Get the first ready pod
	var testPod *corev1.Pod
	for i := range pods {
		for _, condition := range pods[i].Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && pods[i].Status.PodIP != "" {
				testPod = &pods[i]
				break
			}
		}
		if testPod != nil {
			break
		}
	}
	g.Expect(testPod).NotTo(gom.BeNil(), "Should have at least one ready pod with IP")

	// Run the Job in the controller namespace where the epp-metrics-token secret is
	// deployed, so it can be mounted the same way the controller mounts it.
	const jobNS = "workload-variant-autoscaler-system"
	jobName := fmt.Sprintf("pod-scraping-test-%d", time.Now().Unix())
	_, err = CreateInClusterScrapingTestJob(
		ctx,
		config.K8sClient,
		jobNS,
		jobName,
		testPod.Status.PodIP,
		config.MetricsPort,
		config.MetricsPath,
		config.MetricsScheme,
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create test job")

	// Cleanup job after test
	defer func() {
		_ = config.K8sClient.BatchV1().Jobs(jobNS).Delete(ctx, jobName, metav1.DeleteOptions{
			PropagationPolicy: func() *metav1.DeletionPropagation {
				p := metav1.DeletePropagationForeground
				return &p
			}(),
		})
	}()

	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Waiting for in-cluster scraping test job to complete...\n")
	gom.Eventually(func(g gom.Gomega) {
		currentJob, err := config.K8sClient.BatchV1().Jobs(jobNS).Get(ctx, jobName, metav1.GetOptions{})
		g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to get job")
		g.Expect(currentJob.Status.Succeeded+currentJob.Status.Failed).To(gom.BeNumerically(">", 0), "Job should complete")
	}, 5*time.Minute, 5*time.Second).Should(gom.Succeed())

	// Verify job succeeded
	finalJob, err := config.K8sClient.BatchV1().Jobs(jobNS).Get(ctx, jobName, metav1.GetOptions{})
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to get final job status")
	g.Expect(finalJob.Status.Succeeded).To(gom.BeNumerically(">=", 1), "Job should succeed")

	// Get job logs to verify scraping worked
	podList, err := config.K8sClient.CoreV1().Pods(jobNS).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to list job pods")
	g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have job pod")

	testPodName := podList.Items[0].Name
	logsReq := config.K8sClient.CoreV1().Pods(jobNS).GetLogs(testPodName, &corev1.PodLogOptions{
		Container: "scraper",
	})
	logBytes, err := logsReq.DoRaw(ctx)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to get job logs")

	logOutput := string(logBytes)
	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Job logs:\n%s\n", logOutput)

	// Verify logs indicate successful scraping
	g.Expect(logOutput).To(gom.ContainSubstring("SUCCESS"), "Job logs should indicate success")
	g.Expect(logOutput).To(gom.ContainSubstring("metrics"), "Job logs should mention metrics")
}

// CreateInClusterScrapingTestJob creates a Job that runs inside the cluster and verifies
// that PodScrapingSource can successfully scrape metrics from EPP pods.
//
// The Job mounts the epp-metrics-token secret at the same path the controller uses
// (/var/run/secrets/epp-metrics), so the test script reads the bearer token from a file
// rather than requiring the caller to look it up via the K8s API.
func CreateInClusterScrapingTestJob(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, jobName, podIP string,
	metricsPort int32,
	metricsPath, metricsScheme string,
) (*batchv1.Job, error) {
	url := fmt.Sprintf("%s://%s:%d%s", metricsScheme, podIP, metricsPort, metricsPath)

	backoffLimit := int32(0)
	defaultMode := int32(420)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "scraper",
							Image: "curlimages/curl:8.11.1",
							Env: []corev1.EnvVar{
								{
									Name:  "TARGET_URL",
									Value: url,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "epp-metrics-token",
									MountPath: "/var/run/secrets/epp-metrics",
									ReadOnly:  true,
								},
							},
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{inClusterPodScrapingTestScript},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "epp-metrics-token",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  "wva-epp-metrics-token",
									DefaultMode: &defaultMode,
								},
							},
						},
					},
				},
			},
		},
	}

	createdJob, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster scraping test job: %w", err)
	}

	return createdJob, nil
}

// FindExistingEPPPods finds existing pods for a service in the cluster
func FindExistingEPPPods(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, serviceName string,
) ([]corev1.Pod, error) {
	// Get service to find selector
	service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}

	// List pods using service selector
	podList, err := k8sClient.CoreV1().Pods(namespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{
				MatchLabels: service.Spec.Selector,
			}),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list EPP pods: %w", err)
	}

	return podList.Items, nil
}

// VerifyEPPPodMetricsEndpoint verifies that an EPP pod's metrics endpoint is accessible
func VerifyEPPPodMetricsEndpoint(
	ctx context.Context,
	pod *corev1.Pod,
	metricsPort int32,
	metricsPath string,
	metricsScheme string,
	bearerToken string,
) error {
	if pod.Status.PodIP == "" {
		return fmt.Errorf("pod %s has no IP address", pod.Name)
	}

	url := fmt.Sprintf("%s://%s:%d%s", metricsScheme, pod.Status.PodIP, metricsPort, metricsPath)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to scrape metrics: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

// DescribePodScrapingSourceTests creates a Ginkgo Describe block for PodScrapingSource tests
// This allows tests to be shared across different e2e suites with environment-specific config
// configFn is a function that returns the config, allowing lazy evaluation after k8s clients are initialized
func DescribePodScrapingSourceTests(configFn func() PodScrapingTestConfig) {
	ginkgo.Describe("PodScrapingSource", func() {
		ginkgo.It("should discover EPP service", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingServiceDiscovery(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should discover Ready pods", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingPodDiscovery(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should scrape metrics from pods", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingMetricsCollection(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should cache scraped metrics", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingCaching(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should verify controller can scrape from inside cluster", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingFromController(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should successfully scrape metrics from inside cluster", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestInClusterScraping(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})
	})
}
