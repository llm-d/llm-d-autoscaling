package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kedaEPPGuideScaledObject   = "optimized-baseline-keda-epp"
	kedaEPPGuideDeployment     = "optimized-baseline-nvidia-gpu-vllm-decode"
	kedaEPPGuideAuthentication = "keda-prometheus-auth"
	kedaEPPGuideQueueTrigger   = "epp-queue-size"
	kedaEPPGuideRunTrigger     = "epp-running-requests"
	kedaEPPGuideQueueQuery     = `sum(llm_d_epp_flow_control_queue_size{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})`
	kedaEPPGuideRunQuery       = `sum(llm_d_epp_request_running{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})`
	kedaEPPGuideQueueThreshold = "1"
	kedaEPPGuideRunThreshold   = "16"
	kedaEPPGuideHPAName        = "keda-hpa-optimized-baseline"
	kedaEPPGuideServiceMonitor = "optimized-baseline-epp-monitor"
	kedaEPPGuidePrometheusSvc  = "kube-prometheus-stack-prometheus"
	kedaEPPGuideMinReplicas    = int32(1)
	kedaEPPGuideMaxReplicas    = int32(8)
	kedaEPPGuideStableCount    = 3
	kedaEPPGuideLogTailLines   = int64(300)
	kedaEPPGuideLogSinceSecs   = int64(1800)
	kedaEPPGuideMaxLogStreams  = 2
	kedaEPPGuideCurlImage      = "quay.io/curl/curl:8.11.1@sha256:2db4e6a8fd6a0e4d0db5828b2722cf6db15c3005178a4c65588b903e4784ba11"

	kedaEPPGuideAPITimeout                   = 10 * time.Second
	kedaEPPGuideProbeTimeout                 = 60 * time.Second
	kedaEPPGuideRequestStartupTimeout        = 60 * time.Second
	kedaEPPGuideMetricObservationTimeout     = 90 * time.Second
	kedaEPPGuideStabilizationTimeout         = 180 * time.Second
	kedaEPPGuideScaleTransitionTimeout       = 300 * time.Second
	kedaEPPGuideSimulatorReadinessTimeout    = 120 * time.Second
	kedaEPPGuideRequestLifetime              = 1500 * time.Second
	kedaEPPGuideRequestBearingObservationCap = 20 * time.Minute
	kedaEPPGuidePollInterval                 = 5 * time.Second
	kedaEPPGuideQuickPollInterval            = 2 * time.Second
)

var _ = Describe("KEDA EPP guide contract", Label("keda-epp-guide"), Ordered, func() {
	requestPods := make([]string, 0, 3)

	deleteRequests := func() {
		deleteKEDAEPPGuidePods(requestPods)
	}

	BeforeAll(func() {
		Expect(cfg.DeployWVA).To(BeFalse(), "the direct-KEDA guide spec must run with DEPLOY_WVA=false")
		Expect(cfg.UseSimulator).To(BeTrue(), "the direct-KEDA guide spec uses a deterministic simulator workload")
		Expect(cfg.ModelID).To(Equal("Qwen/Qwen3-32B"), "the simulator and canonical EPP metrics must use the same model")
		GinkgoWriter.Printf(
			"KEDA+EPP request timing: requestBearingObservationCap=%s requestLifetime=%s\n",
			kedaEPPGuideRequestBearingObservationCap,
			kedaEPPGuideRequestLifetime,
		)
		Expect(kedaEPPGuideRequestLifetime).To(
			BeNumerically(">", kedaEPPGuideRequestBearingObservationCap),
			"request lifetime must strictly exceed the complete request-bearing observation cap",
		)

		By("observing the pre-created guide-owned deterministic simulator")
		DeferCleanup(func() {
			deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
			defer cancelDelete()
			err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(
				deleteContext,
				kedaEPPGuideDeployment,
				metav1.DeleteOptions{},
			)
			Expect(err == nil || apierrors.IsNotFound(err)).To(
				BeTrue(),
				"failed to delete guide-owned simulator Deployment: %v",
				err,
			)
		})

		DeferCleanup(deleteRequests)

		By("waiting for the guide-owned simulator to be available")
		Eventually(func(g Gomega) {
			callContext, cancelCall := kedaEPPGuideCallContext()
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(
				callContext,
				kedaEPPGuideDeployment,
				metav1.GetOptions{},
			)
			cancelCall()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
		}, kedaEPPGuideSimulatorReadinessTimeout, kedaEPPGuidePollInterval).Should(Succeed())
	})

	It("observes the canonical guide and performs one bounded 1-to-2 transition", func() {
		scaledObject, authentication := waitForKEDAEPPGuideRuntimeObjects()
		assertKEDAEPPGuideScaledObjectContract(scaledObject)
		assertKEDAEPPGuideAuthenticationContract(authentication)

		By("sending a bounded warmup before requiring lazy EPP metric-series liveness")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuideRequestPods(requestPods)
		assertKEDAEPPGuideSecureTrust()
		waitForKEDAEPPGuidePrometheusTarget()
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideRunQuery, 1)
		waitForKEDAEPPGuidePrometheusValue(kedaEPPGuideQueueQuery, 0)

		By("ending warmup and proving it cannot contaminate the deterministic phases")
		deleteKEDAEPPGuidePods(requestPods)
		waitForKEDAEPPGuidePodsDeleted(requestPods)
		requestPods = requestPods[:0]

		By("requiring KEDA liveness only after both per-model series exist")
		scaledObject = waitForKEDAEPPGuideScaledObjectReady()
		hpa := waitForKEDAEPPGuideHPA(scaledObject)
		Expect(hpa.Name).To(Equal(kedaEPPGuideHPAName))
		Expect(hpa.Namespace).To(Equal(cfg.LLMDNamespace))
		Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
		Expect(*hpa.Spec.MinReplicas).To(Equal(kedaEPPGuideMinReplicas))
		Expect(hpa.Spec.MaxReplicas).To(Equal(kedaEPPGuideMaxReplicas))
		Expect(hpa.Spec.ScaleTargetRef.APIVersion).To(Equal("apps/v1"))
		Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(kedaEPPGuideDeployment))
		assertKEDAEPPGuideHPABehavior(hpa.Spec.Behavior)
		Expect(hpa.Spec.Metrics).To(HaveLen(2))

		queueTarget := resource.MustParse(kedaEPPGuideQueueThreshold)
		runningTarget := resource.MustParse(kedaEPPGuideRunThreshold)
		metricNames := map[string]string{}
		for _, metric := range hpa.Spec.Metrics {
			Expect(metric.Type).To(Equal(autoscalingv2.ExternalMetricSourceType))
			Expect(metric.External).NotTo(BeNil())
			Expect(metric.External.Metric.Selector).NotTo(BeNil())
			Expect(metric.External.Metric.Selector.MatchLabels).To(Equal(map[string]string{
				"scaledobject.keda.sh/name": kedaEPPGuideScaledObject,
			}))
			Expect(metric.External.Metric.Selector.MatchExpressions).To(BeEmpty())
			Expect(metric.External.Target.Type).To(Equal(autoscalingv2.AverageValueMetricType))
			Expect(metric.External.Target.Value).To(BeNil())
			Expect(metric.External.Target.AverageValue).NotTo(BeNil())
			Expect(metric.External.Target.AverageUtilization).To(BeNil())

			switch {
			case metric.External.Target.AverageValue.Cmp(queueTarget) == 0:
				metricNames[kedaEPPGuideQueueTrigger] = metric.External.Metric.Name
			case metric.External.Target.AverageValue.Cmp(runningTarget) == 0:
				metricNames[kedaEPPGuideRunTrigger] = metric.External.Metric.Name
			default:
				Fail(fmt.Sprintf("generated HPA metric %q does not match canonical target 1 or 16", metric.External.Metric.Name))
			}
		}
		Expect(metricNames).To(HaveKey(kedaEPPGuideQueueTrigger))
		Expect(metricNames).To(HaveKey(kedaEPPGuideRunTrigger))
		Expect(metricNames[kedaEPPGuideQueueTrigger]).NotTo(Equal(metricNames[kedaEPPGuideRunTrigger]))
		Eventually(func(g Gomega) {
			value, err := kedaEPPGuideExternalMetric(metricNames[kedaEPPGuideRunTrigger])
			g.Expect(err).NotTo(HaveOccurred(), "running external metric should be live")
			g.Expect(value).To(BeNumerically("==", 0), "running external metric should be reset after warmup")
		}, kedaEPPGuideMetricObservationTimeout, kedaEPPGuidePollInterval).Should(Succeed())

		waitForKEDAEPPGuideBaseline()

		By("starting one bounded request")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuideRequestPods(requestPods)

		By("starting one additional request that remains queued")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())
		waitForKEDAEPPGuidePhaseA(
			metricNames[kedaEPPGuideRunTrigger],
			metricNames[kedaEPPGuideQueueTrigger],
			requestPods,
		)

		By("starting exactly one additional request for the bounded scale transition")
		requestPods = append(requestPods, createKEDAEPPGuideRequestPod())

		waitForKEDAEPPGuideScaleUp(
			metricNames[kedaEPPGuideRunTrigger],
			metricNames[kedaEPPGuideQueueTrigger],
			requestPods,
		)

		By("terminating the bounded stimulus after exact-two evidence")
		deleteRequests()
		assertKEDAOperatorLogsClean()
	})
})

func waitForKEDAEPPGuideRuntimeObjects() (*kedav1alpha1.ScaledObject, *kedav1alpha1.TriggerAuthentication) {
	GinkgoHelper()

	var scaledObject *kedav1alpha1.ScaledObject
	var authentication *kedav1alpha1.TriggerAuthentication
	Eventually(func(g Gomega) {
		scaledObjects := &kedav1alpha1.ScaledObjectList{}
		callContext, cancelCall := kedaEPPGuideCallContext()
		err := crClient.List(callContext, scaledObjects, client.InNamespace(cfg.LLMDNamespace))
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(scaledObjects.Items).To(HaveLen(1), "guide namespace must contain exactly the intended ScaledObject")
		g.Expect(scaledObjects.Items[0].Name).To(Equal(kedaEPPGuideScaledObject))

		authentications := &kedav1alpha1.TriggerAuthenticationList{}
		callContext, cancelCall = kedaEPPGuideCallContext()
		err = crClient.List(callContext, authentications, client.InNamespace(cfg.LLMDNamespace))
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(authentications.Items).To(HaveLen(1), "guide namespace must contain exactly the intended TriggerAuthentication")
		g.Expect(authentications.Items[0].Name).To(Equal(kedaEPPGuideAuthentication))

		scaledObject = scaledObjects.Items[0].DeepCopy()
		authentication = authentications.Items[0].DeepCopy()
	}, kedaEPPGuideRequestStartupTimeout, kedaEPPGuidePollInterval).Should(Succeed())
	return scaledObject, authentication
}

func assertKEDAEPPGuideScaledObjectContract(scaledObject *kedav1alpha1.ScaledObject) {
	GinkgoHelper()

	Expect(scaledObject.Namespace).To(Equal(cfg.LLMDNamespace))
	Expect(scaledObject.Name).To(Equal(kedaEPPGuideScaledObject))
	Expect(scaledObject.Spec.Triggers).To(HaveLen(2))

	serverAddress := fmt.Sprintf(
		"https://%s.%s.svc.cluster.local:9090",
		kedaEPPGuidePrometheusSvc,
		cfg.MonitoringNS,
	)
	expectedTriggers := map[string]map[string]string{
		kedaEPPGuideQueueTrigger: {
			"query":         kedaEPPGuideQueueQuery,
			"threshold":     kedaEPPGuideQueueThreshold,
			"serverAddress": serverAddress,
		},
		kedaEPPGuideRunTrigger: {
			"query":         kedaEPPGuideRunQuery,
			"threshold":     kedaEPPGuideRunThreshold,
			"serverAddress": serverAddress,
		},
	}

	seenTriggers := map[string]bool{}
	for _, trigger := range scaledObject.Spec.Triggers {
		expectedMetadata, expected := expectedTriggers[trigger.Name]
		Expect(expected).To(BeTrue(), "unexpected Prometheus trigger %q on the canonical ScaledObject", trigger.Name)
		Expect(seenTriggers).NotTo(HaveKey(trigger.Name), "canonical trigger %s must be unique", trigger.Name)
		seenTriggers[trigger.Name] = true
		Expect(trigger.Type).To(Equal("prometheus"))
		Expect(trigger.MetricType).To(Equal(autoscalingv2.AverageValueMetricType))
		Expect(trigger.Metadata).To(
			Equal(expectedMetadata),
			"trigger %s metadata must contain only its exact query, threshold, and verified HTTPS endpoint",
			trigger.Name,
		)
		Expect(trigger.AuthenticationRef).To(Equal(&kedav1alpha1.AuthenticationRef{
			Name: kedaEPPGuideAuthentication,
		}), "trigger %s must use the namespaced TriggerAuthentication", trigger.Name)
	}
	Expect(seenTriggers).To(Equal(map[string]bool{
		kedaEPPGuideQueueTrigger: true,
		kedaEPPGuideRunTrigger:   true,
	}))
}

func assertKEDAEPPGuideAuthenticationContract(authentication *kedav1alpha1.TriggerAuthentication) {
	GinkgoHelper()

	Expect(authentication.Namespace).To(Equal(cfg.LLMDNamespace))
	Expect(authentication.Name).To(Equal(kedaEPPGuideAuthentication))
	Expect(authentication.Spec).To(Equal(kedav1alpha1.TriggerAuthenticationSpec{
		SecretTargetRef: []kedav1alpha1.AuthSecretTargetRef{
			kedav1alpha1.AuthSecretTargetRef(kedav1alpha1.AuthTargetRef{
				Parameter: "ca",
				Name:      kedaEPPGuideAuthentication,
				Key:       "ca.crt",
			}),
		},
	}), "TriggerAuthentication must contain only the intended CA Secret reference")
}

func waitForKEDAEPPGuideScaledObjectReady() *kedav1alpha1.ScaledObject {
	GinkgoHelper()

	deadline := time.Now().Add(kedaEPPGuideStabilizationTimeout)
	for time.Now().Before(deadline) {
		scaledObject := &kedav1alpha1.ScaledObject{}
		callContext, cancelCall := kedaEPPGuideCallContext()
		err := crClient.Get(callContext, client.ObjectKey{
			Namespace: cfg.LLMDNamespace,
			Name:      kedaEPPGuideScaledObject,
		}, scaledObject)
		cancelCall()
		if err == nil {
			ready := scaledObject.Status.Conditions.GetReadyCondition()
			if ready.Status == metav1.ConditionTrue {
				return scaledObject
			}
		} else if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		time.Sleep(kedaEPPGuidePollInterval)
	}

	Fail(fmt.Sprintf("ScaledObject %s/%s did not reach Ready=True", cfg.LLMDNamespace, kedaEPPGuideScaledObject))
	return nil
}

func waitForKEDAEPPGuideHPA(scaledObject *kedav1alpha1.ScaledObject) *autoscalingv2.HorizontalPodAutoscaler {
	GinkgoHelper()

	var hpa *autoscalingv2.HorizontalPodAutoscaler
	Eventually(func(g Gomega) {
		callContext, cancelCall := kedaEPPGuideCallContext()
		current, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(
			callContext,
			kedaEPPGuideHPAName,
			metav1.GetOptions{},
		)
		cancelCall()
		g.Expect(err).NotTo(HaveOccurred())

		controllerOwners := make([]metav1.OwnerReference, 0, 1)
		for _, owner := range current.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				controllerOwners = append(controllerOwners, owner)
			}
		}
		g.Expect(controllerOwners).To(HaveLen(1), "generated HPA must have exactly one controller owner")
		owner := controllerOwners[0]
		g.Expect(owner.APIVersion).To(Equal("keda.sh/v1alpha1"))
		g.Expect(owner.Kind).To(Equal("ScaledObject"))
		g.Expect(owner.Name).To(Equal(kedaEPPGuideScaledObject))
		g.Expect(owner.UID).To(Equal(scaledObject.UID), "generated HPA must be owned by the current ScaledObject UID")
		hpa = current
	}, kedaEPPGuideMetricObservationTimeout, kedaEPPGuidePollInterval).Should(Succeed())
	return hpa
}

func waitForKEDAEPPGuideBaseline() {
	GinkgoHelper()

	stable := 0
	Eventually(func(g Gomega) {
		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, kedaEPPGuideHPAName, metav1.GetOptions{})
		cancelHPA()
		g.Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deployment.Spec.Replicas).NotTo(BeNil())
		g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically("<=", 2))
		g.Expect(*deployment.Spec.Replicas).To(BeNumerically("<=", 2))
		g.Expect(deployment.Status.Replicas).To(BeNumerically("<=", 2))
		g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically("<=", 2))

		if hpa.Status.DesiredReplicas == 1 &&
			*deployment.Spec.Replicas == 1 &&
			deployment.Status.ReadyReplicas == 1 {
			stable++
		} else {
			stable = 0
		}
		g.Expect(stable).To(BeNumerically(">=", kedaEPPGuideStableCount))
	}, kedaEPPGuideStabilizationTimeout, kedaEPPGuidePollInterval).Should(Succeed())
}

func createKEDAEPPGuideRequestPod() string {
	GinkgoHelper()

	targetURL := fmt.Sprintf(
		"http://%s.%s.svc.cluster.local:80/v1/chat/completions",
		cfg.EPPServiceName,
		cfg.LLMDNamespace,
	)
	payload := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"bounded deterministic contract probe"}],"max_tokens":8,"temperature":0}`,
		cfg.ModelID,
	)
	callContext, cancelCall := kedaEPPGuideCallContext()
	pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Create(callContext, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "keda-epp-guide-request-",
			Labels: map[string]string{
				"app.kubernetes.io/name": "keda-epp-guide-request",
				"test-resource":          boolTrue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{{
				Name:    "request",
				Image:   kedaEPPGuideCurlImage,
				Command: []string{"sh", "-ec"},
				Args: []string{
					fmt.Sprintf(
						`exec curl --fail --silent --show-error --connect-timeout 10 --max-time %d -H 'Content-Type: application/json' --data-binary "$PAYLOAD" "$TARGET_URL"`,
						int(kedaEPPGuideRequestLifetime/time.Second),
					),
				},
				Env: []corev1.EnvVar{
					{Name: "TARGET_URL", Value: targetURL},
					{Name: "PAYLOAD", Value: payload},
				},
			}},
		},
	}, metav1.CreateOptions{})
	cancelCall()
	Expect(err).NotTo(HaveOccurred())
	return pod.Name
}

func deleteKEDAEPPGuidePods(names []string) {
	GinkgoHelper()

	grace := int64(0)
	for _, name := range names {
		deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(deleteContext, name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
		cancelDelete()
		if err != nil && !apierrors.IsNotFound(err) {
			GinkgoWriter.Printf("WARNING: failed to delete guide pod %s: %v\n", name, err)
		}
	}
}

func waitForKEDAEPPGuidePodsDeleted(names []string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, name := range names {
			callContext, cancelCall := kedaEPPGuideCallContext()
			_, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
			cancelCall()
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "guide pod %s should be deleted, got %v", name, err)
		}
	}, kedaEPPGuideRequestStartupTimeout, kedaEPPGuideQuickPollInterval).Should(Succeed())
}

func assertKEDAEPPGuideSecureTrust() {
	GinkgoHelper()

	for namespace, name := range map[string]string{
		cfg.LLMDNamespace: "keda-prometheus-auth",
		cfg.KEDANamespace: "llmd-prometheus-ca",
	} {
		callContext, cancelCall := kedaEPPGuideCallContext()
		secret, err := k8sClient.CoreV1().Secrets(namespace).Get(callContext, name, metav1.GetOptions{})
		cancelCall()
		Expect(err).NotTo(HaveOccurred())
		Expect(secret.Data).To(HaveLen(1), "%s/%s must contain only the public CA", namespace, name)
		Expect(secret.Data).To(HaveKey("ca.crt"), "%s/%s must contain the public CA", namespace, name)
	}

	callContext, cancelCall := kedaEPPGuideCallContext()
	operator, err := k8sClient.AppsV1().Deployments(cfg.KEDANamespace).Get(callContext, "keda-operator", metav1.GetOptions{})
	cancelCall()
	Expect(err).NotTo(HaveOccurred())
	Expect(operator.Spec.Template.Spec.Containers).NotTo(BeEmpty())
	operatorContainer := operator.Spec.Template.Spec.Containers[0]
	Expect(operatorContainer.Args).To(ContainElement("--ca-dir=/custom/ca"))

	var caMount *corev1.VolumeMount
	for _, mount := range operatorContainer.VolumeMounts {
		if mount.Name == "llmd-prometheus-ca" {
			mount := mount
			caMount = &mount
		}
	}
	Expect(caMount).NotTo(BeNil(), "KEDA operator must mount the public Prometheus CA")
	Expect(caMount.MountPath).To(Equal("/custom/ca"))
	Expect(caMount.ReadOnly).To(BeTrue(), "KEDA operator public CA mount must be read-only")

	var caVolume *corev1.Volume
	for _, volume := range operator.Spec.Template.Spec.Volumes {
		if volume.Name == caMount.Name {
			volume := volume
			caVolume = &volume
		}
	}
	Expect(caVolume).NotTo(BeNil(), "KEDA operator CA mount must have a matching volume")
	Expect(caVolume.Secret).NotTo(BeNil(), "KEDA operator CA volume must be Secret-backed")
	Expect(caVolume.Secret.SecretName).To(Equal("llmd-prometheus-ca"))
}

type kedaEPPGuidePrometheusQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func waitForKEDAEPPGuidePrometheusValue(query string, expected float64) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		value, cardinality, err := kedaEPPGuidePrometheusValue(query)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cardinality).To(Equal(1), "PromQL must return exactly one result rather than treating absence as zero")
		g.Expect(value).To(BeNumerically("==", expected))
	}, kedaEPPGuideMetricObservationTimeout, kedaEPPGuidePollInterval).Should(Succeed())
}

func kedaEPPGuidePrometheusValue(query string) (float64, int, error) {
	prometheusURL := fmt.Sprintf(
		"https://%s.%s.svc.cluster.local:9090/api/v1/query",
		kedaEPPGuidePrometheusSvc,
		cfg.MonitoringNS,
	)
	output, err := runKEDAEPPGuideCurlProbe(
		"prom-query",
		[]string{
			"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
			"--cacert", "/var/run/prometheus-ca/ca.crt", "--get", "--data-urlencode", "query=" + query,
			prometheusURL,
		},
		true,
	)
	if err != nil {
		return 0, 0, err
	}

	response := kedaEPPGuidePrometheusQueryResponse{}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return 0, 0, fmt.Errorf("decode Prometheus query response: %w", err)
	}
	if response.Status != "success" {
		return 0, 0, fmt.Errorf("Prometheus query status is %q", response.Status)
	}
	if len(response.Data.Result) != 1 {
		return 0, len(response.Data.Result), nil
	}
	if len(response.Data.Result[0].Value) != 2 {
		return 0, 1, fmt.Errorf("Prometheus result has %d value fields, want 2", len(response.Data.Result[0].Value))
	}
	var stringValue string
	if err := json.Unmarshal(response.Data.Result[0].Value[1], &stringValue); err != nil {
		return 0, 1, fmt.Errorf("decode Prometheus numeric value: %w", err)
	}
	value, err := strconv.ParseFloat(stringValue, 64)
	if err != nil {
		return 0, 1, fmt.Errorf("parse Prometheus numeric value %q: %w", stringValue, err)
	}
	return value, 1, nil
}

type kedaEPPGuidePrometheusTargetsResponse struct {
	Status string `json:"status"`
	Data   struct {
		ActiveTargets []struct {
			Labels    map[string]string `json:"labels"`
			Health    string            `json:"health"`
			LastError string            `json:"lastError"`
		} `json:"activeTargets"`
	} `json:"data"`
}

func waitForKEDAEPPGuidePrometheusTarget() {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		prometheusURL := fmt.Sprintf(
			"https://%s.%s.svc.cluster.local:9090/api/v1/targets?state=active",
			kedaEPPGuidePrometheusSvc,
			cfg.MonitoringNS,
		)
		output, err := runKEDAEPPGuideCurlProbe(
			"prom-targets",
			[]string{
				"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
				"--cacert", "/var/run/prometheus-ca/ca.crt", prometheusURL,
			},
			true,
		)
		g.Expect(err).NotTo(HaveOccurred())
		response := kedaEPPGuidePrometheusTargetsResponse{}
		g.Expect(json.Unmarshal([]byte(output), &response)).To(Succeed())
		g.Expect(response.Status).To(Equal("success"))

		matched := 0
		for _, target := range response.Data.ActiveTargets {
			if target.Labels["namespace"] == cfg.LLMDNamespace && target.Labels["service"] == cfg.EPPServiceName {
				matched++
				g.Expect(target.Health).To(Equal("up"), "EPP Prometheus target is unhealthy: %s", target.LastError)
			}
		}
		g.Expect(matched).To(Equal(1), "Prometheus must discover exactly one EPP target")
	}, kedaEPPGuideMetricObservationTimeout, kedaEPPGuidePollInterval).Should(Succeed())
}

func runKEDAEPPGuideCurlProbe(nameSuffix string, args []string, mountPrometheusCA bool) (string, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "keda-epp-guide-" + nameSuffix + "-",
			Labels: map[string]string{
				"app.kubernetes.io/name": "keda-epp-guide-probe",
				"test-resource":          boolTrue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: kedaEPPGuideCurlImage,
				Args:  args,
			}},
		},
	}
	if mountPrometheusCA {
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "prometheus-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: "keda-prometheus-auth",
			}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name:      "prometheus-ca",
			MountPath: "/var/run/prometheus-ca",
			ReadOnly:  true,
		}}
	}

	callContext, cancelCall := kedaEPPGuideCallContext()
	created, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Create(callContext, pod, metav1.CreateOptions{})
	cancelCall()
	if err != nil {
		return "", fmt.Errorf("create %s probe: %w", nameSuffix, err)
	}
	defer func() {
		grace := int64(0)
		deleteContext, cancelDelete := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		defer cancelDelete()
		_ = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(deleteContext, created.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
	}()

	deadline := time.Now().Add(kedaEPPGuideProbeTimeout)
	for time.Now().Before(deadline) {
		getContext, cancelGet := kedaEPPGuideCallContext()
		current, getErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(getContext, created.Name, metav1.GetOptions{})
		cancelGet()
		if getErr != nil {
			return "", fmt.Errorf("get %s probe %s: %w", nameSuffix, created.Name, getErr)
		}
		if current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed {
			logContext, cancelLogs := context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
			logs, logErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).GetLogs(created.Name, &corev1.PodLogOptions{}).DoRaw(logContext)
			cancelLogs()
			if logErr != nil {
				return "", fmt.Errorf("read %s probe %s logs: %w", nameSuffix, created.Name, logErr)
			}
			if current.Status.Phase == corev1.PodFailed {
				return string(logs), fmt.Errorf("%s probe %s failed: %s", nameSuffix, created.Name, strings.TrimSpace(string(logs)))
			}
			return string(logs), nil
		}
		time.Sleep(kedaEPPGuideQuickPollInterval)
	}
	return "", fmt.Errorf("%s probe %s did not complete within %s", nameSuffix, created.Name, kedaEPPGuideProbeTimeout)
}

func waitForKEDAEPPGuideRequestPods(names []string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, name := range names {
			callContext, cancelCall := kedaEPPGuideCallContext()
			pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
			cancelCall()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod.Status.Phase).To(
				Equal(corev1.PodRunning),
				"request pod %s should remain active: %s",
				name,
				kedaEPPGuidePodState(pod),
			)
			g.Expect(pod.Status.ContainerStatuses).To(HaveLen(1))
			g.Expect(pod.Status.ContainerStatuses[0].State.Running).NotTo(BeNil(), "request pod %s curl should be running", name)
		}
	}, kedaEPPGuideRequestStartupTimeout, kedaEPPGuideQuickPollInterval).Should(Succeed())
}

func kedaEPPGuideExternalMetric(metricName string) (float64, error) {
	gvr := schema.GroupVersionResource{
		Group:    "external.metrics.k8s.io",
		Version:  "v1beta1",
		Resource: metricName,
	}
	callContext, cancelCall := kedaEPPGuideCallContext()
	values, err := dynamicClient.Resource(gvr).Namespace(cfg.LLMDNamespace).List(callContext, metav1.ListOptions{
		LabelSelector: "scaledobject.keda.sh/name=" + kedaEPPGuideScaledObject,
	})
	cancelCall()
	if err != nil {
		return 0, err
	}
	if len(values.Items) != 1 {
		return 0, fmt.Errorf("external metric %s returned %d items, want exactly one", metricName, len(values.Items))
	}
	value, found, err := nestedMetricValue(values.Items[0].Object)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("external metric %s has no value", metricName)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("external metric %s returned invalid value %q: %w", metricName, value, err)
	}
	return quantity.AsApproximateFloat64(), nil
}

func waitForKEDAEPPGuidePhaseA(runningMetricName, queueMetricName string, requestPods []string) {
	GinkgoHelper()

	deadline := time.Now().Add(kedaEPPGuideStabilizationTimeout)
	stable := 0
	sample := 0

	for time.Now().Before(deadline) {
		sample++

		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, kedaEPPGuideHPAName, metav1.GetOptions{})
		cancelHPA()
		Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())

		values := map[string]int32{
			"hpaDesired":       hpa.Status.DesiredReplicas,
			"deploymentSpec":   *deployment.Spec.Replicas,
			"deploymentActual": deployment.Status.Replicas,
			"deploymentReady":  deployment.Status.ReadyReplicas,
		}
		for name, value := range values {
			if value > 2 {
				Fail(fmt.Sprintf("bounded guide stimulus observed %s=%d (>2)", name, value))
			}
		}

		runningValue, runningErr := kedaEPPGuideExternalMetric(runningMetricName)
		queueValue, queueErr := kedaEPPGuideExternalMetric(queueMetricName)
		requestsActive := kedaEPPGuideRequestsActive(requestPods)
		exactPhaseA := runningErr == nil &&
			queueErr == nil &&
			runningValue == 1 &&
			queueValue == 1 &&
			hpa.Status.DesiredReplicas == 1 &&
			*deployment.Spec.Replicas == 1 &&
			deployment.Status.ReadyReplicas == 1 &&
			requestsActive
		if exactPhaseA {
			stable++
		} else {
			stable = 0
		}

		GinkgoWriter.Printf(
			"guide phase A sample=%d hpa=%d/%d deployment=%d/%d/%d rawRunning=%v runningErr=%v rawQueue=%v queueErr=%v requestsActive=%v stable=%d\n",
			sample,
			hpa.Status.CurrentReplicas,
			hpa.Status.DesiredReplicas,
			*deployment.Spec.Replicas,
			deployment.Status.Replicas,
			deployment.Status.ReadyReplicas,
			runningValue,
			runningErr,
			queueValue,
			queueErr,
			requestsActive,
			stable,
		)

		if stable >= kedaEPPGuideStableCount {
			return
		}
		time.Sleep(kedaEPPGuideQuickPollInterval)
	}

	Fail("guide Phase A did not produce stable one-running/one-queued exact-one state")
}

func nestedMetricValue(object map[string]any) (string, bool, error) {
	value, found := object["value"]
	if !found {
		return "", false, nil
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("metric value has type %T, want string", value)
	}
	return stringValue, true, nil
}

func waitForKEDAEPPGuideScaleUp(runningMetricName, queueMetricName string, requestPods []string) {
	GinkgoHelper()
	Expect(requestPods).To(HaveLen(3), "Phase B must retain exactly three bounded request pods")

	deadline := time.Now().Add(kedaEPPGuideScaleTransitionTimeout)
	phaseBMetricsSeen := false
	stable := 0
	sample := 0

	for time.Now().Before(deadline) {
		sample++

		hpaContext, cancelHPA := kedaEPPGuideCallContext()
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, kedaEPPGuideHPAName, metav1.GetOptions{})
		cancelHPA()
		Expect(err).NotTo(HaveOccurred())
		deploymentContext, cancelDeployment := kedaEPPGuideCallContext()
		deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(deploymentContext, kedaEPPGuideDeployment, metav1.GetOptions{})
		cancelDeployment()
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())

		values := map[string]int32{
			"hpaDesired":       hpa.Status.DesiredReplicas,
			"deploymentSpec":   *deployment.Spec.Replicas,
			"deploymentActual": deployment.Status.Replicas,
			"deploymentReady":  deployment.Status.ReadyReplicas,
		}
		for name, value := range values {
			if value > 2 {
				Fail(fmt.Sprintf("bounded guide stimulus observed %s=%d (>2)", name, value))
			}
		}

		runningValue, runningErr := kedaEPPGuideExternalMetric(runningMetricName)
		queueValue, queueErr := kedaEPPGuideExternalMetric(queueMetricName)
		if runningErr == nil && runningValue > 2 {
			Fail(fmt.Sprintf("bounded guide stimulus observed rawRunning=%v (>2)", runningValue))
		}
		if queueErr == nil && queueValue > 2 {
			Fail(fmt.Sprintf("bounded guide stimulus observed rawQueue=%v (>2)", queueValue))
		}
		requestsActive := kedaEPPGuideRequestsActive(requestPods)
		phaseBMetrics := runningErr == nil && queueErr == nil && runningValue == 1 && queueValue == 2 && requestsActive
		if phaseBMetrics {
			phaseBMetricsSeen = true
		}

		exactTwo := hpa.Status.DesiredReplicas == 2 &&
			*deployment.Spec.Replicas == 2 &&
			deployment.Status.ReadyReplicas == 2 &&
			requestsActive
		if exactTwo {
			stable++
		} else {
			stable = 0
		}

		GinkgoWriter.Printf(
			"guide scale sample=%d hpa=%d/%d deployment=%d/%d/%d rawRunning=%v runningErr=%v rawQueue=%v queueErr=%v requestsActive=%v phaseBMetrics=%v stable=%d\n",
			sample,
			hpa.Status.CurrentReplicas,
			hpa.Status.DesiredReplicas,
			*deployment.Spec.Replicas,
			deployment.Status.Replicas,
			deployment.Status.ReadyReplicas,
			runningValue,
			runningErr,
			queueValue,
			queueErr,
			requestsActive,
			phaseBMetricsSeen,
			stable,
		)

		if phaseBMetricsSeen && stable >= kedaEPPGuideStableCount {
			return
		}
		time.Sleep(kedaEPPGuideQuickPollInterval)
	}

	Fail(fmt.Sprintf(
		"bounded guide stimulus did not produce stable exact-two state (phaseBMetricsSeen=%v)",
		phaseBMetricsSeen,
	))
}

func assertKEDAEPPGuideHPABehavior(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) {
	GinkgoHelper()
	Expect(behavior).NotTo(BeNil(), "canonical HPA behavior must be present")
	assertKEDAEPPGuideHPAScalingRules("scaleUp", behavior.ScaleUp, 0)
	assertKEDAEPPGuideHPAScalingRules("scaleDown", behavior.ScaleDown, 300)
}

func assertKEDAEPPGuideHPAScalingRules(name string, rules *autoscalingv2.HPAScalingRules, stabilizationWindow int32) {
	GinkgoHelper()
	Expect(rules).NotTo(BeNil(), "canonical HPA %s rules must be present", name)
	Expect(rules.StabilizationWindowSeconds).NotTo(BeNil(), "canonical HPA %s stabilization must be explicit", name)
	Expect(*rules.StabilizationWindowSeconds).To(Equal(stabilizationWindow), "canonical HPA %s stabilization drifted", name)
	Expect(rules.SelectPolicy).NotTo(BeNil(), "effective HPA %s select policy must be explicit", name)
	Expect(*rules.SelectPolicy).To(Equal(autoscalingv2.MaxChangePolicySelect), "effective HPA %s select policy drifted", name)
	Expect(rules.Policies).To(HaveLen(1), "canonical HPA %s must retain one policy", name)
	Expect(rules.Policies[0].Type).To(Equal(autoscalingv2.PercentScalingPolicy), "canonical HPA %s policy type drifted", name)
	Expect(rules.Policies[0].Value).To(Equal(int32(100)), "canonical HPA %s policy value drifted", name)
	Expect(rules.Policies[0].PeriodSeconds).To(Equal(int32(15)), "canonical HPA %s policy period drifted", name)
	Expect(rules.Tolerance).To(BeNil(), "canonical HPA %s must not set a per-direction tolerance", name)
}

func kedaEPPGuideRequestsActive(names []string) bool {
	GinkgoHelper()

	allActive := true
	for _, name := range names {
		callContext, cancelCall := kedaEPPGuideCallContext()
		pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(callContext, name, metav1.GetOptions{})
		cancelCall()
		Expect(err).NotTo(HaveOccurred())
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			Fail(fmt.Sprintf("bounded request pod %s exited before scale evidence (phase=%s)", name, pod.Status.Phase))
		}
		if pod.Status.Phase != corev1.PodRunning ||
			len(pod.Status.ContainerStatuses) != 1 ||
			pod.Status.ContainerStatuses[0].State.Running == nil {
			allActive = false
		}
	}
	return allActive
}

func kedaEPPGuideCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
}

func kedaEPPGuidePodState(pod *corev1.Pod) string {
	containerStates := make([]string, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		state := "unknown"
		switch {
		case status.State.Waiting != nil:
			state = "waiting:" + status.State.Waiting.Reason
		case status.State.Running != nil:
			state = "running"
		case status.State.Terminated != nil:
			state = fmt.Sprintf("terminated:%s:%d", status.State.Terminated.Reason, status.State.Terminated.ExitCode)
		}
		containerStates = append(containerStates, status.Name+"="+state)
	}
	return fmt.Sprintf("phase=%s containers=[%s]", pod.Status.Phase, strings.Join(containerStates, ","))
}

func assertKEDAOperatorLogsClean() {
	GinkgoHelper()

	operatorLogs, err := readKEDAEPPGuideOperatorLogs()
	Expect(err).NotTo(HaveOccurred())
	Expect(operatorLogs).NotTo(BeEmpty(), "KEDA operator pod should exist before guide validation")

	for _, operatorLog := range operatorLogs {
		lower := strings.ToLower(operatorLog.content)
		for _, pattern := range []string{"x509", "unknown authority"} {
			if strings.Contains(lower, pattern) {
				Fail(fmt.Sprintf("KEDA operator logs from %s contain %q: %s", operatorLog.source, pattern, strings.TrimSpace(operatorLog.content)))
			}
		}
	}
}

type kedaEPPGuideOperatorLog struct {
	source  string
	content string
}

func readKEDAEPPGuideOperatorLogs() ([]kedaEPPGuideOperatorLog, error) {
	listContext, cancelList := context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
	pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(listContext, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=keda-operator",
	})
	cancelList()
	if err != nil {
		return nil, fmt.Errorf("list KEDA operator pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no KEDA operator pods found in namespace %s", cfg.KEDANamespace)
	}

	operatorLogs := make([]kedaEPPGuideOperatorLog, 0, kedaEPPGuideMaxLogStreams)
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if len(operatorLogs) >= kedaEPPGuideMaxLogStreams {
				return operatorLogs, nil
			}
			logContext, cancelLogs := context.WithTimeout(ctx, kedaEPPGuideAPITimeout)
			logs, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container:    container.Name,
				SinceSeconds: ptr.To(kedaEPPGuideLogSinceSecs),
				TailLines:    ptr.To(kedaEPPGuideLogTailLines),
			}).DoRaw(logContext)
			cancelLogs()
			if err != nil {
				return nil, fmt.Errorf("read KEDA operator logs for %s/%s: %w", pod.Name, container.Name, err)
			}
			operatorLogs = append(operatorLogs, kedaEPPGuideOperatorLog{
				source:  pod.Name + "/" + container.Name,
				content: string(logs),
			})
		}
	}
	return operatorLogs, nil
}

func dumpKEDAEPPGuideOperatorLogs() {
	operatorLogs, err := readKEDAEPPGuideOperatorLogs()
	if err != nil {
		GinkgoWriter.Printf("Failed to collect KEDA operator logs: %v\n", err)
		return
	}
	for _, operatorLog := range operatorLogs {
		GinkgoWriter.Printf("\n=== KEDA operator logs: %s ===\n%s\n", operatorLog.source, operatorLog.content)
	}
}

func dumpKEDAEPPGuideDiagnostics() {
	GinkgoWriter.Println("\n=== KEDA+EPP guide focused diagnostics ===")
	dumpKEDAEPPGuideOperatorLogs()

	for _, namespace := range []string{cfg.LLMDNamespace, cfg.MonitoringNS, cfg.KEDANamespace} {
		listContext, cancelList := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		pods, err := k8sClient.CoreV1().Pods(namespace).List(listContext, metav1.ListOptions{})
		cancelList()
		if err != nil {
			GinkgoWriter.Printf("pods namespace=%s error=%v\n", namespace, err)
		} else {
			for _, pod := range pods.Items {
				GinkgoWriter.Printf("pod namespace=%s name=%s %s\n", namespace, pod.Name, kedaEPPGuidePodState(&pod))
			}
		}

		eventContext, cancelEvents := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
		events, err := k8sClient.CoreV1().Events(namespace).List(eventContext, metav1.ListOptions{})
		cancelEvents()
		if err != nil {
			GinkgoWriter.Printf("events namespace=%s error=%v\n", namespace, err)
		} else {
			for _, event := range events.Items {
				GinkgoWriter.Printf(
					"event namespace=%s object=%s/%s reason=%s message=%s\n",
					namespace,
					event.InvolvedObject.Kind,
					event.InvolvedObject.Name,
					event.Reason,
					event.Message,
				)
			}
		}
	}

	deploymentContext, cancelDeployments := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	deployments, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(deploymentContext, metav1.ListOptions{})
	cancelDeployments()
	if err != nil {
		GinkgoWriter.Printf("deployments namespace=%s error=%v\n", cfg.LLMDNamespace, err)
	} else {
		for _, deployment := range deployments.Items {
			desired := int32(-1)
			if deployment.Spec.Replicas != nil {
				desired = *deployment.Spec.Replicas
			}
			GinkgoWriter.Printf(
				"deployment namespace=%s name=%s desired=%d current=%d ready=%d available=%d\n",
				cfg.LLMDNamespace,
				deployment.Name,
				desired,
				deployment.Status.Replicas,
				deployment.Status.ReadyReplicas,
				deployment.Status.AvailableReplicas,
			)
		}
	}

	scaledObject := &kedav1alpha1.ScaledObject{}
	soContext, cancelSO := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	err = crClient.Get(soContext, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: kedaEPPGuideScaledObject}, scaledObject)
	cancelSO()
	if err != nil {
		GinkgoWriter.Printf("scaledobject namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideScaledObject, err)
	} else {
		GinkgoWriter.Printf(
			"scaledobject namespace=%s name=%s hpa=%s conditions=%+v triggers=%+v\n",
			cfg.LLMDNamespace,
			scaledObject.Name,
			scaledObject.Status.HpaName,
			scaledObject.Status.Conditions,
			scaledObject.Spec.Triggers,
		)
	}

	hpaContext, cancelHPA := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(hpaContext, kedaEPPGuideHPAName, metav1.GetOptions{})
	cancelHPA()
	if err != nil {
		GinkgoWriter.Printf("hpa namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideHPAName, err)
	} else {
		GinkgoWriter.Printf("hpa namespace=%s name=%s spec=%+v status=%+v owners=%+v\n", cfg.LLMDNamespace, hpa.Name, hpa.Spec, hpa.Status, hpa.OwnerReferences)
	}

	serviceMonitor := &promoperator.ServiceMonitor{}
	smContext, cancelSM := context.WithTimeout(context.Background(), kedaEPPGuideAPITimeout)
	err = crClient.Get(smContext, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: kedaEPPGuideServiceMonitor}, serviceMonitor)
	cancelSM()
	if err != nil {
		GinkgoWriter.Printf("servicemonitor namespace=%s name=%s error=%v\n", cfg.LLMDNamespace, kedaEPPGuideServiceMonitor, err)
	} else {
		GinkgoWriter.Printf("servicemonitor namespace=%s name=%s selector=%+v endpoints=%+v\n", cfg.LLMDNamespace, serviceMonitor.Name, serviceMonitor.Spec.Selector, serviceMonitor.Spec.Endpoints)
	}

	directMetrics, directErr := runKEDAEPPGuideCurlProbe(
		"diagnostic-metrics",
		[]string{
			"--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
			fmt.Sprintf("http://%s.%s.svc.cluster.local:9090/metrics", cfg.EPPServiceName, cfg.LLMDNamespace),
		},
		false,
	)
	if directErr != nil {
		GinkgoWriter.Printf("direct EPP metrics error=%v\n", directErr)
	} else {
		for _, line := range strings.Split(directMetrics, "\n") {
			if strings.HasPrefix(line, "llm_d_epp_flow_control_queue_size{") || strings.HasPrefix(line, "llm_d_epp_request_running{") {
				GinkgoWriter.Printf("direct EPP metric %s\n", line)
			}
		}
	}

	for name, query := range map[string]string{
		"queue":   kedaEPPGuideQueueQuery,
		"running": kedaEPPGuideRunQuery,
	} {
		value, cardinality, queryErr := kedaEPPGuidePrometheusValue(query)
		GinkgoWriter.Printf("Prometheus query=%s cardinality=%d value=%v error=%v\n", name, cardinality, value, queryErr)
	}
	GinkgoWriter.Println("=== end KEDA+EPP guide focused diagnostics ===")
}
