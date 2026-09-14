package utils

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// DumpControllerLogs fetches and prints the controller manager logs for debugging.
// Call this in AfterEach or DeferCleanup to capture logs on test failure.
func DumpControllerLogs(ctx context.Context, k8sClient *kubernetes.Clientset, controllerNamespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Controller Manager Logs ===\n")

	pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list controller pods: %v\n", err)
		return
	}

	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "No controller pods found in namespace %s\n", controllerNamespace)
		return
	}

	for _, pod := range pods.Items {
		_, _ = fmt.Fprintf(w, "\n--- Logs from pod %s ---\n", pod.Name)
		logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: ptr.To(int64(200)),
		}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Failed to get logs: %v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", string(logs))
	}
}

// DumpManagedScalers fetches and prints the HorizontalPodAutoscalers in every
// namespace for debugging. WVA discovers variants from annotated HPAs (and KEDA
// ScaledObjects, which KEDA in turn manages via HPAs), so the HPA list plus its
// currentMetrics is the observable annotation-discovery surface.
func DumpManagedScalers(ctx context.Context, k8sClient *kubernetes.Clientset, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Managed HorizontalPodAutoscalers ===\n")

	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list HPAs: %v\n", err)
		return
	}

	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		_, _ = fmt.Fprintf(w, "\nHPA: %s/%s\n", hpa.Namespace, hpa.Name)
		_, _ = fmt.Fprintf(w, "  ScaleTargetRef: %s/%s\n", hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.ScaleTargetRef.Name)
		_, _ = fmt.Fprintf(w, "  Annotations: %v\n", hpa.Annotations)
		_, _ = fmt.Fprintf(w, "  DesiredReplicas: %d\n", hpa.Status.DesiredReplicas)
		_, _ = fmt.Fprintf(w, "  CurrentMetrics: %v\n", hpa.Status.CurrentMetrics)
	}
}
