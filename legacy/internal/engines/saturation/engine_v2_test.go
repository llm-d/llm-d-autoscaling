package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// withSatEntryV2 adds a single-saturation AnalyzerResults to req from r.
// Mirrors the helper in cost_aware_optimizer_test.go for use in the saturation package.
// Enabled marks it as a voting ballot member; Live lets it bind as the anchor
// (the engine populates both in production).
func withSatEntryV2(r *domain.AnalyzerResult, req pipeline.ModelScalingRequest) pipeline.ModelScalingRequest {
	if r != nil {
		req.AnalyzerResults = []pipeline.NamedAnalyzerResult{{
			Name:      domain.SaturationAnalyzerName,
			Result:    r,
			Remaining: r.RequiredCapacity,
			Spare:     r.SpareCapacity,
			Enabled:   true,
			Live:      true,
		}}
	}
	return req
}

var _ = Describe("V2 Engine Integration", func() {

	Context("CostAwareOptimizer via engine path", func() {

		It("should scale up cheapest variant by cost-efficiency", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "variant-cheap", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "variant-expensive", AcceleratorName: "H100", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
				},
			}
			requests := []pipeline.ModelScalingRequest{
				withSatEntryV2(r, pipeline.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2},
						{VariantName: "variant-expensive", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cost-efficiency: cheap=5/10000=0.0005, expensive=15/20000=0.00075
			// cheap is more cost-efficient, ceil(5000/10000)=1
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
		})

		It("should scale down most expensive variant", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			r := &domain.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				SpareCapacity: 25000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 3, PerReplicaCapacity: 10000},
					{VariantName: "variant-expensive", Cost: 15.0, ReplicaCount: 2, PerReplicaCapacity: 20000},
				},
			}
			requests := []pipeline.ModelScalingRequest{
				withSatEntryV2(r, pipeline.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 3},
						{VariantName: "variant-expensive", CurrentReplicas: 2},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(1))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
		})

		It("should protect cheapest variant at 1 during scale-down", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			r := &domain.AnalyzerResult{
				ModelID:       "model-1",
				Namespace:     "default",
				SpareCapacity: 30000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "variant-expensive", Cost: 15.0, ReplicaCount: 1, PerReplicaCapacity: 20000},
					{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 1, PerReplicaCapacity: 10000},
				},
			}
			requests := []pipeline.ModelScalingRequest{
				withSatEntryV2(r, pipeline.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-expensive", CurrentReplicas: 1},
						{VariantName: "variant-cheap", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			Expect(dm["variant-expensive"].TargetReplicas).To(Equal(0))
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(1))
		})

		It("should not skip variants with pending replicas", func() {
			optimizer := pipeline.NewCostAwareOptimizer()
			r := &domain.AnalyzerResult{
				ModelID:          "model-1",
				Namespace:        "default",
				RequiredCapacity: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "variant-cheap", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000},
					{VariantName: "variant-mid", Cost: 10.0, ReplicaCount: 1, PerReplicaCapacity: 15000},
				},
			}
			requests := []pipeline.ModelScalingRequest{
				withSatEntryV2(r, pipeline.ModelScalingRequest{
					ModelID:   "model-1",
					Namespace: "default",
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "variant-cheap", CurrentReplicas: 2, PendingReplicas: 1},
						{VariantName: "variant-mid", CurrentReplicas: 1},
					},
				}),
			}

			decisions := optimizer.Optimize(context.Background(), requests, nil)

			dm := decisionsByVariant(decisions)
			// cheap has pending but is more cost-efficient → still gets allocation
			Expect(dm["variant-cheap"].TargetReplicas).To(Equal(3))
			Expect(dm["variant-mid"].TargetReplicas).To(Equal(1))
		})
	})
})

var _ = Describe("getRoleFromScaleTarget", func() {

	It("should return 'both' for nil scale target", func() {
		Expect(getRoleFromScaleTarget(nil)).To(Equal("both"))
	})

	It("should return 'both' for scale target without labels", func() {
		deploy := &appsv1.Deployment{}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})

	It("should return 'prefill' for prefill label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "prefill",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("prefill"))
	})

	It("should return 'decode' for decode label", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "decode",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("decode"))
	})

	It("should return 'both' for unknown role value", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"llm-d.ai/role": "unknown",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})

	It("should return 'both' when no role label present", func() {
		deploy := &appsv1.Deployment{}
		deploy.Spec.Template.Labels = map[string]string{
			"app": "vllm",
		}
		Expect(getRoleFromScaleTarget(scaletarget.NewDeploymentAccessor(deploy))).To(Equal("both"))
	})
})

var _ = Describe("resolveSaturationConfig", func() {

	It("should merge model-specific override onto default", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"default": {
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
			},
			"llama-70b#production": {
				KvCacheThreshold: 0.85,
				Priority:         5.0,
			},
		}
		cfg := resolveSaturationConfig(configMap, "llama-70b", "production")
		// Overridden fields
		Expect(cfg.KvCacheThreshold).To(Equal(0.85))
		Expect(cfg.Priority).To(Equal(5.0))
		// Inherited from default
		Expect(cfg.QueueLengthThreshold).To(Equal(5.0))
		Expect(cfg.KvSpareTrigger).To(Equal(0.10))
		Expect(cfg.QueueSpareTrigger).To(Equal(3.0))
		Expect(cfg.AnalyzerName).To(Equal("saturation"))
	})

	It("should fall back to default config when model-specific not found", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"default": {
				KvCacheThreshold: 0.80,
				AnalyzerName:     "saturation",
			},
		}
		cfg := resolveSaturationConfig(configMap, "unknown-model", "default")
		Expect(cfg.KvCacheThreshold).To(Equal(0.80))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
	})

	It("should return V1 defaults when map is empty", func() {
		configMap := map[string]config.SaturationScalingConfig{}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
		Expect(cfg.KvCacheThreshold).To(Equal(config.DefaultKvCacheThreshold))
		Expect(cfg.QueueLengthThreshold).To(Equal(config.DefaultQueueLengthThreshold))
		Expect(cfg.KvSpareTrigger).To(Equal(config.DefaultKvSpareTrigger))
		Expect(cfg.QueueSpareTrigger).To(Equal(config.DefaultQueueSpareTrigger))
	})

	It("should apply defaults on model-specific config", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"model-1#ns-1": {
				AnalyzerName: "saturation",
			},
		}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
		Expect(cfg.Priority).To(Equal(config.DefaultPriority))
		// V1 defaults also applied
		Expect(cfg.KvCacheThreshold).To(Equal(config.DefaultKvCacheThreshold))
	})

	It("should allow partial override with only one field changed", func() {
		configMap := map[string]config.SaturationScalingConfig{
			"default": {
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			},
			"model-1#ns-1": {
				KvCacheThreshold: 0.90,
			},
		}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.KvCacheThreshold).To(Equal(0.90))
		Expect(cfg.QueueLengthThreshold).To(Equal(5.0))
		Expect(cfg.KvSpareTrigger).To(Equal(0.10))
		Expect(cfg.QueueSpareTrigger).To(Equal(3.0))
		// A V1-style entry stays V1 (selection is decided globally, not here), but the
		// RESOLVED config is calibrated post-merge so that if the global default routes
		// this model to the V2 path it runs with valid thresholds rather than zeros
		// (which would disable the scale-up/scale-down post-step).
		Expect(cfg.IsV2()).To(BeFalse())
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
	})

	It("should not let a V1-style override clobber a tuned global V2 threshold (production parse order)", func() {
		// Regression guard: entries are ApplyDefaults()'d individually at parse time
		// before storage (see parseSaturationConfig). Build the map that way, then
		// resolve. A V1-style override that omits scaleUpThreshold must INHERIT the
		// operator-tuned global 0.95, not silently revert to the 0.85 default.
		def := config.SaturationScalingConfig{
			Analyzers:        []config.AnalyzerScoreConfig{{Name: "saturation"}},
			ScaleUpThreshold: 0.95, // operator tuned away from the 0.85 default
			KvCacheThreshold: 0.80,
		}
		def.ApplyDefaults()
		override := config.SaturationScalingConfig{KvCacheThreshold: 0.90} // V1-style, no V2 thresholds
		override.ApplyDefaults()
		configMap := map[string]config.SaturationScalingConfig{
			"default":                   def,
			"meta/llama-70b#production": override,
		}
		cfg := resolveSaturationConfig(configMap, "meta/llama-70b", "production")
		Expect(cfg.KvCacheThreshold).To(Equal(0.90))
		Expect(cfg.ScaleUpThreshold).To(Equal(0.95), "tuned global scaleUpThreshold must survive a V1-style override")
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
	})

	It("should default the sibling V2 threshold post-merge for a fully V1-style namespace map", func() {
		// Namespace-local map that is V1-style end-to-end (no analyzers anywhere), as
		// when a tenant ships their own saturation ConfigMap. Global selection routes
		// these models to V2. The override sets ONLY scaleUpThreshold; the merged
		// config's IsV2() stays false, so ApplyV2ThresholdDefaults() is the ONLY thing
		// that fills the missing scaleDownBoundary — this test fails if that post-merge
		// call is removed (the explicit ApplyDefaults V2-branch never runs here).
		def := config.SaturationScalingConfig{KvCacheThreshold: 0.80} // V1-style, no analyzers
		def.ApplyDefaults()
		override := config.SaturationScalingConfig{ScaleUpThreshold: 0.90} // only scaleUp set
		override.ApplyDefaults()
		configMap := map[string]config.SaturationScalingConfig{
			"default":      def,
			"model-1#ns-1": override,
		}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.IsV2()).To(BeFalse())
		Expect(cfg.ScaleUpThreshold).To(Equal(0.90), "explicit override must win")
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary), "missing sibling must be defaulted post-merge")
	})

	It("should reset an inverted V2 threshold pair produced by a cross-entry merge", func() {
		// Base scaleUpThreshold 0.85; a V1-style override raises scaleDownBoundary above
		// it. Each entry is valid on its own (so load-time validation passes), but the
		// merged pair is inverted — resolveSaturationConfig must fall back to defaults
		// rather than feed the optimizer scaleUp <= scaleDown.
		def := config.SaturationScalingConfig{
			Analyzers:        []config.AnalyzerScoreConfig{{Name: "saturation"}},
			KvCacheThreshold: 0.80,
		}
		def.ApplyDefaults() // scaleUp=0.85, scaleDown=0.70
		override := config.SaturationScalingConfig{ScaleDownBoundary: 0.95}
		override.ApplyDefaults()
		configMap := map[string]config.SaturationScalingConfig{
			"default":      def,
			"model-1#ns-1": override,
		}
		cfg := resolveSaturationConfig(configMap, "model-1", "ns-1")
		Expect(cfg.ScaleUpThreshold).To(Equal(config.DefaultScaleUpThreshold))
		Expect(cfg.ScaleDownBoundary).To(Equal(config.DefaultScaleDownBoundary))
		Expect(cfg.ScaleUpThreshold).To(BeNumerically(">", cfg.ScaleDownBoundary))
	})
})

var _ = Describe("runAnalyzersAndScore call ordering", func() {

	It("calls each enabled non-saturation analyzer exactly once in registration order", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		ta := &spyAnalyzer{name: "throughput"}
		slo := &spyAnalyzer{name: "slo"}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
				{name: "throughput", analyzer: ta},
				{name: "slo", analyzer: slo},
			},
			started: true,
		}
		cfg := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "throughput"},
				{Name: "slo"},
			},
		}

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		// saturation + throughput + slo all appended
		Expect(results).To(HaveLen(3))
		Expect(ta.callCount).To(Equal(1))
		Expect(slo.callCount).To(Equal(1))
		// saturationV2Analyzer is called via runV2AnalysisOnly, not the loop;
		// the snapshot entry for saturation is skipped by the name guard.
		Expect(fakeSat.Name()).To(Equal(domain.SaturationAnalyzerName)) // sanity
	})
})

var _ = Describe("runAnalyzersAndScore disabled-analyzer gate", func() {

	It("disabled analyzer is not appended and its Analyze is never called", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{SpareCapacity: 1000},
		}
		spy := &spyAnalyzer{name: "spy"}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
				{name: "spy", analyzer: spy},
			},
			started: true,
		}
		f := false
		cfg := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "spy", Enabled: &f},
			},
		}

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(1), "only saturation entry — disabled spy must not be appended")
		Expect(results[0].Name).To(Equal(domain.SaturationAnalyzerName))
		Expect(spy.callCount).To(Equal(0), "Analyze must not be called for a disabled analyzer")
	})
})

var _ = Describe("collectV2ModelRequest Disaggregated flag", func() {

	It("sets Disaggregated=true when any variant has a non-both role", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			},
			started: true,
		}
		cfg := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		variantStates := []domain.VariantReplicaState{
			{VariantName: "prefill-v1", Role: "prefill"},
			{VariantName: "decode-v1", Role: "decode"},
		}

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, variantStates, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(req.Disaggregated).To(BeTrue())
	})

	It("sets Disaggregated=false when all variants have role 'both' or empty", func() {
		fakeSat := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result:       &domain.AnalyzerResult{},
		}
		e := &Engine{
			saturationV2Analyzer: fakeSat,
			analyzersSnapshot: []analyzerEntry{
				{name: domain.SaturationAnalyzerName, analyzer: fakeSat},
			},
			started: true,
		}
		cfg := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		variantStates := []domain.VariantReplicaState{
			{VariantName: "v1", Role: domain.RoleBoth},
			{VariantName: "v2", Role: ""},
		}

		req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, variantStates, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(req.Disaggregated).To(BeFalse())
	})
})

func decisionsByVariant(decisions []domain.VariantDecision) map[string]domain.VariantDecision {
	m := make(map[string]domain.VariantDecision, len(decisions))
	for _, d := range decisions {
		m[d.VariantName] = d
	}
	return m
}
