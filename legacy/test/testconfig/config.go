package testconfig

import (
	"os"
	"strconv"
	"strings"
)

// SharedConfig holds configuration fields common to both e2e and benchmark tests,
// loaded from environment variables.
type SharedConfig struct {
	// Cluster info
	Environment string // "kind", "openshift", "kubernetes"
	Kubeconfig  string

	// Namespaces
	WVANamespace  string // WVA controller namespace
	LLMDNamespace string // llm-d infrastructure namespace
	MonitoringNS  string // Prometheus namespace

	// Infrastructure mode
	UseSimulator bool   // true for emulated GPUs, false for real vLLM
	GPUType      string // "nvidia-mix", "amd-mix", "real"...

	// Scaler backend. KEDA (ScaledObject) is the only supported backend; the
	// field is retained for diagnostics and future backend selection.
	ScalerBackend string
	// KEDANamespace is the namespace where KEDA is installed
	KEDANamespace string

	// EPP configuration
	EPPMode          string            // "poolName" or "endpointSelector"
	PoolName         string            // InferencePool name (if using poolName mode)
	EndpointSelector map[string]string // Pod selector (if using endpointSelector)
	EPPServiceName   string            // EPP service name (e.g. optimized-baseline-epp; matches GAIE chart release name + "-epp")

	// Model configuration
	ModelID         string // e.g., "e2ewva/dummy-model" (dummy name avoids vLLM Render sidecar requirement in v0.9.0+)
	AcceleratorType string // e.g., "H100", "A100" (must be valid Kubernetes label value)
	MaxNumSeqs      int    // vLLM batch size (lower = easier to saturate)

	// Load generation
	LoadStrategy string // "synthetic", "sharegpt"
	RequestRate  int    // Requests per second
	NumPrompts   int    // Total number of requests
	InputTokens  int    // Average input tokens
	OutputTokens int    // Average output tokens

	// Controller isolation
	ControllerInstance string // Controller instance label for multi-controller filtering
}

// LoadSharedConfig reads the shared test configuration from environment variables.
func LoadSharedConfig() SharedConfig {
	env := GetEnv("ENVIRONMENT", "kind-emulator")
	eppServiceDefault := "optimized-baseline-epp"

	return SharedConfig{
		Environment: env,
		Kubeconfig:  GetEnv("KUBECONFIG", os.Getenv("HOME")+"/.kube/config"),

		WVANamespace:  GetEnv("WVA_NAMESPACE", "workload-variant-autoscaler-system"),
		LLMDNamespace: GetEnv("LLMD_NAMESPACE", "llm-d-sim"),
		MonitoringNS:  GetEnv("MONITORING_NAMESPACE", "workload-variant-autoscaler-monitoring"),

		UseSimulator: GetEnvBool("USE_SIMULATOR", true),
		GPUType:      GetEnv("GPU_TYPE", "nvidia-mix"),

		ScalerBackend: GetEnv("SCALER_BACKEND", "keda"),
		KEDANamespace: GetEnv("KEDA_NAMESPACE", "keda-system"),

		EPPMode:          GetEnv("EPP_MODE", "poolName"),
		PoolName:         GetEnv("POOL_NAME", ""),
		EndpointSelector: ParseEndpointSelector(GetEnv("ENDPOINT_SELECTOR", "")),
		EPPServiceName:   GetEnv("EPP_SERVICE_NAME", eppServiceDefault),

		ModelID:         GetEnv("MODEL_ID", "e2ewva/dummy-model"),
		AcceleratorType: GetEnv("ACCELERATOR_TYPE", "H100"),
		MaxNumSeqs:      GetEnvInt("MAX_NUM_SEQS", 5),

		LoadStrategy: GetEnv("LOAD_STRATEGY", "synthetic"),
		RequestRate:  GetEnvInt("REQUEST_RATE", 8),
		NumPrompts:   GetEnvInt("NUM_PROMPTS", 1000),
		InputTokens:  GetEnvInt("INPUT_TOKENS", 100),
		OutputTokens: GetEnvInt("OUTPUT_TOKENS", 50),

		ControllerInstance: GetEnv("CONTROLLER_INSTANCE", ""),
	}
}

// GetEnv returns the value of an environment variable, or a default value if not set.
func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// GetEnvBool returns the boolean value of an environment variable, or a default value if not set.
func GetEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

// GetEnvInt returns the integer value of an environment variable, or a default value if not set.
func GetEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

// ParseEndpointSelector parses a comma-separated list of key=value pairs into a map.
func ParseEndpointSelector(value string) map[string]string {
	if value == "" {
		return nil
	}

	selector := make(map[string]string)
	pairs := strings.Split(value, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			selector[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return selector
}
