package pipeline

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// configWithLimiters builds a test Config whose global "default" saturation
// entry declares the given inline limiters — the sole source of limiter
// selection that NewLimiterFromConfig consults (via EffectiveLimiterMode /
// EffectiveQuotaEntries).
func configWithLimiters(limiters ...config.QuotaLimiterConfig) *config.Config {
	GinkgoHelper()
	cfg := config.NewTestConfig()
	cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
		"default": {Limiters: limiters},
	})
	return cfg
}

var _ = Describe("NewLimiterFromConfig", func() {

	It("returns an inventory limiter when no limiters are declared", func() {
		l, err := NewLimiterFromConfig(config.NewTestConfig(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(l.Name()).To(Equal("gpu-limiter"))
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "inventory mode should produce a DefaultLimiter")
	})

	It("returns an inventory limiter for an inline gpu-inventory entry", func() {
		cfg := configWithLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"})
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Name()).To(Equal("gpu-limiter"))
	})

	It("returns a single DefaultLimiter for one inline quota entry", func() {
		cfg := configWithLimiters(config.QuotaLimiterConfig{
			Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 16},
		})
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(l.Name()).To(Equal("cluster-quota"))
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "single-entry quota should produce a DefaultLimiter")
	})

	It("wraps multiple inline quota entries in a CompositeLimiter", func() {
		cfg := configWithLimiters(
			config.QuotaLimiterConfig{
				Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 16},
			},
			config.QuotaLimiterConfig{
				Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
			},
		)
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())

		comp, ok := l.(*CompositeLimiter)
		Expect(ok).To(BeTrue(), "multi-entry quota should produce a CompositeLimiter")
		Expect(comp.Name()).To(Equal("quota-limiter"))
		Expect(comp.Constituents()).To(HaveLen(2))
		Expect(comp.Constituents()[0].Name()).To(Equal("cluster-quota"))
		Expect(comp.Constituents()[1].Name()).To(Equal("namespace-quota"))
	})
})
