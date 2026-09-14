package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// shouldCollectClusterInventory reports whether the per-cycle
// collector.CollectInventoryK8S call is appropriate for the active
// configuration. The intent is to keep quota-mode deployments free of any
// Node API traffic: when quota mode is active, the physical-capacity path is
// deliberately disabled and listing Nodes here (even just for logging) would
// defeat that contract.
//
// It reads EffectiveLimiterMode so it stays consistent with the limiter that
// NewLimiterFromConfig actually builds — both are driven by the same inline
// limiters: list on the saturation "default" config, live.
func shouldCollectClusterInventory(cfg *config.Config) bool {
	return cfg.EffectiveLimiterMode() == config.LimiterTypeInventory
}
