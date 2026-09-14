package utils

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// IsHPAScaleToZeroEnabled checks if the HPAScaleToZero feature gate is enabled on the cluster.
// On OpenShift, this checks the FeatureGate custom resource.
// On vanilla Kubernetes, this checks the kube-controller-manager pod args.
func IsHPAScaleToZeroEnabled(ctx context.Context, k8sClient *kubernetes.Clientset, w io.Writer) bool {
	// Use discovery to check if this is an OpenShift cluster
	_, resources, err := k8sClient.Discovery().ServerGroupsAndResources()
	if err != nil {
		_, _ = fmt.Fprintf(w, "Warning: Could not discover API resources: %v\n", err)
		return false
	}

	isOpenShift := false
	for _, resourceList := range resources {
		for _, resource := range resourceList.APIResources {
			if resource.Name == "featuregates" && resourceList.GroupVersion == "config.openshift.io/v1" {
				isOpenShift = true
				break
			}
		}
		if isOpenShift {
			break
		}
	}

	if isOpenShift {
		// On OpenShift, check the FeatureGate CR
		// We use raw REST client to avoid importing OpenShift types
		result, err := k8sClient.RESTClient().
			Get().
			AbsPath("/apis/config.openshift.io/v1/featuregates/cluster").
			DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Warning: Could not get FeatureGate CR: %v\n", err)
			return false
		}

		// Check if HPAScaleToZero is in the enabled features
		resultStr := string(result)
		if strings.Contains(resultStr, "HPAScaleToZero") {
			_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is enabled on OpenShift\n")
			return true
		}
		_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is NOT enabled on OpenShift\n")
		return false
	}

	// On vanilla Kubernetes, check kube-controller-manager pod args
	// Try multiple label selectors as different K8s distributions use different labels
	labelSelectors := []string{
		"component=kube-controller-manager",
		"tier=control-plane,component=kube-controller-manager",
	}

	for _, labelSelector := range labelSelectors {
		pods, err := k8sClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			_, _ = fmt.Fprintf(w, "Warning: Could not list pods with selector %s: %v\n", labelSelector, err)
			continue
		}
		if len(pods.Items) == 0 {
			continue
		}

		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				// Check both Command and Args as feature gates can be in either
				allArgs := slices.Concat(container.Command, container.Args)
				for _, arg := range allArgs {
					if strings.Contains(arg, "HPAScaleToZero=true") {
						_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is enabled\n")
						return true
					}
				}
			}
		}
	}

	// Also try to find the pod by name prefix if label selectors didn't work
	pods, err := k8sClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pod := range pods.Items {
			if strings.HasPrefix(pod.Name, "kube-controller-manager") {
				for _, container := range pod.Spec.Containers {
					allArgs := slices.Concat(container.Command, container.Args)
					for _, arg := range allArgs {
						if strings.Contains(arg, "HPAScaleToZero=true") {
							_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is enabled (found by name)\n")
							return true
						}
					}
				}
			}
		}
	}

	_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is NOT enabled\n")
	return false
}
