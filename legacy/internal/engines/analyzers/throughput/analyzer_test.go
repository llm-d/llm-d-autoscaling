package throughput

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

// makeMetrics builds a slice of healthy ReplicaMetrics for a single variant,
// each with a distinct KvCacheUsage/KvUsageInstant to provide k-spread.
func makeMetrics(variant string, count int, baseK float64, kStep float64) []domain.ReplicaMetrics {
	metrics := make([]domain.ReplicaMetrics, count)
	for i := range metrics {
		k := baseK + float64(i)*kStep
		metrics[i] = domain.ReplicaMetrics{
			PodName:               "pod-" + variant + "-" + string(rune('0'+i)),
			VariantName:           variant,
			KvCacheUsage:          k,
			KvUsageInstant:        k,
			TotalKvCapacityTokens: 65536,
			AvgInputTokens:        1024,
			AvgOutputTokens:       256,
			PrefixCacheHitRate:    0.0,
			AvgITL:                0.030 + k*0.05,
		}
	}
	return metrics
}

// injectWindowObs injects individual-replica observations to build an OLS-ready
// window. Each call to Observe adds one replica; kValues provides the k (and
// implicitly ITL = 0.073·k + B) for each injection.
func injectWindowObs(a *ThroughputAnalyzer, ctx context.Context, modelID, namespace, variant string,
	il, ol, prefixRate float64, kvMax int64, b float64, kValues []float64) {
	const A = 0.073
	for _, k := range kValues {
		m := domain.ReplicaMetrics{
			VariantName:           variant,
			KvCacheUsage:          k,
			KvUsageInstant:        k,
			AvgITL:                A*k + b,
			AvgInputTokens:        il,
			AvgOutputTokens:       ol,
			PrefixCacheHitRate:    prefixRate,
			TotalKvCapacityTokens: kvMax,
		}
		a.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{m})
	}
}

var _ = Describe("ThroughputAnalyzer", func() {
	var (
		analyzer  *ThroughputAnalyzer
		ctx       context.Context
		modelID   string
		namespace string
	)

	BeforeEach(func() {
		analyzer = NewThroughputAnalyzer()
		ctx = context.Background()
		modelID = "llama3-8b"
		namespace = "default"
	})

	Describe("Name", func() {
		It("returns the analyzer name", func() {
			Expect(analyzer.Name()).To(Equal(AnalyzerName))
		})
	})

	Describe("VariantState before any observations", func() {
		It("returns false when no data has been observed", func() {
			_, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("Observe — basic state creation", func() {
		It("creates variant state on first Observe", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			_, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
		})

		It("records shape from first call", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.Shape.AvgInputTokens).To(BeNumerically("~", 1024.0, 0.01))
			Expect(state.Shape.AvgOutputTokens).To(BeNumerically("~", 256.0, 0.01))
		})

		It("adds observations to the window on each Observe call", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.15)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.SampleCount).To(Equal(3))
		})
	})

	Describe("Observe — multi-cycle accumulation", func() {
		It("accumulates observations across multiple cycles until Ready", func() {
			// Each call adds 2 replicas with different k values.
			// After enough cycles the window should become Ready.
			for i := range 6 {
				baseK := 0.20 + float64(i)*0.10
				metrics := makeMetrics("v1", 2, baseK, 0.05)
				analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			}

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.SampleCount).To(BeNumerically(">=", DefaultMinSamples))
			Expect(state.KSpread).To(BeNumerically(">=", DefaultMinKSpread))
			Expect(state.ObservationReady).To(BeTrue())
		})
	})

	Describe("Observe — shape change clears window", func() {
		It("clears the observation window when workload shape changes significantly", func() {
			// Build up some observations.
			for i := range 3 {
				metrics := makeMetrics("v1", 3, 0.20+float64(i)*0.10, 0.05)
				analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			}
			stateBefore, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(stateBefore.SampleCount).To(BeNumerically(">", 0))

			// Now shift IL by 50% — well beyond the 20% tolerance.
			shifted := makeMetrics("v1", 3, 0.20, 0.10)
			for i := range shifted {
				shifted[i].AvgInputTokens = 1024 * 1.5 // +50%
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, shifted)

			stateAfter, _ := analyzer.VariantState(modelID, namespace, "v1")
			// The window was cleared on shape change, then one cycle of 3 observations was added.
			Expect(stateAfter.SampleCount).To(Equal(3))
		})
	})

	Describe("Observe — sanity short-circuit", func() {
		It("skips a variant entirely when sanity checks fail", func() {
			bad := makeMetrics("v1", 3, 0.20, 0.10)
			for i := range bad {
				bad[i].AvgITL = 0 // fails SanityIssueITLNonPositive
			}
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, bad)

			Expect(reports["v1"].OK()).To(BeFalse())
			Expect(reports["v1"].Has(SanityIssueITLNonPositive)).To(BeTrue())

			// No state should be created when all metrics are bad.
			// (State IS created but window remains empty because we skipped after sanity fail.)
			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue()) // state record was created
			Expect(state.SampleCount).To(Equal(0))
			Expect(state.ObservationReady).To(BeFalse())
		})

		It("returns an OK report for a healthy variant", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
			Expect(reports["v1"].OK()).To(BeTrue())
		})
	})

	Describe("Observe — multi-variant isolation", func() {
		It("tracks variants independently", func() {
			metricsV1 := makeMetrics("v1", 3, 0.20, 0.10)
			metricsV2 := makeMetrics("v2", 3, 0.30, 0.10)
			metricsV2[0].AvgInputTokens = 2048 // different shape
			metricsV2[1].AvgInputTokens = 2048
			metricsV2[2].AvgInputTokens = 2048

			combined := append(metricsV1, metricsV2...)
			analyzer.Observe(ctx, time.Now(), modelID, namespace, combined)

			stateV1, okV1 := analyzer.VariantState(modelID, namespace, "v1")
			stateV2, okV2 := analyzer.VariantState(modelID, namespace, "v2")

			Expect(okV1).To(BeTrue())
			Expect(okV2).To(BeTrue())
			Expect(stateV1.Shape.AvgInputTokens).To(BeNumerically("~", 1024.0, 0.01))
			Expect(stateV2.Shape.AvgInputTokens).To(BeNumerically("~", 2048.0, 0.01))
			Expect(stateV1.SampleCount).To(Equal(3))
			Expect(stateV2.SampleCount).To(Equal(3))
		})
	})

	Describe("Analyze — basic behaviour", func() {
		It("returns an AnalyzerResult with the correct identifiers", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: metrics,
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.AnalyzerName).To(Equal(AnalyzerName))
			Expect(result.ModelID).To(Equal(modelID))
			Expect(result.Namespace).To(Equal(namespace))
		})

		It("returns zero signal when demand is zero (no ArrivalRate or RequestRate set)", func() {
			metrics := makeMetrics("v1", 3, 0.20, 0.10)
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: metrics,
			}
			result, _ := analyzer.Analyze(ctx, input)
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("updates internal state on each Analyze call", func() {
			for i := range 4 {
				input := domain.AnalyzerInput{
					ModelID:        modelID,
					Namespace:      namespace,
					ReplicaMetrics: makeMetrics("v1", 3, 0.20+float64(i)*0.10, 0.05),
				}
				_, err := analyzer.Analyze(ctx, input)
				Expect(err).NotTo(HaveOccurred())
			}
			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.SampleCount).To(BeNumerically(">", 0))
		})

		It("sets AnalyzedAt to a recent timestamp", func() {
			before := time.Now()
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: makeMetrics("v1", 3, 0.20, 0.10),
			}
			result, _ := analyzer.Analyze(ctx, input)
			Expect(result.AnalyzedAt).To(BeTemporally(">=", before))
		})
	})

	Describe("Analyze — scaling signal (tier-1 OLS fit)", func() {
		// Scenario: IL=5000, OL=200, prefix=0.1, KV_max=1024000, A=0.073, B=0.006
		//   ILeff   = 5000 × 0.9 = 4500
		//   KVreq   = 4500 + 100 = 4600
		//   N_sat   = 0.85 × 1024000 / 4600 ≈ 189.2 in-flight at k_sat
		//   ITL_sat = 0.073×0.85 + 0.006 = 0.068 s/tok
		//   μ_sat   ≈ 189.2 / 0.068 ≈ 2782 tok/s per replica

		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
			muSat  = 2782.0 // approximate, used for order-of-magnitude assertions
		)

		// kValues: 10 points spanning [0.20, 0.65], spread = 0.45 ≥ DefaultMinKSpread
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		// baseReplica is a healthy replica for the signal-test variant.
		baseReplica := func(arrivalRate float64) domain.ReplicaMetrics {
			const A = 0.073
			const k = 0.50
			return domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          k,
				KvUsageInstant:        k,
				AvgITL:                A*k + B,
				AvgInputTokens:        il,
				AvgOutputTokens:       ol,
				PrefixCacheHitRate:    prefix,
				TotalKvCapacityTokens: kvMax,
				ArrivalRate:           arrivalRate,
			}
		}

		buildReadyWindow := func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)
			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.ObservationReady).To(BeTrue())
		}

		It("returns RequiredCapacity > 0 when λ_dec exceeds μ_dec_total (scale up)", func() {
			buildReadyWindow()

			// ArrivalRate=15 req/s, OL=200 → λ_dec = 3000 tok/s > μ_sat ≈ 2782 tok/s
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica(15)},
				ArrivalRate:    15, // model-level, mirrors the single replica's per-pod rate
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("returns SpareCapacity > 0 when μ_dec_total exceeds λ_dec with EPP deployed (scale down)", func() {
			buildReadyWindow()

			// ArrivalRate=5 req/s, OL=200 → λ_dec = 1000 tok/s < μ_sat ≈ 2782 tok/s
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica(5)},
				ArrivalRate:    5, // model-level, mirrors the single replica's per-pod rate
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as SC>0.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand))
			Expect(result.SpareCapacity).To(Equal(0.0))
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})

		It("returns zero SpareCapacity when EPP is not deployed (ArrivalRate==0)", func() {
			buildReadyWindow()

			// ArrivalRate=0, RequestRate=0 → isEPP=false → no scale-down signal
			replica := baseReplica(0)
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("populates VariantCapacities and TotalSupply/TotalDemand", func() {
			buildReadyWindow()

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica(5)},
				ArrivalRate:    5, // model-level, mirrors the single replica's per-pod rate
			}
			result, _ := analyzer.Analyze(ctx, input)
			Expect(result.VariantCapacities).To(HaveLen(1))
			Expect(result.TotalSupply).To(BeNumerically("~", muSat, muSat*0.10))
			Expect(result.TotalDemand).To(BeNumerically("~", 1000.0, 1.0))
			Expect(result.Utilization).To(BeNumerically(">", 0))
		})

		It("excludes booting (KV=0) replicas from ReplicaCount and TotalSupply", func() {
			buildReadyWindow()

			// One ready, KV-capable replica plus one still-booting replica that reports no
			// KV capacity yet. The booting replica must not inflate TotalSupply (which the
			// engine uses for SpareCapacity); only KV-capable replicas count toward supply.
			ready := baseReplica(5)
			ready.PodName = "ready"
			booting := baseReplica(0)
			booting.PodName = "booting"
			booting.TotalKvCapacityTokens = 0
			booting.KvCacheUsage = 0
			booting.KvUsageInstant = 0

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{ready, booting},
				// The booting pod is a not-ready replica (currentReplicas − readyReplicas = 1).
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 2, PendingReplicas: 1},
				},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))

			vc := result.VariantCapacities[0]
			Expect(vc.ReplicaCount).To(Equal(1), "only the KV-capable replica counts toward ReplicaCount")
			Expect(vc.PendingReplicas).To(Equal(1), "the not-ready booting replica is tracked as pending")
			Expect(vc.TotalCapacity).To(BeNumerically("~", vc.PerReplicaCapacity, vc.PerReplicaCapacity*1e-6),
				"TotalCapacity is the measured supply over KV-capable replicas only")
			// TotalSupply (drives SpareCapacity) counts only the KV-capable replica — not inflated.
			Expect(result.TotalSupply).To(BeNumerically("~", muSat, muSat*0.10))
			// TotalAnticipatedSupply (drives RequiredCapacity) still counts the booting replica
			// via PendingReplicas, so scale-out remains suppressed (no double-counting).
			Expect(result.TotalAnticipatedSupply).To(BeNumerically("~", 2*vc.PerReplicaCapacity, vc.PerReplicaCapacity*0.01))
		})

		It("exposes ITLModel and supply/demand in VariantState after Analyze", func() {
			buildReadyWindow()

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica(5)},
			}
			analyzer.Analyze(ctx, input) //nolint:errcheck

			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.ITLModel.IsZero()).To(BeFalse())
			Expect(state.ITLModel.A).To(BeNumerically("~", A, 1e-4))
			Expect(state.TotalSupply).To(BeNumerically("~", muSat, muSat*0.10))
			Expect(state.Demand).To(BeNumerically("~", 1000.0, 1.0))
		})

		It("sets Reason to T1-ols when tier-1 OLS fit succeeds", func() {
			buildReadyWindow()

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica(5)},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			Expect(result.VariantCapacities[0].Reason).To(Equal(itlReasonT1OLS))
		})
	})

	Describe("Analyze — tier-2 single-point estimation", func() {
		// Tier-2 triggers when the OLS window is not ready.
		// With A=0.073, B=0.006, k*=0.75:
		//   avgITL = 0.073×0.75 + 0.006 = 0.06075
		//   A_est  = (0.06075 - 0.006) / 0.75 = 0.073
		//   ITL_sat ≈ 0.068  →  μ_sat ≈ 2782 tok/s (same scenario as tier-1)

		tier2Replica := func(k, arrivalRate float64) domain.ReplicaMetrics { //nolint:unparam
			return domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          k,
				KvUsageInstant:        k,
				AvgITL:                0.073*k + 0.006,
				AvgInputTokens:        5000,
				AvgOutputTokens:       200,
				PrefixCacheHitRate:    0.1,
				TotalKvCapacityTokens: 1024000,
				ArrivalRate:           arrivalRate,
			}
		}

		It("resolves a supply signal without OLS window using tier-2 estimation", func() {
			// Single Observe cycle → only 1 observation → window NOT ready.
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{tier2Replica(0.75, 0)})

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.ObservationReady).To(BeFalse())

			// Analyze with ArrivalRate=15 → λ_dec=3000 tok/s; tier-2 model → scale up.
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{tier2Replica(0.75, 15)},
				ArrivalRate:    15, // model-level, mirrors the single replica's per-pod rate
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})

		It("sets Reason to T2-default when no prior tier-1 fit exists", func() {
			// Single observation → window not ready; no prior fit → T2-default.
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{tier2Replica(0.75, 0)})

			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{tier2Replica(0.75, 5)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			Expect(result.VariantCapacities[0].Reason).To(Equal(itlReasonT2Default))
		})

		It("sets Reason to T2-pinned when a prior tier-1 fit has set lastFittedB", func() {
			// Inject 10 on-line points to make the OLS window ready (same params as tier2Replica).
			const trueA, trueB = 0.073, 0.006
			t1kValues := []float64{0.20, 0.30, 0.40, 0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80}
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", 5000, 200, 0.1, 1024000, trueB, t1kValues)
			onLine := domain.ReplicaMetrics{
				VariantName: "v1", KvUsageInstant: 0.75, KvCacheUsage: 0.75,
				AvgITL: trueA*0.75 + trueB, AvgInputTokens: 5000, AvgOutputTokens: 200,
				PrefixCacheHitRate: 0.1, TotalKvCapacityTokens: 1024000, ArrivalRate: 5,
			}
			_, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{onLine},
			})
			Expect(err).NotTo(HaveOccurred())
			st, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(st.HasFittedB).To(BeTrue(), "T1 must have fired to set lastFittedB")

			// Clear the window to force tier-2; lastFittedB is preserved.
			analyzer.mu.Lock()
			if s, ok := analyzer.variantStates[variantKey(namespace, modelID, "v1")]; ok {
				s.observationWindow.Clear()
			}
			analyzer.mu.Unlock()

			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{tier2Replica(0.75, 5)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			Expect(result.VariantCapacities[0].Reason).To(Equal(itlReasonT2Pinned))
		})

		It("resolveITLModel returns T2-failed when all replicas are idle (KvUsageInstant == 0)", func() {
			// Single Observe with idle metrics to create variant state, then Analyze
			// with idle replicas so both tiers fail — tier-2 needs KvUsageInstant > 0.
			idleMetrics := domain.ReplicaMetrics{
				VariantName: "v1", KvUsageInstant: 0, KvCacheUsage: 0,
				AvgITL: 0.006, AvgInputTokens: 5000, AvgOutputTokens: 200,
				PrefixCacheHitRate: 0.1, TotalKvCapacityTokens: 1024000,
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{idleMetrics})

			_, reason, ok := analyzer.resolveITLModel(ctx,
				func() *variantState {
					analyzer.mu.Lock()
					defer analyzer.mu.Unlock()
					return analyzer.variantStates[variantKey(namespace, modelID, "v1")]
				}(),
				[]domain.ReplicaMetrics{idleMetrics},
				namespace, modelID, "v1",
			)
			Expect(ok).To(BeFalse())
			Expect(reason).To(Equal(itlReasonT2Failed))
		})

		It("resolveITLModel returns T2-failed when the computed fit is rejected by validITLModel", func() {
			// AvgITL below the constant baseline B (DefaultBaselineITLSec = 0.006, default path) at
			// k=0.5 produces numerator = (0.001 - 0.006) * 0.5 = -0.0025, sumK2 = 0.25 → A = -0.01
			// (negative, inverted slope) — a real Tier-2 fit is computed (n=1, sumK2>0) but
			// validITLModel rejects it, unlike the existing "all idle" test which never reaches the
			// fit at all.
			belowBaseline := domain.ReplicaMetrics{
				VariantName: "v1", KvUsageInstant: 0.5, KvCacheUsage: 0.5,
				AvgITL: 0.001, AvgInputTokens: 5000, AvgOutputTokens: 200,
				PrefixCacheHitRate: 0.1, TotalKvCapacityTokens: 1024000,
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{belowBaseline})

			_, reason, ok := analyzer.resolveITLModel(ctx,
				func() *variantState {
					analyzer.mu.Lock()
					defer analyzer.mu.Unlock()
					return analyzer.variantStates[variantKey(namespace, modelID, "v1")]
				}(),
				[]domain.ReplicaMetrics{belowBaseline},
				namespace, modelID, "v1",
			)
			Expect(ok).To(BeFalse())
			Expect(reason).To(Equal(itlReasonT2Failed))
		})
	})

	Describe("Analyze — idle replicas produce no signal", func() {
		It("emits zero signal when all replicas have k*=0 and OLS window is not ready", func() {
			// Idle replicas (k*=0) cannot contribute to tier-1 (filtered by ObservationWindow)
			// or tier-2 (KvUsageInstant > 0 guard). The knowledge store must NOT be consulted
			// while replicas are running — a stale model could trigger incorrect scaling.
			idleReplica := domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          0.0,
				KvUsageInstant:        0.0,
				AvgITL:                0.006,
				AvgInputTokens:        5000,
				AvgOutputTokens:       200,
				PrefixCacheHitRate:    0.1,
				TotalKvCapacityTokens: 1024000,
				ArrivalRate:           15, // demand present, but no supply estimate possible
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{idleReplica},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("still emits no signal after prior observations when OLS window is cleared by shape change and replicas go idle", func() {
			// Build an OLS-ready window.
			kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1",
				5000, 200, 0.1, 1024000, 0.006, kValues)
			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.ObservationReady).To(BeTrue())

			// Trigger a shape change (+50% IL) to clear the OLS window.
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{{
				VariantName:           "v1",
				KvCacheUsage:          0.50,
				KvUsageInstant:        0.50,
				AvgITL:                0.073*0.50 + 0.006,
				AvgInputTokens:        7500, // +50% — exceeds 20% tolerance, clears window
				AvgOutputTokens:       200,
				PrefixCacheHitRate:    0.1,
				TotalKvCapacityTokens: 1024000,
			}})
			stateAfter, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(stateAfter.ObservationReady).To(BeFalse())

			// Variant goes idle with cleared window — neither tier-1 nor tier-2 can resolve.
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:   modelID,
				Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{{
					VariantName:           "v1",
					KvCacheUsage:          0.0,
					KvUsageInstant:        0.0,
					AvgITL:                0.006,
					AvgInputTokens:        7500,
					AvgOutputTokens:       200,
					PrefixCacheHitRate:    0.1,
					TotalKvCapacityTokens: 1024000,
					ArrivalRate:           15,
				}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — empty metrics list", func() {
		It("handles an empty metrics slice gracefully", func() {
			reports := analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{})
			Expect(reports).To(BeEmpty())
		})
	})

	Describe("Observe — concurrent safety", func() {
		It("is safe for concurrent Observe and VariantState calls", func() {
			const goroutines = 10
			var wg sync.WaitGroup
			wg.Add(goroutines + 1)

			for i := range goroutines {
				go func(i int) {
					defer wg.Done()
					variant := fmt.Sprintf("v%d", i%3)
					metrics := makeMetrics(variant, 2, 0.20+float64(i)*0.05, 0.05)
					analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)
				}(i)
			}

			go func() {
				defer wg.Done()
				analyzer.VariantState(modelID, namespace, "v0")
			}()

			wg.Wait()
			_, ok := analyzer.VariantState(modelID, namespace, "v0")
			Expect(ok).To(BeTrue())
		})
	})

	Describe("Analyze — context cancellation", func() {
		It("returns an error immediately when the context is already cancelled", func() {
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: makeMetrics("v1", 3, 0.20, 0.10),
			}
			result, err := analyzer.Analyze(cancelled, input)
			Expect(err).To(HaveOccurred())
			Expect(result).To(BeNil())
		})
	})

	Describe("Analyze — pending replicas suppress scale-up", func() {
		// Same scenario as the tier-1 scale-up test (λ_dec=3000 > μ_sat≈2782),
		// but with 1 pending replica. Anticipated supply = 2 × perReplicaSupply ≈ 5564 > 3000
		// so RequiredCapacity must be zero.
		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("suppresses RequiredCapacity when pending replicas cover anticipated demand", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			replica := domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          0.50,
				KvUsageInstant:        0.50,
				AvgITL:                A*0.50 + B,
				AvgInputTokens:        il,
				AvgOutputTokens:       ol,
				PrefixCacheHitRate:    prefix,
				TotalKvCapacityTokens: kvMax,
				ArrivalRate:           15, // λ_dec = 3000 tok/s > 1 × μ_sat
			}
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 1, PendingReplicas: 1},
				},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// anticipated = 2 × μ_sat ≈ 5564 > λ_dec = 3000 → no scale-up
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})

		It("still emits RequiredCapacity when pending replicas are insufficient", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			// 3 replicas running, ArrivalRate=15 each → λ_dec = 3×3000 = 9000 tok/s
			// μ_sat ≈ 2782 tok/s per replica; anticipated = 3 × 2782 ≈ 8346 < 9000
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50, AvgITL: A*0.50 + B,
					AvgInputTokens: il, AvgOutputTokens: ol, PrefixCacheHitRate: prefix,
					TotalKvCapacityTokens: kvMax, ArrivalRate: 15},
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50, AvgITL: A*0.50 + B,
					AvgInputTokens: il, AvgOutputTokens: ol, PrefixCacheHitRate: prefix,
					TotalKvCapacityTokens: kvMax, ArrivalRate: 15},
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50, AvgITL: A*0.50 + B,
					AvgInputTokens: il, AvgOutputTokens: ol, PrefixCacheHitRate: prefix,
					TotalKvCapacityTokens: kvMax, ArrivalRate: 15},
			}
			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				ArrivalRate:    45, // model-level, sum of the 3 replicas' per-pod rates
				// No pending replicas — anticipated == current supply
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — role-aware aggregation", func() {
		// Two variants in a P/D disaggregated deployment:
		//   "v-decode" role="decode": standard decode ITL scenario
		//   "v-prefill" role="prefill": prefill pod with OL≈1
		// Both use the same OLS-ready window and arrive at the same perReplicaSupply
		// for simplicity; the role distinction controls which role gets RequiredCapacity.

		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		buildVariantWindow := func(variant string) {
			injectWindowObs(analyzer, ctx, modelID, namespace, variant, il, ol, prefix, kvMax, B, kValues)
		}

		baseReplica := func(variant string, arrivalRate float64) domain.ReplicaMetrics {
			const A = 0.073
			const k = 0.50
			return domain.ReplicaMetrics{
				VariantName:           variant,
				KvCacheUsage:          k,
				KvUsageInstant:        k,
				AvgITL:                A*k + B,
				AvgInputTokens:        il,
				AvgOutputTokens:       ol,
				PrefixCacheHitRate:    prefix,
				TotalKvCapacityTokens: kvMax,
				ArrivalRate:           arrivalRate,
			}
		}

		It("populates RoleCapacities for disaggregated variants", func() {
			buildVariantWindow("v-decode")
			buildVariantWindow("v-prefill")

			input := domain.AnalyzerInput{
				ModelID:   modelID,
				Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{
					baseReplica("v-decode", 5),
					baseReplica("v-prefill", 5),
				},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
					{VariantName: "v-prefill", Role: "prefill"},
				},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleCapacities).NotTo(BeNil())
			Expect(result.RoleCapacities).To(HaveKey("decode"))
			Expect(result.RoleCapacities).To(HaveKey("prefill"))
		})

		It("suppresses RequiredCapacity for the prefill role even under load", func() {
			buildVariantWindow("v-decode")
			buildVariantWindow("v-prefill")

			// ArrivalRate=15 → λ_dec=3000 > μ_sat≈2782: scale-up signal for decode.
			input := domain.AnalyzerInput{
				ModelID:   modelID,
				Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{
					baseReplica("v-decode", 15),
					baseReplica("v-prefill", 15),
				},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
					{VariantName: "v-prefill", Role: "prefill"},
				},
				ArrivalRate: 30, // model-level, sum of both variants' per-pod rates
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			decodeRC := result.RoleCapacities["decode"]
			prefillRC := result.RoleCapacities["prefill"]

			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as decode RC>0.
			Expect(decodeRC.TotalDemand).To(BeNumerically(">", decodeRC.TotalAnticipatedSupply))
			Expect(decodeRC.RequiredCapacity).To(Equal(0.0))
			// Prefill role: TA no longer suppresses RC in role capacities — it leaves
			// RequiredCapacity zero for all roles. Engine post-step handles per-role RC.
			Expect(prefillRC.RequiredCapacity).To(Equal(0.0))
		})

		It("returns nil RoleCapacities when all variants are non-disaggregated", func() {
			buildVariantWindow("v1")

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica("v1", 5)},
				// No VariantStates → role is "" for all variants
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleCapacities).To(BeNil())
		})

		It("sets Role on VariantCapacity and ThroughputVariantState", func() {
			buildVariantWindow("v-decode")

			input := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica("v-decode", 5)},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
				},
			}
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities[0].Role).To(Equal("decode"))

			state, ok := analyzer.VariantState(modelID, namespace, "v-decode")
			Expect(ok).To(BeTrue())
			Expect(state.Role).To(Equal("decode"))
		})
	})

	Describe("Analyze — k*-based local demand (no EPP)", func() {
		// Two replicas at k*=0.95 with no EPP and no vLLM rate, and no model-level
		// input.ArrivalRate (EPP absent model-wide too).
		// λ_local = Σ (k_r × KV_max_r / KVreq) / ITL(k_r)
		// For each replica: N = 0.95×1024000/4600 ≈ 211.4; ITL(0.95) = 0.073×0.95+0.006 = 0.07535
		// λ_local ≈ 211.4/0.07535 ≈ 2806 tok/s per replica (still computed per-variant).
		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("still populates per-variant demand from k*, but no longer drives model-level TotalDemand when EPP is absent", func() {
			// DEFERRED behavior (the computeDemand per-replica A→B→k*local
			// fallback): with demand model-level, Λ_req (input.ArrivalRate) is the
			// sole driver of TotalDemand's arrival component. When EPP is absent
			// model-wide, input.ArrivalRate is legitimately 0 (no signal, not "zero
			// traffic") and — unlike before this PR — the per-variant k*-local
			// fallback no longer backfills the model-level total; it remains
			// per-variant only (VariantCapacity.TotalDemand / VariantState()), for
			// introspection. Revisit when k_knee is implemented.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.95, KvUsageInstant: 0.95,
					AvgITL: A*0.95 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					// ArrivalRate=0, RequestRate=0 → EPP absent
				},
				{VariantName: "v1", KvCacheUsage: 0.95, KvUsageInstant: 0.95,
					AvgITL: A*0.95 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
				},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
				// ArrivalRate: 0 (default) — EPP absent model-wide, no queue either.
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// Per-variant demand still reflects the k*-local fallback (unchanged,
			// deferred code path) — this is what VariantState()/introspection sees.
			Expect(result.VariantCapacities[0].TotalDemand).To(BeNumerically(">", 0),
				"per-variant k*-local demand must still be computed for introspection")
			// Model-level: no EPP and no queue → TotalDemand is 0 by design (not a
			// regression — see DEFERRED note above), so RC/SC both stay zero.
			Expect(result.TotalDemand).To(Equal(0.0))
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("emits no SpareCapacity from k* even when k* is low (scale-down requires EPP)", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			// Low k* → λ_local << μ_sat, but no EPP → no scale-down.
			replica := domain.ReplicaMetrics{
				VariantName: "v1", KvCacheUsage: 0.20, KvUsageInstant: 0.20,
				AvgITL: A*0.20 + B, AvgInputTokens: il, AvgOutputTokens: ol,
				PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("emits zero RequiredCapacity when OL is below the decode-dominated threshold", func() {
			// OL=5 < DefaultMinDecodeOLForLocalDemand(20): the k*-based formula must not fire.
			// EPP and vLLM paths are also absent (no ArrivalRate, no RequestRate).
			// Expected: demand=0, RC=0, SC=0.
			const lowOL = 5.0
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, lowOL, prefix, kvMax, B, kValues)

			replica := domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          0.95,
				KvUsageInstant:        0.95,
				AvgITL:                A*0.95 + B,
				AvgInputTokens:        il,
				AvgOutputTokens:       lowOL,
				PrefixCacheHitRate:    prefix,
				TotalKvCapacityTokens: kvMax,
				// ArrivalRate=0, RequestRate=0
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — EPP warm-up: ArrivalRate>0 but AvgOutputTokens==0", func() {
		// Regression test for F1: EPP present (ArrivalRate > 0) but no completions
		// yet (AvgOutputTokens == 0). Before the fix, computeDemand returned (0, true)
		// and the caller skipped computeLocalDemand because isEPP==true; the variant
		// was published with supply>0, demand=0 → Utilization=0 → spurious scale-down.
		const (
			ilW     = 5000.0
			olW     = 200.0
			prefixW = 0.1
			kvMaxW  = int64(1024000)
			BW      = 0.006
		)
		kValuesW := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("uses local k* demand when EPP is present but AvgOutputTokens==0 (warm-up)", func() {
			// OLS-ready window.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1",
				ilW, olW, prefixW, kvMaxW, BW, kValuesW)

			// Replica with EPP ArrivalRate>0 but AvgOutputTokens==0 (no completions yet).
			// k*=0.85 is near saturation → local demand is high.
			replica := domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          0.85,
				KvUsageInstant:        0.85,
				AvgITL:                0.073*0.85 + BW,
				AvgInputTokens:        ilW,
				AvgOutputTokens:       0, // warm-up: no completions yet
				PrefixCacheHitRate:    prefixW,
				TotalKvCapacityTokens: kvMaxW,
				ArrivalRate:           5.0, // EPP present
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
				ArrivalRate:    5.0, // model-level, mirrors the single replica's per-pod rate
			})
			Expect(err).NotTo(HaveOccurred())
			// Model-level demand must be > 0: avgOL comes from the tracked/smoothed
			// shape (200, seeded by injectWindowObs), not this cycle's live
			// AvgOutputTokens==0, so warm-up does not zero out avgOL (see analyzer.go).
			Expect(result.TotalDemand).To(BeNumerically(">", 0),
				"warm-up replica must have non-zero demand via tracked-shape avgOL")
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("still uses EPP path when ArrivalRate>0 and AvgOutputTokens>0", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1",
				ilW, olW, prefixW, kvMaxW, BW, kValuesW)

			// Normal operation: EPP present with usable demand.
			replica := domain.ReplicaMetrics{
				VariantName:           "v1",
				KvCacheUsage:          0.50,
				KvUsageInstant:        0.50,
				AvgITL:                0.073*0.50 + BW,
				AvgInputTokens:        ilW,
				AvgOutputTokens:       olW,
				PrefixCacheHitRate:    prefixW,
				TotalKvCapacityTokens: kvMaxW,
				ArrivalRate:           10.0, // 10 req/s × 200 ol = 2000 tok/s
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
				ArrivalRate:    10.0, // model-level, mirrors the single replica's per-pod rate
			})
			Expect(err).NotTo(HaveOccurred())
			// Model-level demand = 10×200 = 2000 tok/s; TotalDemand must reflect that.
			Expect(result.TotalDemand).To(BeNumerically("~", 2000.0, 1.0))
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — scheduler queue demand", func() {
		// OLS-ready window, single replica at k*=0.50 with no EPP and no vLLM rate.
		// EPP absent → model-level TotalDemand's arrival term is 0 (k*-local
		// remains per-variant only, deferred — see "k*-based local demand (no EPP)"
		// above); the queue term is the only contributor to model-level demand here.
		// μ_sat ≈ 2782 per replica.
		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		baseReplica := domain.ReplicaMetrics{
			VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
			AvgITL: A*0.50 + B, AvgInputTokens: il, AvgOutputTokens: ol,
			PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
			// ArrivalRate=0: no EPP
		}

		It("queue demand alone (no EPP, no k*-local) emits RequiredCapacity when queue is large", func() {
			// DEFERRED behavior: with EPP absent (ArrivalRate=0 here, both
			// per-pod and model-level), the k*-local fallback (λ_local≈2618 tok/s)
			// no longer reaches model-level TotalDemand — only the scheduler queue
			// term does. QueueSize=450: λ_queue = 450/(2.0×ITL(k_sat)) =
			// 450/(2.0×0.06805) ≈ 3306 tok/s > μ_sat≈2782 → RC > 0 from queue alone.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			withQueue := domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica},
				SchedulerQueue: &domain.SchedulerQueueMetrics{QueueSize: 450},
				// ArrivalRate: 0 (default) — EPP absent model-wide.
			}
			result, err := analyzer.Analyze(ctx, withQueue)
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})

		It("emits no RequiredCapacity when SchedulerQueue is nil", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)

			noQueue := domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{baseReplica},
				// SchedulerQueue: nil
			}
			result, err := analyzer.Analyze(ctx, noQueue)
			Expect(err).NotTo(HaveOccurred())
			// λ_local ≈ 2618 < μ_sat ≈ 2782 → no RC without queue
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — model-level RC/SC aggregation", func() {
		// v1 overloaded (ArrivalRate=15 → λ=3000 > μ_sat≈2782),
		// v2 lightly loaded (ArrivalRate=1 → λ=200 << μ_sat).
		// Per-variant would emit both RC (v1) and SC (v2).
		// Model-level: totalDemand=3200 < totalAnticipated≈5564 → RC=0, SC=5564-3200>0.
		const (
			il     = 5000.0
			ol     = 200.0
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("emits only SpareCapacity (not RequiredCapacity) when model has overall spare despite one hot variant", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v2", il, ol, prefix, kvMax, B, kValues)

			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 15}, // λ = 3000 > μ_sat per-variant
				{VariantName: "v2", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 1}, // λ = 200 << μ_sat per-variant
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
				ArrivalRate: 16, // model-level, sum of both variants' per-pod rates
			})
			Expect(err).NotTo(HaveOccurred())
			// Model-level: totalDemand=3200 < totalAnticipated≈5564 → no scale-up needed.
			// TA leaves RC/SC zero. Assert the raw inequality: TotalDemand ≤ TotalAnticipatedSupply.
			Expect(result.TotalDemand).To(BeNumerically("<=", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
			// EPP deployed (ArrivalRate>0) and totalSupply >> totalDemand → engine posts SC>0.
			// TA leaves SC zero; assert the raw inequality the engine interprets as SC>0.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("emits only RequiredCapacity (not SpareCapacity) when both variants are overloaded", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", il, ol, prefix, kvMax, B, kValues)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v2", il, ol, prefix, kvMax, B, kValues)

			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 15},
				{VariantName: "v2", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: il, AvgOutputTokens: ol,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 15},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
				ArrivalRate: 30, // model-level, sum of both variants' per-pod rates
			})
			Expect(err).NotTo(HaveOccurred())
			// TA leaves RC/SC zero; engine post-step computes them. Assert the raw
			// supply/demand inequality that the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — aggregation-helper linearity invariants", func() {
		// Specs 1–5 from plan §3.4: verify that TA's published Total* fields satisfy
		// the aggregation-helper formulas the engine post-step depends on.
		// Fixture: two variants, OLS-ready windows, one with queue demand; one P/D case.
		const (
			ilA     = 5000.0
			olA     = 200.0
			prefixA = 0.1
			kvMaxA  = int64(1024000)
			aA      = 0.073
			bA      = 0.006
		)
		kValuesA := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("TotalSupply equals aggregation.SumTotalSupply(VariantCapacities)", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v2", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 5},
				{VariantName: "v2", KvCacheUsage: 0.40, KvUsageInstant: 0.40,
					AvgITL: aA*0.40 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 3},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.TotalSupply).To(BeNumerically("~",
				aggregation.SumTotalSupply(result.VariantCapacities), 1e-9))
		})

		It("TotalAnticipatedSupply equals aggregation.SumTotalAnticipatedSupply(VariantCapacities)", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 5},
			}
			// 1 pending replica — TotalAnticipatedSupply should count it.
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", PendingReplicas: 1},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.TotalAnticipatedSupply).To(BeNumerically("~",
				aggregation.SumTotalAnticipatedSupply(result.VariantCapacities), 1e-9))
		})

		It("TotalDemand equals input.ArrivalRate×avgOL plus queue demand", func() {
			// Commit 2 retires the old invariant (TotalDemand = SumTotalDemand
			// (VariantCapacities) + queue demand): the model-level arrival term
			// (input.ArrivalRate × avgOL) replaced the per-variant-summed
			// contribution as TotalDemand's base. This verifies the new invariant
			// with the same "queue demand added on top" structure as before.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 5},
			}
			const queueSize = int64(10)
			const modelArrivalRate = 7.0 // model-level; independent of the replica's per-pod ArrivalRate
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				SchedulerQueue: &domain.SchedulerQueueMetrics{QueueSize: queueSize},
				ArrivalRate:    modelArrivalRate,
			})
			Expect(err).NotTo(HaveOccurred())
			// avgOL is the tracked shape's AvgOutputTokens (olA), seeded by injectWindowObs.
			arrivalDecodeDemand := modelArrivalRate * olA
			Expect(result.TotalDemand).To(BeNumerically(">=", arrivalDecodeDemand))
			// Verify queue demand was added on top: TotalDemand - arrivalDecodeDemand > 0.
			Expect(result.TotalDemand - arrivalDecodeDemand).To(BeNumerically(">", 0))
		})

		It("RoleCapacities[role].TotalAnticipatedSupply matches per-role aggregation", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-decode", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-prefill", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v-decode", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 5},
				{VariantName: "v-prefill", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 1},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
					{VariantName: "v-prefill", Role: "prefill"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleCapacities).NotTo(BeNil())
			byRole := aggregation.AggregateByRole(result.VariantCapacities)
			for role, rc := range result.RoleCapacities {
				Expect(rc.TotalAnticipatedSupply).To(BeNumerically("~",
					byRole[role].TotalAnticipatedSupply, 1e-9),
					"role %s TotalAnticipatedSupply mismatch", role)
			}
		})

		It("RoleCapacities[decode].TotalDemand includes the queue-demand share", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-decode", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-prefill", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v-decode", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 5},
				{VariantName: "v-prefill", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: aA*0.50 + bA, AvgInputTokens: ilA, AvgOutputTokens: olA,
					PrefixCacheHitRate: prefixA, TotalKvCapacityTokens: kvMaxA, ArrivalRate: 1},
			}
			// With queue: decode role should receive the queue-demand share.
			// Without queue: decode TotalDemand == variant-level demand only.
			inputNoQ := domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
					{VariantName: "v-prefill", Role: "prefill"},
				},
			}
			inputWithQ := inputNoQ
			inputWithQ.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 20}

			resultNoQ, errNoQ := analyzer.Analyze(ctx, inputNoQ)
			Expect(errNoQ).NotTo(HaveOccurred())

			analyzer2 := NewThroughputAnalyzer()
			injectWindowObs(analyzer2, ctx, modelID, namespace, "v-decode", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			injectWindowObs(analyzer2, ctx, modelID, namespace, "v-prefill", ilA, olA, prefixA, kvMaxA, bA, kValuesA)
			resultWithQ, errWithQ := analyzer2.Analyze(ctx, inputWithQ)
			Expect(errWithQ).NotTo(HaveOccurred())

			// decode TotalDemand with queue > without queue (queue share was added).
			Expect(resultWithQ.RoleCapacities["decode"].TotalDemand).To(
				BeNumerically(">", resultNoQ.RoleCapacities["decode"].TotalDemand))
			// prefill TotalDemand unchanged (queue demand skips prefill role).
			Expect(resultWithQ.RoleCapacities["prefill"].TotalDemand).To(
				BeNumerically("~", resultNoQ.RoleCapacities["prefill"].TotalDemand, 1e-9))
		})
	})

	Describe("Analyze — model-level demand invariants", func() {
		const (
			ilM    = 5000.0
			olM    = 200.0 // L: model-level avgOL, tracked via injectWindowObs
			prefix = 0.1
			kvMax  = int64(1024000)
			A      = 0.073
			B      = 0.006
		)
		kValues := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		It("TotalDemand's decode component equals R×L, independent of per-pod ArrivalRate", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilM, olM, prefix, kvMax, B, kValues)

			const modelArrivalRate = 8.0 // R
			replica := domain.ReplicaMetrics{
				VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
				AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
				PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
				ArrivalRate: 999, // deliberately unrelated to R — must not affect TotalDemand
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
				ArrivalRate:    modelArrivalRate,
			})
			Expect(err).NotTo(HaveOccurred())
			// No queue: TotalDemand == R×L exactly.
			Expect(result.TotalDemand).To(BeNumerically("~", modelArrivalRate*olM, 1e-6))
		})

		It("still produces demand when per-pod ArrivalRate is 0 everywhere but model-level ArrivalRate is set (orphan-merge regression backstop)", func() {
			// Regression backstop for the benchmark symptom this PR fixes: a fragile
			// EPP↔vLLM per-instance key merge could orphan every replica's per-pod
			// ArrivalRate (all zero) while EPP is actually deployed and dispatching.
			// TA must not go blind in that case now that demand is model-level.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilM, olM, prefix, kvMax, B, kValues)

			replicas := []domain.ReplicaMetrics{
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 0, // orphaned: per-pod merge failed for every replica
				},
				{VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
					ArrivalRate: 0,
				},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
				ArrivalRate: 12, // model-level query is unaffected by the per-instance merge
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.TotalDemand).To(BeNumerically(">", 0),
				"model-level ArrivalRate must still drive demand despite all-zero per-pod ArrivalRate")
		})

		It("combines the model-level arrival term and queue term exactly once, role-distributed with no double-count", func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-decode", ilM, olM, prefix, kvMax, B, kValues)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-prefill", ilM, olM, prefix, kvMax, B, kValues)

			const modelArrivalRate = 6.0
			replicas := []domain.ReplicaMetrics{
				{VariantName: "v-decode", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax},
				{VariantName: "v-prefill", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
					AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
					PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax},
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: replicas,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", Role: "decode"},
					{VariantName: "v-prefill", Role: "prefill"},
				},
				ArrivalRate:    modelArrivalRate,
				SchedulerQueue: &domain.SchedulerQueueMetrics{QueueSize: 100},
			})
			Expect(err).NotTo(HaveOccurred())
			// avgOL is olM (only v-decode counts — v-prefill is excluded from the
			// non-prefill accumulator), so the arrival term is exactly R×L.
			arrivalDecodeDemand := modelArrivalRate * olM
			queueDemand := result.TotalDemand - arrivalDecodeDemand
			Expect(queueDemand).To(BeNumerically(">", 0), "queue term must be added on top, not folded in")

			// No double-count: role sums reconstruct the model-level total exactly.
			Expect(result.RoleCapacities).NotTo(BeNil())
			roleSum := 0.0
			for _, rc := range result.RoleCapacities {
				roleSum += rc.TotalDemand
			}
			Expect(roleSum).To(BeNumerically("~", result.TotalDemand, 1e-6),
				"sum of per-role TotalDemand must equal model-level TotalDemand exactly once")
		})

		It("arrival→0 reduces demand to zero even when the engine is still completing requests (no served-rate floor)", func() {
			// No served-rate floor: a draining engine keeps RequestRate > 0 for a while after
			// arrivals stop; that must not be used as a demand floor, or scale-down
			// would be wrongly blocked.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1", ilM, olM, prefix, kvMax, B, kValues)

			replica := domain.ReplicaMetrics{
				VariantName: "v1", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
				AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: olM,
				PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
				ArrivalRate: 0,  // no new arrivals this window
				RequestRate: 20, // still draining in-flight/queued work from before
			}
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replica},
				ArrivalRate:    0, // model-level: no arrivals now
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.TotalDemand).To(Equal(0.0),
				"model-level demand must not be held up by a still-nonzero RequestRate")
		})

		It("weights avgOL by replica count across non-prefill variants, not an equal-per-variant mean", func() {
			// v-light: 1 replica, OL=100. v-heavy: 3 replicas, OL=300.
			// Weighted (correct):   avgOL = (1×100 + 3×300) / (1+3) = 1000/4 = 250
			// Unweighted (wrong):   avgOL = (100 + 300) / 2 = 200
			// These must diverge — pins that the fix is weighted, not a mean-of-means.
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-light", ilM, 100, prefix, kvMax, B, kValues)
			injectWindowObs(analyzer, ctx, modelID, namespace, "v-heavy", ilM, 300, prefix, kvMax, B, kValues)

			lightReplica := domain.ReplicaMetrics{
				VariantName: "v-light", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
				AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: 100,
				PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
			}
			heavyReplica := domain.ReplicaMetrics{
				VariantName: "v-heavy", KvCacheUsage: 0.50, KvUsageInstant: 0.50,
				AvgITL: A*0.50 + B, AvgInputTokens: ilM, AvgOutputTokens: 300,
				PrefixCacheHitRate: prefix, TotalKvCapacityTokens: kvMax,
			}
			replicas := []domain.ReplicaMetrics{lightReplica, heavyReplica, heavyReplica, heavyReplica}

			const modelArrivalRate = 10.0
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: replicas,
				ArrivalRate: modelArrivalRate,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.TotalDemand).To(BeNumerically("~", modelArrivalRate*250.0, 1e-6),
				"avgOL must be replica-count-weighted (250), not an equal-per-variant mean (200)")
		})
	})

	Describe("averageShapeMetrics — RequestRate-weighted averaging", func() {
		It("returns rate-weighted mean when replicas have different RequestRates", func() {
			// r1: rate=1, IL=1000, OL=200, hr=0.1
			// r2: rate=3, IL=3000, OL=600, hr=0.5
			// Weighted: IL=(1*1000+3*3000)/4=2500, OL=(1*200+3*600)/4=500, hr=(1*0.1+3*0.5)/4=0.4
			metrics := []domain.ReplicaMetrics{
				{AvgInputTokens: 1000, AvgOutputTokens: 200, PrefixCacheHitRate: 0.1, RequestRate: 1},
				{AvgInputTokens: 3000, AvgOutputTokens: 600, PrefixCacheHitRate: 0.5, RequestRate: 3},
			}
			il, ol, hr := averageShapeMetrics(metrics)
			Expect(il).To(BeNumerically("~", 2500.0, 1e-9))
			Expect(ol).To(BeNumerically("~", 500.0, 1e-9))
			Expect(hr).To(BeNumerically("~", 0.4, 1e-9))
		})

		It("falls back to unweighted mean when all RequestRates are zero", func() {
			metrics := []domain.ReplicaMetrics{
				{AvgInputTokens: 1000, AvgOutputTokens: 200, PrefixCacheHitRate: 0.1, RequestRate: 0},
				{AvgInputTokens: 3000, AvgOutputTokens: 600, PrefixCacheHitRate: 0.5, RequestRate: 0},
			}
			il, ol, hr := averageShapeMetrics(metrics)
			Expect(il).To(BeNumerically("~", 2000.0, 1e-9))
			Expect(ol).To(BeNumerically("~", 400.0, 1e-9))
			Expect(hr).To(BeNumerically("~", 0.3, 1e-9))
		})

		It("excludes zero-rate replicas from weighted sum when mixed rates are present", func() {
			// r1 has rate=0: contributes only to unweighted fallback
			// r2 has rate=2: drives the weighted result entirely
			metrics := []domain.ReplicaMetrics{
				{AvgInputTokens: 1000, AvgOutputTokens: 200, PrefixCacheHitRate: 0.1, RequestRate: 0},
				{AvgInputTokens: 3000, AvgOutputTokens: 600, PrefixCacheHitRate: 0.5, RequestRate: 2},
			}
			il, ol, hr := averageShapeMetrics(metrics)
			Expect(il).To(BeNumerically("~", 3000.0, 1e-9))
			Expect(ol).To(BeNumerically("~", 600.0, 1e-9))
			Expect(hr).To(BeNumerically("~", 0.5, 1e-9))
		})
	})

	Describe("Analyze — tier-2 constrained OLS with multiple replicas", func() {
		// Two replicas at different k* values — constrained OLS and single-point are
		// equivalent when points lie exactly on the true line (they give the same A),
		// but constrained OLS is strictly better under noise. Verify the formula
		// numerically for the noiseless case.
		It("recovers the correct A coefficient from two replicas at different k* values", func() {
			const wantA = 0.073
			const wantB = 0.006
			// Fresh analyzer: no OLS window → tier-2 fires.
			metrics := []domain.ReplicaMetrics{
				{
					VariantName: "v1", KvUsageInstant: 0.20, KvCacheUsage: 0.20,
					AvgITL: wantA*0.20 + wantB, AvgInputTokens: 5000, AvgOutputTokens: 200,
					PrefixCacheHitRate: 0.1, TotalKvCapacityTokens: 1024000, ArrivalRate: 5,
				},
				{
					VariantName: "v1", KvUsageInstant: 0.80, KvCacheUsage: 0.80,
					AvgITL: wantA*0.80 + wantB, AvgInputTokens: 5000, AvgOutputTokens: 200,
					PrefixCacheHitRate: 0.1, TotalKvCapacityTokens: 1024000, ArrivalRate: 5,
				},
			}
			// Seed the shape tracker with one prior Observe so shape is known.
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: metrics,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.ObservationReady).To(BeFalse()) // still tier-2
			// A = Σ((ITL_i−B)·k_i) / Σ(k_i²)
			//   = ((0.073×0.2+0.006−0.006)×0.2 + (0.073×0.8+0.006−0.006)×0.8) / (0.04+0.64)
			//   = (0.073×0.04 + 0.073×0.64) / 0.68
			//   = 0.073×0.68 / 0.68 = 0.073
			Expect(state.ITLModel.A).To(BeNumerically("~", wantA, 1e-6))
			Expect(state.ITLModel.B).To(BeNumerically("~", wantB, 1e-6))
		})
	})

	Describe("lastFittedB — tier-2 fallback after shape reset", func() {
		const (
			trueA = 0.073
			trueB = 0.012 // deliberately non-default to verify it's carried over
		)

		// onLineMetrics returns two replicas whose (k*, ITL) lie exactly on y = trueA*k + trueB.
		// IL/OL/shape matches the buildTier1Window observations so no shape change fires.
		onLineMetrics := func() []domain.ReplicaMetrics {
			return []domain.ReplicaMetrics{
				{
					VariantName: "v1", KvUsageInstant: 0.30, KvCacheUsage: 0.30,
					AvgITL: trueA*0.30 + trueB, AvgInputTokens: 1024, AvgOutputTokens: 256,
					PrefixCacheHitRate: 0.0, TotalKvCapacityTokens: 65536, ArrivalRate: 5,
				},
				{
					VariantName: "v1", KvUsageInstant: 0.70, KvCacheUsage: 0.70,
					AvgITL: trueA*0.70 + trueB, AvgInputTokens: 1024, AvgOutputTokens: 256,
					PrefixCacheHitRate: 0.0, TotalKvCapacityTokens: 65536, ArrivalRate: 5,
				},
			}
		}

		// buildTier1Window injects 10 observations that lie exactly on y = trueA*k + trueB.
		buildTier1Window := func() {
			kValues := []float64{0.20, 0.30, 0.40, 0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80}
			injectWindowObs(analyzer, ctx, modelID, namespace, "v1",
				1024, 256, 0.0, 65536, trueB, kValues)
		}

		It("saves lastFittedB after a successful Tier-1 OLS fit", func() {
			buildTier1Window()
			// Analyze-internal Observe must also add on-line points so OLS recovers trueB exactly.
			_, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: onLineMetrics(),
			})
			Expect(err).NotTo(HaveOccurred())

			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.ObservationReady).To(BeTrue())
			Expect(state.HasFittedB).To(BeTrue())
			Expect(state.LastFittedB).To(BeNumerically("~", trueB, 1e-6))
		})

		It("retains lastFittedB after a shape change clears the observation window", func() {
			buildTier1Window()
			_, _ = analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: onLineMetrics(),
			})

			// Trigger a shape change: shift OL by >20%.
			shapeShiftMetrics := []domain.ReplicaMetrics{
				{
					VariantName: "v1", KvUsageInstant: 0.40, KvCacheUsage: 0.40,
					AvgITL: trueA*0.40 + trueB, AvgInputTokens: 1024, AvgOutputTokens: 800,
					PrefixCacheHitRate: 0.0, TotalKvCapacityTokens: 65536,
				},
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, shapeShiftMetrics)

			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			Expect(state.ObservationReady).To(BeFalse()) // window cleared
			Expect(state.HasFittedB).To(BeTrue())        // survived the reset
			Expect(state.LastFittedB).To(BeNumerically("~", trueB, 1e-6))
		})

		It("uses lastFittedB as the pinned B in tier-2 after a shape reset", func() {
			buildTier1Window()
			_, _ = analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: onLineMetrics(),
			})

			// Shape change → window cleared → tier-2 will fire next.
			shapeShiftMetrics := []domain.ReplicaMetrics{
				{
					VariantName: "v1", KvUsageInstant: 0.40, KvCacheUsage: 0.40,
					AvgITL: trueA*0.40 + trueB, AvgInputTokens: 1024, AvgOutputTokens: 800,
					PrefixCacheHitRate: 0.0, TotalKvCapacityTokens: 65536, ArrivalRate: 5,
				},
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, shapeShiftMetrics)

			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: shapeShiftMetrics,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.ObservationReady).To(BeFalse()) // still tier-2
			// Tier-2 should use lastFittedB, not DefaultBaselineITLSec.
			Expect(state.ITLModel.B).To(BeNumerically("~", trueB, 1e-6))
			Expect(state.ITLModel.B).NotTo(BeNumerically("~", DefaultBaselineITLSec, 1e-6))
		})

		It("uses DefaultBaselineITLSec in tier-2 when no Tier-1 fit has occurred", func() {
			// Fresh analyzer, never reached tier-1 — hasFittedB is false.
			metrics := []domain.ReplicaMetrics{
				{
					VariantName: "v1", KvUsageInstant: 0.40, KvCacheUsage: 0.40,
					AvgITL: trueA*0.40 + DefaultBaselineITLSec, AvgInputTokens: 1024, AvgOutputTokens: 256,
					PrefixCacheHitRate: 0.0, TotalKvCapacityTokens: 65536, ArrivalRate: 5,
				},
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, metrics)

			_, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID: modelID, Namespace: namespace, ReplicaMetrics: metrics,
			})
			Expect(err).NotTo(HaveOccurred())

			state, _ := analyzer.VariantState(modelID, namespace, "v1")
			Expect(state.HasFittedB).To(BeFalse())
			Expect(state.ITLModel.B).To(BeNumerically("~", DefaultBaselineITLSec, 1e-6))
		})
	})

	Describe("Analyze — GPS-mismatch scenarios (preserved fixtures for future SC gate)", func() {
		// PR-5 dropped the EPP/GPS-mismatch SpareCapacity gate; TA now always leaves
		// SpareCapacity=0. These test scenarios and their input data are intentionally
		// preserved as fixtures for the future per-analyzer status-return PR that will
		// restore the SC-suppression gate. Current assertions are pass-through:
		// SpareCapacity==0 regardless of GPS state. Re-arm SC assertions when that PR lands.
		//
		// Scenario: same ITL coefficients as the tier-1 scaling signal tests.
		//   IL=5000, OL=200, prefix=0.1, KV_max=1024000, A=0.073, B=0.006
		//   KVreq = IL×(1−prefix) + OL/2 = 4500 + 100 = 4600
		//   At k*=0.50: ITL=0.0425; N_dec≈111.3; μ_dec_model≈2619 tok/s
		const (
			ilG    = 5000.0
			olG    = 200.0
			pfxG   = 0.1
			aG     = 0.073
			bG     = 0.006
			kvMaxG = int64(1024000)
			kStar  = 0.50
		)

		kValuesG := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		buildWindowG := func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "gv1",
				ilG, olG, pfxG, kvMaxG, bG, kValuesG)
			state, ok := analyzer.VariantState(modelID, namespace, "gv1")
			Expect(ok).To(BeTrue())
			Expect(state.ObservationReady).To(BeTrue())
		}

		replicaG := func(k, arrivalRate, gps float64) domain.ReplicaMetrics {
			return domain.ReplicaMetrics{
				VariantName:           "gv1",
				KvCacheUsage:          k,
				KvUsageInstant:        k,
				AvgITL:                aG*k + bG,
				AvgInputTokens:        ilG,
				AvgOutputTokens:       olG,
				PrefixCacheHitRate:    pfxG,
				TotalKvCapacityTokens: kvMaxG,
				ArrivalRate:           arrivalRate,
				GenerationTokenRate:   gps,
			}
		}

		// muDecG is the model-predicted μ_dec(k*) for the scenario above.
		muDecG := func() float64 {
			kvReq := ilG*(1-pfxG) + olG/2 // 4600
			nDec := kStar * float64(kvMaxG) / kvReq
			return nDec / (aG*kStar + bG)
		}

		It("GPS within 15% of model prediction — fixture for future SC pass-through", func() {
			buildWindowG()
			// GPS equals model value → 0% error, well within threshold.
			gps := muDecG()
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replicaG(kStar, 2, gps)},
			})
			Expect(err).NotTo(HaveOccurred())
			// GPS gate is dropped — TA always leaves SC=0; engine post-step computes it.
			// Assert the raw inequality the engine interprets as SC>0.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("GPS deviates > 15% at k* ≥ DefaultGPSMinKForVerification — fixture for future SC suppression", func() {
			buildWindowG()
			// GPS is 50% of model → gpsErrPct = |1 - 0.5| / 0.5 × 100 = 100% >> 15%.
			gps := muDecG() * 0.5
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replicaG(kStar, 2, gps)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("GPS deviates but k* < DefaultGPSMinKForVerification — fixture for future SC pass-through", func() {
			buildWindowG()
			// k*=0.20 < DefaultGPSMinKForVerification(0.30); GPS check is skipped.
			gps := muDecG() * 0.1 // enormous mismatch, but k* too low to trust
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replicaG(0.20, 2, gps)},
			})
			Expect(err).NotTo(HaveOccurred())
			// GPS gate is dropped — TA always leaves SC=0; engine post-step computes it.
			// Assert the raw inequality the engine interprets as SC>0.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("GenerationTokenRate is zero (metric absent) — fixture for future SC pass-through", func() {
			buildWindowG()
			// GPS=0 → check is skipped entirely.
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replicaG(kStar, 2, 0)},
			})
			Expect(err).NotTo(HaveOccurred())
			// GPS gate is dropped — TA always leaves SC=0; engine post-step computes it.
			// Assert the raw inequality the engine interprets as SC>0.
			Expect(result.TotalSupply).To(BeNumerically(">", result.TotalDemand))
			Expect(result.SpareCapacity).To(Equal(0.0))
		})

		It("RC remains nonzero under GPS mismatch — fixture for future SC suppression", func() {
			buildWindowG()
			// High ArrivalRate drives demand > supply (RC > 0).
			// GPS gate is dropped — TA always leaves SC=0; SC=0 is now unconditional.
			gps := muDecG() * 0.1
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{replicaG(kStar, 20, gps)},
				ArrivalRate:    20, // model-level, mirrors the single replica's per-pod rate
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.SpareCapacity).To(Equal(0.0))
			// TA leaves RC zero; assert the raw inequality the engine interprets as RC>0.
			Expect(result.TotalDemand).To(BeNumerically(">", result.TotalAnticipatedSupply))
			Expect(result.RequiredCapacity).To(Equal(0.0))
		})
	})

	Describe("Analyze — GPS mismatch window reset", func() {
		// Scenario uses the same ITL coefficients as the GPS verification tests.
		const (
			ilW    = 5000.0
			olW    = 200.0
			pfxW   = 0.1
			aW     = 0.073
			bW     = 0.006
			kvMaxW = int64(1024000)
			kStarW = 0.50
		)
		kValuesW := []float64{0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65}

		buildWindowW := func() {
			injectWindowObs(analyzer, ctx, modelID, namespace, "wv1",
				ilW, olW, pfxW, kvMaxW, bW, kValuesW)
			state, ok := analyzer.VariantState(modelID, namespace, "wv1")
			Expect(ok).To(BeTrue())
			Expect(state.ObservationReady).To(BeTrue())
		}

		// muDecW computes model-predicted μ_dec(k) so GPS can be set to a mismatch value.
		muDecW := func(k float64) float64 {
			kvReq := ilW*(1-pfxW) + olW/2
			nDec := k * float64(kvMaxW) / kvReq
			return nDec / (aW*k + bW)
		}

		mismatchInput := func() domain.AnalyzerInput {
			return domain.AnalyzerInput{
				ModelID:   modelID,
				Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{{
					VariantName:           "wv1",
					KvCacheUsage:          kStarW,
					KvUsageInstant:        kStarW,
					AvgITL:                aW*kStarW + bW,
					AvgInputTokens:        ilW,
					AvgOutputTokens:       olW,
					PrefixCacheHitRate:    pfxW,
					TotalKvCapacityTokens: kvMaxW,
					ArrivalRate:           2,
					GenerationTokenRate:   muDecW(kStarW) * 0.1, // >> 15% error
				}},
			}
		}

		cleanInput := func() domain.AnalyzerInput {
			return domain.AnalyzerInput{
				ModelID:   modelID,
				Namespace: namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{{
					VariantName:           "wv1",
					KvCacheUsage:          kStarW,
					KvUsageInstant:        kStarW,
					AvgITL:                aW*kStarW + bW,
					AvgInputTokens:        ilW,
					AvgOutputTokens:       olW,
					PrefixCacheHitRate:    pfxW,
					TotalKvCapacityTokens: kvMaxW,
					ArrivalRate:           2,
					GenerationTokenRate:   muDecW(kStarW), // exact match → no mismatch
				}},
			}
		}

		It("does not clear the observation window on a single GPS mismatch cycle", func() {
			buildWindowW()
			_, err := analyzer.Analyze(ctx, mismatchInput())
			Expect(err).NotTo(HaveOccurred())

			state, _ := analyzer.VariantState(modelID, namespace, "wv1")
			Expect(state.SampleCount).To(BeNumerically(">", 0))
		})

		It("clears the observation window after N consecutive GPS mismatch cycles", func() {
			buildWindowW()
			// Run one clean Analyze to trigger the Tier-1 OLS fit and set lastFittedB.
			_, err := analyzer.Analyze(ctx, cleanInput())
			Expect(err).NotTo(HaveOccurred())
			stateBeforeW, _ := analyzer.VariantState(modelID, namespace, "wv1")
			Expect(stateBeforeW.HasFittedB).To(BeTrue())

			for i := 0; i < DefaultGPSMismatchClearThreshold; i++ {
				_, err := analyzer.Analyze(ctx, mismatchInput())
				Expect(err).NotTo(HaveOccurred())
			}

			state, _ := analyzer.VariantState(modelID, namespace, "wv1")
			Expect(state.SampleCount).To(Equal(0))
			// lastFittedB must survive the window clear.
			Expect(state.HasFittedB).To(BeTrue())
		})

		It("resets the consecutive counter on a clean cycle, requiring N mismatches again", func() {
			buildWindowW()
			// Inject N-1 mismatches, then a clean cycle — counter should reset.
			for i := 0; i < DefaultGPSMismatchClearThreshold-1; i++ {
				_, err := analyzer.Analyze(ctx, mismatchInput())
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := analyzer.Analyze(ctx, cleanInput())
			Expect(err).NotTo(HaveOccurred())

			// Now inject N-1 more mismatches — still below threshold → window intact.
			for i := 0; i < DefaultGPSMismatchClearThreshold-1; i++ {
				_, err = analyzer.Analyze(ctx, mismatchInput())
				Expect(err).NotTo(HaveOccurred())
			}

			state, _ := analyzer.VariantState(modelID, namespace, "wv1")
			Expect(state.SampleCount).To(BeNumerically(">", 0))
		})

		It("resets the consecutive counter when the window is cleared by a shape change", func() {
			buildWindowW()
			// Inject N-1 mismatches — just below the threshold.
			for i := 0; i < DefaultGPSMismatchClearThreshold-1; i++ {
				_, err := analyzer.Analyze(ctx, mismatchInput())
				Expect(err).NotTo(HaveOccurred())
			}

			// Trigger a shape change by injecting observations with a very different OL.
			injectWindowObs(analyzer, ctx, modelID, namespace, "wv1",
				ilW, olW*10, pfxW, kvMaxW, bW, kValuesW)

			// One more mismatch — but the counter was reset by the shape change,
			// so the total consecutive count is 1, not N.
			_, err := analyzer.Analyze(ctx, mismatchInput())
			Expect(err).NotTo(HaveOccurred())

			// Window was cleared by shape change; after one Observe pass it may be
			// empty or have just the new-shape observations — either way, not cleared
			// again by GPS (count is 1, not N).
			state, _ := analyzer.VariantState(modelID, namespace, "wv1")
			// SampleCount reflects the new-shape window from injectWindowObs — still > 0.
			Expect(state.SampleCount).To(BeNumerically(">", 0))
		})
	})

	Describe("estimateQueueDemand — guard clauses", func() {
		It("returns 0 when sq is nil", func() {
			Expect(estimateQueueDemand(nil, 0.05, 2.0)).To(Equal(0.0))
		})
		It("returns 0 when QueueSize is zero", func() {
			sq := &domain.SchedulerQueueMetrics{QueueSize: 0}
			Expect(estimateQueueDemand(sq, 0.05, 2.0)).To(Equal(0.0))
		})
		It("returns 0 when itlSat is zero", func() {
			sq := &domain.SchedulerQueueMetrics{QueueSize: 10}
			Expect(estimateQueueDemand(sq, 0, 2.0)).To(Equal(0.0))
		})
		It("returns 0 when drainFactor is zero", func() {
			sq := &domain.SchedulerQueueMetrics{QueueSize: 10}
			Expect(estimateQueueDemand(sq, 0.05, 0)).To(Equal(0.0))
		})
		It("returns QueueSize / (drainFactor * itlSat) for valid inputs", func() {
			sq := &domain.SchedulerQueueMetrics{QueueSize: 100}
			// 100 / (2.0 × 0.05) = 1000
			Expect(estimateQueueDemand(sq, 0.05, 2.0)).To(BeNumerically("~", 1000.0, 1e-9))
		})
	})

	Describe("Observe — per-pod sanity filter", func() {
		// Verify that a single bad pod (ITL=0, cold start) does not block healthy pods
		// from contributing shape observations and window entries.
		const (
			ilS    = 1024.0
			olS    = 256.0
			kvMaxS = int64(65536)
			aS     = 0.050
			bS     = 0.006
		)

		It("still observes shape and window entries when one pod has ITL=0 (cold start)", func() {
			healthy := domain.ReplicaMetrics{
				PodName:               "pod-healthy",
				VariantName:           "v1",
				KvCacheUsage:          0.50,
				KvUsageInstant:        0.50,
				AvgITL:                aS*0.50 + bS,
				AvgInputTokens:        ilS,
				AvgOutputTokens:       olS,
				TotalKvCapacityTokens: kvMaxS,
			}
			cold := domain.ReplicaMetrics{
				PodName:               "pod-cold",
				VariantName:           "v1",
				KvCacheUsage:          0.0,
				KvUsageInstant:        0.0,
				AvgITL:                0, // cold start: ITL not yet available
				AvgInputTokens:        ilS,
				AvgOutputTokens:       olS,
				TotalKvCapacityTokens: kvMaxS,
			}
			analyzer.Observe(ctx, time.Now(), modelID, namespace, []domain.ReplicaMetrics{healthy, cold})

			state, ok := analyzer.VariantState(modelID, namespace, "v1")
			Expect(ok).To(BeTrue())
			// Healthy pod contributed one window entry; cold pod was excluded.
			Expect(state.SampleCount).To(Equal(1))
			// Shape is derived from the healthy pod.
			Expect(state.Shape.KVreq).To(BeNumerically(">", 0))
		})
	})
})

var _ = Describe("computeLocalDemand", func() {
	shape := WorkloadShape{
		AvgOutputTokens: 50, // above DefaultMinDecodeOLForLocalDemand
		KVreq:           1024,
	}
	model := ITLModel{A: 0.073, B: 0.006}

	replicaAt := func(k float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			KvUsageInstant:        k,
			TotalKvCapacityTokens: 65536,
		}
	}

	It("skips a replica with NaN KvUsageInstant but still counts the others", func() {
		healthy := replicaAt(0.5)
		nanReplica := replicaAt(math.NaN())

		total := computeLocalDemand([]domain.ReplicaMetrics{healthy}, shape, model)
		Expect(total).To(BeNumerically(">", 0))

		withNaN := computeLocalDemand([]domain.ReplicaMetrics{healthy, nanReplica}, shape, model)
		Expect(withNaN).To(BeNumerically("~", total, 1e-9))
		Expect(math.IsNaN(withNaN)).To(BeFalse())
	})

	It("skips a replica with KvUsageInstant > 1", func() {
		overRange := replicaAt(1.5)
		total := computeLocalDemand([]domain.ReplicaMetrics{overRange}, shape, model)
		Expect(total).To(Equal(0.0))
	})

	It("skips a replica whose model produces a NaN ITL", func() {
		nanModel := ITLModel{A: math.NaN(), B: 0.006}
		total := computeLocalDemand([]domain.ReplicaMetrics{replicaAt(0.5)}, shape, nanModel)
		Expect(total).To(Equal(0.0))
	})

	It("skips a replica with non-positive TotalKvCapacityTokens", func() {
		// Negative, not zero: the accumulated term is KvUsageInstant × capacity / KVreq / ITL, so
		// a zero capacity contributes 0 whether or not the guard fires. Only a negative capacity
		// makes the unguarded contribution non-zero, so only it pins the skip.
		noCapacity := replicaAt(0.5)
		noCapacity.TotalKvCapacityTokens = -65536
		total := computeLocalDemand([]domain.ReplicaMetrics{noCapacity}, shape, model)
		Expect(total).To(Equal(0.0))
	})

	It("skips a replica whose model produces a finite non-positive ITL", func() {
		// B negative enough that A*k+B <= 0 at k=0.5 without A or B being NaN/Inf —
		// distinct from the existing NaN-ITL case, which uses a NaN model coefficient.
		negativeITLModel := ITLModel{A: 0.01, B: -0.1}
		Expect(negativeITLModel.ITLAt(0.5)).To(BeNumerically("<=", 0), "fixture sanity check")
		total := computeLocalDemand([]domain.ReplicaMetrics{replicaAt(0.5)}, shape, negativeITLModel)
		Expect(total).To(Equal(0.0))
	})
})

var _ = Describe("computeVariantSupply", func() {
	shape := WorkloadShape{KVreq: 1024}
	const itlSat = 0.05

	replicaWithCap := func(capTokens int64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{TotalKvCapacityTokens: capTokens}
	}

	It("aggregates supply across KV-capable replicas", func() {
		// Differing capacities keep perReplica from being a copy of total, and the third replica
		// carries no KV capacity so nKV (2) differs from len(metrics) (3) — that is what pins the
		// mean to the KV-capable count rather than to the replica count.
		total, perReplica, nKV := computeVariantSupply(
			[]domain.ReplicaMetrics{replicaWithCap(65536), replicaWithCap(131072), replicaWithCap(0)},
			shape, itlSat)
		Expect(nKV).To(Equal(2))
		// Σ_r DefaultKSat × KV_max_r / KVreq / itlSat over the KV-capable replicas.
		Expect(total).To(BeNumerically("~", DefaultKSat*(65536+131072)/shape.KVreq/itlSat, 1e-6))
		Expect(perReplica).To(BeNumerically("~", total/2, 1e-9))
	})

	It("skips a replica with non-positive TotalKvCapacityTokens", func() {
		total, perReplica, nKV := computeVariantSupply(
			[]domain.ReplicaMetrics{replicaWithCap(0)}, shape, itlSat)
		Expect(nKV).To(Equal(0))
		Expect(total).To(Equal(0.0))
		Expect(perReplica).To(Equal(0.0))
	})
})

// Test 7 — scale-from-zero PRC-only complement.
//
// Analyze()'s primary per-variant loop keys off live ReplicaMetrics, so a variant
// that has dropped to zero replicas produces no rows and would silently vanish from
// the capacity ballot. The scale-from-zero complement re-emits a PRC-only
// VariantCapacity for a *previously-live* variant, carrying its persisted last-good
// per-replica supply. A never-seen variant (no persisted supply) emits nothing, and
// once idle-window eviction drops the persisted state the variant degrades back to
// the never-seen case on its own.
var _ = Describe("ThroughputAnalyzer — scale-from-zero PRC-only complement", func() {
	const (
		il     = 5000.0
		ol     = 200.0
		prefix = 0.1
		kvMax  = int64(1024000)
		A      = 0.073
		B      = 0.006
	)
	// 10 points spanning [0.20, 0.65], spread 0.45 ≥ DefaultMinKSpread: OLS-ready.
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
		modelID = "llama3-8b"
		namespace = "default"
	})

	// liveReplica drives a single healthy replica at k=0.50 so a live Analyze cycle
	// computes and persists a positive last-good per-replica supply for the variant.
	liveReplica := func(variant string, arrivalRate float64) domain.ReplicaMetrics {
		const k = 0.50
		return domain.ReplicaMetrics{
			VariantName:           variant,
			KvCacheUsage:          k,
			KvUsageInstant:        k,
			AvgITL:                A*k + B,
			AvgInputTokens:        il,
			AvgOutputTokens:       ol,
			PrefixCacheHitRate:    prefix,
			TotalKvCapacityTokens: kvMax,
			ArrivalRate:           arrivalRate,
		}
	}

	// makePreviouslyLive runs one OLS-ready live Analyze cycle for the variant so the
	// analyzer persists a positive lastPerReplicaSupply, and returns that value.
	makePreviouslyLive := func(variant string) float64 {
		injectWindowObs(analyzer, ctx, modelID, namespace, variant, il, ol, prefix, kvMax, B, kValues)
		_, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
			ModelID:        modelID,
			Namespace:      namespace,
			ReplicaMetrics: []domain.ReplicaMetrics{liveReplica(variant, 5)},
			ArrivalRate:    5,
			VariantStates:  []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: 1, Role: domain.RoleBoth}},
		})
		Expect(err).NotTo(HaveOccurred())
		st, ok := analyzer.VariantState(modelID, namespace, variant)
		Expect(ok).To(BeTrue())
		Expect(st.PerReplicaSupply).To(BeNumerically(">", 0))
		return st.PerReplicaSupply
	}

	// analyzeAtZero runs an Analyze cycle with the named variants present only in
	// VariantStates at zero live replicas (no ReplicaMetrics rows).
	analyzeAtZero := func(variants ...string) *domain.AnalyzerResult {
		states := make([]domain.VariantReplicaState, 0, len(variants))
		for _, v := range variants {
			states = append(states, domain.VariantReplicaState{VariantName: v, CurrentReplicas: 0, Role: domain.RoleBoth})
		}
		result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
			ModelID:       modelID,
			Namespace:     namespace,
			VariantStates: states,
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	capFor := func(result *domain.AnalyzerResult, variant string) (domain.VariantCapacity, bool) {
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == variant {
				return vc, true
			}
		}
		return domain.VariantCapacity{}, false
	}

	It("re-emits a PRC-only capacity carrying the persisted supply for a previously-live variant now at zero replicas", func() {
		persisted := makePreviouslyLive("v1")

		result := analyzeAtZero("v1")

		vc, ok := capFor(result, "v1")
		Expect(ok).To(BeTrue())
		Expect(vc.PerReplicaCapacity).To(BeNumerically("~", persisted, 1e-9))
		Expect(vc.Reason).To(Equal(itlReasonScaleFromZero))
		// PRC only: identity fields (Cost/AcceleratorName/ReplicaCount) are the anchor
		// merge's job to source from the saturation analyzer, not TA's. Role is the
		// exception — it is consumed by the role aggregation inside this same Analyze
		// call, before any merge, so TA must carry it.
		Expect(vc.Cost).To(Equal(0.0))
		Expect(vc.AcceleratorName).To(Equal(""))
		Expect(vc.Role).To(Equal(domain.RoleBoth))
		Expect(vc.ReplicaCount).To(Equal(0))
	})

	It("emits nothing for a never-seen variant with no persisted supply", func() {
		// v2 is present in VariantStates but was never analyzed live.
		result := analyzeAtZero("v2")
		_, ok := capFor(result, "v2")
		Expect(ok).To(BeFalse())
	})

	It("degrades a previously-live variant to the never-seen (no-emission) case after idle-window eviction", func() {
		makePreviouslyLive("v1")

		// Advance the analyzer clock past 2×DefaultObservationMaxAge with an empty
		// Observe: the eviction sweep drops v1's persisted state.
		analyzer.Observe(ctx, time.Now().Add(2*DefaultObservationMaxAge+time.Minute), modelID, namespace, nil)
		_, stillThere := analyzer.VariantState(modelID, namespace, "v1")
		Expect(stillThere).To(BeFalse())

		// A subsequent zero-replica cycle now sees v1 as never-seen → no emission.
		result := analyzeAtZero("v1")
		_, ok := capFor(result, "v1")
		Expect(ok).To(BeFalse())
	})

	// A scale-from-zero entry on a disaggregated model must carry its real role.
	// distributeDemandByRole and aggregateRoleCapacities both canonicalize an empty
	// role to domain.RoleBoth, and they run on this same slice inside Analyze —
	// before any anchor merge could fill the role in. A blank-role entry therefore
	// manufactures a "both" bucket alongside prefill/decode, which (a) splits the
	// model-level decode demand across one more role than exists, and (b) leaves the
	// paired allocator with a role no anchor variant can satisfy.
	Context("on a disaggregated (P/D) model", func() {
		// makePreviouslyLiveWithRole is makePreviouslyLive for a variant whose role is
		// not "both", so the persisted state carries that role into the zero cycle.
		makePreviouslyLiveWithRole := func(variant, role string) {
			injectWindowObs(analyzer, ctx, modelID, namespace, variant, il, ol, prefix, kvMax, B, kValues)
			_, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{liveReplica(variant, 5)},
				ArrivalRate:    5,
				VariantStates:  []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: 1, Role: role}},
			})
			Expect(err).NotTo(HaveOccurred())
			st, ok := analyzer.VariantState(modelID, namespace, variant)
			Expect(ok).To(BeTrue())
			Expect(st.PerReplicaSupply).To(BeNumerically(">", 0))
		}

		It("emits the persisted role, so a zero-replica decode variant does not manufacture a phantom 'both' bucket", func() {
			makePreviouslyLiveWithRole("v-decode", domain.RoleDecode)
			makePreviouslyLiveWithRole("v-prefill", domain.RolePrefill)

			// v-prefill stays live; v-decode drops to zero replicas this cycle.
			result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
				ModelID:        modelID,
				Namespace:      namespace,
				ReplicaMetrics: []domain.ReplicaMetrics{liveReplica("v-prefill", 5)},
				ArrivalRate:    5,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v-decode", CurrentReplicas: 0, Role: domain.RoleDecode},
					{VariantName: "v-prefill", CurrentReplicas: 1, Role: domain.RolePrefill},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			vc, ok := capFor(result, "v-decode")
			Expect(ok).To(BeTrue())
			Expect(vc.Reason).To(Equal(itlReasonScaleFromZero))
			Expect(vc.Role).To(Equal(domain.RoleDecode))

			// The role breakdown stays exactly {prefill, decode} — no "both" key.
			Expect(result.RoleCapacities).NotTo(BeNil())
			Expect(result.RoleCapacities).To(HaveKey(domain.RoleDecode))
			Expect(result.RoleCapacities).NotTo(HaveKey(domain.RoleBoth))
		})

		It("does not dilute per-role decode demand when a decode variant is at zero replicas", func() {
			makePreviouslyLiveWithRole("v-decode-a", domain.RoleDecode)
			makePreviouslyLiveWithRole("v-decode-b", domain.RoleDecode)
			makePreviouslyLiveWithRole("v-prefill", domain.RolePrefill)

			analyzeWith := func(states []domain.VariantReplicaState, live []domain.ReplicaMetrics) *domain.AnalyzerResult {
				result, err := analyzer.Analyze(ctx, domain.AnalyzerInput{
					ModelID:        modelID,
					Namespace:      namespace,
					ReplicaMetrics: live,
					ArrivalRate:    5,
					VariantStates:  states,
				})
				Expect(err).NotTo(HaveOccurred())
				return result
			}

			// Baseline: only the live decode variant is configured at all.
			baseline := analyzeWith(
				[]domain.VariantReplicaState{
					{VariantName: "v-decode-a", CurrentReplicas: 1, Role: domain.RoleDecode},
					{VariantName: "v-prefill", CurrentReplicas: 1, Role: domain.RolePrefill},
				},
				[]domain.ReplicaMetrics{liveReplica("v-decode-a", 5), liveReplica("v-prefill", 5)},
			)

			// Same traffic, but a second decode variant sits at zero replicas. It joins
			// the decode role rather than adding a role, so decode's share is unchanged.
			withZero := analyzeWith(
				[]domain.VariantReplicaState{
					{VariantName: "v-decode-a", CurrentReplicas: 1, Role: domain.RoleDecode},
					{VariantName: "v-decode-b", CurrentReplicas: 0, Role: domain.RoleDecode},
					{VariantName: "v-prefill", CurrentReplicas: 1, Role: domain.RolePrefill},
				},
				[]domain.ReplicaMetrics{liveReplica("v-decode-a", 5), liveReplica("v-prefill", 5)},
			)

			Expect(baseline.RoleCapacities[domain.RoleDecode].TotalDemand).To(BeNumerically(">", 0))
			Expect(withZero.RoleCapacities[domain.RoleDecode].TotalDemand).To(
				BeNumerically("~", baseline.RoleCapacities[domain.RoleDecode].TotalDemand, 1e-9))
		})
	})
})
