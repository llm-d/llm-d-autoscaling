package fixtures

import (
	"context"
	"fmt"
	"time"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateService creates a Kubernetes Service for the model server. Fails if the service already exists.
func CreateService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, baseName, appLabel string, port int) error {
	service := buildService(namespace, baseName, appLabel, port)
	_, err := k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	return err
}

// DeleteService deletes the Kubernetes Service. Idempotent; ignores NotFound.
func DeleteService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, baseName string) error {
	serviceName := baseName + serviceNameSuffix
	err := k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", serviceName, err)
	}
	return nil
}

// EnsureService creates or replaces the Service (idempotent for test setup).
func EnsureService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, baseName, appLabel string, port int) error {
	serviceName := baseName + serviceNameSuffix
	_, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing service %s: %w", serviceName, err)
		}
	} else {
		deleteErr := k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing service %s: %w", serviceName, deleteErr)
		}
		if waitErr := WaitUntilServiceDeleted(ctx, k8sClient, namespace, serviceName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for service %s to be deleted: %w", serviceName, waitErr)
		}
	}
	service := buildService(namespace, baseName, appLabel, port)
	_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		if waitErr := WaitUntilServiceDeleted(ctx, k8sClient, namespace, serviceName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for service %s to be deleted before recreate: %w", serviceName, waitErr)
		}
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	}
	return err
}

func buildService(namespace, baseName, appLabel string, port int) *corev1.Service {
	serviceName := baseName + serviceNameSuffix
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": defaultLabelValueTrue,
				"test-resource":             defaultTestResourceLabelValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": defaultLabelValueTrue,
			},
			Ports: []corev1.ServicePort{
				{Name: defaultServicePortName, Port: int32(port), Protocol: corev1.ProtocolTCP},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// CreateServiceMonitor creates a ServiceMonitor for Prometheus. Fails if it already exists.
func CreateServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, targetNamespace, baseName, appLabel string) error {
	serviceMonitor := buildServiceMonitor(monitoringNamespace, targetNamespace, baseName, appLabel)
	return crClient.Create(ctx, serviceMonitor)
}

// DeleteServiceMonitor deletes the ServiceMonitor. Idempotent; ignores NotFound.
func DeleteServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, baseName string) error {
	serviceMonitorName := baseName + serviceMonitorNameSuffix
	sm := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: serviceMonitorName, Namespace: monitoringNamespace},
	}
	err := crClient.Delete(ctx, sm)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ServiceMonitor %s: %w", serviceMonitorName, err)
	}
	return nil
}

// EnsureServiceMonitor creates or replaces the ServiceMonitor (idempotent for test setup).
func EnsureServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, targetNamespace, baseName, appLabel string) error {
	serviceMonitorName := baseName + serviceMonitorNameSuffix
	existingSM := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: serviceMonitorName, Namespace: monitoringNamespace},
	}
	err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: monitoringNamespace}, existingSM)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing ServiceMonitor %s: %w", serviceMonitorName, err)
		}
	} else {
		deleteErr := crClient.Delete(ctx, existingSM)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing ServiceMonitor %s: %w", serviceMonitorName, deleteErr)
		}
		if waitErr := WaitUntilServiceMonitorDeleted(ctx, crClient, monitoringNamespace, serviceMonitorName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for ServiceMonitor %s to be deleted: %w", serviceMonitorName, waitErr)
		}
	}
	serviceMonitor := buildServiceMonitor(monitoringNamespace, targetNamespace, baseName, appLabel)
	return crClient.Create(ctx, serviceMonitor)
}

func buildServiceMonitor(monitoringNamespace, targetNamespace, baseName, appLabel string) *promoperator.ServiceMonitor {
	serviceMonitorName := baseName + serviceMonitorNameSuffix
	return &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceMonitorName,
			Namespace: monitoringNamespace,
			Labels: map[string]string{
				"app":           appLabel,
				"release":       kubePrometheusStackReleaseLabelValue,
				"test-resource": defaultTestResourceLabelValue,
			},
		},
		Spec: promoperator.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Endpoints: []promoperator.Endpoint{
				{
					Port:     defaultServicePortName,
					Path:     defaultServiceMonitorMetricsPath,
					Interval: promoperator.Duration("15s"),
					// Propagate the llm-d.ai/variant pod label into scraped metrics.
					// __meta_* labels are only available during target relabeling, so this
					// must live under RelabelConfigs (not MetricRelabelConfigs).
					RelabelConfigs: []promoperator.RelabelConfig{
						{
							SourceLabels: []promoperator.LabelName{"__meta_kubernetes_pod_label_llm_d_ai_variant"},
							TargetLabel:  "llm_d_ai_variant",
							Action:       "replace",
						},
					},
				},
			},
			NamespaceSelector: promoperator.NamespaceSelector{MatchNames: []string{targetNamespace}},
		},
	}
}
