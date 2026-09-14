package config

import (
	"os"
	"strconv"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// DefaultConfigMapName is the default name of the ConfigMap containing autoscaler configuration
	DefaultConfigMapName = "wva-manager-config"
	// DefaultSaturationConfigMapName is the default name of the ConfigMap for saturation scaling
	DefaultSaturationConfigMapName = "wva-saturation-scaling-config"
	// DefaultQMAnalyzerConfigMapName is the default name of the ConfigMap for queueing model based scaling
	DefaultQMAnalyzerConfigMapName = "wva-queueing-model-config"
	// DefaultNamespace is the default namespace for the controller
	DefaultNamespace = "workload-variant-autoscaler-system"
)

// ConfigValue retrieves a value from a ConfigMap with a default fallback
func ConfigValue(data map[string]string, key, def string) string {
	if v, ok := data[key]; ok {
		return v
	}
	return def
}

// ParseDurationFromConfig parses a duration string from ConfigMap with default fallback
// Returns the parsed duration or the default value if parsing fails or key is missing
func ParseDurationFromConfig(data map[string]string, key string, defaultValue time.Duration) time.Duration {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		if val, err := time.ParseDuration(valStr); err == nil {
			return val
		}
		ctrl.Log.Info("Invalid duration value in ConfigMap, using default", "value", valStr, "key", key, "default", defaultValue)
	}
	return defaultValue
}

// ParseIntFromConfig parses an integer from ConfigMap with default fallback and minimum value validation
// Returns the parsed integer or the default value if parsing fails, is less than minValue, or key is missing
func ParseIntFromConfig(data map[string]string, key string, defaultValue int, minValue int) int {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		if val, err := strconv.Atoi(valStr); err == nil && val >= minValue {
			return val
		}
		ctrl.Log.Info("Invalid integer value in ConfigMap, using default", "value", valStr, "key", key, "minValue", minValue, "default", defaultValue)
	}
	return defaultValue
}

// ParseBoolFromConfig parses a boolean from ConfigMap with default fallback
// Accepts "true", "1", or "yes" as true values (case-sensitive)
// Returns the parsed boolean or the default value if key is missing or value is not recognized
func ParseBoolFromConfig(data map[string]string, key string, defaultValue bool) bool {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		// Accept "true", "1", "yes" as true
		return valStr == "true" || valStr == "1" || valStr == "yes"
	}
	return defaultValue
}

// SystemNamespace returns the controller's system namespace where WVA is deployed.
// This is the namespace containing global ConfigMaps and the controller deployment.
//
// Returns: POD_NAMESPACE environment variable if set, otherwise "workload-variant-autoscaler-system"
func SystemNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// ConfigMapName returns the main ConfigMap name from environment variable or default (DefaultConfigMapName).
// Set CONFIG_MAP_NAME in the deployment manifest to use a non-default name.
func ConfigMapName() string {
	if name := os.Getenv("CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultConfigMapName
}

// SaturationConfigMapName returns the saturation scaling ConfigMap name from environment variable or default.
func SaturationConfigMapName() string {
	if name := os.Getenv("SATURATION_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultSaturationConfigMapName
}

// QMAnalyzerConfigMapName returns the queueing model config ConfigMap name from environment variable or default.
func QMAnalyzerConfigMapName() string {
	if name := os.Getenv("QUEUEING_MODEL_CONFIG_MAP_NAME"); name != "" {
		return name // TODO: check setting QUEUEING_MODEL_CONFIG_MAP_NAME
	}
	return DefaultQMAnalyzerConfigMapName
}
