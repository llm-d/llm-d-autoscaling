package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

var _ = Describe("Engine.resolveRescaleFlags", func() {
	// Builds a config with a global default flag plus per-namespace defaults, then
	// checks the scope-coupled resolution: cluster from the global default, each
	// namespace flag from its OWN default only (no global fallback).
	newEngine := func(clusterOn bool) *Engine {
		c := config.NewTestConfig()
		c.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{"default": {EnableRescale: clusterOn}})
		// team: its own flag on. plain: has a local config but flag off. absent: no config.
		c.UpdateSaturationConfigForNamespace("team", map[string]config.SaturationScalingConfig{"default": {EnableRescale: true}})
		c.UpdateSaturationConfigForNamespace("plain", map[string]config.SaturationScalingConfig{"default": {KvCacheThreshold: 0.8}})
		return &Engine{Config: c}
	}

	req := func(ns string) pipeline.ModelScalingRequest {
		return pipeline.ModelScalingRequest{Namespace: ns}
	}

	It("sets the cluster flag from the global default", func() {
		flags := newEngine(true).resolveRescaleFlags([]pipeline.ModelScalingRequest{req("team")})
		Expect(flags.Cluster).To(BeTrue())
	})

	It("leaves the cluster flag off when the global default is off", func() {
		flags := newEngine(false).resolveRescaleFlags([]pipeline.ModelScalingRequest{req("team")})
		Expect(flags.Cluster).To(BeFalse())
	})

	It("enables only namespaces whose own default sets the flag", func() {
		reqs := []pipeline.ModelScalingRequest{
			req("team"), req("team"), // duplicate namespace must dedup, not double-count
			req("plain"),  // local config, flag off → excluded
			req("absent"), // no local config → excluded (no global fallback)
			req(""),       // empty namespace → skipped
		}
		flags := newEngine(true).resolveRescaleFlags(reqs)

		Expect(flags.ByNamespace).To(HaveKeyWithValue("team", true))
		Expect(flags.ByNamespace).ToNot(HaveKey("plain"), "cluster flag must not leak onto a namespace quota")
		Expect(flags.ByNamespace).ToNot(HaveKey("absent"))
		Expect(flags.ByNamespace).ToNot(HaveKey(""))
	})
})
