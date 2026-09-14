package fixtures

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
)

const defaultPollInterval = 500 * time.Millisecond

// WaitUntilDeploymentDeleted polls until the Deployment is not found or the timeout elapses.
func WaitUntilDeploymentDeleted(ctx context.Context, k8s kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, defaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := k8s.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

// WaitUntilServiceDeleted polls until the Service is not found or the timeout elapses.
func WaitUntilServiceDeleted(ctx context.Context, k8s kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, defaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := k8s.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

// WaitUntilServiceMonitorDeleted polls until the ServiceMonitor is not found.
func WaitUntilServiceMonitorDeleted(ctx context.Context, crClient client.Client, namespace, name string, timeout time.Duration) error {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	return wait.PollUntilContextTimeout(ctx, defaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		sm := &promoperator.ServiceMonitor{}
		err := crClient.Get(ctx, key, sm)
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}
