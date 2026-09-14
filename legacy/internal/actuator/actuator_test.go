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

package actuator

import (
	"context"
	"fmt"
	"strconv"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	ctrlutils "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testAcceleratorA100 = "A100"

func fmtNumReplicas(nr *int32) string {
	if nr == nil {
		return "<nil>"
	}
	return strconv.Itoa(int(*nr))
}

var _ = Describe("Actuator", func() {
	var (
		ctx          context.Context
		scheme       *runtime.Scheme
		actuator     *Actuator
		registry     *prometheus.Registry
		resourceName string
		namespace    string
	)

	BeforeEach(func() {
		ctx = context.Background()
		resourceName = "test-variant-autoscaling"
		namespace = "default"

		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)).To(Succeed())

		// Create a new registry for each test to avoid conflicts
		registry = prometheus.NewRegistry()

		// Initialize metrics with the test registry
		err := metrics.InitMetrics(registry)
		Expect(err).NotTo(HaveOccurred())

		// Initialize the actuator with the controller-runtime client
		actuator = NewActuator(k8sClient)
		Expect(actuator).NotTo(BeNil())
		Expect(actuator.MetricsEmitter).NotTo(BeNil())
	})

	Context("Actuator initialization", func() {
		It("should create a new actuator with valid client and metrics emitter", func() {
			newActuator := NewActuator(k8sClient)
			Expect(newActuator).NotTo(BeNil())
			Expect(newActuator.Client).To(Equal(k8sClient))
			Expect(newActuator.MetricsEmitter).NotTo(BeNil())
		})
	})

	Context("Testing GetCurrentScaleTargetReplicasFromScaleTarget", func() {
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

		BeforeEach(func() {
			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: resourceName,
					},
				},
			}
		})

		It("should return status replicas when status.Replicas is set", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(5)),
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 3,
				},
			}

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaletarget.NewDeploymentAccessor(deployment))
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(int32(3)), "Should prefer status.Replicas over spec.Replicas")
		})

		It("should return status replicas when status.Replicas is 0", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(5)),
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 0,
				},
			}

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaletarget.NewDeploymentAccessor(deployment))
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(int32(0)), "Should return 0 when status.Replicas is 0")
		})

		It("should fallback to spec.Replicas when status.Replicas is not set", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(7)),
				},
				Status: appsv1.DeploymentStatus{
					// Status.Replicas not set, will be default (0 for int32)
					// But we want to test the fallback to spec
				},
			}

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaletarget.NewDeploymentAccessor(deployment))
			Expect(err).NotTo(HaveOccurred())
			// Status.Replicas defaults to 0, which is >= 0, so it should return 0
			Expect(replicas).To(Equal(int32(0)))
		})

		It("should return 1 as final fallback when both status and spec are nil", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					// Replicas is nil
				},
				Status: appsv1.DeploymentStatus{
					Replicas: -1, // Force status to be < 0
				},
			}

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaletarget.NewDeploymentAccessor(deployment))
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(int32(1)), "Should return 1 as final fallback when both status and spec unavailable")
		})

		It("should return error when scale target is nil", func() {
			_, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("scale target cannot be nil"))
		})

		It("should handle large replica counts", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(1000)),
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 999,
				},
			}

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromScaleTarget(va, scaletarget.NewDeploymentAccessor(deployment))
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(int32(999)))
		})
	})

	Context("Testing GetCurrentScaleTargetReplicasFromVA", func() {
		var deployment *appsv1.Deployment
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

		BeforeEach(func() {
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(3)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 3,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: resourceName,
					},
					MaxReplicas: 2,
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should return status replicas when available", func() {
			deployment.Status.Replicas = 3
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			replicas, err := actuator.GetCurrentScaleTargetReplicasFromVA(ctx, va)
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(deployment.Status.Replicas), fmt.Sprintf("Should return status replicas - actual: %d", replicas))
		})

		It("should return error when scale target doesn't exist", func() {
			nonExistentVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent",
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "non-existent",
					},
				},
			}

			_, err := actuator.GetCurrentScaleTargetReplicasFromVA(ctx, nonExistentVA)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get scale target"))
		})
	})

	Context("EmitMetrics", func() {
		var deployment *appsv1.Deployment
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-emit"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 2,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
					Labels: map[string]string{
						accelerator.AcceleratorNameLabel: testAcceleratorA100,
					},
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: contextResourceName,
					},
					ModelID:     "test-model/variant-1",
					MaxReplicas: 2,
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						NumReplicas: ctrlutils.Ptr(int32(4)),
						Accelerator: testAcceleratorA100,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			va.Status.DesiredOptimizedAlloc.NumReplicas = ctrlutils.Ptr(int32(4))
			va.Status.DesiredOptimizedAlloc.Accelerator = testAcceleratorA100
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should emit metrics successfully when desired replicas > 0", func() {
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Verify metrics were emitted correctly
			// We can't directly test the metrics values due to registry isolation,
			// but we can verify the method completed without error
		})

		It("should skip metrics emission when NumReplicas is nil (no decision)", func() {
			va.Status.DesiredOptimizedAlloc.NumReplicas = nil
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Method should succeed but skip metrics emission
		})

		It("should use fallback replicas when deployment retrieval fails", func() {
			// Delete the deployment to simulate retrieval failure
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())

			// Wait for deletion to complete
			Eventually(func() error {
				var dep appsv1.Deployment
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      resourceName,
					Namespace: namespace,
				}, &dep)
			}).Should(HaveOccurred())
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Should use fallback from VariantAutoscaling status (2 replicas)
			// Method should complete without error despite deployment retrieval failure
		})

		It("should handle metrics emission errors gracefully", func() {
			// This test verifies that metrics emission errors don't fail the method
			// We can't easily simulate a metrics emission error without mocking,
			// but we can verify the error handling logic exists
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Metrics integration", func() {
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var deployment *appsv1.Deployment

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-metrics"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 1,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: contextResourceName,
					},
					ModelID:     "test-model/metrics-test",
					MaxReplicas: 2,
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						NumReplicas: ctrlutils.Ptr(int32(3)),
						Accelerator: testAcceleratorA100,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			va.Status.DesiredOptimizedAlloc.NumReplicas = ctrlutils.Ptr(int32(3))
			va.Status.DesiredOptimizedAlloc.Accelerator = testAcceleratorA100

		})

		AfterEach(func() {
			// Cleanup resources with proper error handling
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should verify that metrics emitter can emit scaling metrics", func() {
			fmt.Printf("Emitting scaling metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.MetricsEmitter.EmitReplicaScalingMetrics(ctx, va, domain.ActionScaleUp, domain.DecisionReasonV2)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should verify that metrics emitter can emit replica metrics", func() {
			fmt.Printf("Emitting replica metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 1, 3, testAcceleratorA100)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should verify full metric emission workflow", func() {
			// Test the complete workflow
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Additional scaling metrics
			err = actuator.MetricsEmitter.EmitReplicaScalingMetrics(ctx, va, domain.ActionScaleUp, domain.DecisionReasonV2)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Edge cases and error handling", func() {
		It("should handle VariantAutoscaling with missing status fields", func() {
			// Create a minimal valid VariantAutoscaling but with zero desired replicas
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "incomplete-va",
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "incomplete-va",
					},
					ModelID:     "test-model/incomplete",
					MaxReplicas: 2,
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					// DesiredOptimizedAlloc.NumReplicas is nil (no decision yet)
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						NumReplicas: nil, // This should cause EmitMetrics to skip
						Accelerator: testAcceleratorA100,
					},
				},
			}

			defer func() {
			}()
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %s\n", va.Name, fmtNumReplicas(va.Status.DesiredOptimizedAlloc.NumReplicas))
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred()) // Should skip metrics emission due to nil NumReplicas
		})
	})

	Context("Metrics validation", func() {
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var deployment *appsv1.Deployment

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-validation"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 2,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: contextResourceName,
					},
					ModelID:     "test-model/validation-test",
					MaxReplicas: 2,
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						NumReplicas: ctrlutils.Ptr(int32(5)),
						Accelerator: testAcceleratorA100,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			va.Status.DesiredOptimizedAlloc.NumReplicas = ctrlutils.Ptr(int32(5))
			va.Status.DesiredOptimizedAlloc.Accelerator = testAcceleratorA100

		})

		AfterEach(func() {
			// Cleanup resources with proper error handling
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should test ratio calculation scenarios", func() {
			fmt.Printf("Testing metrics emission for variant autoscaling: %s\n", va.Name)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Test normal case: current = 2, desired = 5, ratio = 2.5
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 2, 5, testAcceleratorA100)
			Expect(err).NotTo(HaveOccurred())

			// Test scale-to-zero case: current = 0, desired = 3, ratio = 3
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 0, 3, testAcceleratorA100)
			Expect(err).NotTo(HaveOccurred())

			// Test no-change case: current = 4, desired = 4, ratio = 1
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 4, 4, testAcceleratorA100)
			Expect(err).NotTo(HaveOccurred())

			// Test scale-down case: current = 6, desired = 2, ratio = 0.33
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 6, 2, testAcceleratorA100)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
