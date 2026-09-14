package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// Scale-to-zero configuration constants
const (
	// DefaultScaleToZeroRetentionPeriod is the default time to wait after the last request
	// before scaling down to zero replicas. This default applies when scale-to-zero is enabled
	// but no explicit retention period is specified.
	DefaultScaleToZeroRetentionPeriod = 10 * time.Minute

	// DefaultScaleToZeroConfigMapName is the default name of the ConfigMap that stores
	// per-model scale-to-zero configuration.
	DefaultScaleToZeroConfigMapName = "wva-model-scale-to-zero-config"

	// GlobalDefaultsKey is the key in the ConfigMap used to specify global defaults
	// for all models. Models can override these defaults with their specific configuration.
	// This follows the same pattern as saturation-scaling-config (deployed as wva-saturation-scaling-config).
	GlobalDefaultsKey = "default"
)

// ModelScaleToZeroConfig represents the scale-to-zero configuration for a single model.
// Uses pointer for EnableScaleToZero to distinguish between "not set" (nil) and explicitly set to false.
// This allows partial overrides where a model can inherit enableScaleToZero from global defaults
// while overriding only the retentionPeriod.
// Field naming follows saturation-scaling-config (deployed as wva-saturation-scaling-config) convention (snake_case for YAML).
type ModelScaleToZeroConfig struct {
	// ModelID is the unique identifier for the model (only used in override entries)
	ModelID string `yaml:"model_id,omitempty" json:"model_id,omitempty"`
	// Namespace is the namespace for this override (only used in override entries)
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// EnableScaleToZero enables scaling the model to zero replicas when there is no traffic.
	// Use pointer to allow omitting this field and inheriting from global defaults.
	// nil = not set (inherit from defaults), true = enabled, false = disabled
	EnableScaleToZero *bool `yaml:"enable_scale_to_zero,omitempty" json:"enable_scale_to_zero,omitempty"`
	// RetentionPeriod specifies how long to wait after the last request before scaling to zero.
	// This is stored as a string duration (e.g., "5m", "1h", "30s").
	// Empty string = not set (inherit from defaults)
	RetentionPeriod string `yaml:"retention_period,omitempty" json:"retention_period,omitempty"`
}

// ScaleToZeroConfigData holds pre-read scale-to-zero configuration data for all models.
// This follows the project pattern of reading ConfigMaps once per reconcile loop.
// Maps model ID to its configuration.
type ScaleToZeroConfigData map[string]ModelScaleToZeroConfig

// IsScaleToZeroEnabled determines if scale-to-zero is enabled for a specific model.
// Supports partial overrides: if a model config exists but EnableScaleToZero is nil,
// it falls through to check global defaults.
//
// Configuration priority (highest to lowest):
// 1. Per-model configuration in ConfigMap (if EnableScaleToZero is set)
// 2. Global defaults in ConfigMap (under "__defaults__" key)
// 3. WVA_SCALE_TO_ZERO environment variable
// 4. System default (false)
func IsScaleToZeroEnabled(configData ScaleToZeroConfigData, modelID string) bool {
	// Check per-model setting first (highest priority)
	if config, exists := configData[modelID]; exists {
		if config.EnableScaleToZero != nil {
			return *config.EnableScaleToZero
		}
		// If nil, fall through to check global defaults (allows partial override)
	}

	// Check global defaults in ConfigMap (second priority)
	if globalConfig, exists := configData[GlobalDefaultsKey]; exists {
		if globalConfig.EnableScaleToZero != nil {
			return *globalConfig.EnableScaleToZero
		}
	}

	// Fall back to global environment variable (third priority)
	return strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true")
}

// ResolveScaleToZeroEnabled reports whether scale-to-zero is enabled for a model,
// preferring the inline scaleToZero setting on the resolved saturation scaling
// entry when present. When that inline field is nil (or sat is nil), it falls
// back to the separate scale-to-zero ConfigMap and env precedence via
// IsScaleToZeroEnabled. Retention period is unaffected — it always comes from the
// scale-to-zero ConfigMap.
func ResolveScaleToZeroEnabled(sat *SaturationScalingConfig, configData ScaleToZeroConfigData, modelID string) bool {
	if sat != nil && sat.ScaleToZero != nil && sat.ScaleToZero.Enabled != nil {
		return *sat.ScaleToZero.Enabled
	}
	return IsScaleToZeroEnabled(configData, modelID)
}

// ValidateRetentionPeriod validates a retention period string.
// Returns the parsed duration and an error if validation fails.
func ValidateRetentionPeriod(retentionPeriod string) (time.Duration, error) {
	if retentionPeriod == "" {
		return 0, errors.New("retention period cannot be empty")
	}

	duration, err := time.ParseDuration(retentionPeriod)
	if err != nil {
		return 0, fmt.Errorf("invalid duration format: %w", err)
	}

	if duration <= 0 {
		return 0, fmt.Errorf("retention period must be positive, got %v", duration)
	}

	// Warn if retention period is unusually long (> 24 hours)
	if duration > 24*time.Hour {
		ctrl.Log.Info("Retention period is unusually long",
			"retentionPeriod", retentionPeriod,
			"duration", duration,
			"recommendation", "Consider using a shorter period for better resource utilization")
	}

	return duration, nil
}

// ScaleToZeroRetentionPeriod returns the retention period for scale-to-zero for a specific model.
// Configuration priority (highest to lowest):
// 1. Per-model retention period in ConfigMap
// 2. Global defaults retention period in ConfigMap (under "__defaults__" key)
// 3. System default (10 minutes)
func ScaleToZeroRetentionPeriod(configData ScaleToZeroConfigData, modelID string) time.Duration {
	// Check per-model retention period first (highest priority)
	if config, exists := configData[modelID]; exists && config.RetentionPeriod != "" {
		duration, err := ValidateRetentionPeriod(config.RetentionPeriod)
		if err != nil {
			ctrl.Log.Info("Invalid retention period for model, checking global defaults",
				"modelID", modelID,
				"retentionPeriod", config.RetentionPeriod,
				"error", err)
			// Fall through to check global defaults
		} else {
			return duration
		}
	}

	// Check global defaults retention period (second priority)
	if globalConfig, exists := configData[GlobalDefaultsKey]; exists && globalConfig.RetentionPeriod != "" {
		duration, err := ValidateRetentionPeriod(globalConfig.RetentionPeriod)
		if err != nil {
			ctrl.Log.Info("Invalid global default retention period, using system default",
				"retentionPeriod", globalConfig.RetentionPeriod,
				"error", err)
			return DefaultScaleToZeroRetentionPeriod
		}
		return duration
	}

	// Fall back to system default (lowest priority)
	return DefaultScaleToZeroRetentionPeriod
}

// MinNumReplicas returns the minimum number of replicas for a specific model based on
// scale-to-zero configuration. Returns 0 if scale-to-zero is enabled, otherwise returns 1.
func MinNumReplicas(configData ScaleToZeroConfigData, modelID string) int {
	if IsScaleToZeroEnabled(configData, modelID) {
		return 0
	}
	return 1
}

// ParseScaleToZeroConfigMap parses scale-to-zero configuration from a ConfigMap's data.
// The ConfigMap follows the same format as saturation-scaling-config (deployed as wva-saturation-scaling-config):
//   - "default": global defaults for all models
//   - "<override-name>": per-model configuration with model_id field
//
// Returns an empty map if the data is nil or empty.
func ParseScaleToZeroConfigMap(data map[string]string) ScaleToZeroConfigData {
	if data == nil {
		return make(ScaleToZeroConfigData)
	}

	out := make(ScaleToZeroConfigData)
	// Track which keys define which modelIDs to detect duplicates
	modelIDToKeys := make(map[string][]string)

	// Sort keys to ensure deterministic processing order
	// This is critical because map iteration in Go is non-deterministic.
	// If there are duplicate modelIDs, the lexicographically first key will win.
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		configStr := data[key]

		var config ModelScaleToZeroConfig
		if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
			ctrl.Log.Info("Failed to parse scale-to-zero config entry, skipping",
				"key", key,
				"error", err)
			continue
		}

		// Handle global defaults (special key)
		if key == GlobalDefaultsKey {
			out[GlobalDefaultsKey] = config
			continue
		}

		// Handle per-model overrides (must include model_id)
		if config.ModelID == "" {
			ctrl.Log.Info("Skipping scale-to-zero config without model_id field",
				"key", key)
			continue
		}

		// Check for duplicate modelID
		if existingKeys, exists := modelIDToKeys[config.ModelID]; exists {
			ctrl.Log.Info("Duplicate model_id found in scale-to-zero ConfigMap - first key wins",
				"model_id", config.ModelID,
				"winningKey", existingKeys[0],
				"duplicateKey", key)
			// Skip this duplicate - first key already processed wins
			continue
		}
		modelIDToKeys[config.ModelID] = append(modelIDToKeys[config.ModelID], key)

		out[config.ModelID] = config
	}

	ctrl.Log.V(logging.DEBUG).Info("Parsed scale-to-zero config",
		"modelCount", len(out))

	return out
}
