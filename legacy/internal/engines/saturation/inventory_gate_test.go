package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("shouldCollectClusterInventory", func() {
	// withLimiters returns a test Config whose global "default" saturation entry
	// declares the given inline limiters.
	withLimiters := func(limiters ...config.QuotaLimiterConfig) *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {Limiters: limiters},
		})
		return cfg
	}

	It("collects inventory when no limiters are configured (inventory default)", func() {
		Expect(shouldCollectClusterInventory(config.NewTestConfig())).To(BeTrue())
	})

	It("collects inventory when a gpu-inventory limiter is configured", func() {
		cfg := withLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"})
		Expect(shouldCollectClusterInventory(cfg)).To(BeTrue())
	})

	It("skips inventory collection when an inline quota limiter is configured", func() {
		cfg := withLimiters(config.QuotaLimiterConfig{
			Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 8},
		})
		Expect(shouldCollectClusterInventory(cfg)).To(BeFalse())
	})
})
