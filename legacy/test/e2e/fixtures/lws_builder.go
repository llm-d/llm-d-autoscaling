package fixtures

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// EnsureModelServiceLWS creates or replaces a LeaderWorkerSet for model service (idempotent for test setup).
// variantName is stamped as the llm-d.ai/variant label on both leader and worker pod templates
// so the collector can attribute pod metrics to the right annotated scaler. Pass "" to omit.
func EnsureModelServiceLWS(ctx context.Context, crClient client.Client, namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int, groupSize int32) error {
	lwsName := name + decodeNameSuffix
	desiredLWS := buildModelServiceLWS(namespace, name, poolName, modelID, variantName, useSimulator, maxNumSeqs, groupSize)

	// Check if LWS already exists
	existingLWS := &lwsv1.LeaderWorkerSet{}
	err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: namespace}, existingLWS)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing LWS %s: %w", lwsName, err)
		}
	} else {
		// LWS exists, check if it's ready
		if existingLWS.Status.ReadyReplicas > 0 && modelServiceLWSMatchesDesired(*existingLWS, *desiredLWS) {
			return nil
		}
		// Not ready, delete and recreate
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := crClient.Delete(ctx, existingLWS, &client.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) && !errors.IsConflict(deleteErr) {
			return fmt.Errorf("delete existing LWS %s: %w", lwsName, deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			checkErr := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: namespace}, &lwsv1.LeaderWorkerSet{})
			if errors.IsNotFound(checkErr) {
				return true, nil
			}
			if checkErr != nil {
				return false, checkErr
			}
			return false, nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for LWS %s to be deleted: %w", lwsName, waitErr)
		}
	}

	err = crClient.Create(ctx, desiredLWS)
	if err != nil && errors.IsAlreadyExists(err) {
		propagationPolicy := metav1.DeletePropagationForeground
		_ = crClient.Delete(ctx, desiredLWS, &client.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		waitErr := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			checkErr := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: namespace}, &lwsv1.LeaderWorkerSet{})
			if errors.IsNotFound(checkErr) {
				return true, nil
			}
			if checkErr != nil {
				return false, checkErr
			}
			return false, nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for LWS %s to be deleted before recreate: %w", lwsName, waitErr)
		}
		err = crClient.Create(ctx, desiredLWS)
	}
	return err
}

func modelServiceLWSMatchesDesired(existing, desired lwsv1.LeaderWorkerSet) bool {
	return apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		apiequality.Semantic.DeepEqual(existing.Annotations, desired.Annotations)
}

// DeleteModelServiceLWS deletes the LeaderWorkerSet for model service. Idempotent; ignores NotFound.
func DeleteModelServiceLWS(ctx context.Context, crClient client.Client, namespace, name string) error {
	lwsName := name + decodeNameSuffix
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lwsName,
			Namespace: namespace,
		},
	}
	err := crClient.Delete(ctx, lws)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete LeaderWorkerSet %s: %w", lwsName, err)
	}
	return nil
}

func buildModelServiceLWS(namespace, name, poolName, modelID, variantName string, useSimulator bool, maxNumSeqs int, groupSize int32) *lwsv1.LeaderWorkerSet {
	appLabel := name + decodeNameSuffix
	image := defaultModelServiceSimulatorImage
	if !useSimulator {
		image = defaultModelServiceRuntimeImage
	}
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs, nil)
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

	// Leader template (runs vLLM API server)
	leaderTemplate := &corev1.PodTemplateSpec{
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
	}

	// Worker template (can be same as leader for simple cases, or different for distributed inference)
	workerTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            appLabel + "-worker",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            args,
					Env:             envVars,
					Resources:       buildModelServiceResources(useSimulator),
					VolumeMounts:    volumeMounts,
				},
			},
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}

	startupPolicy := lwsv1.LeaderReadyStartupPolicy

	return &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas:      ptr.To(int32(1)),
			StartupPolicy: startupPolicy,
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size:           ptr.To(groupSize),
				LeaderTemplate: leaderTemplate,
				WorkerTemplate: workerTemplate,
			},
		},
	}
}
