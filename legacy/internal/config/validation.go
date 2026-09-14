package config

import (
	"errors"
	"fmt"
	"strings"
)

// Validate performs validation on the loaded configuration.
// It returns an error if any required configuration is missing or invalid.
// This implements fail-fast behavior: the controller should not start with invalid configuration.
func Validate(cfg *Config) error {
	// Prometheus config is required
	if cfg.PrometheusBaseURL() == "" {
		return errors.New("prometheus BaseURL is required")
	}

	// Optimization interval must be positive. Configs built by Load never trip
	// this: sanitizeOptimizationInterval already replaced anything below
	// MinOptimizationInterval with the default. This guards a Config assembled
	// directly in code.
	interval := cfg.OptimizationInterval()
	if interval <= 0 {
		return fmt.Errorf("optimization interval must be positive, got %v", interval)
	}

	// Scale-from-zero max concurrency must be positive
	if cfg.ScaleFromZeroMaxConcurrency() <= 0 {
		return fmt.Errorf("scale-from-zero max concurrency must be positive, got %d", cfg.ScaleFromZeroMaxConcurrency())
	}

	// Note: the GPU limiter selection is not validated here. It is driven entirely
	// by the saturation ConfigMap's limiters: list (see SaturationScalingConfig.
	// validateLimiters, run at ConfigMap parse time) and is applied live, so there
	// is no startup flag to validate.

	return nil
}

// ImmutableParameterChange represents a detected attempt to change an immutable parameter.
type ImmutableParameterChange struct {
	Key       string
	OldValue  string
	NewValue  string
	Parameter string // Human-readable parameter name
}

// DetectImmutableParameterChanges detects if a ConfigMap is attempting to change
// immutable parameters that require a controller restart.
//
// Immutable parameters (require restart):
// - PROMETHEUS_BASE_URL (connection endpoint)
// - METRICS_BIND_ADDRESS (infrastructure)
// - HEALTH_PROBE_BIND_ADDRESS (infrastructure)
// - LEADER_ELECTION_ID (coordination)
// - TLS certificate paths (security-sensitive)
//
// Returns:
// - A list of detected immutable parameter changes
// - An error if any immutable parameters are being changed
//
// This function should be called by the ConfigMap handler before applying updates
// to detect and reject attempts to change immutable parameters at runtime.
func DetectImmutableParameterChanges(cfg *Config, configMapData map[string]string) ([]ImmutableParameterChange, error) {
	var changes []ImmutableParameterChange

	// Check PROMETHEUS_BASE_URL
	if newURL, ok := configMapData["PROMETHEUS_BASE_URL"]; ok {
		currentURL := cfg.PrometheusBaseURL()
		if newURL != currentURL {
			changes = append(changes, ImmutableParameterChange{
				Key:       "PROMETHEUS_BASE_URL",
				OldValue:  currentURL,
				NewValue:  newURL,
				Parameter: "Prometheus BaseURL",
			})
		}
	}

	// Check METRICS_BIND_ADDRESS
	if newAddr, ok := configMapData["METRICS_BIND_ADDRESS"]; ok {
		currentAddr := cfg.MetricsAddr()
		if newAddr != currentAddr {
			changes = append(changes, ImmutableParameterChange{
				Key:       "METRICS_BIND_ADDRESS",
				OldValue:  currentAddr,
				NewValue:  newAddr,
				Parameter: "Metrics bind address",
			})
		}
	}

	// Check HEALTH_PROBE_BIND_ADDRESS
	if newAddr, ok := configMapData["HEALTH_PROBE_BIND_ADDRESS"]; ok {
		currentAddr := cfg.ProbeAddr()
		if newAddr != currentAddr {
			changes = append(changes, ImmutableParameterChange{
				Key:       "HEALTH_PROBE_BIND_ADDRESS",
				OldValue:  currentAddr,
				NewValue:  newAddr,
				Parameter: "Health probe bind address",
			})
		}
	}

	// Check LEADER_ELECTION_ID
	if newID, ok := configMapData["LEADER_ELECTION_ID"]; ok {
		currentID := cfg.LeaderElectionID()
		if newID != currentID {
			changes = append(changes, ImmutableParameterChange{
				Key:       "LEADER_ELECTION_ID",
				OldValue:  currentID,
				NewValue:  newID,
				Parameter: "Leader election ID",
			})
		}
	}

	// Check TLS certificate paths (if they exist in ConfigMap)
	// Note: These are typically set via CLI flags, but we check for completeness
	tlsKeys := []struct {
		key       string
		getter    func() string
		paramName string
	}{
		{"WEBHOOK_CERT_PATH", cfg.WebhookCertPath, "Webhook certificate path"},
		{"WEBHOOK_CERT_NAME", cfg.WebhookCertName, "Webhook certificate name"},
		{"WEBHOOK_CERT_KEY", cfg.WebhookCertKey, "Webhook certificate key"},
		{"METRICS_CERT_PATH", cfg.MetricsCertPath, "Metrics certificate path"},
		{"METRICS_CERT_NAME", cfg.MetricsCertName, "Metrics certificate name"},
		{"METRICS_CERT_KEY", cfg.MetricsCertKey, "Metrics certificate key"},
	}

	for _, tlsKey := range tlsKeys {
		if newValue, ok := configMapData[tlsKey.key]; ok {
			currentValue := tlsKey.getter()
			if newValue != currentValue {
				changes = append(changes, ImmutableParameterChange{
					Key:       tlsKey.key,
					OldValue:  currentValue,
					NewValue:  newValue,
					Parameter: tlsKey.paramName,
				})
			}
		}
	}

	// If any immutable changes detected, return error
	if len(changes) > 0 {
		changeList := make([]string, 0, len(changes))
		for _, change := range changes {
			changeList = append(changeList, fmt.Sprintf("%s (old: %q, new: %q)", change.Parameter, change.OldValue, change.NewValue))
		}
		return changes, fmt.Errorf("attempted to change immutable parameters that require controller restart: %s. Please restart the controller to apply these changes", strings.Join(changeList, "; "))
	}

	return nil, nil
}
