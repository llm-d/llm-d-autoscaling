package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func p1BoolPtr(v bool) *bool { return &v }

// validBaseConfig returns a defaulted, valid SaturationScalingConfig to isolate
// the field under test in Validate cases.
func validBaseConfig() SaturationScalingConfig {
	c := SaturationScalingConfig{}
	c.ApplyDefaults()
	return c
}

var _ = Describe("ScalingPolicy Phase 1 schema", func() {

	Describe("AnalyzerScoreConfig.EffectiveType", func() {
		It("returns Type when set", func() {
			a := AnalyzerScoreConfig{Type: "throughput", Name: "ignored"}
			Expect(a.EffectiveType()).To(Equal("throughput"))
		})
		It("falls back to Name when Type is empty", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveType()).To(Equal("saturation"))
		})
	})

	Describe("AnalyzerScoreConfig.Normalize", func() {
		It("folds well-known parameters into typed fields", func() {
			a := AnalyzerScoreConfig{
				Type: "saturation",
				Parameters: map[string]any{
					"scaleUpThreshold":  0.95,
					"scaleDownBoundary": 0.60,
					"score":             2.0,
					"enabled":           false,
				},
			}
			Expect(a.Normalize()).To(Succeed())
			Expect(a.ScaleUpThreshold).NotTo(BeNil())
			Expect(*a.ScaleUpThreshold).To(Equal(0.95))
			Expect(a.ScaleDownBoundary).NotTo(BeNil())
			Expect(*a.ScaleDownBoundary).To(Equal(0.60))
			Expect(a.Score).To(Equal(2.0))
			Expect(a.Enabled).NotTo(BeNil())
			Expect(*a.Enabled).To(BeFalse())
		})

		It("coerces integer parameters to float", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"score": 3}}
			Expect(a.Normalize()).To(Succeed())
			Expect(a.Score).To(Equal(3.0))
		})

		It("does not override a typed field already set from a top-level key", func() {
			a := AnalyzerScoreConfig{
				Type:             "saturation",
				ScaleUpThreshold: float64Ptr(0.80),
				Parameters:       map[string]any{"scaleUpThreshold": 0.99},
			}
			Expect(a.Normalize()).To(Succeed())
			Expect(*a.ScaleUpThreshold).To(Equal(0.80))
		})

		It("tolerates unknown parameter keys", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"future": "value"}}
			Expect(a.Normalize()).To(Succeed())
		})

		It("errors on a wrongly-typed known parameter", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"enabled": "yes"}}
			Expect(a.Normalize()).To(HaveOccurred())
		})
	})

	Describe("Validate — analyzers", func() {
		It("accepts a known analyzer type", func() {
			c := SaturationScalingConfig{Analyzers: []AnalyzerScoreConfig{{Type: "saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
		It("accepts the legacy name-only form", func() {
			c := SaturationScalingConfig{Analyzers: []AnalyzerScoreConfig{{Name: "saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
		It("accepts an unrecognized analyzer type (extensible via RegisterAnalyzer; ignored at runtime)", func() {
			c := SaturationScalingConfig{Analyzers: []AnalyzerScoreConfig{{Type: "epp-saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
	})

	Describe("Validate — limiters", func() {
		It("accepts a gpu-inventory entry with no quota fields", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "gpu-inventory"}}
			Expect(c.Validate()).To(Succeed())
		})
		It("rejects a gpu-inventory entry carrying quota fields", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "gpu-inventory", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("rejects an unknown limiter type", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "bogus"}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("accepts a valid inline quota entry", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{
				Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 32},
			}}
			Expect(c.Validate()).To(Succeed())
		})
		It("rejects an inline quota entry missing a name (via reused quota validation)", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 32}}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("rejects two inline quota entries sharing a name (via reused quota validation)", func() {
			c := validBaseConfig()
			dup := QuotaLimiterConfig{Type: "quota", Name: "dup", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}}
			c.Limiters = []QuotaLimiterConfig{dup, dup}
			err := c.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate limiter name"))
		})
	})

	Describe("Merge", func() {
		It("carries ScaleToZero from the override but not Limiters (cluster-default-scope)", func() {
			base := SaturationScalingConfig{}
			override := SaturationScalingConfig{
				ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(false)},
				Limiters:    []QuotaLimiterConfig{{Type: "gpu-inventory"}},
			}
			base.Merge(override)
			Expect(base.ScaleToZero).NotTo(BeNil())
			Expect(*base.ScaleToZero.Enabled).To(BeFalse())
			Expect(base.Limiters).To(BeEmpty(), "per-model limiters are not merged; only the global default is read")
		})
		It("keeps base ScaleToZero when the override omits it", func() {
			base := SaturationScalingConfig{ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(true)}}
			base.Merge(SaturationScalingConfig{Priority: 2.0})
			Expect(base.ScaleToZero).NotTo(BeNil())
			Expect(*base.ScaleToZero.Enabled).To(BeTrue())
		})
	})

	Describe("v1 opt-out contract", func() {
		It("is V2 when an analyzers list is present", func() {
			c := SaturationScalingConfig{Analyzers: []AnalyzerScoreConfig{{Type: "saturation"}}}
			Expect(c.IsV2()).To(BeTrue())
		})
		It("is V1 when the analyzers section is deleted", func() {
			c := SaturationScalingConfig{}
			Expect(c.IsV2()).To(BeFalse())
		})
	})
})

var _ = Describe("Phase 1 envelope — YAML round-trip", func() {
	It("parses the full envelope and folds parameters through the parse path", func() {
		const y = `
model_id: granite-premium
namespace: production
priority: 2.0
scaleToZero: { enabled: false }
analyzers:
  - type: saturation
    parameters: { scaleUpThreshold: 0.95 }
limiters:
  - type: gpu-inventory
  - type: quota
    name: cluster-h100
    scope: cluster
    quotas: { H100: 32 }
`
		var c SaturationScalingConfig
		Expect(yaml.Unmarshal([]byte(y), &c)).To(Succeed())
		// Same order the reconciler applies: Normalize -> ApplyDefaults -> Validate.
		Expect(c.Normalize()).To(Succeed())
		c.ApplyDefaults()
		Expect(c.Validate()).To(Succeed())

		Expect(c.IsV2()).To(BeTrue())
		Expect(c.Analyzers).To(HaveLen(1))
		Expect(c.Analyzers[0].EffectiveType()).To(Equal("saturation"))
		Expect(c.Analyzers[0].ScaleUpThreshold).NotTo(BeNil())
		Expect(*c.Analyzers[0].ScaleUpThreshold).To(Equal(0.95))
		Expect(c.ScaleToZero).NotTo(BeNil())
		Expect(*c.ScaleToZero.Enabled).To(BeFalse())
		Expect(c.Limiters).To(HaveLen(2))
	})
})

var _ = Describe("ResolveScaleToZeroEnabled", func() {
	data := ScaleToZeroConfigData{
		GlobalDefaultsKey: {EnableScaleToZero: p1BoolPtr(true)},
	}

	It("prefers the inline saturation setting over the ConfigMap", func() {
		sat := &SaturationScalingConfig{ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(false)}}
		Expect(ResolveScaleToZeroEnabled(sat, data, "m")).To(BeFalse())
	})
	It("falls back to the ConfigMap when the inline field is nil", func() {
		sat := &SaturationScalingConfig{}
		Expect(ResolveScaleToZeroEnabled(sat, data, "m")).To(BeTrue())
	})
	It("falls back to the ConfigMap when sat is nil", func() {
		Expect(ResolveScaleToZeroEnabled(nil, data, "m")).To(BeTrue())
	})
})

var _ = Describe("Config.EffectiveLimiterMode / EffectiveQuotaEntries", func() {
	It("selects quota when the default entry declares an inline quota limiter", func() {
		c := &Config{}
		c.UpdateSaturationConfig(map[string]SaturationScalingConfig{
			"default": {Limiters: []QuotaLimiterConfig{{
				Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 32},
			}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
		entries := c.EffectiveQuotaEntries()
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name).To(Equal("cluster-h100"))
	})

	It("selects inventory when the default entry declares a gpu-inventory limiter", func() {
		c := &Config{}
		c.UpdateSaturationConfig(map[string]SaturationScalingConfig{
			"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory))
	})

	It("defaults to inventory when no inline limiters are declared", func() {
		c := &Config{}
		c.UpdateSaturationConfig(map[string]SaturationScalingConfig{"default": {}})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory))
		Expect(c.EffectiveQuotaEntries()).To(BeEmpty())
	})

	It("ignores limiters declared on a non-default (per-model) entry", func() {
		c := &Config{}
		c.UpdateSaturationConfig(map[string]SaturationScalingConfig{
			"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
			"some/model#ns": {Limiters: []QuotaLimiterConfig{{
				Type: "quota", Name: "sneaky", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 999},
			}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory), "only the default entry selects the limiter")
		Expect(c.EffectiveQuotaEntries()).To(BeEmpty())
	})

	It("quota wins over a co-present gpu-inventory entry, regardless of order", func() {
		quota := QuotaLimiterConfig{
			Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 32},
		}
		inv := QuotaLimiterConfig{Type: "gpu-inventory"}
		for _, limiters := range [][]QuotaLimiterConfig{{inv, quota}, {quota, inv}} {
			c := &Config{}
			c.UpdateSaturationConfig(map[string]SaturationScalingConfig{
				"default": {Limiters: limiters},
			})
			Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
			Expect(c.EffectiveQuotaEntries()).To(HaveLen(1))
		}
	})
})
