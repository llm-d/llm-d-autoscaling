package fixtures

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// CreateModelService creates the model-server Deployment only (name + "-decode").
// It does not create a Kubernetes Service; callers must use CreateService or EnsureService
// (typically naming the Service name + "-service") to expose the deployment.
// variantName is stamped as the llm-d.ai/variant label on the pod template so the
// collector can attribute pod metrics to the right annotated scaler. Pass "" to omit
// the label (no WVA-managed scaler targets this deployment).
func CreateModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int) error {
	return CreateModelServiceWithExtraArgs(ctx, k8sClient, namespace, name, poolName, modelID, variantName, useSimulator, maxNumSeqs, nil)
}

// CreateModelServiceWithExtraArgs is like CreateModelService but appends additional
// CLI args to the model-server (simulator or vLLM) container. Used by tests that
// need to inject engine-specific configuration such as --fake-metrics.
//
// The caller is responsible for ensuring extraArgs are valid for the chosen
// runtime — this function performs no flag validation. Engine-specific flags
// (e.g., --fake-metrics, which exists only on llm-d-inference-sim) will cause
// the container to crash-loop if passed to the wrong runtime. Tests that use
// simulator-only flags should gate their suite on `cfg.UseSimulator` and Skip
// otherwise.
func CreateModelServiceWithExtraArgs(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int, extraArgs []string) error {
	deployment := buildModelServiceDeployment(namespace, name, poolName, modelID, variantName, useSimulator, maxNumSeqs, extraArgs)
	_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	return err
}

// DeleteModelService deletes the model service deployment. Idempotent; ignores NotFound.
func DeleteModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	deploymentName := name + decodeNameSuffix
	err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete model service deployment %s: %w", deploymentName, err)
	}
	return nil
}

// EnsureModelService creates or replaces the model-server Deployment only (name + "-decode").
// It does not create a Kubernetes Service; pair with EnsureService for a ClusterIP Service.
// variantName is stamped as the llm-d.ai/variant label on the pod template so the
// collector can attribute pod metrics to the right annotated scaler. Pass "" to omit.
func EnsureModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int) error {
	appLabel := name + decodeNameSuffix
	deploymentName := appLabel
	desiredDeployment := buildModelServiceDeployment(namespace, name, poolName, modelID, variantName, useSimulator, maxNumSeqs, nil)

	existingDeployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing deployment %s: %w", deploymentName, err)
		}
	} else {
		if existingDeployment.Status.ReadyReplicas > 0 && modelServiceDeploymentMatchesDesired(*existingDeployment, *desiredDeployment) {
			return nil
		}
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) && !errors.IsConflict(deleteErr) {
			return fmt.Errorf("delete existing deployment %s: %w", deploymentName, deleteErr)
		}
		if err := WaitUntilDeploymentDeleted(ctx, k8sClient, namespace, deploymentName, 2*time.Minute); err != nil {
			return fmt.Errorf("timeout waiting for deployment %s to be deleted: %w", deploymentName, err)
		}
	}

	_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, desiredDeployment, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		propagationPolicy := metav1.DeletePropagationForeground
		_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if waitErr := WaitUntilDeploymentDeleted(ctx, k8sClient, namespace, deploymentName, 2*time.Minute); waitErr != nil {
			return fmt.Errorf("timeout waiting for deployment %s to be deleted before recreate: %w", deploymentName, waitErr)
		}
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, desiredDeployment, metav1.CreateOptions{})
	}
	return err
}

func modelServiceDeploymentMatchesDesired(existing, desired appsv1.Deployment) bool {
	return apiequality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Template.Labels, desired.Spec.Template.Labels) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Template.Spec, desired.Spec.Template.Spec)
}

func buildModelServiceDeployment(namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int, extraArgs []string) *appsv1.Deployment {
	appLabel := name + decodeNameSuffix
	image := defaultModelServiceSimulatorImage
	if !useSimulator {
		image = defaultModelServiceRuntimeImage
	}
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs, extraArgs)
	labels := map[string]string{
		"app":                          appLabel,
		"llm-d.ai/inferenceServing":    defaultLabelValueTrue,
		"llm-d.ai/model":               defaultModelServiceLabelValue,
		"llm-d.ai/model-pool":          poolName,
		"test-resource":                defaultTestResourceLabelValue,
		"llm-d.ai/guide":               defaultGuideLabelValue,
		"llm-d.ai/inference-serving":   defaultLabelValueTrue,
		"llm-d.ai/accelerator-variant": defaultAcceleratorVariantValue,
	}
	if variantName != "" {
		labels["llm-d.ai/variant"] = variantName
	}

	envVars := []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
	}
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if !useSimulator {
		envVars = append(envVars,
			corev1.EnvVar{Name: "HF_HOME", Value: "/model-cache"},
			corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: defaultHFTokenSecretName},
					Key:                  defaultHFTokenSecretKey,
				},
			}},
		)
		volumes = []corev1.Volume{
			{Name: "model-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr("100Gi")}}},
			{Name: "torch-compile-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "metrics-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "triton-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		volumeMounts = []corev1.VolumeMount{
			{Name: "model-storage", MountPath: "/model-cache"},
			{Name: "torch-compile-cache", MountPath: "/.cache"},
			{Name: "metrics-volume", MountPath: "/.config"},
			{Name: "triton-cache", MountPath: "/.triton"},
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                          appLabel,
					"llm-d.ai/inferenceServing":    defaultLabelValueTrue,
					"llm-d.ai/model":               defaultModelServiceLabelValue,
					"llm-d.ai/model-pool":          poolName,
					"llm-d.ai/guide":               defaultGuideLabelValue,
					"llm-d.ai/inference-serving":   defaultLabelValueTrue,
					"llm-d.ai/accelerator-variant": defaultAcceleratorVariantValue,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            appLabel,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Ports: []corev1.ContainerPort{
								{Name: defaultServicePortName, ContainerPort: defaultModelServiceContainerPort, Protocol: corev1.ProtocolTCP},
							},
							Env:          envVars,
							Resources:    buildModelServiceResources(useSimulator),
							VolumeMounts: volumeMounts,
						},
					},
					Volumes:       volumes,
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func resourcePtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// buildModelServiceResources returns resource requirements appropriate for the
// deployment mode. Real vLLM requires a GPU to detect the device type at startup;
// the simulator runs on CPU only.
func buildModelServiceResources(useSimulator bool) corev1.ResourceRequirements {
	if useSimulator {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		}
	}
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("1"),
		},
	}
}

func buildModelServerArgs(modelID string, useSimulator bool, maxNumSeqs int, extraArgs []string) []string {
	return append(buildModelServerBaseArgs(modelID, useSimulator, maxNumSeqs), extraArgs...)
}

func buildModelServerBaseArgs(modelID string, useSimulator bool, maxNumSeqs int) []string {
	if useSimulator {
		// Simulator is configured to be deliberately slow so that Prometheus
		// can observe non-zero KV-cache and queue metrics between scrapes (every 15s).
		// With TTFT=2000ms + ITL=100ms and ~250 output tokens, each request takes ~27s.
		// With max-num-seqs=5, the 5 slots fill quickly and incoming requests queue,
		// producing visible num_requests_waiting and kv_cache_usage_perc metrics.
		//
		// KV cache sizing is critical: the simulator uses reference-counted unique
		// block hashes, so all requests with the same prompt share a single block.
		// The burst load prompt (~8 tokens) with blockSize=8 produces exactly
		// 1 block (8/8 = 1). With kv-cache-size=1 (max 1 block), usage = 1/1 = 100%,
		// which exceeds the WVA saturation spare trigger threshold and fires scale-up.
		// IMPORTANT: The load generator must use /v1/completions (text completion),
		// NOT /v1/chat/completions — the simulator only tracks KV cache for the
		// text completion API.
		// Note: blockSize must be one of {8, 16, 32, 64, 128} per simulator validation.
		const (
			simulatorKVCacheSize = 1        // minimal cache: 1 unique block / 1 max block = 100% usage during load
			simulatorBlockSize   = 8        // minimum valid block size; 8 tokens / 8 = 1 block per request
			simulatorMaxModelLen = 512      // must exceed prompt tokens + max_tokens (burst load uses ~9 + 400 = 409)
			simulatorTTFT        = "2000ms" // time-to-first-token (slow to hold KV cache)
			simulatorITL         = "100ms"  // inter-token latency (slow to keep requests active)
		)
		return []string{
			"--model", modelID,
			"--port", "8000",
			"--time-to-first-token=" + simulatorTTFT,
			"--inter-token-latency=" + simulatorITL,
			"--mode=random",
			"--enable-kvcache",
			fmt.Sprintf("--kv-cache-size=%d", simulatorKVCacheSize),
			fmt.Sprintf("--block-size=%d", simulatorBlockSize),
			"--max-num-seqs", strconv.Itoa(maxNumSeqs),
			"--max-model-len", strconv.Itoa(simulatorMaxModelLen),
		}
	}
	return []string{
		"--model", modelID,
		"--max-num-seqs", strconv.Itoa(maxNumSeqs),
		"--max-model-len", "1024",
		"--served-model-name", modelID,
		"--disable-log-requests",
	}
}
