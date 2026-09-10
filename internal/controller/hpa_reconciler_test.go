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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

var _ = Describe("HPAReconciler metrics eviction wiring", func() {
	var (
		reconciler *HPAReconciler
		emitter    *metrics.MetricsEmitter
		ds         datastore.Datastore
		registry   *prometheus.Registry
		nsName     string
	)

	seriesCount := func(name string) int {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == name {
				return len(mf.GetMetric())
			}
		}
		return 0
	}

	allGaugesEmpty := func() bool {
		return seriesCount(constants.WVACurrentReplicas) == 0 &&
			seriesCount(constants.WVADesiredReplicas) == 0 &&
			seriesCount(constants.WVADesiredRatio) == 0
	}

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
		emitter = metrics.NewMetricsEmitter()
		ds = datastore.NewDatastore(config.NewTestConfig())
		nsName = "hpa-reconciler-test"

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

		reconciler = &HPAReconciler{
			Client:         k8sClient,
			Datastore:      ds,
			MetricsEmitter: emitter,
		}
	})

	It("evicts all replica-gauge series when the managed HPA is deleted", func() {
		hpaName := "hpa-delete-evict"

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hpaName,
				Namespace: nsName,
				Annotations: map[string]string{
					annotations.Managed: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "target",
				},
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		va := &llmdOptv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: hpaName, Namespace: nsName},
		}
		Expect(emitter.EmitReplicaMetrics(ctx, va, 2, 3, "A100")).To(Succeed())
		Expect(seriesCount(constants.WVACurrentReplicas)).To(Equal(1), "series must exist after emit")

		Expect(k8sClient.Delete(ctx, hpa)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: hpaName, Namespace: nsName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(allGaugesEmpty()).To(BeTrue(), "all replica-gauge series must be evicted after HPA deletion")
	})

	It("evicts replica-gauge series when the managed annotation is removed", func() {
		hpaName := "hpa-deannotate-evict"

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hpaName,
				Namespace: nsName,
				Annotations: map[string]string{
					annotations.Managed: "true",
				},
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "target",
				},
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		va := &llmdOptv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: hpaName, Namespace: nsName},
		}
		Expect(emitter.EmitReplicaMetrics(ctx, va, 1, 2, "H100")).To(Succeed())
		Expect(seriesCount(constants.WVACurrentReplicas)).To(Equal(1), "series must exist after emit")

		// Strip the managed annotation — reconciler must treat this as removal.
		hpa.Annotations = map[string]string{}
		Expect(k8sClient.Update(ctx, hpa)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: hpaName, Namespace: nsName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(allGaugesEmpty()).To(BeTrue(), "all replica-gauge series must be evicted after annotation removal")
	})
})
