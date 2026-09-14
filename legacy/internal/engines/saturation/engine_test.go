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

package saturation

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	utils "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// newManagedHPA builds a WVA-managed HorizontalPodAutoscaler targeting the given
// Deployment. The engine discovers variants by synthesizing them from such
// annotated HPAs.
func newManagedHPA(name, namespace, targetDeployment, modelID string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				annotations.Managed:     "true",
				annotations.ModelID:     modelID,
				annotations.VariantCost: "10.0",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetDeployment,
			},
			MaxReplicas: 2,
		},
	}
}

var _ = Describe("Saturation Engine", func() {

	// Use config.SystemNamespace() instead of local function

	// CreateServiceClassConfigMap creates a service class ConfigMap for testing
	var CreateServiceClassConfigMap = func(controllerNamespace string, models ...string) *v1.ConfigMap {
		data := map[string]string{}

		// Build premium.yaml with all models
		var premiumBuilder, freemiumBuilder strings.Builder

		for _, model := range models {
			fmt.Fprintf(&premiumBuilder, "  - model: %s\n    slo-tpot: 24\n    slo-ttft: 500\n", model)
			fmt.Fprintf(&freemiumBuilder, "  - model: %s\n    slo-tpot: 200\n    slo-ttft: 2000\n", model)
		}
		premiumModels := premiumBuilder.String()
		freemiumModels := freemiumBuilder.String()

		data["premium.yaml"] = "name: Premium\npriority: 1\ndata:\n" + premiumModels

		data["freemium.yaml"] = "name: Freemium\npriority: 10\ndata:\n" + freemiumModels

		return &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "service-classes-config",
				Namespace: controllerNamespace,
			},
			Data: data,
		}
	}

	Context("When handling multiple VariantAutoscalings", func() {
		const totalVAs = 3
		const configMapName = "wva-manager-config"
		var configMapNamespace = config.SystemNamespace()

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			// Use custom configmap creation function
			modelNames := make([]string, 0, totalVAs)
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments")
			for i := range totalVAs {
				modelID := fmt.Sprintf("model-%d-model-%d", i, i)
				name := fmt.Sprintf("multi-test-resource-%d", i)

				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": name},
						},
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": name},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "test-container",
										Image: "registry.k8s.io/pause:3.9",
										Ports: []v1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				// Variants are discovered from annotated HPAs targeting the Deployment.
				Expect(k8sClient.Create(ctx, newManagedHPA(name, "default", name, modelID))).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			var hpaList autoscalingv2.HorizontalPodAutoscalerList
			err = k8sClient.List(ctx, &hpaList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list HPAs")

			var deploymentList appsv1.DeploymentList
			err = k8sClient.List(ctx, &deploymentList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

			// Clean up all deployments
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "multi-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all managed HPAs
			for i := range hpaList.Items {
				err = k8sClient.Delete(ctx, &hpaList.Items[i])
				Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete HPA")
			}
		})

		It("should run the optimization loop over annotation-discovered variants", func() {
			By("Using a working mock Prometheus API with sample data")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			// Initialize MetricsCollector with mock Prometheus API
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {},
			})
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			By("Performing optimization loop")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Prometheus was queried for the discovered variants")
			// Each annotated HPA is synthesized into an in-memory variant; the engine
			// queries Prometheus for saturation metrics for each. Non-empty QueryCallCounts
			// proves the variants were discovered and fed through the saturation pipeline.
			Expect(mockPromAPI.QueryCallCounts).NotTo(BeEmpty(),
				"engine should have queried Prometheus for at least one discovered variant")
		})

		It("refuses the queueing-model path loudly and holds, rather than silently running V2/V1", func() {
			By("Using a mock Prometheus API that records every query")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}

			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			// Saturation config present (as in the V2 test above) plus a
			// queueing-model ConfigMap. Presence of the QM "default" entry routes the
			// dispatch to the refusal path regardless of the saturation analyzerName.
			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {},
			})
			testConfig.UpdateQMAnalyzerConfig(map[string]domain.QueueingModelScalingConfig{
				"default": {},
			})
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			By("Running the optimization loop with an observed logger")
			// A Ginkgo It cannot call zapObserverCtx (it needs *testing.T), so build
			// the observed-logger context inline.
			core, recorded := observer.New(zapcore.InfoLevel)
			observedCtx := logr.NewContext(ctx, zapr.NewLogger(zap.New(core)))
			err := engine.optimize(observedCtx)

			By("Verifying the loop completed without error (models are held, not dropped)")
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the queueing-model path was refused, not silently dispatched")
			var refused bool
			for _, e := range recorded.All() {
				if strings.Contains(e.Message, "refusing to dispatch the queueing-model path") {
					refused = true
					break
				}
			}
			Expect(refused).To(BeTrue(), "the engine must log the explicit queueing-model refusal")

			By("Verifying no fall-through to the saturation (V2) or V1 path")
			// The refusal path never calls prepareModelData, so it issues no
			// Prometheus queries. A non-empty QueryCallCounts would mean the engine
			// silently ran V2/V1 instead of refusing.
			Expect(mockPromAPI.QueryCallCounts).To(BeEmpty(),
				"a refused queueing-model cycle must not query Prometheus (no V2/V1 fall-through)")

			By("Verifying the hold is surfaced to operators as a Warning event")
			// The held variants are synthesized in-memory, so their
			// TypeOptimizationReady=False/OptimizationRefused condition is set on a
			// copy with no API object to read back. The event is the observable
			// half of the same signal, and the one that must not regress to
			// silence: without it, a cluster whose autoscaling has stopped
			// entirely looks identical to a healthy idle one.
			var refusedEvent string
			for len(fakeRecorder.Events) > 0 {
				e := <-fakeRecorder.Events
				if strings.Contains(e, constants.K8SEventOptimizationRefused) {
					refusedEvent = e
					break
				}
			}
			Expect(refusedEvent).To(ContainSubstring(v1.EventTypeWarning),
				"the refusal must raise a Warning event on the held variants")
		})
	})

	Context("convertSaturationTargetsToDecisions", func() {
		BeforeEach(func() {
			logging.NewTestLogger()
		})

		It("should include ActionNoChange decisions in the result", func() {
			By("Creating test data where target equals current replicas")
			saturationTargets := map[string]int{
				"variant-a": 3,
				"variant-b": 5,
				"variant-c": 2,
			}

			saturationAnalysis := &domain.ModelSaturationAnalysis{
				ModelID:   "test-model",
				Namespace: "test-ns",
				VariantAnalyses: []domain.VariantSaturationAnalysis{
					{VariantName: "variant-a", AcceleratorName: "A100", Cost: 10.0},
					{VariantName: "variant-b", AcceleratorName: "A100", Cost: 10.0},
					{VariantName: "variant-c", AcceleratorName: "A100", Cost: 10.0},
				},
			}

			// Populate Role to verify it propagates to VariantDecision in the
			// P/D-aware path (empty, prefill, decode cover the common cases).
			variantStates := []domain.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 3, DesiredReplicas: 3, Role: ""},
				{VariantName: "variant-b", CurrentReplicas: 3, DesiredReplicas: 3, Role: "prefill"},
				{VariantName: "variant-c", CurrentReplicas: 2, DesiredReplicas: 2, Role: "decode"},
			}

			By("Converting saturation targets to decisions")
			sourceRegistry := source.NewSourceRegistry()
			sourceRegistry.Register("prometheus", source.NewNoOpSource()) // nolint:errcheck
			// Create minimal test config
			testConfig := config.NewTestConfig()
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))
			decisions := engine.convertSaturationTargetsToDecisions(context.Background(), saturationTargets, saturationAnalysis, variantStates)

			By("Verifying all variants are included in decisions")
			Expect(decisions).To(HaveLen(3), "All 3 variants should have decisions including ActionNoChange")

			By("Verifying ActionNoChange decisions are present")
			decisionMap := make(map[string]domain.VariantDecision)
			for _, d := range decisions {
				decisionMap[d.VariantName] = d
			}

			Expect(decisionMap).To(HaveKey("variant-a"))
			Expect(decisionMap["variant-a"].Action).To(Equal(domain.ActionNoChange))
			Expect(decisionMap["variant-b"].Action).To(Equal(domain.ActionScaleUp))
			Expect(decisionMap["variant-c"].Action).To(Equal(domain.ActionNoChange))

			By("Verifying Role propagates from VariantReplicaState to VariantDecision")
			Expect(decisionMap["variant-a"].Role).To(Equal(""), "empty role must pass through unchanged")
			Expect(decisionMap["variant-b"].Role).To(Equal("prefill"))
			Expect(decisionMap["variant-c"].Role).To(Equal("decode"))
		})
	})

	Context("Source Infrastructure Optimization Tests", func() {
		const totalVAs = 3
		const configMapName = "wva-manager-config"
		var configMapNamespace = config.SystemNamespace()
		var sourceRegistry *source.SourceRegistry
		var mockPromAPI *testutils.MockPromAPI

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("Using a working mock Prometheus API with no data")
			mockPromAPI = &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			By("creating the source registry with mock Prometheus API")
			sourceRegistry = source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			By("creating the required configmaps")
			// Use custom configmap creation function
			modelNames := make([]string, 0, totalVAs)
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("v2-model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments for source infrastructure tests")
			for i := range totalVAs {
				modelID := fmt.Sprintf("v2-model-%d-model-%d", i, i)
				name := fmt.Sprintf("v2-test-resource-%d", i)

				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": name},
						},
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": name},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "test-container",
										Image: "registry.k8s.io/pause:3.9",
										Ports: []v1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				// Variants are discovered from annotated HPAs targeting the Deployment.
				Expect(k8sClient.Create(ctx, newManagedHPA(name, "default", name, modelID))).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			var hpaList autoscalingv2.HorizontalPodAutoscalerList
			err = k8sClient.List(ctx, &hpaList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list HPAs")

			var deploymentList appsv1.DeploymentList
			err = k8sClient.List(ctx, &deploymentList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

			// Clean up all deployments created by v2 tests
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "v2-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all managed HPAs created by v2 tests
			for i := range hpaList.Items {
				hpa := &hpaList.Items[i]
				if strings.HasPrefix(hpa.Name, "v2-test-resource") {
					err = k8sClient.Delete(ctx, hpa)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete HPA")
				}
			}

		})

		It("should successfully run optimization with source infrastructure", func() {

			// Initialize legacy MetricsCollector for non-saturation metrics
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {},
			})
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			By("Performing optimization loop with source infrastructure")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the annotated variants were discovered")
			// Variants are synthesized from the annotated HPAs; confirm all test
			// HPAs are present so the loop had the expected discovery surface.
			var hpaList autoscalingv2.HorizontalPodAutoscalerList
			err = k8sClient.List(ctx, &hpaList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())

			testVariants := 0
			for _, hpa := range hpaList.Items {
				if strings.HasPrefix(hpa.Name, "v2-test-resource") && hpa.DeletionTimestamp.IsZero() {
					testVariants++
				}
			}
			Expect(testVariants).To(Equal(totalVAs), "Expected all test variants to be present")
		})

	})

	Context("Optimizer selection based on EnableLimiter", func() {
		const testName = "optimizer-toggle-test"
		const modelID = "test-model"

		BeforeEach(func() {
			logging.NewTestLogger()

			By("Creating test deployment and VA")
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: utils.Ptr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": testName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": testName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, d)).To(Succeed())

			// Variant is discovered from an annotated HPA targeting the Deployment.
			Expect(k8sClient.Create(ctx, newManagedHPA(testName, "default", testName, modelID))).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up test resources")
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
			}
			err := k8sClient.Delete(ctx, d)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
			}
			err = k8sClient.Delete(ctx, hpa)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should update e.optimizer when EnableLimiter toggles", func() {
			By("Creating engine with EnableLimiter=false")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {
					AnalyzerName:  domain.SaturationAnalyzerName, // V2 path
					EnableLimiter: false,
				},
			})
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			By("Running optimize() with EnableLimiter=false")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("cost-aware"),
				"Expected CostAwareOptimizer when EnableLimiter=false")

			By("Updating config to EnableLimiter=true")
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {
					AnalyzerName:  domain.SaturationAnalyzerName, // V2 path
					EnableLimiter: true,
				},
			})

			By("Running optimize() with EnableLimiter=true")
			err = engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("greedy-by-score"),
				"Expected GreedyByScoreOptimizer when EnableLimiter=true")

			By("Updating config back to EnableLimiter=false")
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {
					AnalyzerName:  domain.SaturationAnalyzerName, // V2 path
					EnableLimiter: false,
				},
			})

			By("Running optimize() with EnableLimiter=false again")
			err = engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("cost-aware"),
				"Expected CostAwareOptimizer when EnableLimiter=false (second toggle)")
		})
	})

})
