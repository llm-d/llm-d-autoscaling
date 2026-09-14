package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

var _ = Describe("live GPU limiter rebuild", func() {
	ctx := context.Background()

	It("rebuilds the limiter only when the effective limiter config changes", func() {
		cfg := config.NewTestConfig()
		e := &Engine{Config: cfg, GPULimiter: pipeline.NewNoOpLimiter("initial")}

		builds := 0
		e.SetLimiterBuilder(func() (pipeline.Limiter, error) {
			builds++
			// Name the rebuilt limiter after the mode so we can observe the swap.
			return pipeline.NewNoOpLimiter(string(cfg.EffectiveLimiterMode())), nil
		})

		// No config change since SetLimiterBuilder seeded the signature: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(0))
		Expect(e.GPULimiter.Name()).To(Equal("initial"))

		// Switch to quota mode via inline limiters: the signature changes -> rebuild.
		cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 8},
			}}},
		})
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
		Expect(e.GPULimiter.Name()).To(Equal("quota"))

		// No further change: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
	})

	It("rebuilds when a quota value changes within the same mode", func() {
		cfg := config.NewTestConfig()
		quotaCfg := func(cap int) map[string]config.SaturationScalingConfig {
			return map[string]config.SaturationScalingConfig{
				"default": {Limiters: []config.QuotaLimiterConfig{{
					Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
					ClusterQuotas: map[string]int{"H100": cap},
				}}},
			}
		}
		cfg.UpdateSaturationConfig(quotaCfg(8))
		e := &Engine{Config: cfg, GPULimiter: pipeline.NewNoOpLimiter("initial")}

		builds := 0
		e.SetLimiterBuilder(func() (pipeline.Limiter, error) {
			builds++
			// Name encodes the current quota so the swap is observable.
			return pipeline.NewNoOpLimiter(string(rune('0' + cfg.EffectiveQuotaEntries()[0].ClusterQuotas["H100"]))), nil
		})

		// Same quota mode AND same entries: no rebuild.
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(0))

		// Same mode, changed quota value: signature changes -> rebuild.
		cfg.UpdateSaturationConfig(quotaCfg(9))
		e.refreshLimiter(ctx)
		Expect(builds).To(Equal(1))
		Expect(e.GPULimiter.Name()).To(Equal("9"))
	})

	It("keeps the previous limiter when a rebuild fails", func() {
		cfg := config.NewTestConfig()
		e := &Engine{Config: cfg, GPULimiter: pipeline.NewNoOpLimiter("keep-me")}
		e.SetLimiterBuilder(func() (pipeline.Limiter, error) {
			return nil, context.DeadlineExceeded
		})
		// Force a signature change so refresh attempts a rebuild.
		cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 8},
			}}},
		})
		e.refreshLimiter(ctx)
		Expect(e.GPULimiter.Name()).To(Equal("keep-me"), "a failed rebuild must not drop the limiter")
	})
})
