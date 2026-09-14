package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func float64Ptr(v float64) *float64 { return &v }

var _ = Describe("SaturationScalingConfig", func() {

	Context("Validate", func() {

		DescribeTable("validation cases",
			func(config SaturationScalingConfig, expectErr bool) {
				err := config.Validate()
				if expectErr {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("valid default config", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			}, false),
			Entry("valid custom config", SaturationScalingConfig{
				KvCacheThreshold:     0.75,
				QueueLengthThreshold: 10,
				KvSpareTrigger:       0.15,
				QueueSpareTrigger:    5,
			}, false),
			Entry("invalid KvCacheThreshold too high", SaturationScalingConfig{
				KvCacheThreshold:     1.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvCacheThreshold negative", SaturationScalingConfig{
				KvCacheThreshold:     -0.1,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid QueueLengthThreshold negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: -1,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvSpareTrigger too high", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       1.5,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvSpareTrigger negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       -0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid QueueSpareTrigger negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    -1,
			}, true),
			Entry("invalid KvCacheThreshold less than KvSpareTrigger", SaturationScalingConfig{
				KvCacheThreshold:     0.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.6,
				QueueSpareTrigger:    3,
			}, true),
			Entry("edge case: zero values are valid", SaturationScalingConfig{
				KvCacheThreshold:     0.0,
				QueueLengthThreshold: 0,
				KvSpareTrigger:       0.0,
				QueueSpareTrigger:    0,
			}, false),
			Entry("edge case: max values are valid", SaturationScalingConfig{
				KvCacheThreshold:     1.0,
				QueueLengthThreshold: 1000,
				KvSpareTrigger:       1.0,
				QueueSpareTrigger:    1000,
			}, false),
			Entry("V2 valid config with explicit thresholds (old-style analyzerName)", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
			}, false),
			Entry("V2 valid config with analyzers list (new-style)", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 1.0},
				},
			}, false),
			Entry("V2 invalid: scaleUpThreshold > 1", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     1.5,
				ScaleDownBoundary:    0.70,
			}, true),
			Entry("V2 invalid: scaleUpThreshold <= scaleDownBoundary", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.60,
				ScaleDownBoundary:    0.70,
			}, true),
			Entry("V2 thresholds ignored when not V2", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     0,
				ScaleDownBoundary:    0,
			}, false),
			// A V1-style entry's explicit V2 thresholds can still reach the V2 path via
			// Merge (selection is global), so out-of-range/inverted values are rejected
			// even though the entry itself is not V2.
			Entry("V1-style entry with out-of-range explicit scaleUpThreshold is rejected", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     1.5,
			}, true),
			Entry("V1-style entry with inverted explicit V2 thresholds is rejected", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     0.60,
				ScaleDownBoundary:    0.70,
			}, true),
			Entry("V1-style entry with valid explicit V2 thresholds is accepted", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
			}, false),
			Entry("valid priority", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             5.0,
			}, false),
			Entry("invalid negative priority", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             -1.0,
			}, true),
			Entry("V2 valid per-analyzer threshold override", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.90)},
				},
			}, false),
			Entry("V2 invalid per-analyzer scaleUpThreshold > 1", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(1.5)},
				},
			}, true),
			Entry("V2 invalid per-analyzer scaleDownBoundary > 1", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleDownBoundary: float64Ptr(1.5)},
				},
			}, true),
			Entry("V2 invalid per-analyzer effective up <= down", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.60)},
				},
			}, true),
		)
	})

	Context("ApplyDefaults", func() {

		It("should apply defaults for V2 via analyzerName (backward compat)", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "saturation",
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(DefaultScaleUpThreshold))
			Expect(config.ScaleDownBoundary).To(Equal(DefaultScaleDownBoundary))
			Expect(config.Analyzers).To(HaveLen(1))
		})

		It("should apply defaults for V2 via analyzers list (new-style)", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(DefaultScaleUpThreshold))
			Expect(config.ScaleDownBoundary).To(Equal(DefaultScaleDownBoundary))
			Expect(config.Analyzers[0].Score).To(Equal(1.0))
			Expect(config.Analyzers[0].Enabled).NotTo(BeNil())
			Expect(*config.Analyzers[0].Enabled).To(BeTrue())
		})

		It("should not overwrite explicit values", func() {
			config := SaturationScalingConfig{
				AnalyzerName:      "saturation",
				ScaleUpThreshold:  0.90,
				ScaleDownBoundary: 0.60,
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(0.90))
			Expect(config.ScaleDownBoundary).To(Equal(0.60))
		})

		It("should keep V2 thresholds zero on ApplyDefaults when not V2, then calibrate via ApplyV2ThresholdDefaults", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "",
			}
			config.ApplyDefaults()
			Expect(config.KvCacheThreshold).To(Equal(DefaultKvCacheThreshold))
			Expect(config.QueueLengthThreshold).To(Equal(DefaultQueueLengthThreshold))
			Expect(config.KvSpareTrigger).To(Equal(DefaultKvSpareTrigger))
			Expect(config.QueueSpareTrigger).To(Equal(DefaultQueueSpareTrigger))
			// V2 thresholds stay zero for a V1-style entry: defaulting them on the stored
			// entry would let a V1-style override clobber a tuned global during Merge.
			Expect(config.ScaleUpThreshold).To(Equal(0.0))
			Expect(config.ScaleDownBoundary).To(Equal(0.0))
			// The analyzer list is NOT seeded, so selection stays V1 (IsV2 false).
			Expect(config.Analyzers).To(BeEmpty())
			Expect(config.IsV2()).To(BeFalse())
			// The final resolved config is calibrated post-merge, independent of IsV2().
			config.ApplyV2ThresholdDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(DefaultScaleUpThreshold))
			Expect(config.ScaleDownBoundary).To(Equal(DefaultScaleDownBoundary))
		})

		It("ApplyV2ThresholdDefaults should not overwrite already-set (tuned) V2 thresholds", func() {
			// The whole point of applying this post-merge instead of in ApplyDefaults:
			// an inherited/tuned value must survive, so it must be a no-op when non-zero.
			config := SaturationScalingConfig{
				ScaleUpThreshold:  0.95,
				ScaleDownBoundary: 0.55,
			}
			config.ApplyV2ThresholdDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(0.95))
			Expect(config.ScaleDownBoundary).To(Equal(0.55))
		})

		It("should not overwrite explicit V1 values", func() {
			config := SaturationScalingConfig{
				KvCacheThreshold:     0.75,
				QueueLengthThreshold: 10,
				KvSpareTrigger:       0.15,
				QueueSpareTrigger:    5,
			}
			config.ApplyDefaults()
			Expect(config.KvCacheThreshold).To(Equal(0.75))
			Expect(config.QueueLengthThreshold).To(Equal(10.0))
			Expect(config.KvSpareTrigger).To(Equal(0.15))
			Expect(config.QueueSpareTrigger).To(Equal(5.0))
		})

		It("should apply default priority when zero", func() {
			config := SaturationScalingConfig{}
			config.ApplyDefaults()
			Expect(config.Priority).To(Equal(DefaultPriority))
		})

		It("should not overwrite explicit priority", func() {
			config := SaturationScalingConfig{
				Priority: 5.0,
			}
			config.ApplyDefaults()
			Expect(config.Priority).To(Equal(5.0))
		})

		It("should not overwrite explicit analyzers", func() {
			disabled := false
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 0.5, Enabled: &disabled},
				},
			}
			config.ApplyDefaults()
			Expect(config.Analyzers[0].Score).To(Equal(0.5))
			Expect(*config.Analyzers[0].Enabled).To(BeFalse())
		})

		It("should apply per-entry defaults for zero score", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.Analyzers[0].Score).To(Equal(1.0))
			Expect(config.Analyzers[0].Enabled).NotTo(BeNil())
			Expect(*config.Analyzers[0].Enabled).To(BeTrue())
		})

		It("should pass validation after ApplyDefaults with zero-valued omitempty fields", func() {
			config := SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.Validate()).To(Succeed())
		})
	})

	Context("Merge", func() {

		It("should overlay non-zero fields from override", func() {
			base := SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			}
			override := SaturationScalingConfig{
				KvCacheThreshold: 0.85,
				KvSpareTrigger:   0.15,
			}
			base.Merge(override)
			Expect(base.KvCacheThreshold).To(Equal(0.85))
			Expect(base.KvSpareTrigger).To(Equal(0.15))
			// Unset fields in override should not change base
			Expect(base.QueueLengthThreshold).To(Equal(5.0))
			Expect(base.QueueSpareTrigger).To(Equal(3.0))
		})

		It("should overlay all fields when all are set", func() {
			base := SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             1.0,
			}
			override := SaturationScalingConfig{
				KvCacheThreshold:     0.90,
				QueueLengthThreshold: 15,
				KvSpareTrigger:       0.05,
				QueueSpareTrigger:    2,
				Priority:             5.0,
			}
			base.Merge(override)
			Expect(base.KvCacheThreshold).To(Equal(0.90))
			Expect(base.QueueLengthThreshold).To(Equal(15.0))
			Expect(base.KvSpareTrigger).To(Equal(0.05))
			Expect(base.QueueSpareTrigger).To(Equal(2.0))
			Expect(base.Priority).To(Equal(5.0))
		})

		It("should not change base when override is empty", func() {
			base := SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			}
			override := SaturationScalingConfig{}
			base.Merge(override)
			Expect(base.KvCacheThreshold).To(Equal(0.80))
			Expect(base.QueueLengthThreshold).To(Equal(5.0))
			Expect(base.KvSpareTrigger).To(Equal(0.10))
			Expect(base.QueueSpareTrigger).To(Equal(3.0))
		})

		It("should overlay V2 fields", func() {
			base := SaturationScalingConfig{
				AnalyzerName:      "saturation",
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
			}
			override := SaturationScalingConfig{
				ScaleUpThreshold: 0.90,
			}
			base.Merge(override)
			Expect(base.ScaleUpThreshold).To(Equal(0.90))
			Expect(base.ScaleDownBoundary).To(Equal(0.70))
			Expect(base.AnalyzerName).To(Equal("saturation"))
		})

		It("should overlay analyzers list", func() {
			enabled := true
			base := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 1.0, Enabled: &enabled},
				},
			}
			override := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "custom", Score: 0.5},
				},
			}
			base.Merge(override)
			Expect(base.Analyzers).To(HaveLen(1))
			Expect(base.Analyzers[0].Name).To(Equal("custom"))
		})

		It("should overlay ModelID and Namespace", func() {
			base := SaturationScalingConfig{}
			override := SaturationScalingConfig{
				ModelID:   "llama-70b",
				Namespace: "production",
			}
			base.Merge(override)
			Expect(base.ModelID).To(Equal("llama-70b"))
			Expect(base.Namespace).To(Equal("production"))
		})
	})

	Context("IsV2", func() {

		It("should return false when no analyzers and no analyzerName", func() {
			config := SaturationScalingConfig{}
			Expect(config.IsV2()).To(BeFalse())
		})

		It("should return true when analyzerName is saturation (backward compat)", func() {
			config := SaturationScalingConfig{AnalyzerName: "saturation"}
			Expect(config.IsV2()).To(BeTrue())
		})

		It("should return true when analyzers list is populated", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.IsV2()).To(BeTrue())
		})

		It("should return true when both analyzerName and analyzers set", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "saturation",
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.IsV2()).To(BeTrue())
		})
	})

	Context("GetAnalyzerName", func() {

		It("should return saturation when analyzers list populated", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.GetAnalyzerName()).To(Equal("saturation"))
		})

		It("should return raw analyzerName when no analyzers list", func() {
			config := SaturationScalingConfig{AnalyzerName: "saturation"}
			Expect(config.GetAnalyzerName()).To(Equal("saturation"))
		})

		It("should return empty when no analyzers and no analyzerName", func() {
			config := SaturationScalingConfig{}
			Expect(config.GetAnalyzerName()).To(BeEmpty())
		})
	})
})

var _ = Describe("AnalyzerScoreConfig", func() {

	Context("EffectiveScaleUpThreshold", func() {

		It("should return global when per-analyzer not set", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.85))
		})

		It("should return per-analyzer when set", func() {
			a := AnalyzerScoreConfig{
				Name:             "saturation",
				ScaleUpThreshold: float64Ptr(0.90),
			}
			Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.90))
		})
	})

	Context("EffectiveScaleDownBoundary", func() {

		It("should return global when per-analyzer not set", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.70))
		})

		It("should return per-analyzer when set", func() {
			a := AnalyzerScoreConfig{
				Name:              "saturation",
				ScaleDownBoundary: float64Ptr(0.60),
			}
			Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.60))
		})
	})

	It("should support partial override (only scaleUpThreshold)", func() {
		a := AnalyzerScoreConfig{
			Name:             "saturation",
			ScaleUpThreshold: float64Ptr(0.95),
		}
		Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.95))
		Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.70))
	})
})
