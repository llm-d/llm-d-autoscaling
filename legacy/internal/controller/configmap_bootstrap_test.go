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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testutil"
)

var _ = Describe("ConfigMap Bootstrap", func() {
	var (
		reconciler      *ConfigMapReconciler
		ctx             context.Context
		cfg             *config.Config
		systemNamespace string
	)

	BeforeEach(func() {
		setup := setupConfigMapReconcilerTest("workload-variant-autoscaler-system", "")
		ctx = setup.ctx //nolint:fatcontext // shared across BeforeEach/It blocks
		cfg = setup.cfg
		reconciler = setup.reconciler
		systemNamespace = setup.systemNamespace
	})

	It("should bootstrap global secondary ConfigMaps and mark config sync complete", func() {
		By("Creating global saturation and scale-to-zero ConfigMaps")
		saturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: systemNamespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.70\nqueueLengthThreshold: 8\nkvSpareTrigger: 0.15\nqueueSpareTrigger: 5",
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, saturationCM))).To(Succeed())
		currentSaturationCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saturationCM.Name, Namespace: saturationCM.Namespace}, currentSaturationCM)).To(Succeed())
		currentSaturationCM.Data = saturationCM.Data
		Expect(k8sClient.Update(ctx, currentSaturationCM)).To(Succeed())

		scaleToZeroCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.DefaultScaleToZeroConfigMapName,
				Namespace: systemNamespace,
			},
			Data: map[string]string{
				"default": "enable_scale_to_zero: true\nretention_period: 5m",
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, scaleToZeroCM))).To(Succeed())
		currentScaleToZeroCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scaleToZeroCM.Name, Namespace: scaleToZeroCM.Namespace}, currentScaleToZeroCM)).To(Succeed())
		currentScaleToZeroCM.Data = scaleToZeroCM.Data
		Expect(k8sClient.Update(ctx, currentScaleToZeroCM)).To(Succeed())

		By("Bootstrapping configmaps")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())

		By("Verifying saturation config was loaded")
		satConfigMap := cfg.SaturationConfig()
		satConfig, exists := satConfigMap["default"]
		Expect(exists).To(BeTrue())
		Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.70, 0.01))

		By("Verifying scale-to-zero config was loaded")
		s2zConfig := cfg.ScaleToZeroConfig()
		defaultModel, exists := s2zConfig["default"]
		Expect(exists).To(BeTrue())
		Expect(defaultModel.EnableScaleToZero).NotTo(BeNil())
		Expect(*defaultModel.EnableScaleToZero).To(BeTrue())
	})

	It("should mark sync complete when optional ConfigMaps are absent", func() {
		By("Bootstrapping with no configmaps present")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())
	})

	It("should bootstrap namespace-local ConfigMaps from all namespaces when watching all namespaces", func() {
		By("Creating multiple namespaces")
		namespace1 := "test-namespace-1"
		namespace2 := "test-namespace-2"

		ns1 := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace1,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "true",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns1))).To(Succeed())

		ns2 := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace2,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "true",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns2))).To(Succeed())

		By("Creating global ConfigMap in system namespace")
		globalSaturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: systemNamespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.60\nqueueLengthThreshold: 10",
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, globalSaturationCM))).To(Succeed())
		currentGlobalSatCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: globalSaturationCM.Name, Namespace: globalSaturationCM.Namespace}, currentGlobalSatCM)).To(Succeed())
		currentGlobalSatCM.Data = globalSaturationCM.Data
		Expect(k8sClient.Update(ctx, currentGlobalSatCM)).To(Succeed())

		By("Creating namespace-local saturation ConfigMap in namespace1")
		ns1SaturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: namespace1,
			},
			Data: map[string]string{
				"model-a": "kvCacheThreshold: 0.75\nqueueLengthThreshold: 5",
			},
		}
		Expect(k8sClient.Create(ctx, ns1SaturationCM)).To(Succeed())

		By("Creating namespace-local scale-to-zero ConfigMap in namespace2")
		ns2ScaleToZeroCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.DefaultScaleToZeroConfigMapName,
				Namespace: namespace2,
			},
			Data: map[string]string{
				"model-b": "model_id: model-b\nenable_scale_to_zero: true\nretention_period: 10m",
			},
		}
		Expect(k8sClient.Create(ctx, ns2ScaleToZeroCM)).To(Succeed())

		By("Bootstrapping ConfigMaps (watching all namespaces)")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())

		By("Verifying global saturation config was loaded")
		globalSatConfig := cfg.SaturationConfigForNamespace("")
		satConfig, exists := globalSatConfig["default"]
		Expect(exists).To(BeTrue())
		Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.60, 0.01))

		By("Verifying namespace1 saturation config was loaded")
		ns1SatConfig := cfg.SaturationConfigForNamespace(namespace1)
		ns1ModelConfig, exists := ns1SatConfig["model-a"]
		Expect(exists).To(BeTrue())
		Expect(ns1ModelConfig.KvCacheThreshold).To(BeNumerically("~", 0.75, 0.01))
		Expect(ns1ModelConfig.QueueLengthThreshold).To(BeNumerically("==", 5))

		By("Verifying namespace2 scale-to-zero config was loaded")
		ns2S2ZConfig := cfg.ScaleToZeroConfigForNamespace(namespace2)
		ns2ModelConfig, exists := ns2S2ZConfig["model-b"]
		Expect(exists).To(BeTrue())
		Expect(ns2ModelConfig.EnableScaleToZero).NotTo(BeNil())
		Expect(*ns2ModelConfig.EnableScaleToZero).To(BeTrue())
	})

	It("should skip namespaces with exclude annotation during bootstrap", func() {
		By("Creating namespaces with and without exclude annotation")
		includedNamespace := "test-included-namespace"
		excludedNamespace := "test-excluded-namespace"

		// Namespace with config-enabled label and without exclude annotation (should be scanned)
		nsIncluded := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: includedNamespace,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "true",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, nsIncluded))).To(Succeed())

		// Namespace with config-enabled label and exclude annotation set to "true" (should be skipped)
		nsExcluded := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: excludedNamespace,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "true",
				},
				Annotations: map[string]string{
					constants.NamespaceExcludeAnnotationKey: "true",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, nsExcluded))).To(Succeed())

		By("Creating namespace-local ConfigMaps in both namespaces")
		// ConfigMap in included namespace
		includedSaturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: includedNamespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.85\nqueueLengthThreshold: 7",
			},
		}
		Expect(k8sClient.Create(ctx, includedSaturationCM)).To(Succeed())

		// ConfigMap in excluded namespace (should NOT be loaded)
		excludedSaturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: excludedNamespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.95\nqueueLengthThreshold: 15",
			},
		}
		Expect(k8sClient.Create(ctx, excludedSaturationCM)).To(Succeed())

		By("Bootstrapping ConfigMaps")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())

		By("Verifying ConfigMap from included namespace was loaded")
		includedConfig := cfg.SaturationConfigForNamespace(includedNamespace)
		includedSatConfig, exists := includedConfig["default"]
		Expect(exists).To(BeTrue())
		Expect(includedSatConfig.KvCacheThreshold).To(BeNumerically("~", 0.85, 0.01))
		Expect(includedSatConfig.QueueLengthThreshold).To(BeNumerically("==", 7))

		By("Verifying ConfigMap from excluded namespace was NOT loaded")
		excludedConfig := cfg.SaturationConfigForNamespace(excludedNamespace)
		// Should either be empty or contain only global defaults, not the excluded namespace's config
		if len(excludedConfig) > 0 {
			// If fallback to global is implemented, should not have the excluded namespace's values
			if excludedSatConfig, exists := excludedConfig["default"]; exists {
				Expect(excludedSatConfig.KvCacheThreshold).NotTo(BeNumerically("~", 0.95, 0.01))
				Expect(excludedSatConfig.QueueLengthThreshold).NotTo(BeNumerically("==", 15))
			}
		}
	})

	It("should include namespaces with exclude annotation set to false", func() {
		By("Creating namespace with exclude annotation set to 'false'")
		namespace := "test-exclude-false-namespace"

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "true",
				},
				Annotations: map[string]string{
					constants.NamespaceExcludeAnnotationKey: "false",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		By("Creating namespace-local ConfigMap")
		saturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: namespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.78\nqueueLengthThreshold: 6",
			},
		}
		Expect(k8sClient.Create(ctx, saturationCM)).To(Succeed())

		By("Bootstrapping ConfigMaps")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())

		By("Verifying ConfigMap was loaded (exclude=false should not exclude)")
		nsConfig := cfg.SaturationConfigForNamespace(namespace)
		satConfig, exists := nsConfig["default"]
		Expect(exists).To(BeTrue())
		Expect(satConfig.KvCacheThreshold).To(BeNumerically("~", 0.78, 0.01))
		Expect(satConfig.QueueLengthThreshold).To(BeNumerically("==", 6))
	})

	It("should skip namespaces with config-enabled label set to false", func() {
		By("Creating namespace with config-enabled label set to 'false'")
		namespace := "test-config-disabled-namespace"

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					constants.NamespaceConfigEnabledLabelKey: "false",
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		By("Creating namespace-local ConfigMap")
		saturationCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.SaturationConfigMapName(),
				Namespace: namespace,
			},
			Data: map[string]string{
				"default": "kvCacheThreshold: 0.88\nqueueLengthThreshold: 12",
			},
		}
		Expect(k8sClient.Create(ctx, saturationCM)).To(Succeed())

		By("Bootstrapping ConfigMaps")
		Expect(reconciler.BootstrapInitialConfigMaps(ctx)).To(Succeed())

		By("Verifying bootstrap readiness state")
		Expect(cfg.ConfigMapsBootstrapComplete()).To(BeTrue())

		By("Verifying ConfigMap from namespace with config-enabled=false was NOT loaded")
		nsConfig := cfg.SaturationConfigForNamespace(namespace)
		// Should either be empty or contain only global defaults, not the disabled namespace's config
		if len(nsConfig) > 0 {
			// If fallback to global is implemented, should not have the disabled namespace's values
			if satConfig, exists := nsConfig["default"]; exists {
				Expect(satConfig.KvCacheThreshold).NotTo(BeNumerically("~", 0.88, 0.01))
				Expect(satConfig.QueueLengthThreshold).NotTo(BeNumerically("==", 12))
			}
		}
	})

	Describe("ConfigMap Bootstrap Error Metrics", func() {
		var (
			ctx      context.Context
			registry *prometheus.Registry
		)

		BeforeEach(func() {
			ctx = context.Background()
			// Create a new registry for each test to ensure metric isolation
			registry = prometheus.NewRegistry()
			err := metrics.InitMetrics(registry)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should record error metric when Config is nil", func() {
			By("Creating reconciler with nil Config")
			reconciler := &ConfigMapReconciler{
				Reader: k8sClient,
				Config: nil, // nil config to trigger error
			}

			By("Attempting to bootstrap ConfigMaps")
			err := reconciler.BootstrapInitialConfigMaps(ctx)

			By("Verifying error was returned")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("config is nil"))

			By("Verifying error metric was incremented")
			metricValue := testutil.GetErrorMetricValue(registry, constants.ComponentController, "Config is nil in ConfigMapReconciler bootstrap")
			Expect(metricValue).To(BeNumerically(">=", 1.0),
				"Error metric should be incremented when Config is nil")
		})

		It("should record error metric when List fails", func() {
			By("Creating a test config")
			cfg, err := newTestConfigWithPrometheus("https://prometheus:9090")
			Expect(err).NotTo(HaveOccurred())

			By("Creating a failing client that errors on List")
			failingClient := &failingK8sClient{
				Reader:    k8sClient,
				failGet:   false,
				failList:  true,
				listError: errors.New("simulated list error"),
			}

			reconciler := &ConfigMapReconciler{
				Reader: failingClient,
				Config: cfg,
			}

			By("Attempting to bootstrap ConfigMaps")
			err = reconciler.BootstrapInitialConfigMaps(ctx)

			By("Verifying error was returned")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list namespaces"))

			By("Verifying error metric was incremented")
			metricValue := testutil.GetErrorMetricValue(registry, constants.ComponentController, "Failed to list namespaces during bootstrap")
			Expect(metricValue).To(BeNumerically(">=", 1.0),
				"Error metric should be incremented when List fails")

			By("Verifying bootstrap failed state was marked")
			Expect(cfg.ConfigMapsBootstrapComplete()).To(BeFalse())
		})
	})
})
