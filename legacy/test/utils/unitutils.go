package utils

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The following utility functions are used to create Prometheus queries for testing
func CreateArrivalQuery(modelID, namespace string) string {
	return fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace)
}

func CreatePromptToksQuery(modelID, namespace string) string {
	return fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestPromptTokensSum,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace,
		constants.VLLMRequestPromptTokensCount,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace)
}

func CreateDecToksQuery(modelID, namespace string) string {
	return fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestGenerationTokensSum,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace,
		constants.VLLMRequestGenerationTokensCount,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace)
}

func CreateTTFTQuery(modelID, namespace string) string {
	return fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace)
}

func CreateITLQuery(modelID, namespace string) string {
	return fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimePerOutputTokenSecondsSum,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace,
		constants.VLLMTimePerOutputTokenSecondsCount,
		constants.LabelModelName, modelID,
		constants.LabelNamespace, namespace)
}

// createServiceClassConfigMap creates the serviceclass ConfigMap
func CreateServiceClassConfigMap(controllerNamespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-classes-config",
			Namespace: controllerNamespace,
		},
		Data: map[string]string{
			"premium.yaml": `name: Premium
priority: 1
data:
  - model: default-default
    slo-tpot: 24
    slo-ttft: 500
  - model: meta/llama0-70b
    slo-tpot: 80
    slo-ttft: 500`,
			"freemium.yaml": `name: Freemium
priority: 10
data:
  - model: ibm/granite-13b
    slo-tpot: 200
    slo-ttft: 2000
  - model: meta/llama0-7b
    slo-tpot: 150
    slo-ttft: 1500`,
		},
	}
}

func CreateVariantAutoscalingConfigMap(cmName, controllerNamespace string) *corev1.ConfigMap {

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: controllerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "workload-variant-autoscaler",
			},
		},
		Data: map[string]string{
			"PROMETHEUS_BASE_URL": "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090",
			"GLOBAL_OPT_INTERVAL": "60s",
			"GLOBAL_OPT_TRIGGER":  "false",
			"WVA_SCALE_TO_ZERO":   "false",
			"DISABLING_TTFT":      "false", // TODO: this will be removed in future releases once llm-d-sim supports TTFT metrics
		},
	}
}

// MakeService creates a wrapper for a Service.
func MakeService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "epp-svc", // Matches the labels on the target Pods
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http-metrics",
					Protocol:   corev1.ProtocolTCP,
					Port:       9090,
					TargetPort: intstr.FromInt(9090),
					NodePort:   30007,
				},
			},
			Type: corev1.ServiceTypeNodePort, // Exposes the service on a static port on each Node's IP
		},
	}
}

func MakeDeployment(deploymentName, namespace string, replicas int32, labels map[string]string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "pool1",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "pool1",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "vllm",
							Image:           "ghcr.io/llm-d/llm-d-inference-sim:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								"--port=8000",
								"--model=unsloth/Meta-Llama-3.1-8B",
								"--enable-kvcache=false",
								"--kv-cache-size=1024",
								"--block-size=16",
								"--zmq-endpoint=tcp://epp-pool1-svc.pool1-ns.svc.cluster.local:5557",
								"--event-batch-size=16",
								"--tokenizers-cache-dir=/tokenizer-cache",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 8000,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "PORT",
									Value: "8000",
								},
								{
									Name:  "PYTHONHASHSEED",
									Value: "42",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/tokenizer-cache",
									Name:      "tokenizer-cache",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name:         "tokenizer-cache",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
	return deployment
}

// MockPromAPI is a mock implementation of promv1.API for testing
type MockPromAPI struct {
	QueryResults    map[string]model.Value
	QueryErrors     map[string]error
	QueryCallCounts map[string]int // Track number of calls per query for retry testing
	QueryFailCounts map[string]int // Number of times to fail before succeeding (for retry testing)
	mu              sync.RWMutex
}

func (m *MockPromAPI) Query(ctx context.Context, query string, ts time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	m.mu.Lock()
	// Initialize call count if not exists
	if m.QueryCallCounts == nil {
		m.QueryCallCounts = make(map[string]int)
	}

	m.QueryCallCounts[query]++
	callCount := m.QueryCallCounts[query]

	// Check if this query should fail a certain number of times before succeeding
	var shouldFail bool
	var failCount int
	if m.QueryFailCounts != nil {
		if fc, exists := m.QueryFailCounts[query]; exists {
			failCount = fc
			if callCount <= failCount {
				shouldFail = true
			}
		}
	}

	// Check for permanent errors
	var permErr error
	if err, exists := m.QueryErrors[query]; exists {
		permErr = err
	}

	// Get successful result
	var result model.Value
	var hasResult bool
	if val, exists := m.QueryResults[query]; exists {
		result = val
		hasResult = true
	}

	m.mu.Unlock()

	if shouldFail {
		return nil, nil, fmt.Errorf("transient error (attempt %d/%d)", callCount, failCount+1)
	}

	if permErr != nil {
		return nil, nil, permErr
	}

	if hasResult {
		return result, nil, nil
	}

	// Default return vector with one sample (to pass metrics validation)
	// This simulates Prometheus having scraped at least one metric
	return model.Vector{
		&model.Sample{
			Metric:    model.Metric{},
			Value:     0,
			Timestamp: model.TimeFromUnix(ts.Unix()),
		},
	}, nil, nil
}

func (m *MockPromAPI) QueryRange(ctx context.Context, query string, r promv1.Range, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) QueryExemplars(ctx context.Context, query string, startTime, endTime time.Time) ([]promv1.ExemplarQueryResult, error) {
	return nil, nil
}

func (m *MockPromAPI) Buildinfo(ctx context.Context) (promv1.BuildinfoResult, error) {
	return promv1.BuildinfoResult{}, nil
}

func (m *MockPromAPI) Config(ctx context.Context) (promv1.ConfigResult, error) {
	return promv1.ConfigResult{}, nil
}

func (m *MockPromAPI) Flags(ctx context.Context) (promv1.FlagsResult, error) {
	return promv1.FlagsResult{}, nil
}

func (m *MockPromAPI) LabelNames(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...promv1.Option) ([]string, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) LabelValues(ctx context.Context, label string, matches []string, startTime, endTime time.Time, opts ...promv1.Option) (model.LabelValues, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) Series(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...promv1.Option) ([]model.LabelSet, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) GetValue(ctx context.Context, timestamp time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) Metadata(ctx context.Context, metric, limit string) (map[string][]promv1.Metadata, error) {
	return nil, nil
}

func (m *MockPromAPI) TSDB(ctx context.Context, opts ...promv1.Option) (promv1.TSDBResult, error) {
	return promv1.TSDBResult{}, nil
}

func (m *MockPromAPI) WalReplay(ctx context.Context) (promv1.WalReplayStatus, error) {
	return promv1.WalReplayStatus{}, nil
}

func (m *MockPromAPI) Targets(ctx context.Context) (promv1.TargetsResult, error) {
	return promv1.TargetsResult{}, nil
}

func (m *MockPromAPI) TargetsMetadata(ctx context.Context, matchTarget, metric, limit string) ([]promv1.MetricMetadata, error) {
	return nil, nil
}

func (m *MockPromAPI) AlertManagers(ctx context.Context) (promv1.AlertManagersResult, error) {
	return promv1.AlertManagersResult{}, nil
}

func (m *MockPromAPI) CleanTombstones(ctx context.Context) error {
	return nil
}

func (m *MockPromAPI) DeleteSeries(ctx context.Context, matches []string, startTime, endTime time.Time) error {
	return nil
}

func (m *MockPromAPI) Snapshot(ctx context.Context, skipHead bool) (promv1.SnapshotResult, error) {
	return promv1.SnapshotResult{}, nil
}

func (m *MockPromAPI) Rules(ctx context.Context) (promv1.RulesResult, error) {
	return promv1.RulesResult{}, nil
}

func (m *MockPromAPI) Alerts(ctx context.Context) (promv1.AlertsResult, error) {
	return promv1.AlertsResult{}, nil
}

func (m *MockPromAPI) Runtimeinfo(ctx context.Context) (promv1.RuntimeinfoResult, error) {
	return promv1.RuntimeinfoResult{}, nil
}
