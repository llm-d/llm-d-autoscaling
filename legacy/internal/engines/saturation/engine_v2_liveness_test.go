package saturation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("analyzer liveness gate (engine level)", func() {

	// liveEngine builds a minimal Engine with a single saturation analyzer
	// entry, suitable for calling runAnalyzersAndScore directly. Config is
	// left nil so the liveness threshold falls back to the 30s default
	// (analyzerLivenessStaleCycles=3 → 90s window).
	liveEngine := func(sat domain.Analyzer) *Engine {
		return &Engine{
			saturationV2Analyzer: sat,
			analyzersSnapshot:    []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}},
			started:              true,
		}
	}

	cfg := config.SaturationScalingConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}

	It("recovers: a non-live analyzer becomes live again once it produces a fresh informative result", func() {
		noData := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now(),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "no-data"}},
			},
		}
		e := liveEngine(noData)

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeFalse())

		informative := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now(),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e.saturationV2Analyzer = informative
		e.analyzersSnapshot = []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: informative}}

		results, err = e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeTrue())
	})

	It("staleness boundary: just inside the threshold is live, just past it is not", func() {
		// The threshold is 90s (3 × default 30s interval). AnalyzedAt is captured
		// here, before runAnalyzersAndScore captures its own "now" a moment later,
		// so testing the exact millisecond boundary would be flaky — use a small
		// margin on each side instead.
		atThreshold := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now().Add(-89 * time.Second),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e := liveEngine(atThreshold)
		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeTrue())

		pastThreshold := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now().Add(-95 * time.Second),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e2 := liveEngine(pastThreshold)
		results, err = e2.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeFalse())
	})

	It("scopes liveness per model: one model's fresh result does not make another model's never-analyzed entry live", func() {
		// model-a's own call below must be non-informative (Reason: "no-data"), so it never
		// writes its own timestamp. If it were informative (even with an old AnalyzedAt), that
		// write alone would force Live=false regardless of whether the map is keyed correctly
		// per (name, model, namespace) or buggily by name only — the two keyings would be
		// indistinguishable, and the test would guard nothing. With a non-informative model-a
		// result, correct per-tuple keying leaves model-a's entry absent (never written) →
		// Live=false; a name-only-keyed map would instead read back model-b's fresh write under
		// the shared "saturation" key → Live=true — so this discriminates the two keyings.
		neverAnalyzed := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now(),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "no-data"}},
			},
		}
		e := liveEngine(neverAnalyzed)

		// model-b, fresh and informative: refreshes lastGoodAnalysis for model-b only.
		fresh := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now(),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e.saturationV2Analyzer = fresh
		e.analyzersSnapshot = []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: fresh}}
		_, err := e.runAnalyzersAndScore(context.Background(), "model-b", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())

		// model-a's never-analyzed, non-informative result must still be non-live — not
		// contaminated by model-b's freshness.
		e.saturationV2Analyzer = neverAnalyzed
		e.analyzersSnapshot = []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: neverAnalyzed}}
		results, err := e.runAnalyzersAndScore(context.Background(), "model-a", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeFalse())
	})

	It("falls back to the 30s default interval when a present Config reports a non-positive one", func() {
		// Load() always sanitizes the interval to at least MinOptimizationInterval, so a
		// non-positive OptimizationInterval() can only be reached via direct field
		// manipulation (config.SetOptimizationIntervalForTest) — this guards against that
		// Config ever reaching updateLivenessAndSetLive with a zero/negative interval, which
		// would otherwise zero the threshold and latch every analyzer non-live.
		fresh := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Now(),
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e := liveEngine(fresh)
		e.Config = config.NewTestConfig()
		config.SetOptimizationIntervalForTest(e.Config, 0)

		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeTrue())
	})

	It("treats a zero-valued AnalyzedAt on an informative result as current, not instantly-stale", func() {
		zeroTimestamp := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Time{},
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "P1-obs"}},
			},
		}
		e := liveEngine(zeroTimestamp)
		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeTrue())
	})

	It("leaves a non-informative result with a zero-valued AnalyzedAt excluded from liveness", func() {
		zeroTimestampNoData := &fakeAnalyzerWithResult{
			analyzerName: domain.SaturationAnalyzerName,
			result: &domain.AnalyzerResult{
				AnalyzedAt:        time.Time{},
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", Reason: "no-data"}},
			},
		}
		e := liveEngine(zeroTimestampNoData)
		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(namedByName(results)[domain.SaturationAnalyzerName].Live).To(BeFalse())
	})
})
