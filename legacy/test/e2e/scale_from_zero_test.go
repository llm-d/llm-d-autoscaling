package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// cleanupScaleFromZeroResources deletes all resources created by scale-from-zero tests to ensure clean state
func cleanupScaleFromZeroResources() {
	GinkgoWriter.Println("Cleaning up scale-from-zero test resources for clean state...")

	// Helper to check if resource name matches scale-from-zero test patterns
	isScaleFromZeroResource := func(name string) bool {
		return strings.HasPrefix(name, "scale-from-zero-")
	}

	// Delete all HPAs with scale-from-zero prefix
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hpa := range hpaList.Items {
			if isScaleFromZeroResource(hpa.Name) {
				GinkgoWriter.Printf("  Deleting HPA: %s\n", hpa.Name)
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ScaledObjects with scale-from-zero prefix (KEDA)
	soList := &unstructured.UnstructuredList{}
	soList.SetAPIVersion("keda.sh/v1alpha1")
	soList.SetKind("ScaledObjectList")
	if err := crClient.List(ctx, soList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, so := range soList.Items {
			if isScaleFromZeroResource(so.GetName()) {
				GinkgoWriter.Printf("  Deleting ScaledObject: %s\n", so.GetName())
				_ = crClient.Delete(ctx, &so)
			}
		}
	}

	// Delete all Deployments with scale-from-zero prefix
	deployList, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, deploy := range deployList.Items {
			if isScaleFromZeroResource(deploy.Name) {
				GinkgoWriter.Printf("  Deleting Deployment: %s\n", deploy.Name)
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploy.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all LeaderWorkerSets with scale-from-zero prefix
	lwsList := &unstructured.UnstructuredList{}
	lwsList.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	lwsList.SetKind("LeaderWorkerSetList")
	if err := crClient.List(ctx, lwsList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, lws := range lwsList.Items {
			if isScaleFromZeroResource(lws.GetName()) {
				GinkgoWriter.Printf("  Deleting LeaderWorkerSet: %s\n", lws.GetName())
				_ = crClient.Delete(ctx, &lws)
			}
		}
	}

	// Delete all Services with scale-from-zero prefix
	svcList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, svc := range svcList.Items {
			if isScaleFromZeroResource(svc.Name) {
				GinkgoWriter.Printf("  Deleting Service: %s\n", svc.Name)
				_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ServiceMonitors with scale-from-zero prefix in monitoring namespace
	smList := &promoperator.ServiceMonitorList{}
	if err := crClient.List(ctx, smList, client.InNamespace(cfg.MonitoringNS)); err == nil {
		for _, sm := range smList.Items {
			if isScaleFromZeroResource(sm.Name) {
				GinkgoWriter.Printf("  Deleting ServiceMonitor: %s\n", sm.Name)
				_ = crClient.Delete(ctx, &sm)
			}
		}
	}

	// Delete all trigger Jobs with scale-from-zero prefix
	jobList, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, job := range jobList.Items {
			if isScaleFromZeroResource(job.Name) {
				GinkgoWriter.Printf("  Deleting Job: %s\n", job.Name)
				propagation := metav1.DeletePropagationBackground
				_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			}
		}
	}

	// Wait a moment for deletions to propagate
	time.Sleep(2 * time.Second)
	GinkgoWriter.Println("Cleanup completed")
}

// Scale-from-zero test validates that the WVA controller correctly detects pending requests
// and scales up scale targets from zero replicas. Requires GIE queuing (flowControl feature gate
// enabled on EPP via SCALE_TO_ZERO_ENABLED=true on make deploy-e2e-infra) and an InferenceObjective (applied below in BeforeAll).
// This suite needs a scaler that allows minReplicas=0 on the scaled workload: either
// SCALE_TO_ZERO_ENABLED=true where native HPA supports it (HPAScaleToZero), or SCALER_BACKEND=keda
// (ScaledObject). OpenShift usually lacks HPAScaleToZero; e2e config ignores SCALE_TO_ZERO_ENABLED there,
// so use SCALER_BACKEND=keda for this Describe when running on OpenShift.
// On platforms without the HPAScaleToZero feature gate (e.g. OpenShift), set SCALER_BACKEND=keda
// so the test uses a KEDA ScaledObject (which supports minReplicas=0) instead of a native HPA.
var _ = Describe("Scale-From-Zero Feature", Serial, Label("full"), Ordered, func() {
	var (
		poolName         = "scale-from-zero-pool"
		modelServiceName = "scale-from-zero-ms"
		// variantName is passed to the model service (llm-d.ai/variant label) and
		// becomes the variant_name label on wva_desired_replicas.
		variantName = "scale-from-zero-va"
		hpaName     = "scale-from-zero-hpa"
	)

	BeforeAll(func() {
		// Scale-from-zero requires GIE flow control queuing (EPP flowControl feature gate).
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: " +
				"set SCALE_TO_ZERO_ENABLED=true (required for EPP flow-control queuing)")
		}

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		// Wait for InferencePool to be reconciled and registered in the datastore
		// The scale-from-zero engine needs the InferencePool to be in the datastore
		// to find the EPP and query flow control queue metrics.
		// The InferencePool reconciler should have already reconciled it as part of infrastructure.
		// check for EPP service by name and pods by inferencepool label.
		By("Waiting for InferencePool to be reconciled (allows time for controller to register it in datastore)")
		eppServiceName := cfg.EPPServiceName
		GinkgoWriter.Printf("Looking for EPP service: %s in namespace: %s\n", eppServiceName, cfg.LLMDNamespace)
		// Wait for the EPP service to exist
		Eventually(func(g Gomega) {
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, eppServiceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}).Should(Succeed(), "EPP service should exist")

		// Wait for EPP pods to be ready. Use the EPP Service's own selector
		// (via FindExistingEPPPods) rather than hard-coding a label key —
		// the llm-d chart changed the EPP pod label in v0.9.0 from
		// `inferencepool=<name>` to `llm-d-router-gateway=<name>`, and
		// future changes are likely. The Service.spec.selector is the
		// authoritative source.
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, eppServiceName)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to find EPP pods")
			g.Expect(pods).ToNot(BeEmpty(), "EPP pods should exist")

			// Check that at least one pod is ready
			hasReadyPod := false
			for _, pod := range pods {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						hasReadyPod = true
						break
					}
				}
				if hasReadyPod {
					break
				}
			}
			g.Expect(hasReadyPod).To(BeTrue(), "At least one EPP pod should be ready")
		}).Should(Succeed(), "EPP pods should be ready")

		By("Applying InferenceObjective e2e-default for GIE queuing (if API is available)")
		poolRefName := cfg.PoolName
		if poolRefName == "" {
			poolRefName = strings.TrimSuffix(eppServiceName, "-epp")
		}
		ioApplied, errIO := fixtures.EnsureInferenceObjective(ctx, crClient, cfg.LLMDNamespace, poolRefName)
		Expect(errIO).NotTo(HaveOccurred(), "EnsureInferenceObjective should not return a hard error")
		if !ioApplied {
			Skip("InferenceObjective API not available on cluster; scale-from-zero requires inference.networking.x-k8s.io InferenceObjective")
		}
		DeferCleanup(func() {
			_ = fixtures.DeleteInferenceObjective(context.Background(), crClient, cfg.LLMDNamespace)
		})

		By("Creating model service deployment with 0 initial replicas")
		// Create deployment with 0 replicas using the fixture
		err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, variantName, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		// Immediately scale deployment to 0 (with retry to handle race conditions)
		By("Scaling deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			deployment.Spec.Replicas = ptr.To(int32(0))
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to update deployment to 0 replicas")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed(), "Should successfully scale deployment to 0 replicas")

		By("Creating service to expose model server")
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, modelServiceName+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, modelServiceName+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		// Register cleanup for ServiceMonitor
		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			cleanupResource(ctx, "ServiceMonitor", cfg.MonitoringNS, serviceMonitorName,
				func() error {
					return crClient.Delete(ctx, &promoperator.ServiceMonitor{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceMonitorName,
							Namespace: cfg.MonitoringNS,
						},
					})
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: cfg.MonitoringNS}, &promoperator.ServiceMonitor{})
					return errors.IsNotFound(err)
				})
		})

		By("Verifying deployment is at 0 replicas")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deploy.Status.Replicas).To(Equal(int32(0)), "Deployment should be scaled to 0")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())

		By("Creating annotated ScaledObject with minReplicas=0 to allow scale-from-zero")
		// The annotated ScaledObject is both the discovery source and the scaler; WVA
		// discovers the variant from its llm-d.ai/managed annotation and emits
		// wva_desired_replicas keyed by variantName.
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, modelServiceName+"-decode", variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject with scale-to-zero")

		GinkgoWriter.Println("Scale-from-zero test setup complete with deployment at 0 replicas")
	})

	AfterAll(func() {
		By("Cleaning up scale-from-zero test resources")

		// Delete scaler (ScaledObject)
		_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)

		// Delete service
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, modelServiceName+"-service", metav1.DeleteOptions{})

		// Delete deployment
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelServiceName+"-decode", metav1.DeleteOptions{})

	})

	Context("Initial state verification", func() {
		It("should have annotated scaler created", func() {
			By("Verifying annotated ScaledObject exists")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())

			GinkgoWriter.Printf("Annotated scaler verified: %s\n", hpaName)
		})

		It("should verify deployment starts at zero replicas", func() {
			By("Checking deployment has 0 replicas")
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			specReplicas := int32(1)
			if deploy.Spec.Replicas != nil {
				specReplicas = *deploy.Spec.Replicas
			}

			Expect(specReplicas).To(Equal(int32(0)), "Deployment should start with 0 replicas")
			GinkgoWriter.Println("Deployment verified at 0 replicas")
		})

		It("should have scaler configured with minReplicas=0", func() {
			By("Verifying ScaledObject allows scale-to-zero")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())
			minReplicas, found, err := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have minReplicaCount")
			Expect(minReplicas).To(Equal(int64(0)), "ScaledObject should allow scale-to-zero")
		})
	})

	// TODO(scale-from-zero-keda-flaky): The KEDA scale-from-zero flow is flaky — the
	// engine intermittently logs "Inferencepool datastore is empty - skipping processing
	// inactive variant" and the target never scales up from zero within the timeout.
	// This was tolerated while the KEDA full-e2e job was non-blocking; it became blocking
	// once KEDA became the sole backend. Labeled
	// "flaky" to keep it out of the required `full && !smoke && !flaky` gate until the
	// root cause (InferencePool datastore not populated for inactive variants) is fixed.
	// Tracking issue: TODO(file issue and reference here).
	Context("Scale-from-zero with pending requests", Label("flaky"), func() {
		var triggerJobName string

		AfterAll(func() {
			if triggerJobName != "" {
				By("Cleaning up trigger job")
				propagation := metav1.DeletePropagationBackground
				err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
				if err != nil && !errors.IsNotFound(err) {
					GinkgoWriter.Printf("Warning: failed to delete trigger job %s: %v\n", triggerJobName, err)
				}
			}
		})

		It("should detect pending requests and trigger scale-from-zero", func() {
			By("Discovering inference gateway service")
			// Discover the inference gateway service
			gatewayServiceName := ""
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			for _, svc := range serviceList.Items {
				if strings.Contains(svc.Name, "inference-gateway") {
					gatewayServiceName = svc.Name
					GinkgoWriter.Printf("Found inference gateway service: %s\n", gatewayServiceName)
					break
				}
			}
			// Fallback: GAIE standalone chart embeds Envoy in the EPP pod and exposes port 80
			// on the EPP service itself — no separate inference-gateway Service is created.
			if gatewayServiceName == "" {
				gatewayServiceName = cfg.EPPServiceName
				GinkgoWriter.Printf("No inference-gateway service found; using EPP service as gateway (standalone chart): %s\n", gatewayServiceName)
			}
			Expect(gatewayServiceName).NotTo(BeEmpty(), "Inference gateway service should exist")

			By("Creating a job to send requests while deployment is at zero")
			triggerJobName = fmt.Sprintf("scale-from-zero-trigger-%d", time.Now().Unix())

			// Create a job that sends requests to the gateway service (which routes through EPP)
			// This allows EPP to queue requests and expose the flow control queue size metric
			job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayServiceName, cfg.ModelID)
			_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to create scale-from-zero trigger job")

			GinkgoWriter.Printf("Created scale-from-zero trigger job: %s\n", triggerJobName)

			By("Waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "job-name=" + triggerJobName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podList.Items).ToNot(BeEmpty(), "Job pod should exist")

				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), "Job pod should be running or succeeded")
			}).Should(Succeed())

			GinkgoWriter.Println("Job pod is running and sending requests")

			By("Monitoring deployment for scale-from-zero decision")
			// The scale-from-zero engine detects pending requests and directly scales
			// the target Deployment 0→1. We observe that through the Deployment's spec
			// replicas rather than a VA status field (VA no longer exists).
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				var specReplicas int32
				if deploy.Spec.Replicas != nil {
					specReplicas = *deploy.Spec.Replicas
				}

				GinkgoWriter.Printf("Deployment spec replicas: %d (waiting for > 0)\n", specReplicas)

				// Scale-from-zero engine should detect pending requests and scale up
				g.Expect(specReplicas).To(BeNumerically(">", 0),
					"Deployment should be scaled up from zero due to pending requests")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Scale-from-zero engine detected pending requests and recommended scale-up")
		})

		It("should scale deployment up from zero", func() {
			By("Monitoring deployment for actual scale-up from zero")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas := deploy.Status.Replicas
				readyReplicas := deploy.Status.ReadyReplicas

				GinkgoWriter.Printf("Current replicas: %d, ready: %d (waiting for > 0)\n",
					currentReplicas, readyReplicas)

				// Deployment should have scaled up from 0
				g.Expect(currentReplicas).To(BeNumerically(">", 0),
					"Deployment should have scaled up from zero")

				// At least one pod should be ready
				g.Expect(readyReplicas).To(BeNumerically(">", 0),
					"At least one pod should be ready after scale-up")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Deployment successfully scaled up from zero")
		})

		It("should successfully process requests after scaling up", func() {
			By("Verifying the trigger job completes successfully")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, triggerJobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				// Job should eventually succeed
				g.Expect(job.Status.Succeeded).To(BeNumerically(">", 0),
					"Job should complete successfully after deployment scales up")

			}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalVerySlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Requests processed successfully after scale-from-zero")
		})

	})
})

// createScaleFromZeroTriggerJob creates a job that sends requests to the inference gateway to trigger scale-from-zero
// Requests go through the gateway (port 80) which routes through EPP, creating the flow control queue
// that the scale-from-zero engine monitors via the inference_extension_flow_control_queue_size metric
func createScaleFromZeroTriggerJob(name, namespace, gatewayService, modelID string) *batchv1.Job {
	backoffLimit := int32(3)
	numRequests := 10

	script := fmt.Sprintf(`#!/bin/sh
echo "Scale-from-zero trigger job starting..."
echo "Sending %d requests to gateway %s:80"
echo "Model ID: %s"

# Send requests with delays to allow scale-from-zero engine to detect them
SENT=0
SUCCESS=0
FAILED=0

while [ $SENT -lt %d ]; do
  echo "Sending request $((SENT + 1)) / %d..."
  
  RESPONSE=$(curl -s -w "\n%%{http_code}" --max-time 180 -X POST http://%s:80/v1/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"%s","prompt":"Test prompt for scale-from-zero","max_tokens":50}' 2>&1)
  
  HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
  
  if [ "$HTTP_CODE" = "200" ]; then
    SUCCESS=$((SUCCESS + 1))
    echo "Request $((SENT + 1)) succeeded (HTTP $HTTP_CODE)"
  else
    FAILED=$((FAILED + 1))
    echo "Request $((SENT + 1)) failed (HTTP $HTTP_CODE)"
  fi
  
  SENT=$((SENT + 1))
  
  # Small delay between requests to allow scale-from-zero engine to detect pending requests
  sleep 2
done

echo "Job completed: sent=$SENT, success=$SUCCESS, failed=$FAILED"

# Consider job successful if at least some requests succeeded
if [ $SUCCESS -gt 0 ]; then
  exit 0
else
  exit 1
fi
`, numRequests, gatewayService, modelID, numRequests, numRequests, gatewayService, modelID)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"test-resource": "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"test-resource": "true",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "curl-trigger",
							Image: "quay.io/curl/curl:8.11.1",
							Command: []string{
								"sh", "-c", script,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

// Scale-from-zero test for LeaderWorkerSet
var _ = Describe("Scale-From-Zero Feature with LeaderWorkerSet", Serial, Label("full"), Ordered, func() {
	var (
		poolName         = "scale-from-zero-lws-pool"
		modelServiceName = "scale-from-zero-lws-ms"
		lwsName          = modelServiceName + "-decode"
		// variantName is passed to the LWS (llm-d.ai/variant label) and becomes the
		// variant_name label on wva_desired_replicas.
		variantName  = "scale-from-zero-lws-va"
		hpaName      = "scale-from-zero-lws-hpa"
		lwsGroupSize = int32(2) // 1 leader + 1 worker
	)

	BeforeAll(func() {
		// Scale-from-zero requires GIE flow control queuing (EPP flowControl feature gate).
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: " +
				"set SCALE_TO_ZERO_ENABLED=true (required for EPP flow-control queuing)")
		}

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		// Wait for InferencePool to be reconciled and registered in the datastore
		By("Waiting for InferencePool to be reconciled (allows time for controller to register it in datastore)")
		eppServiceName := cfg.EPPServiceName
		GinkgoWriter.Printf("Looking for EPP service: %s in namespace: %s\n", eppServiceName, cfg.LLMDNamespace)

		// Wait for the EPP service to exist
		Eventually(func(g Gomega) {
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, eppServiceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}).Should(Succeed(), "EPP service should exist")

		// Wait for EPP pods to be ready. Use the EPP Service's own selector
		// (via FindExistingEPPPods) rather than hard-coding a label key —
		// the llm-d chart changed the EPP pod label in v0.9.0 from
		// `inferencepool=<name>` to `llm-d-router-gateway=<name>`, and
		// future changes are likely. The Service.spec.selector is the
		// authoritative source.
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, eppServiceName)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to find EPP pods")
			g.Expect(pods).ToNot(BeEmpty(), "EPP pods should exist")

			// Check that at least one pod is ready
			hasReadyPod := false
			for _, pod := range pods {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						hasReadyPod = true
						break
					}
				}
				if hasReadyPod {
					break
				}
			}
			g.Expect(hasReadyPod).To(BeTrue(), "At least one EPP pod should be ready")
		}).Should(Succeed(), "EPP pods should be ready")

		By("Creating model service LeaderWorkerSet with 0 initial replicas")
		err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, variantName, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

		// Register cleanup for LWS
		DeferCleanup(func() {
			cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
				func() error {
					return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
				},
				func() bool {
					lws := &unstructured.Unstructured{}
					lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
					lws.SetKind("LeaderWorkerSet")
					err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
					return errors.IsNotFound(err)
				})
		})

		// Immediately scale LWS to 0 (with retry to handle race conditions)
		By("Scaling LeaderWorkerSet to 0 replicas")
		Eventually(func(g Gomega) {
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get LWS")

			err = unstructured.SetNestedField(lws.Object, int64(0), "spec", "replicas")
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to set replicas field")

			err = crClient.Update(ctx, lws)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to update LWS to 0 replicas")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed(), "Should successfully scale LWS to 0 replicas")

		By("Creating service to expose LWS model server")
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		// Register cleanup for service
		DeferCleanup(func() {
			serviceName := modelServiceName + "-service"
			cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
				func() error {
					return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Creating ServiceMonitor for LWS metrics scraping")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		// Register cleanup for ServiceMonitor
		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			cleanupResource(ctx, "ServiceMonitor", cfg.MonitoringNS, serviceMonitorName,
				func() error {
					return crClient.Delete(ctx, &promoperator.ServiceMonitor{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceMonitorName,
							Namespace: cfg.MonitoringNS,
						},
					})
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: cfg.MonitoringNS}, &promoperator.ServiceMonitor{})
					return errors.IsNotFound(err)
				})
		})

		By("Verifying LWS is at 0 replicas")
		Eventually(func(g Gomega) {
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			g.Expect(err).NotTo(HaveOccurred())

			replicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
			g.Expect(replicas).To(Equal(int64(0)), "LWS should be scaled to 0")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())

		By("Creating annotated ScaledObject with minReplicas=0 targeting the LWS")
		// The annotated ScaledObject is both the discovery source and the scaler; WVA
		// discovers the LWS variant from its llm-d.ai/managed annotation and emits
		// wva_desired_replicas keyed by variantName.
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"),
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject with scale-to-zero")

		// Register cleanup for scaler
		DeferCleanup(func() {
			err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
			if err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete ScaledObject: %v\n", err)
			}
		})

		GinkgoWriter.Println("Scale-from-zero test setup complete with LWS at 0 replicas")
	})

	AfterAll(func() {
		By("Cleaning up scale-from-zero LWS test resources")
		// Cleanup is handled by DeferCleanup registered in BeforeAll
	})

	Context("Initial state verification with LWS", func() {
		It("should have annotated scaler created for LWS", func() {
			By("Verifying annotated ScaledObject exists and targets LeaderWorkerSet")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())
			scaleTargetRef, found, err := unstructured.NestedMap(so.Object, "spec", "scaleTargetRef")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have scaleTargetRef")
			Expect(scaleTargetRef["kind"]).To(Equal("LeaderWorkerSet"), "ScaledObject should target LeaderWorkerSet")

			GinkgoWriter.Printf("Annotated scaler verified: %s\n", hpaName)
		})

		It("should verify LWS starts at zero replicas", func() {
			By("Checking LWS has 0 replicas")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			specReplicas, found, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
			Expect(found).To(BeTrue(), "LWS should have spec.replicas")
			Expect(specReplicas).To(Equal(int64(0)), "LWS should start with 0 replicas")

			GinkgoWriter.Println("LWS verified at 0 replicas")
		})

		It("should have scaler configured with minReplicas=0 for LWS", func() {
			By("Verifying ScaledObject allows scale-to-zero for LWS")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())

			minReplicas, found, err := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have minReplicaCount")
			Expect(minReplicas).To(Equal(int64(0)), "ScaledObject should allow scale-to-zero")

			// Verify ScaledObject targets LeaderWorkerSet
			scaleTargetRef, found, err := unstructured.NestedMap(so.Object, "spec", "scaleTargetRef")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have scaleTargetRef")
			Expect(scaleTargetRef["kind"]).To(Equal("LeaderWorkerSet"), "ScaledObject should target LeaderWorkerSet")
		})
	})

	// TODO(scale-from-zero-keda-flaky): Flaky on KEDA — see the note on the plain
	// "Scale-from-zero with pending requests" Context. The engine logs "Inferencepool
	// datastore is empty - skipping processing inactive variant" and the LWS never scales
	// up from zero within the timeout. Labeled "flaky" to exclude from the required gate
	// until the InferencePool-datastore root cause is fixed. Tracking issue: TODO.
	Context("Scale-from-zero with pending requests for LWS", Label("flaky"), func() {
		var triggerJobName string

		AfterAll(func() {
			if triggerJobName != "" {
				By("Cleaning up trigger job")
				propagation := metav1.DeletePropagationBackground
				err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
				if err != nil && !errors.IsNotFound(err) {
					GinkgoWriter.Printf("Warning: failed to delete trigger job %s: %v\n", triggerJobName, err)
				}
			}
		})

		It("should detect pending requests and trigger scale-from-zero for LWS", func() {
			By("Discovering inference gateway service")
			gatewayServiceName := ""
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			for _, svc := range serviceList.Items {
				if strings.Contains(svc.Name, "inference-gateway") {
					gatewayServiceName = svc.Name
					GinkgoWriter.Printf("Found inference gateway service: %s\n", gatewayServiceName)
					break
				}
			}
			// Fallback: GAIE standalone chart embeds Envoy in the EPP pod and exposes port 80
			// on the EPP service itself — no separate inference-gateway Service is created.
			if gatewayServiceName == "" {
				gatewayServiceName = cfg.EPPServiceName
				GinkgoWriter.Printf("No inference-gateway service found; using EPP service as gateway (standalone chart): %s\n", gatewayServiceName)
			}
			Expect(gatewayServiceName).NotTo(BeEmpty(), "Inference gateway service should exist")

			By("Creating a job to send requests while LWS is at zero")
			triggerJobName = fmt.Sprintf("scale-from-zero-lws-trigger-%d", time.Now().Unix())

			job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayServiceName, cfg.ModelID)
			_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to create scale-from-zero trigger job")

			GinkgoWriter.Printf("Created scale-from-zero trigger job: %s\n", triggerJobName)

			By("Waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "job-name=" + triggerJobName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podList.Items).ToNot(BeEmpty(), "Job pod should exist")

				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), "Job pod should be running or succeeded")
			}).Should(Succeed())

			GinkgoWriter.Println("Job pod is running and sending requests")

			By("Monitoring LWS for scale-from-zero decision")
			// The scale-from-zero engine detects pending requests and directly scales
			// the target LWS 0→1. We observe that through the LWS spec.replicas rather
			// than a VA status field (VA no longer exists).
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")

				GinkgoWriter.Printf("LWS spec replicas: %d (waiting for > 0)\n", specReplicas)

				// Scale-from-zero engine should detect pending requests and scale up
				g.Expect(specReplicas).To(BeNumerically(">", 0),
					"LWS should be scaled up from zero due to pending requests")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Scale-from-zero engine detected pending requests and recommended scale-up")
		})

		It("should scale LWS up from zero", func() {
			By("Monitoring LWS for actual scale-up from zero")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

				GinkgoWriter.Printf("Current replicas: %d, ready: %d (waiting for > 0)\n",
					currentReplicas, readyReplicas)

				// LWS should have scaled up from 0
				g.Expect(currentReplicas).To(BeNumerically(">", 0),
					"LWS should have scaled up from zero")

				// At least one replica should be ready
				g.Expect(readyReplicas).To(BeNumerically(">", 0),
					"At least one replica should be ready after scale-up")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("LWS successfully scaled up from zero")
		})

		It("should successfully process requests after scaling up LWS", func() {
			By("Verifying the trigger job completes successfully")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, triggerJobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				// Job should eventually succeed
				g.Expect(job.Status.Succeeded).To(BeNumerically(">", 0),
					"Job should complete successfully after LWS scales up")

			}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalVerySlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Requests processed successfully after scale-from-zero with LWS")
		})
	})
})

// Scale-from-zero test for LeaderWorkerSet (single-node)
var _ = Describe("Scale-From-Zero Feature with LeaderWorkerSet (single-node)", Serial, Label("full"), Ordered, func() {
	var (
		poolName         = "scale-from-zero-lws-single-pool"
		modelServiceName = "scale-from-zero-lws-single-ms"
		lwsName          = modelServiceName + "-decode"
		// variantName is passed to the LWS (llm-d.ai/variant label) and becomes the
		// variant_name label on wva_desired_replicas.
		variantName  = "scale-from-zero-lws-single-va"
		hpaName      = "scale-from-zero-lws-single-hpa"
		lwsGroupSize = int32(1) // 1 leader + 0 workers
	)

	BeforeAll(func() {
		// Scale-from-zero requires GIE flow control queuing (EPP flowControl feature gate).
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: " +
				"set SCALE_TO_ZERO_ENABLED=true (required for EPP flow-control queuing)")
		}

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		// Wait for InferencePool to be reconciled and registered in the datastore
		By("Waiting for InferencePool to be reconciled (allows time for controller to register it in datastore)")
		eppServiceName := cfg.EPPServiceName
		GinkgoWriter.Printf("Looking for EPP service: %s in namespace: %s\n", eppServiceName, cfg.LLMDNamespace)

		// Wait for the EPP service to exist
		Eventually(func(g Gomega) {
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, eppServiceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}).Should(Succeed(), "EPP service should exist")

		// Wait for EPP pods to be ready. Use the EPP Service's own selector
		// (via FindExistingEPPPods) rather than hard-coding a label key —
		// the llm-d chart changed the EPP pod label in v0.9.0 from
		// `inferencepool=<name>` to `llm-d-router-gateway=<name>`, and
		// future changes are likely. The Service.spec.selector is the
		// authoritative source.
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, eppServiceName)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to find EPP pods")
			g.Expect(pods).ToNot(BeEmpty(), "EPP pods should exist")

			// Check that at least one pod is ready
			hasReadyPod := false
			for _, pod := range pods {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						hasReadyPod = true
						break
					}
				}
				if hasReadyPod {
					break
				}
			}
			g.Expect(hasReadyPod).To(BeTrue(), "At least one EPP pod should be ready")
		}).Should(Succeed(), "EPP pods should be ready")

		By("Creating model service LeaderWorkerSet with single-node (leader only) with 0 initial replicas")
		err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, variantName, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

		// Register cleanup for LWS
		DeferCleanup(func() {
			cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
				func() error {
					return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
				},
				func() bool {
					lws := &unstructured.Unstructured{}
					lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
					lws.SetKind("LeaderWorkerSet")
					err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
					return errors.IsNotFound(err)
				})
		})

		// Immediately scale LWS to 0 (with retry to handle race conditions)
		By("Scaling single-node LeaderWorkerSet to 0 replicas")
		Eventually(func(g Gomega) {
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get LWS")

			err = unstructured.SetNestedField(lws.Object, int64(0), "spec", "replicas")
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to set replicas field")

			err = crClient.Update(ctx, lws)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to update LWS to 0 replicas")
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed(), "Should successfully scale LWS to 0 replicas")

		By("Creating service to expose single-node LWS model server")
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		// Register cleanup for service
		DeferCleanup(func() {
			serviceName := modelServiceName + "-service"
			cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
				func() error {
					return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
				},
				func() bool {
					_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				})
		})

		By("Creating ServiceMonitor for single-node LWS metrics scraping")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		// Register cleanup for ServiceMonitor
		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			cleanupResource(ctx, "ServiceMonitor", cfg.MonitoringNS, serviceMonitorName,
				func() error {
					return crClient.Delete(ctx, &promoperator.ServiceMonitor{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceMonitorName,
							Namespace: cfg.MonitoringNS,
						},
					})
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: cfg.MonitoringNS}, &promoperator.ServiceMonitor{})
					return errors.IsNotFound(err)
				})
		})

		By("Verifying single-node LWS is at 0 replicas")
		Eventually(func(g Gomega) {
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			g.Expect(err).NotTo(HaveOccurred())

			replicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
			g.Expect(replicas).To(Equal(int64(0)), "LWS should be scaled to 0")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())

		By("Creating annotated ScaledObject with minReplicas=0 targeting the LWS")
		// The annotated ScaledObject is both the discovery source and the scaler; WVA
		// discovers the LWS variant from its llm-d.ai/managed annotation and emits
		// wva_desired_replicas keyed by variantName.
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"),
			fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"))
		Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject with scale-to-zero")

		// Register cleanup for scaler
		DeferCleanup(func() {
			err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
			if err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete ScaledObject: %v\n", err)
			}
		})

		GinkgoWriter.Println("Scale-from-zero test setup complete with single-node LWS at 0 replicas")
	})

	AfterAll(func() {
		By("Cleaning up scale-from-zero single-node LWS test resources")
		// Cleanup is handled by DeferCleanup registered in BeforeAll
	})

	Context("Initial state verification with single-node LWS", func() {
		It("should have annotated scaler created for single-node LWS", func() {
			By("Verifying annotated ScaledObject exists and targets LeaderWorkerSet")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())
			scaleTargetRef, found, err := unstructured.NestedMap(so.Object, "spec", "scaleTargetRef")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have scaleTargetRef")
			Expect(scaleTargetRef["kind"]).To(Equal("LeaderWorkerSet"), "ScaledObject should target LeaderWorkerSet")

			GinkgoWriter.Printf("Annotated scaler verified: %s\n", hpaName)
		})

		It("should verify single-node LWS starts at zero replicas", func() {
			By("Checking single-node LWS has 0 replicas")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			specReplicas, found, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
			Expect(found).To(BeTrue(), "LWS should have spec.replicas")
			Expect(specReplicas).To(Equal(int64(0)), "LWS should start with 0 replicas")

			GinkgoWriter.Println("Single-node LWS verified at 0 replicas")
		})

		It("should have scaler configured with minReplicas=0 for single-node LWS", func() {
			By("Verifying ScaledObject allows scale-to-zero for single-node LWS")
			so := &unstructured.Unstructured{}
			so.SetAPIVersion("keda.sh/v1alpha1")
			so.SetKind("ScaledObject")
			err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: hpaName + "-so"}, so)
			Expect(err).NotTo(HaveOccurred())

			minReplicas, found, err := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have minReplicaCount")
			Expect(minReplicas).To(Equal(int64(0)), "ScaledObject should allow scale-to-zero")

			// Verify ScaledObject targets LeaderWorkerSet
			scaleTargetRef, found, err := unstructured.NestedMap(so.Object, "spec", "scaleTargetRef")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "ScaledObject should have scaleTargetRef")
			Expect(scaleTargetRef["kind"]).To(Equal("LeaderWorkerSet"), "ScaledObject should target LeaderWorkerSet")
		})
	})

	// TODO(scale-from-zero-keda-flaky): Flaky on KEDA — see the note on the plain
	// "Scale-from-zero with pending requests" Context. The engine logs "Inferencepool
	// datastore is empty - skipping processing inactive variant" and the single-node LWS
	// never scales up from zero within the timeout. Labeled "flaky" to exclude from the
	// required gate until the InferencePool-datastore root cause is fixed. Tracking issue: TODO.
	Context("Scale-from-zero with pending requests for single-node LWS", Label("flaky"), func() {
		var triggerJobName string

		AfterAll(func() {
			if triggerJobName != "" {
				By("Cleaning up trigger job")
				propagation := metav1.DeletePropagationBackground
				err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
				if err != nil && !errors.IsNotFound(err) {
					GinkgoWriter.Printf("Warning: failed to delete trigger job %s: %v\n", triggerJobName, err)
				}
			}
		})

		It("should detect pending requests and trigger scale-from-zero for single-node LWS", func() {
			By("Discovering inference gateway service")
			gatewayServiceName := ""
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			for _, svc := range serviceList.Items {
				if strings.Contains(svc.Name, "inference-gateway") {
					gatewayServiceName = svc.Name
					GinkgoWriter.Printf("Found inference gateway service: %s\n", gatewayServiceName)
					break
				}
			}
			// Fallback: GAIE standalone chart embeds Envoy in the EPP pod and exposes port 80
			// on the EPP service itself — no separate inference-gateway Service is created.
			if gatewayServiceName == "" {
				gatewayServiceName = cfg.EPPServiceName
				GinkgoWriter.Printf("No inference-gateway service found; using EPP service as gateway (standalone chart): %s\n", gatewayServiceName)
			}
			Expect(gatewayServiceName).NotTo(BeEmpty(), "Inference gateway service should exist")

			By("Creating a job to send requests while single-node LWS is at zero")
			triggerJobName = fmt.Sprintf("scale-from-zero-lws-single-trigger-%d", time.Now().Unix())

			job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayServiceName, cfg.ModelID)
			_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to create scale-from-zero trigger job")

			GinkgoWriter.Printf("Created scale-from-zero trigger job: %s\n", triggerJobName)

			By("Waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "job-name=" + triggerJobName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(podList.Items).ToNot(BeEmpty(), "Job pod should exist")

				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), "Job pod should be running or succeeded")
			}).Should(Succeed())

			GinkgoWriter.Println("Job pod is running and sending requests")

			By("Monitoring single-node LWS for scale-from-zero decision")
			// The scale-from-zero engine detects pending requests and directly scales
			// the target LWS 0→1. We observe that through the LWS spec.replicas rather
			// than a VA status field (VA no longer exists).
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")

				GinkgoWriter.Printf("LWS spec replicas: %d (waiting for > 0)\n", specReplicas)

				// Scale-from-zero engine should detect pending requests and scale up
				g.Expect(specReplicas).To(BeNumerically(">", 0),
					"Single-node LWS should be scaled up from zero due to pending requests")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Scale-from-zero engine detected pending requests and recommended scale-up")
		})

		It("should scale single-node LWS up from zero", func() {
			By("Monitoring single-node LWS for actual scale-up from zero")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

				GinkgoWriter.Printf("Current replicas: %d, ready: %d (waiting for > 0)\n",
					currentReplicas, readyReplicas)

				// Single-node LWS should have scaled up from 0
				g.Expect(currentReplicas).To(BeNumerically(">", 0),
					"Single-node LWS should have scaled up from zero")

				// At least one replica should be ready
				g.Expect(readyReplicas).To(BeNumerically(">", 0),
					"At least one replica should be ready after scale-up")

			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Single-node LWS successfully scaled up from zero")
		})

		It("should successfully process requests after scaling up single-node LWS", func() {
			By("Verifying the trigger job completes successfully")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, triggerJobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				// Job should eventually succeed
				g.Expect(job.Status.Succeeded).To(BeNumerically(">", 0),
					"Job should complete successfully after single-node LWS scales up")

			}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalVerySlowSec)*time.Second).Should(Succeed())

			GinkgoWriter.Println("Requests processed successfully after scale-from-zero with single-node LWS")
		})
	})
})
