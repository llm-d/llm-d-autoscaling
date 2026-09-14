package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("analyzer matching by EffectiveType", func() {
	It("matches a type-only analyzer entry (no name) for score/threshold/enabled", func() {
		up := 0.95
		disabled := false
		cfg := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.90,
			ScaleDownBoundary: 0.60,
			Analyzers: []config.AnalyzerScoreConfig{
				{Type: "saturation", Score: 2.0, ScaleUpThreshold: &up},
				{Type: "throughput", Enabled: &disabled},
			},
		}

		Expect(scoreForAnalyzer("saturation", cfg)).To(Equal(2.0))
		gotUp, _ := resolveThresholds("saturation", cfg)
		Expect(gotUp).To(Equal(0.95), "per-analyzer override resolved via EffectiveType")
		Expect(effectiveEnabled("saturation", cfg)).To(BeTrue())
		Expect(effectiveEnabled("throughput", cfg)).To(BeFalse(), "type-only disabled entry is honored")
	})
})
