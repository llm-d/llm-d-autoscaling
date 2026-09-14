package controller

import (
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseSaturationConfig Normalize error handling", func() {
	It("skips an entry whose analyzer parameter is wrongly typed but keeps valid siblings", func() {
		data := map[string]string{
			"default": "analyzers:\n  - type: saturation\n",
			// YAML parses fine, but Normalize rejects a non-boolean `enabled` parameter.
			"bad#ns": "model_id: m\nnamespace: ns\nanalyzers:\n  - type: saturation\n    parameters: { enabled: \"yes\" }\n",
		}

		configs, count := parseSaturationConfig(data, logr.Discard())

		Expect(count).To(Equal(1))
		Expect(configs).To(HaveKey("default"))
		Expect(configs).NotTo(HaveKey("bad#ns"), "the entry failing Normalize must be skipped")
	})

	It("folds analyzer parameters into typed fields through the parse pipeline", func() {
		data := map[string]string{
			"default": "analyzers:\n  - type: saturation\n    parameters: { scaleUpThreshold: 0.93 }\n",
		}

		configs, count := parseSaturationConfig(data, logr.Discard())

		Expect(count).To(Equal(1))
		entry := configs["default"]
		Expect(entry.Analyzers).To(HaveLen(1))
		Expect(entry.Analyzers[0].EffectiveType()).To(Equal("saturation"))
		Expect(entry.Analyzers[0].ScaleUpThreshold).NotTo(BeNil())
		Expect(*entry.Analyzers[0].ScaleUpThreshold).To(Equal(0.93), "parameters.scaleUpThreshold folded into the typed field")
	})
})
