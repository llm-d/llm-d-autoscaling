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

package controller

import (
	"context"
	"errors"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testutil"
)

const annotationValueTrue = "true"

var _ = Describe("ConfigMapReconciler", func() {
	var (
		reconciler      *ConfigMapReconciler
		ctx             context.Context
		cfg             *config.Config
		ds              datastore.Datastore
		systemNamespace string
		testNamespace   string
	)

	BeforeEach(func() {
		setup := setupConfigMapReconcilerTest("workload-variant-autoscaler-system", "test-namespace")
		ctx = setup.ctx //nolint:fatcontext // shared across BeforeEach/It blocks
		cfg = setup.cfg
		ds = setup.ds
		reconciler = setup.reconciler
		systemNamespace = setup.systemNamespace
		testNamespace = setup.testNamespace
	})

	Context("Reconcile - Global ConfigMaps", func() {
		It("should reconcile global saturation ConfigMap successfully", func() {
			By("Creating a global saturation ConfigMap")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: systemNamespace,
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.75\nqueueLengthThreshold: 5\nkvSpareTrigger: 0.10\nqueueSpareTrigger: 3",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())
			currentCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, currentCM)).To(Succeed())
			currentCM.Data = cm.Data
			Expect(k8sClient.Update(ctx, currentCM)).To(Succeed())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying the config was updated")
			satConfigMap := cfg.SaturationConfig()
			Expect(satConfigMap).NotTo(BeNil())
			satConfig, exists := satConfigMap["default"]
			Expect(exists).To(BeTrue())
			Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.75, 0.01))
			Expect(satConfig.QueueLengthThreshold).To(BeNumerically("~", 5.0, 0.01))
		})

		It("should reconcile global scale-to-zero ConfigMap successfully", func() {
			By("Creating a global scale-to-zero ConfigMap")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DefaultScaleToZeroConfigMapName,
					Namespace: systemNamespace,
				},
				Data: map[string]string{
					"default": "enable_scale_to_zero: true\nretention_period: 5m",
					"model1":  "model_id: model1\nenable_scale_to_zero: true\nretention_period: 10m",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())
			currentCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, currentCM)).To(Succeed())
			currentCM.Data = cm.Data
			Expect(k8sClient.Update(ctx, currentCM)).To(Succeed())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying the config was updated")
			s2zConfigMap := cfg.ScaleToZeroConfig()
			Expect(s2zConfigMap).NotTo(BeNil())
			model1Config, exists := s2zConfigMap["model1"]
			Expect(exists).To(BeTrue())
			Expect(model1Config.EnableScaleToZero).NotTo(BeNil())
			Expect(*model1Config.EnableScaleToZero).To(BeTrue())
			Expect(model1Config.RetentionPeriod).To(Equal("10m"))
		})

	})

	Context("Reconcile - Throughput Analyzer Registration Divergence", func() {
		const satConfigBase = "kvCacheThreshold: 0.75\nqueueLengthThreshold: 5\n" +
			"kvSpareTrigger: 0.10\nqueueSpareTrigger: 3\n"
		const satConfigTAEnabled = satConfigBase + "analyzers:\n  - name: throughput\n"
		const satConfigTAOmitted = satConfigBase + "analyzers:\n  - name: saturation\n"

		reconcileGlobalSaturationConfigMap := func(data string) {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: systemNamespace,
				},
				Data: map[string]string{"default": data},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())
			currentCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, currentCM)).To(Succeed())
			currentCM.Data = cm.Data
			Expect(k8sClient.Update(ctx, currentCM)).To(Succeed())

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		}

		// Divergence detection runs only once the registration decision has been
		// frozen, which the controller does immediately after the initial
		// ConfigMap bootstrap completes. Every runtime reconcile executes in that
		// state, so these cases mark bootstrap complete to mirror production.
		Context("after the initial bootstrap has completed", func() {
			BeforeEach(func() {
				reconciler.Config.MarkConfigMapsBootstrapComplete()
			})

			It("emits a Warning event when registered=false and the live global config enables TA", func() {
				reconciler.ThroughputRegistered = false

				reconcileGlobalSaturationConfigMap(satConfigTAEnabled)

				fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
				Expect(ok).To(BeTrue())
				var got string
				Eventually(fakeRecorder.Events).Should(Receive(&got))
				Expect(got).To(ContainSubstring(constants.K8SEventThroughputAnalyzerRestartRequired))
				Expect(got).To(ContainSubstring("Warning"))
			})

			It("emits a Warning event when registered=true and the live global config omits TA", func() {
				reconciler.ThroughputRegistered = true

				reconcileGlobalSaturationConfigMap(satConfigTAOmitted)

				fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
				Expect(ok).To(BeTrue())
				var got string
				Eventually(fakeRecorder.Events).Should(Receive(&got))
				Expect(got).To(ContainSubstring(constants.K8SEventThroughputAnalyzerRestartRequired))
				Expect(got).To(ContainSubstring("Warning"))
			})

			It("emits no event when registered=true and the live global config enables TA (match)", func() {
				reconciler.ThroughputRegistered = true

				reconcileGlobalSaturationConfigMap(satConfigTAEnabled)

				fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
				Expect(ok).To(BeTrue())
				Consistently(fakeRecorder.Events).ShouldNot(Receive())
			})

			It("emits no event when registered=false and the live global config omits TA (match)", func() {
				reconciler.ThroughputRegistered = false

				reconcileGlobalSaturationConfigMap(satConfigTAOmitted)

				fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
				Expect(ok).To(BeTrue())
				Consistently(fakeRecorder.Events).ShouldNot(Receive())
			})

			It("emits no event for a namespace-local saturation ConfigMap even when it diverges", func() {
				By("Tracking the test namespace in datastore")
				ds.NamespaceTrack("VariantAutoscaling", "test-va", testNamespace)

				reconciler.ThroughputRegistered = false

				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      config.SaturationConfigMapName(),
						Namespace: testNamespace,
					},
					Data: map[string]string{"default": satConfigTAEnabled},
				}
				Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())
				DeferCleanup(func() {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cm))).To(Succeed())
				})
				currentCM := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, currentCM)).To(Succeed())
				currentCM.Data = cm.Data
				Expect(k8sClient.Update(ctx, currentCM)).To(Succeed())

				req := ctrl.Request{
					NamespacedName: types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace},
				}
				result, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
				Expect(ok).To(BeTrue())
				Consistently(fakeRecorder.Events).ShouldNot(Receive())
			})

			It("does not panic when the Recorder is nil and the live global config diverges", func() {
				// The Recorder is optional; warnIfThroughputRegistrationDiverged
				// must guard it. Reconcile must complete without panicking on a
				// divergence even when no Recorder is wired. Reaching the end of
				// the divergent reconcile below without a panic is the assertion.
				reconciler.Recorder = nil
				reconciler.ThroughputRegistered = false

				reconcileGlobalSaturationConfigMap(satConfigTAEnabled)
			})
		})

		It("emits no event during the initial bootstrap pass, before the decision is frozen", func() {
			// Bootstrap is deliberately NOT marked complete here: this reproduces
			// the synchronous bootstrap reconcile, where ThroughputRegistered is
			// still its zero value. Without the bootstrap gate this diverges
			// (want=true vs registered=false) and would emit a spurious "restart
			// required" warning on every healthy startup.
			reconciler.ThroughputRegistered = false

			reconcileGlobalSaturationConfigMap(satConfigTAEnabled)

			fakeRecorder, ok := reconciler.Recorder.(*record.FakeRecorder)
			Expect(ok).To(BeTrue())
			Consistently(fakeRecorder.Events).ShouldNot(Receive())
		})
	})

	Context("Reconcile - Namespace-Local ConfigMaps", func() {
		BeforeEach(func() {
			By("Tracking the test namespace in datastore")
			// Simulate namespace tracking by creating a VA
			ds.NamespaceTrack("VariantAutoscaling", "test-va", testNamespace)
		})

		It("should reconcile namespace-local saturation ConfigMap", func() {
			By("Creating a namespace-local saturation ConfigMap")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.60\nqueueLengthThreshold: 10\nkvSpareTrigger: 0.15\nqueueSpareTrigger: 5",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying the namespace-local config was updated")
			satConfigMap := cfg.SaturationConfigForNamespace(testNamespace)
			Expect(satConfigMap).NotTo(BeNil())
			satConfig, exists := satConfigMap["default"]
			Expect(exists).To(BeTrue())
			Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.60, 0.01))
			Expect(satConfig.QueueLengthThreshold).To(BeNumerically("~", 10.0, 0.01))
		})

		It("should ignore ConfigMaps from untracked namespaces", func() {
			By("Creating a ConfigMap in untracked namespace")
			untrackedNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "untracked-namespace",
				},
			}
			Expect(k8sClient.Create(ctx, untrackedNS)).To(Succeed())

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: "untracked-namespace",
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.99\nqueueLengthThreshold: 99",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying the config was NOT updated with untracked namespace values")
			// Should fallback to global config, not use the untracked namespace values
			satConfigMap := cfg.SaturationConfigForNamespace("untracked-namespace")
			Expect(satConfigMap).NotTo(BeNil())
			if satConfig, exists := satConfigMap["default"]; exists {
				Expect(satConfig.KvCacheThreshold).NotTo(BeNumerically("~", 0.99, 0.01))
			}
		})
	})

	Context("Reconcile - ConfigMap Deletion", func() {
		BeforeEach(func() {
			By("Tracking the test namespace")
			ds.NamespaceTrack("VariantAutoscaling", "test-va", testNamespace)
		})

		It("should handle deletion of namespace-local ConfigMap", func() {
			By("Creating and reconciling a namespace-local ConfigMap")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.60\nqueueLengthThreshold: 10\nkvSpareTrigger: 0.15\nqueueSpareTrigger: 5",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).NotTo(HaveOccurred())

			// Reconcile to apply config
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the ConfigMap")
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())

			By("Reconciling the deletion")
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, req)
				return err == nil
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			By("Verifying namespace-local config was cleaned up")
			// Should now fallback to global config
			satConfigMap := cfg.SaturationConfigForNamespace(testNamespace)
			Expect(satConfigMap).NotTo(BeNil())
			if satConfig, exists := satConfigMap["default"]; exists {
				Expect(satConfig.KvCacheThreshold).NotTo(BeNumerically("~", 0.60, 0.01))
			}
		})

		It("should return no error when ConfigMap doesn't exist", func() {
			By("Reconciling a non-existent ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent-configmap",
					Namespace: systemNamespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("Namespace Tracking", func() {
		It("should watch ConfigMaps for tracked namespaces", func() {
			By("Tracking a namespace")
			ds.NamespaceTrack("VariantAutoscaling", "test-va", "tracked-ns")

			By("Checking if namespace is watched")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, "tracked-ns")
			Expect(result).To(BeTrue())
		})

		It("should watch ConfigMaps for namespaces with opt-in label", func() {
			By("Creating a namespace with opt-in label")
			labeledNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "labeled-ns",
					Labels: map[string]string{
						constants.NamespaceConfigEnabledLabelKey: annotationValueTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, labeledNS)).To(Succeed())

			By("Checking if labeled namespace is watched")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, "labeled-ns")
			Expect(result).To(BeTrue())
		})

		It("should not watch ConfigMaps for excluded namespaces", func() {
			By("Creating a namespace with exclusion annotation")
			excludedNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "excluded-ns",
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: annotationValueTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, excludedNS)).To(Succeed())

			By("Tracking the namespace in datastore")
			ds.NamespaceTrack("VariantAutoscaling", "test-va", "excluded-ns")

			By("Checking if excluded namespace is NOT watched despite being tracked")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, "excluded-ns")
			Expect(result).To(BeFalse(), "Excluded namespace should not be watched even if tracked")
		})

		It("should not watch ConfigMaps for untracked namespaces without opt-in label", func() {
			By("Creating a plain namespace")
			plainNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "plain-ns",
				},
			}
			Expect(k8sClient.Create(ctx, plainNS)).To(Succeed())

			By("Checking if plain namespace is NOT watched")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, "plain-ns")
			Expect(result).To(BeFalse())
		})
	})

	Context("Single-Namespace Mode", func() {
		var watchedNamespace string

		BeforeEach(func() {
			watchedNamespace = "watched-namespace"

			// Create watched namespace
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: watchedNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			// Create config with watch namespace
			Expect(os.Setenv("WATCH_NAMESPACE", watchedNamespace)).To(Succeed())
			var err error
			cfg, err = newTestConfigWithPrometheus("https://prometheus:9090")
			Expect(err).NotTo(HaveOccurred())

			// Update reconciler with new config
			reconciler.Config = cfg
		})

		AfterEach(func() {
			_ = os.Unsetenv("WATCH_NAMESPACE")
		})

		It("should watch ConfigMaps in watched namespace even with exclusion annotation", func() {
			By("Adding exclusion annotation to watched namespace")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: watchedNamespace}, ns)).To(Succeed())
			if ns.Annotations == nil {
				ns.Annotations = make(map[string]string)
			}
			ns.Annotations[constants.NamespaceExcludeAnnotationKey] = annotationValueTrue
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			By("Checking if watched namespace is still watched despite exclusion")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, watchedNamespace)
			Expect(result).To(BeTrue(), "Should watch ConfigMaps in watched namespace even with exclusion annotation")
		})

		It("should watch ConfigMaps in watched namespace without opt-in label", func() {
			By("Ensuring watched namespace has no opt-in label")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: watchedNamespace}, ns)).To(Succeed())
			if ns.Labels != nil {
				delete(ns.Labels, constants.NamespaceConfigEnabledLabelKey)
				Expect(k8sClient.Update(ctx, ns)).To(Succeed())
			}

			By("Checking if watched namespace is watched without opt-in label")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, watchedNamespace)
			Expect(result).To(BeTrue(), "Should watch ConfigMaps in watched namespace without opt-in label")
		})

		It("should not watch ConfigMaps in non-watched namespace with exclusion annotation", func() {
			By("Creating non-watched namespace with exclusion annotation")
			nonWatchedNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "non-watched-excluded",
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: annotationValueTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, nonWatchedNS)).To(Succeed())

			By("Checking if non-watched excluded namespace is NOT watched")
			result := reconciler.shouldWatchNamespaceLocalConfigMap(ctx, "non-watched-excluded")
			Expect(result).To(BeFalse(), "Should not watch ConfigMaps in non-watched excluded namespace")
		})

		It("should reconcile ConfigMap in watched namespace with exclusion annotation", func() {
			By("Adding exclusion annotation to watched namespace")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: watchedNamespace}, ns)).To(Succeed())
			if ns.Annotations == nil {
				ns.Annotations = make(map[string]string)
			}
			ns.Annotations[constants.NamespaceExcludeAnnotationKey] = annotationValueTrue
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			By("Creating namespace-local saturation ConfigMap in watched namespace")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: watchedNamespace,
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.85\nqueueLengthThreshold: 15",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying the config was updated despite exclusion annotation")
			satConfigMap := cfg.SaturationConfigForNamespace(watchedNamespace)
			Expect(satConfigMap).NotTo(BeNil())
			satConfig, exists := satConfigMap["default"]
			Expect(exists).To(BeTrue())
			Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.85, 0.01))
		})
	})

	Context("Error Handling", func() {
		It("should handle invalid saturation config gracefully", func() {
			By("Creating a ConfigMap with invalid YAML")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: systemNamespace,
				},
				Data: map[string]string{
					"default": "invalid: yaml: content: [[[",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).NotTo(HaveOccurred())

			By("Reconciling the ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cm.Name,
					Namespace: cm.Namespace,
				},
			}
			// Should not error, just skip invalid entries
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

	})

	Context("Metrics Recording", func() {
		It("should record error metric when Get fails during reconciliation", func() {
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a failing client that errors on Get")
			failingClient := &failingK8sClient{
				Reader:   k8sClient,
				failGet:  true,
				getError: errors.New("simulated get error"),
			}

			testReconciler := &ConfigMapReconciler{
				Reader:    failingClient,
				Scheme:    reconciler.Scheme,
				Config:    cfg,
				Datastore: ds,
				Recorder:  reconciler.Recorder,
			}

			By("Attempting to reconcile a ConfigMap")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      config.SaturationConfigMapName(),
					Namespace: systemNamespace,
				},
			}
			_, err = testReconciler.Reconcile(ctx, req)

			By("Verifying error was returned")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("simulated get error"))

			By("Verifying error metric was incremented")
			metricValue := testutil.GetErrorMetricValue(testRegistry, constants.ComponentController, "Failed to get ConfigMap")
			Expect(metricValue).To(BeNumerically(">=", 1.0),
				"Error metric should be incremented when Get fails")
		})

		It("should record different error types separately", func() {
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Recording different error types")
			metrics.RecordError(constants.ComponentController, "Failed to get ConfigMap")
			metrics.RecordError(constants.ComponentController, "Failed to update ConfigMap")
			metrics.RecordError(constants.ComponentCollector, "Failed to collect metrics")

			By("Verifying all errors were recorded")
			metricFamilies, err := testRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			// Count the number of error metrics
			var errorMetricCount int
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAErrorsTotal {
					errorMetricCount = len(mf.GetMetric())
					break
				}
			}
			// Should have 3 separate error counters (one for each unique component+error_type combination)
			Expect(errorMetricCount).To(Equal(3), "Should have 3 separate error metrics")
		})
	})
})
