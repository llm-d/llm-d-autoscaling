package pipeline

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
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
//   - LimiterTypeNamespaceInventory: a NamespaceLimiter over a NamespaceInventory,
//     which discovers physical GPUs like the inventory mode but partitions them
//     per namespace by node label selector, so one tenant cannot consume capacity
//     that physically belongs to another's nodes.
//
// The kubeClient is used by both inventory paths (for GPU operator discovery);
// the quota path ignores it. Inline limiter entries are validated at ConfigMap
// parse time (SaturationScalingConfig.validateLimiters), so unknown limiter
// types reaching the default branch represent a programming error.
func NewLimiterFromConfig(cfg *config.Config, kubeClient client.Client) (Limiter, error) {
	switch t := cfg.EffectiveLimiterMode(); t {
	case config.LimiterTypeInventory:
		return newInventoryLimiter(kubeClient), nil
	case config.LimiterTypeQuota:
		return newQuotaLimiter(cfg)
	case config.LimiterTypeNamespaceInventory:
		return newNamespaceInventoryLimiter(cfg, kubeClient)
	default:
		return nil, fmt.Errorf("limiter factory: unknown limiter type %q (valid: %q, %q, %q)",
			t, config.LimiterTypeInventory, config.LimiterTypeQuota, config.LimiterTypeNamespaceInventory)
	}
}

// newNamespaceInventoryLimiter builds the namespace-scoped physical GPU limiter
// from the inline namespace-inventory entry. Node label selectors are compiled
// here so an invalid selector is reported once per rebuild rather than per
// cycle; parse-time validation already rejects them, so reaching the error here
// means the config changed underneath a validated parse.
func newNamespaceInventoryLimiter(cfg *config.Config, kubeClient client.Client) (Limiter, error) {
	entry, ok := cfg.EffectiveNamespaceInventoryEntry()
	if !ok {
		return nil, errors.New("limiter factory: namespace-inventory mode requires an inline " +
			"limiters: namespace-inventory entry on the saturation \"default\" config")
	}
	selectors := make(map[string]labels.Selector, len(entry.Selectors))
	for ns, spec := range entry.Selectors {
		sel, err := spec.Compile()
		if err != nil {
			return nil, fmt.Errorf("limiter factory: invalid node selector for namespace %q: %w", ns, err)
		}
		selectors[ns] = sel
	}
	inv := NewNamespaceInventory("namespace-gpu-inventory",
		discovery.NewK8sWithGpuOperator(kubeClient), sets.New(entry.Exclude...), selectors)
	return NewNamespaceLimiter(inv, NewGreedyBySaturation()), nil
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
