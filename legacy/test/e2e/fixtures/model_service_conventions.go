package fixtures

const (
	// decodeNameSuffix follows the llm-d fixture convention where model-serving
	// workloads are named with "<base>-decode".
	decodeNameSuffix = "-decode"
	// serviceNameSuffix follows the fixture convention where Service names are
	// derived from "<base>-service".
	serviceNameSuffix = "-service"
	// serviceMonitorNameSuffix follows the fixture convention "<base>-monitor".
	serviceMonitorNameSuffix = "-monitor"

	// kubePrometheusStackReleaseLabelValue matches the Helm release label used by
	// kube-prometheus-stack ServiceMonitor discovery.
	kubePrometheusStackReleaseLabelValue = "kube-prometheus-stack"
	defaultServicePortName               = "http"
	defaultServiceMonitorMetricsPath     = "/metrics"

	defaultModelServiceSimulatorImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.9.0"
	defaultModelServiceRuntimeImage   = "ghcr.io/llm-d/llm-d-cuda-dev:latest"
	defaultModelServiceContainerPort  = 8000
	defaultHFTokenSecretName          = "llm-d-hf-token"
	defaultHFTokenSecretKey           = "HF_TOKEN"
	defaultModelServiceLabelValue     = "random"
	defaultLabelValueTrue             = "true"
	defaultGuideLabelValue            = "optimized-baseline" // matches v0.7.0 InferencePool selector (llm-d.ai/guide: optimized-baseline)
	defaultTestResourceLabelValue     = defaultLabelValueTrue
	// InferencePool selector label for accelerator variant
	defaultAcceleratorVariantValue = "cpu"
)
