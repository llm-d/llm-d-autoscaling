package resources

import (
	"fmt"
	"strconv"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	SimulatorImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.9.0"

	// gpuResourceDefault is the extended K8s resource type requested when none is specified.
	gpuResourceDefault = "nvidia.com/gpu"
)

// creates a llm-d-sim deployment with the specified configuration
func CreateLlmdSimDeployment(namespace, deployName, modelName, appLabel, port string, avgTTFT, avgITL int, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                       appLabel,
						"llm-d.ai/inferenceServing": "true",
						"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            appLabel,
							Image:           SimulatorImage,
							ImagePullPolicy: corev1.PullAlways,
							Args: []string{
								"--model",
								modelName,
								"--port",
								port,
								fmt.Sprintf("--time-to-first-token=%dms", avgTTFT),
								fmt.Sprintf("--inter-token-latency=%dms", avgITL),
								"--mode=random",
								"--enable-kvcache",
								"--kv-cache-size=1024",
								"--block-size=16",
							},
							Env: []corev1.EnvVar{
								{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "metadata.name",
									},
								}},
								{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "metadata.namespace",
									},
								}},
								{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "status.podIP",
									},
								}},
							},
							Ports: []corev1.ContainerPort{
								{ContainerPort: 8000, Name: "http", Protocol: corev1.ProtocolTCP},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

// CreateLlmdSimDeploymentWithGPU creates a llm-d-sim deployment with GPU resource requests.
// gpusPerReplica specifies the number of GPUs to request per replica.
// gpuResource specifies the GPU (vendor specific) extended K8s resource type to request.
// If gpusPerReplica is 0, no GPU resources are requested (same as CreateLlmdSimDeployment).
func CreateLlmdSimDeploymentWithGPU(namespace, deployName, modelName, appLabel, port string, avgTTFT, avgITL int, replicas int32, gpusPerReplica int, gpuResource string) *appsv1.Deployment {
	if gpuResource == "" {
		gpuResource = gpuResourceDefault
	}
	gpuResourceName := corev1.ResourceName(gpuResource)

	container := corev1.Container{
		Name:            appLabel,
		Image:           SimulatorImage,
		ImagePullPolicy: corev1.PullAlways,
		Args: []string{
			"--model",
			modelName,
			"--port",
			port,
			fmt.Sprintf("--time-to-first-token=%dms", avgTTFT),
			fmt.Sprintf("--inter-token-latency=%dms", avgITL),
			"--mode=random",
			"--enable-kvcache",
			"--kv-cache-size=1024",
			"--block-size=16",
		},
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			}},
			{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIP",
				},
			}},
		},
		Ports: []corev1.ContainerPort{
			{ContainerPort: 8000, Name: "http", Protocol: corev1.ProtocolTCP},
		},
	}

	// Add GPU resource requests if specified
	if gpusPerReplica > 0 {
		gpuQty := resource.MustParse(strconv.Itoa(gpusPerReplica))
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				gpuResourceName: gpuQty,
			},
			Limits: corev1.ResourceList{
				gpuResourceName: gpuQty,
			},
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                       appLabel,
						"llm-d.ai/inferenceServing": "true",
						"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					},
				},
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{container},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

// CreateLlmdSimDeploymentWithGPUAndNodeSelector creates a deployment with GPU resources
// and node selector to target specific GPU configurations.
// nodeSelector maps label keys to values (e.g., "gpu-config": "4H100")
func CreateLlmdSimDeploymentWithGPUAndNodeSelector(
	namespace, deployName, modelName, appLabel, port string,
	avgTTFT, avgITL int, replicas int32,
	gpusPerReplica int, gpuResource string,
	nodeSelector map[string]string,
) *appsv1.Deployment {
	deployment := CreateLlmdSimDeploymentWithGPU(
		namespace, deployName, modelName, appLabel, port,
		avgTTFT, avgITL, replicas, gpusPerReplica, gpuResource,
	)

	if len(nodeSelector) > 0 {
		deployment.Spec.Template.Spec.NodeSelector = nodeSelector
		// Add tolerations for control-plane nodes as H100s might be on control-plane in kind-emulator
		deployment.Spec.Template.Spec.Tolerations = []corev1.Toleration{
			{
				Key:      "node-role.kubernetes.io/control-plane",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
			{
				Key:      "node-role.kubernetes.io/master",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}
	}

	return deployment
}

// CreateLlmdSimDeploymentWithGPUAndLabels creates a deployment with GPU resources,
// node selector, and additional pod labels for InferencePool selection.
// extraLabels are added to both selector and template labels.
func CreateLlmdSimDeploymentWithGPUAndLabels(
	namespace, deployName, modelName, appLabel, port string,
	avgTTFT, avgITL int, replicas int32,
	gpusPerReplica int, gpuResource string,
	nodeSelector map[string]string,
	extraLabels map[string]string,
) *appsv1.Deployment {
	deployment := CreateLlmdSimDeploymentWithGPUAndNodeSelector(
		namespace, deployName, modelName, appLabel, port,
		avgTTFT, avgITL, replicas, gpusPerReplica, gpuResource,
		nodeSelector,
	)

	// Add extra labels to both selector and template labels
	if len(extraLabels) > 0 {
		for k, v := range extraLabels {
			deployment.Spec.Selector.MatchLabels[k] = v
			deployment.Spec.Template.Labels[k] = v
		}
	}

	return deployment
}

// creates a service for the llm-d-sim deployment
func CreateLlmdSimService(namespace, serviceName, appLabel string, nodePort, port int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       int32(port),
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt32(int32(port)),
					NodePort:   int32(nodePort),
				},
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}
}

// creates a ServiceMonitor for llm-d-sim metrics collection
func CreateLlmdSimServiceMonitor(name, namespace, targetNamespace, appLabel string) *promoperator.ServiceMonitor {
	return &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":     appLabel,
				"release": "kube-prometheus-stack",
			},
		},
		Spec: promoperator.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": appLabel,
				},
			},
			Endpoints: []promoperator.Endpoint{
				{
					Port:     "http",
					Path:     "/metrics",
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
			NamespaceSelector: promoperator.NamespaceSelector{
				MatchNames: []string{targetNamespace},
			},
		},
	}
}
