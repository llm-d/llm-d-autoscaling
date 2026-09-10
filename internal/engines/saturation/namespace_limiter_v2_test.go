package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// The V2 optimizer only enforces per-namespace pools if the engine recognizes
// the selected limiter as a constraint provider, so pin that wiring down.
var _ = Describe("Namespace limiter on the V2 constraint path", func() {
	nsLimiter := func() pipeline.Limiter {
		GinkgoHelper()
		cfg := config.NewTestConfig()
		cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
			"default": {Limiters: []config.QuotaLimiterConfig{{
				Type: string(config.LimiterTypeNamespaceInventory), Name: "namespace-inventory",
				Selectors: map[string]config.NodeSelector{
					"ns-prod": {MatchLabels: map[string]string{"team": "prod"}},
				},
			}}},
		})
		lim, err := pipeline.NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		return lim
	}

	It("is discovered as a constraint provider", func() {
		providers := gpuConstraintProviders(nsLimiter())
		Expect(providers).To(HaveLen(1))
		Expect(providers[0].Name()).To(Equal(pipeline.NamespaceLimiterName))
	})

	It("supplies constraints through the engine's selected limiter", func() {
		// The selected mode is the limiter, so namespace-inventory reaches the V2
		// constraint path without any special-casing.
		e := &Engine{Config: config.NewTestConfig(), GPULimiter: nsLimiter()}
		providers := gpuConstraintProviders(e.currentGPULimiter())
		Expect(providers).To(HaveLen(1))
		Expect(providers[0].Name()).To(Equal(pipeline.NamespaceLimiterName))
	})

	It("still discovers the cluster-wide limiter when that mode is selected", func() {
		cfg := config.NewTestConfig()
		lim, err := pipeline.NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())

		e := &Engine{Config: cfg, GPULimiter: lim}
		providers := gpuConstraintProviders(e.currentGPULimiter())
		Expect(providers).To(HaveLen(1))
		Expect(providers[0].Name()).To(Equal("gpu-limiter"))
	})
})
