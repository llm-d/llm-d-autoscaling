package throughput

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// TestFallbackKSatMatchesConfigDefault pins the duplicated k_sat literal against
// the value ApplyDefaults writes into SaturationScalingConfig.KvCacheThreshold.
// The literal is duplicated rather than imported because internal/config is a
// lower layer than the analyzers and its own in-package tests import this
// package, so a production import here is a test-binary import cycle. This test
// restores the coupling at test time: a change to either value fails here rather
// than silently giving the no-config path a different definition of "full".
//
// The mirror image of TestThroughputAnalyzerName_MatchesCanonical in
// internal/config, which guards the literal duplicated in the other direction.
func TestFallbackKSatMatchesConfigDefault(t *testing.T) {
	if fallbackKSat != config.DefaultKvCacheThreshold {
		t.Fatalf("fallbackKSat = %v, want %v (config.DefaultKvCacheThreshold); the duplicated literal has drifted",
			fallbackKSat, config.DefaultKvCacheThreshold)
	}
}

// otherAnalyzerConfig is an AnalyzerConfig that exposes no KSat, standing in for
// any future analyzer config that has not adopted the accessor.
type otherAnalyzerConfig struct{}

func (otherAnalyzerConfig) GetAnalyzerName() string { return "some-other-analyzer" }

var _ = Describe("resolveKSat", func() {
	DescribeTable("reads the configured k_sat, falling back only when there is none",
		func(cfg domain.AnalyzerConfig, want float64) {
			Expect(resolveKSat(cfg)).To(Equal(want))
		},
		Entry("a saturation config carries its threshold through verbatim",
			&config.SaturationScalingConfig{KvCacheThreshold: 0.5}, 0.5),
		Entry("an unset threshold is not a configured zero",
			&config.SaturationScalingConfig{}, fallbackKSat),
		Entry("a negative threshold is rejected the same way",
			&config.SaturationScalingConfig{KvCacheThreshold: -0.3}, fallbackKSat),
		Entry("no config at all", nil, fallbackKSat),
		Entry("a config that exposes no k_sat", otherAnalyzerConfig{}, fallbackKSat),
	)

	It("is not the scale-up watermark", func() {
		// The bug this replaced: the hard-coded k_sat held ScaleUpThreshold's value,
		// which is a margin the engine applies to RC/SC after Analyze returns, not
		// the KV fraction at which a replica counts as full. A config with the two
		// set apart must resolve to the KV threshold.
		cfg := &config.SaturationScalingConfig{
			KvCacheThreshold:  0.6,
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		Expect(resolveKSat(cfg)).To(Equal(0.6))
	})
})

var _ = Describe("Analyze — per-replica capacity tracks the configured k_sat", func() {
	// Same scenario as the tier-1 OLS block in analyzer_test.go: IL=5000, OL=200,
	// prefix=0.1, KV_max=1024000, A=0.073, B=0.006, so KVreq = 4600.
	//
	//   mu_sat(k) = (k × KV_max / KVreq) / (A×k + B)
	//
	// k enters numerator and denominator, so the ratio is deliberately insensitive
	// to it: over the whole plausible range mu_sat moves by well under 1%. That is
	// why these assertions are tight where the rest of the file uses an
	// order-of-magnitude +/-10% band: swapping 0.85 for 0.80 moves mu_sat by 0.55%,
	// which is how a k_sat that mirrored the scale-up watermark survived in a file
	// full of capacity assertions. k = 0.5 is chosen far outside the plausible
	// range to get a deviation that is unmissable rather than merely detectable.
	//
	//   k = 0.85: N_sat = 189.22, ITL_sat = 0.06805 -> mu_sat = 2780.56 (the bug)
	//   k = 0.80: N_sat = 178.09, ITL_sat = 0.0644  -> mu_sat = 2765.33
	//   k = 0.50: N_sat = 111.30, ITL_sat = 0.0425  -> mu_sat = 2618.93
	const (
		il     = 5000.0
		ol     = 200.0
		prefix = 0.1
		kvMax  = int64(1024000)
		A      = 0.073
		B      = 0.006

		muSatAtDefault = 2765.33
		muSatAtHalf    = 2618.93
		// Relative, and deliberately far tighter than the +/-10% band the rest of
		// the file uses for order-of-magnitude checks: 0.85 vs 0.80 is only a 0.55%
		// move in mu_sat, so at 10% -- or even at 1% -- the wrong constant passes.
		// The fit is OLS over exact synthetic points, so it recovers A and B to
		// float precision and there is no noise to leave room for.
		tolerance = 0.002
	)

	kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

	var (
		analyzer  *ThroughputAnalyzer
		ctx       context.Context
		modelID   string
		namespace string
	)

	BeforeEach(func() {
		analyzer = NewThroughputAnalyzer()
		ctx = context.Background()
		modelID = testModelID
		namespace = testNamespace
		injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)
		state, ok := analyzer.VariantState(modelID, namespace, "v1")
		Expect(ok).To(BeTrue())
		Expect(state.ObservationReady).To(BeTrue(), "the tier-1 fit must be the model under test")
	})

	analyzeWith := func(cfg domain.AnalyzerConfig) *domain.AnalyzerResult {
		const k = 0.50
		result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
			ModelID:   modelID,
			Namespace: namespace,
			Config:    cfg,
			ReplicaMetrics: []domain.ReplicaMetrics{{
				VariantName:           "v1",
				KvCacheUsage:          k,
				KvUsageInstant:        k,
				AvgITL:                A*k + B,
				AvgInputTokens:        il,
				AvgOutputTokens:       ol,
				PrefixCacheHitRate:    prefix,
				TotalKvCapacityTokens: kvMax,
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	DescribeTable("prices a replica at the k_sat the config asks for",
		func(cfg domain.AnalyzerConfig, wantMuSat float64) {
			result := analyzeWith(cfg)
			Expect(result.VariantCapacities).To(HaveLen(1))
			Expect(result.VariantCapacities[0].PerReplicaCapacity).
				To(BeNumerically("~", wantMuSat, wantMuSat*tolerance))
			Expect(result.TotalSupply).To(BeNumerically("~", wantMuSat, wantMuSat*tolerance))
		},
		Entry("k_sat = 0.5 from the saturation config",
			&config.SaturationScalingConfig{KvCacheThreshold: 0.5}, muSatAtHalf),
		Entry("no config falls back to the default k_sat", nil, muSatAtDefault),
	)

	It("distinguishes the two, so neither entry above can pass by accident", func() {
		configured := analyzeWith(&config.SaturationScalingConfig{KvCacheThreshold: 0.5})
		fallback := analyzeWith(nil)
		Expect(configured.TotalSupply).NotTo(BeNumerically("~", fallback.TotalSupply, fallback.TotalSupply*tolerance))
	})

	It("does not read the scale-up watermark", func() {
		// The exact shape of the trap: a config whose KV threshold is the default
		// and whose scale-up watermark is 0.85. Reading the watermark prices this
		// at k = 0.85; the correct k is the KV threshold's 0.80. The two are
		// different numbers for different jobs, which is why they are set apart
		// here rather than left at their defaults.
		cfg := &config.SaturationScalingConfig{
			KvCacheThreshold:  config.DefaultKvCacheThreshold,
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
		}
		Expect(analyzeWith(cfg).TotalSupply).
			To(BeNumerically("~", muSatAtDefault, muSatAtDefault*tolerance))
	})

	It("keeps the observation window and the fit independent of k_sat", func() {
		// k_sat prices the fit; it does not decide which observations are
		// admissible (DefaultMinObservableK/DefaultMaxObservableK) nor what A and
		// B come out as. A k_sat far below the observed range must not disturb it.
		before, _ := analyzer.VariantState(modelID, namespace, "v1")
		analyzeWith(&config.SaturationScalingConfig{KvCacheThreshold: 0.5})
		after, ok := analyzer.VariantState(modelID, namespace, "v1")
		Expect(ok).To(BeTrue())
		Expect(after.ObservationReady).To(BeTrue())
		Expect(after.SampleCount).To(BeNumerically(">=", before.SampleCount))
		Expect(after.ITLModel.A).To(BeNumerically("~", A, A*tolerance))
		Expect(after.ITLModel.B).To(BeNumerically("~", B, B*tolerance))
	})
})
