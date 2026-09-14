package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("SaturationScalingConfig enableRescale", func() {
	// Locks the `enableRescale` yaml tag and confirms it defaults to false.
	DescribeTable("yaml parsing",
		func(doc string, want bool) {
			var cfg SaturationScalingConfig
			Expect(yaml.Unmarshal([]byte(doc), &cfg)).To(Succeed())
			Expect(cfg.EnableRescale).To(Equal(want))
		},
		Entry("absent defaults false", "kvCacheThreshold: 0.8\n", false),
		Entry("explicit true", "kvCacheThreshold: 0.8\nenableRescale: true\n", true),
		Entry("explicit false", "enableRescale: false\n", false),
	)

	// enableRescale is budget-scoped: it is read only from the global/namespace
	// `default` entry and must NOT be settable via a per-model override, so Merge()
	// deliberately ignores it. This locks that exclusion against an accidental
	// copy-paste of the EnableLimiter merge branch that would reintroduce a leak.
	It("does not let a per-model override set enableRescale via Merge", func() {
		base := SaturationScalingConfig{EnableRescale: false}
		base.Merge(SaturationScalingConfig{EnableRescale: true, ModelID: "m", Namespace: "ns"})
		Expect(base.EnableRescale).To(BeFalse())
	})
})

// Scope-coupled flag resolution: the cluster flag from the global default, and the
// per-namespace flag from a namespace's OWN config only (the cluster flag must never
// leak onto a namespace quota).
var _ = Describe("rescale flag accessors", func() {
	var c *Config

	BeforeEach(func() {
		c = NewTestConfig()
		c.UpdateSaturationConfig(map[string]SaturationScalingConfig{"default": {EnableRescale: true}})
	})

	It("reads the cluster flag from the global default", func() {
		Expect(c.RescaleEnabledCluster()).To(BeTrue())
	})

	It("reads a namespace flag from that namespace's own config", func() {
		c.UpdateSaturationConfigForNamespace("team", map[string]SaturationScalingConfig{"default": {EnableRescale: true}})
		en, has := c.RescaleEnabledForNamespaceLocal("team")
		Expect(has).To(BeTrue())
		Expect(en).To(BeTrue())
	})

	It("does not leak the cluster flag onto a namespace whose own flag is off", func() {
		c.UpdateSaturationConfigForNamespace("plain", map[string]SaturationScalingConfig{"default": {KvCacheThreshold: 0.8}})
		en, has := c.RescaleEnabledForNamespaceLocal("plain")
		Expect(has).To(BeTrue(), "namespace has a local config")
		Expect(en).To(BeFalse(), "cluster flag must not leak onto the namespace quota")
	})

	It("reports no local config for an unconfigured namespace", func() {
		en, has := c.RescaleEnabledForNamespaceLocal("absent")
		Expect(has).To(BeFalse())
		Expect(en).To(BeFalse())
	})
})
