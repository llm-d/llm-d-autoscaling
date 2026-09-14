package pipeline

import (
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

// NewLimiterFromConfig constructs the GPU limiter selected via
// Config.EffectiveLimiterMode — the inline limiters: list on the saturation
// "default" config, or the LimiterTypeInventory default when none is declared.
//
//   - LimiterTypeInventory: TypeInventoryWithUsage + GreedyBySaturation wrapped
//     in a DefaultLimiter. Discovers physical GPUs via the GPU operator.
//   - LimiterTypeQuota: builds one DefaultLimiter per Config.EffectiveQuotaEntries
//     entry, each wrapping a QuotaInventory. Multiple entries are wrapped in a
//     CompositeLimiter that runs them sequentially. Pure operator-declared caps —
//     physical capacity is NOT consulted.
//
// The kubeClient is only used by the inventory path (for GPU operator
// discovery); the quota path ignores it. Inline limiter entries are validated at
// ConfigMap parse time (SaturationScalingConfig.validateLimiters), so unknown
// limiter types reaching the default branch represent a programming error.
func NewLimiterFromConfig(cfg *config.Config, kubeClient client.Client) (Limiter, error) {
	switch t := cfg.EffectiveLimiterMode(); t {
	case config.LimiterTypeInventory:
		return newInventoryLimiter(kubeClient), nil
	case config.LimiterTypeQuota:
		return newQuotaLimiter(cfg)
	default:
		return nil, fmt.Errorf("limiter factory: unknown limiter type %q (valid: %q, %q)",
			t, config.LimiterTypeInventory, config.LimiterTypeQuota)
	}
}

// newInventoryLimiter builds the physical-capacity GPU limiter: a
// TypeInventoryWithUsage (GPUs discovered via the GPU operator) driven by the
// GreedyBySaturation algorithm, wrapped in a DefaultLimiter.
func newInventoryLimiter(kubeClient client.Client) Limiter {
	gpuDiscovery := discovery.NewK8sWithGpuOperator(kubeClient)
	gpuInventory := NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
	gpuAlgorithm := NewGreedyBySaturation()
	return NewDefaultLimiter("gpu-limiter", gpuInventory, gpuAlgorithm)
}

// newQuotaLimiter builds one DefaultLimiter per QuotaLimiterConfig entry.
// Each wraps a QuotaInventory with GreedyBySaturation. When more than one
// entry is configured, the result is wrapped in a CompositeLimiter so they
// run in declaration order against the shared decisions slice.
func newQuotaLimiter(cfg *config.Config) (Limiter, error) {
	entries := cfg.EffectiveQuotaEntries()
	if len(entries) == 0 {
		return nil, errors.New("limiter factory: quota mode requires at least one inline " +
			"limiters: quota entry on the saturation \"default\" config")
	}
	constituents := make([]Limiter, 0, len(entries))
	for _, entry := range entries {
		inv := NewQuotaInventory(entry)
		// One algorithm instance per constituent — GreedyBySaturation is
		// stateless today, but a per-limiter instance avoids a shared-state
		// surprise if that ever changes.
		constituents = append(constituents, NewDefaultLimiter(entry.Name, inv, NewGreedyBySaturation()))
	}
	if len(constituents) == 1 {
		return constituents[0], nil
	}
	return NewCompositeLimiter("quota-limiter", constituents), nil
}
