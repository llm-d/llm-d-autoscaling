package utils

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogsLabelSelectorContain fetches logs from every pod in namespace matching
// labelSelector (concatenated with pod separators) and reports whether substr
// appears in the combined text. sinceSeconds limits how far back log lines are
// retrieved per pod (Kubernetes PodLogOptions.SinceSeconds).
func PodLogsLabelSelectorContain(
	ctx context.Context,
	k8s kubernetes.Interface,
	namespace string,
	labelSelector string,
	substr string,
	sinceSeconds int64,
) (bool, string, error) {
	podList, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return false, "", err
	}
	if len(podList.Items) == 0 {
		return false, "", fmt.Errorf("no pods found for label selector %q in namespace %s", labelSelector, namespace)
	}
	var b strings.Builder
	since := sinceSeconds
	for _, pod := range podList.Items {
		opts := &corev1.PodLogOptions{SinceSeconds: &since}
		stream, streamErr := k8s.CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
		if streamErr != nil {
			return false, "", streamErr
		}
		raw, readErr := func() ([]byte, error) {
			defer func() { _ = stream.Close() }()
			return io.ReadAll(stream)
		}()
		if readErr != nil {
			return false, "", readErr
		}
		fmt.Fprintf(&b, "\n--- pod: %s ---\n", pod.Name)
		b.Write(raw)
	}
	combined := b.String()
	return strings.Contains(combined, substr), combined, nil
}
