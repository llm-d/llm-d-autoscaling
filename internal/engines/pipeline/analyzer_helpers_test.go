package pipeline

import (
	"context"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// makeNamed builds a NamedAnalyzerResult with the given RC, SC, and per-variant
// (variantName, perReplicaCapacity) pairs. Live defaults to true — tests
// exercising the liveness gate (needsScaleDownForRole, safeRemovalReplicasForRole)
// override it explicitly on the entries they want treated as non-live.
func makeNamed(name string, rc, sc float64, vcs ...any) NamedAnalyzerResult {
	var caps []domain.VariantCapacity
	for i := 0; i+1 < len(vcs); i += 2 {
		vName := vcs[i].(string)
		prc := vcs[i+1].(float64)
		caps = append(caps, domain.VariantCapacity{
			VariantName:        vName,
			PerReplicaCapacity: prc,
		})
	}
	return NamedAnalyzerResult{
		Name: name,
		Result: &domain.AnalyzerResult{
			RequiredCapacity:  rc,
			SpareCapacity:     sc,
			VariantCapacities: caps,
		},
		Remaining: rc,
		Spare:     sc,
		Live:      true,
		Enabled:   true,
	}
}

// withScore sets the belief weight the combine gives one entry's votes.
func withScore(e NamedAnalyzerResult, score float64) NamedAnalyzerResult {
	e.Score = score
	return e
}

var _ = Describe("analyzer helpers", func() {

	Describe("applyAllocation", func() {
		It("subtracts n×PRC from each analyzer's Remaining counter", func() {
			// PRC=100, n=2 → subtract 200 from each Remaining
			s := []NamedAnalyzerResult{
				makeNamed("sat", 500, 0, "v", 100.0),
				makeNamed("ta", 300, 0, "v", 100.0),
			}
			applyAllocation(s, "v", 2)
			Expect(s[0].Remaining).To(BeNumerically("~", 300.0, 1e-9))
			Expect(s[1].Remaining).To(BeNumerically("~", 100.0, 1e-9))
			// Result.RequiredCapacity is not mutated
			Expect(s[0].Result.RequiredCapacity).To(Equal(500.0))
		})

		It("clamps Remaining to 0", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 50, 0, "v", 100.0)}
			applyAllocation(s, "v", 2) // would subtract 200 from 50
			Expect(s[0].Remaining).To(Equal(0.0))
		})

		It("is a no-op for variants not in the result", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 200, 0, "other", 100.0)}
			applyAllocation(s, "v", 3)
			Expect(s[0].Remaining).To(Equal(200.0))
		})
	})

	Describe("bindingAnchor", func() {
		// Test 1 — merged-anchor construction (non-vacuous).
		// A two-entry ballot where saturation is the identity carrier and a
		// live throughput analyzer is the sizing binder. The merged anchor must
		// take identity (accelerator, cost, replica count, model ID) from
		// saturation and sizing (PRC, reason, model-level RC) from throughput, and
		// recompute TotalCapacity. The fixtures make the anchor differ from
		// ballot[0] (saturation) in both analyzer name and PRC, so an implementation
		// that merely returned the saturation entry would fail — proving the merge.
		It("merges identity from saturation with sizing from the binding analyzer", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // present as the identity carrier, not voting
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName:     domain.SaturationAnalyzerName,
					ModelID:          "m1",
					Namespace:        "ns1",
					RequiredCapacity: 999, // must NOT surface (sizing comes from binding)
					VariantCapacities: []domain.VariantCapacity{{
						VariantName:        "v1",
						AcceleratorName:    "A100",
						Cost:               10.0,
						Role:               domain.RoleBoth,
						ReplicaCount:       2,
						PerReplicaCapacity: 100.0, // sat's own sizing — must NOT surface for v1
						Reason:             "P1-obs",
						TotalDemand:        150.0,
					}},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName:     "throughput",
					ModelID:          "ignored", // identity comes from the identity carrier
					RequiredCapacity: 50,
					VariantCapacities: []domain.VariantCapacity{{
						VariantName:        "v1",
						PerReplicaCapacity: 200.0, // binding sizing — this is what surfaces
						Reason:             "T1-ols",
						TotalDemand:        300.0,
					}},
				},
			}
			s := []NamedAnalyzerResult{sat, ta}

			anchor := bindingAnchor(s)
			Expect(anchor).NotTo(BeNil())
			// Non-vacuous: the anchor is a fresh merge, not either source Result.
			Expect(anchor).NotTo(BeIdenticalTo(s[0].Result))
			Expect(anchor).NotTo(BeIdenticalTo(s[1].Result))

			// Model-level: identity from saturation, sizing from binding.
			Expect(anchor.AnalyzerName).To(Equal("throughput"))
			Expect(anchor.ModelID).To(Equal("m1"))
			Expect(anchor.Namespace).To(Equal("ns1"))
			Expect(anchor.RequiredCapacity).To(Equal(50.0))

			Expect(anchor.VariantCapacities).To(HaveLen(1))
			vc := anchor.VariantCapacities[0]
			Expect(vc.VariantName).To(Equal("v1"))
			// identity from saturation
			Expect(vc.AcceleratorName).To(Equal("A100"))
			Expect(vc.Cost).To(Equal(10.0))
			Expect(vc.ReplicaCount).To(Equal(2))
			// sizing from binding (throughput)
			Expect(vc.PerReplicaCapacity).To(Equal(200.0))
			Expect(vc.Reason).To(Equal("T1-ols"))
			// TotalCapacity recomputed = ReplicaCount(identity) × PerReplicaCapacity(sizing)
			Expect(vc.TotalCapacity).To(Equal(400.0))
		})

		// Test 2 — per-variant completeness + ordering.
		// The identity carrier (saturation) lists v1+v2; the binding analyzer lists only
		// v1. Variant ordering follows the identity carrier. For v2 (omitted by the
		// binder) there is no fallback to saturation's own sizing — it abstains
		// with PRC=0 uniformly, whether or not saturation votes:
		//   - saturation enabled (but non-binding here because non-live): still no
		//     fallback — an enabled-but-not-binding saturation is, by definition,
		//     not live+informative, so its own sizing would be untrustworthy anyway;
		//   - saturation not enabled (throughput-only): same result, tested
		//     separately below.
		It("abstains (PRC=0) for a variant the binder omits, even when saturation votes", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: true,  // votes, but does not bind (below)
				Live:    false, // non-live → does not bind; throughput binds
				Result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 100.0, Reason: "P1-obs"},
						{VariantName: "v2", ReplicaCount: 1, PerReplicaCapacity: 110.0, Reason: "P1-obs"},
					},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}
			anchor := bindingAnchor([]NamedAnalyzerResult{sat, ta})
			Expect(anchor).NotTo(BeNil())
			Expect(anchor.VariantCapacities).To(HaveLen(2))
			// Ordering follows the identity carrier (saturation): v1 then v2.
			Expect(anchor.VariantCapacities[0].VariantName).To(Equal("v1"))
			Expect(anchor.VariantCapacities[1].VariantName).To(Equal("v2"))
			// v1 sized by the binding analyzer.
			Expect(anchor.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))
			Expect(anchor.VariantCapacities[0].Reason).To(Equal("T1-ols"))
			// v2 omitted by the binder → abstains, not a fallback to
			// saturation's own sizing=110.0 despite saturation being enabled.
			Expect(anchor.VariantCapacities[1].PerReplicaCapacity).To(Equal(0.0))
			Expect(anchor.VariantCapacities[1].Reason).To(Equal(""))
		})

		It("leaves an omitted variant at PRC=0 under a throughput-only (non-voting saturation) config", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // throughput-only: saturation carries identity but does not vote
				Live:    false,
				Result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 100.0, Reason: "P1-obs"},
						{VariantName: "v2", ReplicaCount: 1, PerReplicaCapacity: 110.0, Reason: "P1-obs"},
					},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}
			anchor := bindingAnchor([]NamedAnalyzerResult{sat, ta})
			Expect(anchor).NotTo(BeNil())
			Expect(anchor.VariantCapacities).To(HaveLen(2))
			// v2 has no consistent fallback source → PRC stays 0 (reactive
			// scale-from-zero owns cold-start).
			Expect(anchor.VariantCapacities[1].VariantName).To(Equal("v2"))
			Expect(anchor.VariantCapacities[1].PerReplicaCapacity).To(Equal(0.0))
			Expect(anchor.VariantCapacities[1].TotalCapacity).To(Equal(0.0))
		})

		// Test 3 — no source mutation (aliasing guard).
		// bindingAnchor must build fresh VariantCapacity literals; mutating the
		// returned anchor must not write through to either source Result.
		It("does not mutate the source Results' VariantCapacities", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName,
					ModelID:      "m1",
					VariantCapacities: []domain.VariantCapacity{{
						VariantName:        "v1",
						AcceleratorName:    "A100",
						Cost:               10.0,
						ReplicaCount:       2,
						PerReplicaCapacity: 100.0,
						TotalCapacity:      200.0,
					}},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{{
						VariantName:        "v1",
						PerReplicaCapacity: 200.0,
						Reason:             "T1-ols",
						TotalCapacity:      400.0,
					}},
				},
			}
			s := []NamedAnalyzerResult{sat, ta}

			anchor := bindingAnchor(s)
			Expect(anchor).NotTo(BeNil())
			Expect(anchor.VariantCapacities).To(HaveLen(1))

			// Mutate the merged output; the sources must be unaffected.
			anchor.VariantCapacities[0].PerReplicaCapacity = 9999.0
			anchor.VariantCapacities[0].AcceleratorName = "MUTATED"

			Expect(sat.Result.VariantCapacities[0].AcceleratorName).To(Equal("A100"))
			Expect(sat.Result.VariantCapacities[0].PerReplicaCapacity).To(Equal(100.0))
			Expect(sat.Result.VariantCapacities[0].TotalCapacity).To(Equal(200.0))
			Expect(ta.Result.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))
			Expect(ta.Result.VariantCapacities[0].TotalCapacity).To(Equal(400.0))
		})

		// Rescale read-source characterization. The rescale path resolves the
		// model's accelerator type via singleAccType(bindingAnchor(...).VariantCapacities).
		// Under a throughput-only config the throughput analyzer binds (it is the
		// sole voting+live member) but leaves AcceleratorName empty; the accelerator
		// identity comes from saturation's identity contribution through the merge. This
		// pins the wiring so a later change that repoints the read at the raw binding
		// result (which has no AcceleratorName) can't silently drop the model from
		// rescale. Throughput-only rescale *correctness* is a later change; this only
		// freezes the read-source.
		It("resolves the accelerator type via the merged anchor when the binding analyzer omits it", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // throughput-only: saturation carries identity but does not vote
				Live:    false,
				Result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", AcceleratorName: "A100", ReplicaCount: 1, PerReplicaCapacity: 100.0, Reason: "P1-obs"},
					},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						// AcceleratorName deliberately empty — throughput does not set it.
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}
			s := []NamedAnalyzerResult{sat, ta}

			anchor := bindingAnchor(s)
			Expect(anchor).NotTo(BeNil())
			Expect(anchor.VariantCapacities).To(HaveLen(1))
			// Throughput binds the sizing (PRC=200), confirming it is the binder.
			Expect(anchor.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))
			// Accelerator identity survives via saturation's identity, even though the
			// binding analyzer's own result left it empty.
			Expect(anchor.VariantCapacities[0].AcceleratorName).To(Equal("A100"))

			// The exact expression the rescale path uses to key its GPU budgets.
			accType, ok := singleAccType(anchor.VariantCapacities)
			Expect(ok).To(BeTrue(), "rescale must resolve a single accelerator type from the merged anchor")
			Expect(accType).To(Equal("A100"))
		})

		// Test 4 — degenerate ballots produce no anchor (the per-model hold).
		// bindingAnchor returns nil whenever nothing can bind; each optimizer's
		// nil-anchor guard then holds the model (no decision this cycle) rather
		// than indexing into an empty or unbindable ballot. These pin the two
		// nil paths that remain after the deterministic binder tie-break (a multi-binder
		// tie no longer holds — see Test 5): no index panic on an empty ballot,
		// and a deliberate hold when no analyzer is live+informative at all.
		It("returns nil for an empty ballot", func() {
			Expect(bindingAnchor(nil)).To(BeNil())
			Expect(bindingAnchor([]NamedAnalyzerResult{})).To(BeNil())
		})

		It("returns nil when no enabled+live+informative analyzer is present", func() {
			// Saturation and throughput are both present, enabled, and informative,
			// but neither is live this cycle → no binder → hold the model.
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: true,
				Live:    false,
				Result: &domain.AnalyzerResult{
					AnalyzerName: domain.SaturationAnalyzerName,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 100.0, Reason: "P1-obs"},
					},
				},
			}
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    false,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}
			Expect(bindingAnchor([]NamedAnalyzerResult{sat, ta})).To(BeNil())
		})

		// Test 5 — deterministic binder tie-break (two non-saturation live
		// analyzers, no saturation entry). The combine admits multiple
		// non-saturation voters; rather than hold the model on a tie, the lowest-ballot-index
		// qualifying entry binds. This asserts the tie-break, not a hold.
		It("binds the lowest-ballot-index candidate when two non-saturation analyzers both qualify", func() {
			ta := NamedAnalyzerResult{
				Name:    "throughput",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "throughput",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 200.0, Reason: "T1-ols"},
					},
				},
			}
			lat := NamedAnalyzerResult{
				Name:    "latency",
				Enabled: true,
				Live:    true,
				Result: &domain.AnalyzerResult{
					AnalyzerName: "latency",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", PerReplicaCapacity: 150.0, Reason: "L1-obs"},
					},
				},
			}
			// ta is ballot index 0 → binds; lat (index 1) votes but does not bind.
			anchor := bindingAnchor([]NamedAnalyzerResult{ta, lat})
			Expect(anchor).NotTo(BeNil(), "a multi-binder tie must bind deterministically, not hold")
			Expect(anchor.AnalyzerName).To(Equal("throughput"))
			Expect(anchor.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))

			// Reversing ballot order flips the binder: still the lowest index, not a fixed name.
			anchorReversed := bindingAnchor([]NamedAnalyzerResult{lat, ta})
			Expect(anchorReversed).NotTo(BeNil())
			Expect(anchorReversed.AnalyzerName).To(Equal("latency"))
			Expect(anchorReversed.VariantCapacities[0].PerReplicaCapacity).To(Equal(150.0))
		})

		Context("A Variant the Binder Omits", func() {

			// The from-zero admission exception's territory, pinning what the merge
			// does today for a variant the binder leaves out.
			//
			// The ballot below is the only one where this arises: saturation is
			// enabled but not live, so it does not bind and throughput does, while
			// saturation stays the identity carrier because it is located by name
			// rather than by vote. Under a saturation binder there is nothing to
			// omit, which is why the [sat]-only goldens cannot cover this in either
			// direction.
			//
			// The newcomer's Cost is 0 and its AcceleratorName empty because that is
			// what saturation actually produces for a variant with no replicas and no
			// store record -- both come from the same zero-replica lookup. Rigging
			// either to a plausible-looking value would be testing a state
			// production cannot reach.
			fzBallot := func(newcomerReplicas int) []NamedAnalyzerResult {
				return []NamedAnalyzerResult{
					{
						Name: domain.SaturationAnalyzerName, Enabled: true, Live: false,
						Result: &domain.AnalyzerResult{
							AnalyzerName: domain.SaturationAnalyzerName,
							ModelID:      "fz", Namespace: "default",
							VariantCapacities: []domain.VariantCapacity{
								{VariantName: "measured", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 8000, Reason: "P1-obs"},
								{VariantName: "newcomer", ReplicaCount: newcomerReplicas},
							},
						},
					},
					{
						Name: "throughput", Enabled: true, Live: true,
						Result: &domain.AnalyzerResult{
							AnalyzerName:     "throughput",
							RequiredCapacity: 300,
							VariantCapacities: []domain.VariantCapacity{
								{VariantName: "measured", PerReplicaCapacity: 100.0, Reason: "T1-ols"},
							},
						},
					},
				}
			}

			DescribeTable("abstains on capacity, whatever the replica count",
				func(replicas int) {
					anchor := bindingAnchor(fzBallot(replicas))
					Expect(anchor).NotTo(BeNil())
					Expect(anchor.AnalyzerName).To(Equal("throughput"), "throughput must bind for this to be reachable")

					vc, ok := variantCapacityByName(anchor.VariantCapacities, "newcomer")
					Expect(ok).To(BeTrue(), "the identity carrier lists it, so the merge must keep it")
					Expect(vc.PerReplicaCapacity).To(BeZero(), "omitted by the binder means unsized, not sized at some default")
					Expect(vc.Reason).To(BeEmpty(), "and carries no tag for a ceiling to key on")
					Expect(vc.TotalCapacity).To(BeZero())

					// The binder's own variant is untouched by any of this.
					m, _ := variantCapacityByName(anchor.VariantCapacities, "measured")
					Expect(m.PerReplicaCapacity).To(Equal(100.0))
					Expect(m.Reason).To(Equal("T1-ols"))
				},
				// Both rows abstain today. The zero row is the one the deferred
				// from-zero admission would change, and it is pinned here so that change is
				// visible as a deliberate edit to this table rather than a silent
				// behavioural drift. The running row must keep abstaining either way:
				// a binder also omits variants that ARE up but had no usable metric
				// this cycle, and inventing a size for something whose real size is
				// merely unknown this cycle is never right.
				Entry("newcomer the binder never sized, holds no replicas", 0),
				Entry("running, but unmeasured this cycle", 3),
			)
		})
	})

	Describe("maxTargetReplicas", func() {

		tagged := domain.VariantCapacity{VariantName: "v", Reason: ReasonFromZeroAdmission}
		plain := domain.VariantCapacity{VariantName: "v"}
		withMax := func(n int) domain.VariantReplicaState {
			return domain.VariantReplicaState{VariantName: "v", MaxReplicas: &n}
		}

		It("reports no ceiling for an ordinary variant with no MaxReplicas", func() {
			_, bounded := maxTargetReplicas(plain, domain.VariantReplicaState{VariantName: "v"})
			Expect(bounded).To(BeFalse(), "callers must keep treating this as unbounded")
		})

		It("reports MaxReplicas verbatim for an ordinary variant", func() {
			bound, bounded := maxTargetReplicas(plain, withMax(7))
			Expect(bounded).To(BeTrue())
			Expect(bound).To(Equal(7))
		})

		It("ceilings an admitted variant even with no MaxReplicas configured", func() {
			// The reason this is a helper at all. Two of the three grant sites read
			// "no MaxReplicas" as unbounded -- one returns math.MaxInt, the other
			// runs a loop bounded only inside the MaxReplicas branch -- so a ceiling
			// written into that branch is absent on exactly the configurations that
			// leave MaxReplicas unset. The variant would be admitted and then
			// allowed to draw without limit, silently: nothing errors, no gate
			// objects, no golden moves.
			bound, bounded := maxTargetReplicas(tagged, domain.VariantReplicaState{VariantName: "v"})
			Expect(bounded).To(BeTrue())
			Expect(bound).To(Equal(1))
		})

		It("takes the tighter of the two bounds", func() {
			bound, bounded := maxTargetReplicas(tagged, withMax(9))
			Expect(bounded).To(BeTrue())
			Expect(bound).To(Equal(1), "admission never widens a configured bound")

			// A configured bound already at the ceiling is not loosened either.
			bound, _ = maxTargetReplicas(tagged, withMax(1))
			Expect(bound).To(Equal(1))
		})
	})

	Describe("ResultIsInformative", func() {
		It("returns false for a nil Result", func() {
			Expect(ResultIsInformative(NamedAnalyzerResult{Result: nil})).To(BeFalse())
		})

		It("returns false when every VariantCapacity is no-data or error", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", Reason: "no-data"},
					{VariantName: "b", Reason: "error"},
				},
			}}
			Expect(ResultIsInformative(nr)).To(BeFalse())
		})

		It("returns false for an empty VariantCapacities slice (e.g. throughput with no resolvable ITL model)", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{}}
			Expect(ResultIsInformative(nr)).To(BeFalse())
		})

		It("returns true when at least one VariantCapacity carries a usable reason", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", Reason: "no-data"},
					{VariantName: "b", Reason: "T1-ols"},
				},
			}}
			Expect(ResultIsInformative(nr)).To(BeTrue())
		})
	})
})

// makeNamedPD builds a NamedAnalyzerResult with RoleCapacities for P/D tests.
// RoleSpare is initialized from pSC/dSC (as initDisaggregatedRemaining would do).
// Live defaults to true; override explicitly for non-live-analyzer scenarios.
func makeNamedPD(name string, pRC, dRC, pSC, dSC float64, pDemand, dDemand float64, vPPRC float64, vDPRC float64) NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name: name,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "pf", Role: "prefill", PerReplicaCapacity: vPPRC},
				{VariantName: "dc", Role: "decode", PerReplicaCapacity: vDPRC},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: pRC, SpareCapacity: pSC, TotalDemand: pDemand},
				"decode":  {Role: "decode", RequiredCapacity: dRC, SpareCapacity: dSC, TotalDemand: dDemand},
			},
		},
		Remaining: pRC, // P-scope after initDisaggregatedRemaining
		RoleSpare: map[string]float64{"prefill": pSC, "decode": dSC},
		Live:      true,
		Enabled:   true,
	}
}

var _ = Describe("the cross-analyzer combine core", func() {

	Describe("combineVotes", func() {
		It("collapses to the plain extremum when scores are uniform, both directions", func() {
			votes := []replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 1}}

			up, upBinder := combineVotes(votes, true)
			Expect(up).To(Equal(10.0))
			Expect(upBinder).To(Equal(0))

			down, downBinder := combineVotes(votes, false)
			Expect(down).To(Equal(5.0))
			Expect(downBinder).To(Equal(1))
		})

		It("converges on a dominant analyzer's own vote as its score grows", func() {
			// s_1 -> infinity pulls the result onto v_1 = 5 even though the
			// extremum (and the binder) is analyzer 0's 10.
			votes := []replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 1000}}
			value, binder := combineVotes(votes, true)
			Expect(value).To(BeNumerically("~", 5.0, 0.02))
			Expect(binder).To(Equal(0), "the binder is still the extremum, whatever the weighting does to the number")
		})

		It("never leaves [min, max], in either direction", func() {
			votes := []replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 2}, {Index: 2, Value: 7, Score: 3}}

			// up: e=10, correction = ((10-5)*1 + (10-7)*2)/6 = 11/6
			up, _ := combineVotes(votes, true)
			Expect(up).To(BeNumerically("~", 10.0-11.0/6.0, 1e-9))
			Expect(up).To(And(BeNumerically(">=", 5.0), BeNumerically("<=", 10.0)))

			// down: e=5, correction = ((5-7)*1)/6 = -1/3, so the subtraction adds
			down, _ := combineVotes(votes, false)
			Expect(down).To(BeNumerically("~", 5.0+1.0/3.0, 1e-9))
			Expect(down).To(And(BeNumerically(">=", 5.0), BeNumerically("<=", 10.0)))
		})

		It("is monotone in each score: raising a dissenter's pulls the result toward its vote", func() {
			lo, _ := combineVotes([]replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 2}}, true)
			hi, _ := combineVotes([]replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 4}}, true)
			Expect(lo).To(BeNumerically("~", 10.0-5.0/3.0, 1e-9)) // 8.333 -- the design's worked example
			Expect(hi).To(BeNumerically("~", 7.0, 1e-9))
			Expect(hi).To(BeNumerically("<", lo), "more trust in the dissenter moves the result toward its 5")
		})

		It("survives the caller's rounding: a corrected result lands on neither analyzer's own replica count", func() {
			// Two analyzers disagree by enough replicas that the dominance
			// correction is still visible after the scale-up ceil. This is the
			// arithmetic half of the multi-analyzer Score story: the number moves,
			// and it stops somewhere neither analyzer asked for.
			votes := []replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 5, Score: 2}}

			value, binder := combineVotes(votes, true)
			Expect(value).To(BeNumerically("~", 10.0-5.0/3.0, 1e-9)) // 8.333

			// The correction pulls the value off the extremum without reassigning
			// the binder: identity comes from who held the extremum, not from who
			// moved the number.
			Expect(binder).To(Equal(0))

			Expect(int(math.Ceil(value))).To(Equal(9), "distinguishable from both a plain max (10) and the dissenter's own vote (5)")
		})

		It("returns a single vote unchanged, whatever its score", func() {
			votes := []replicaVote{{Index: 3, Value: 7.5, Score: 9}}
			up, upBinder := combineVotes(votes, true)
			Expect(up).To(Equal(7.5))
			Expect(upBinder).To(Equal(3))
			down, downBinder := combineVotes(votes, false)
			Expect(down).To(Equal(7.5))
			Expect(downBinder).To(Equal(3))
		})

		It("signals no basis to act when nothing participates", func() {
			value, binder := combineVotes(nil, true)
			Expect(value).To(Equal(0.0))
			Expect(binder).To(Equal(-1))
		})

		It("keeps the lowest ballot index on a tie, regardless of slice order", func() {
			_, binder := combineVotes([]replicaVote{{Index: 0, Value: 10, Score: 1}, {Index: 1, Value: 10, Score: 1}}, true)
			Expect(binder).To(Equal(0))

			// Same tie presented out of ballot order: the tie-break reads Index,
			// not position, so the answer does not move.
			_, binder = combineVotes([]replicaVote{{Index: 1, Value: 10, Score: 1}, {Index: 0, Value: 10, Score: 1}}, true)
			Expect(binder).To(Equal(0))
		})

		It("falls back to the extremum rather than dividing by zero when no vote carries a score", func() {
			// Only reachable from a hand-built ballot -- the config layer coerces
			// a zero score to 1.0.
			value, binder := combineVotes([]replicaVote{{Index: 0, Value: 4}, {Index: 1, Value: 9}}, true)
			Expect(value).To(Equal(9.0))
			Expect(binder).To(Equal(1))
		})
	})

	Describe("the collectors' participation filter", func() {
		It("excludes an analyzer with no PRC for the variant, so it cannot dilute the weighting", func() {
			// A third analyzer that does not size "v" must leave the combine
			// exactly as the two-analyzer ballot found it. If it were counted in
			// the score denominator, sum_j s_j would be 8 instead of 3 and the
			// result would drift from 8.333 to 9.375 -- an analyzer that says
			// nothing about "v" would make the system trust the binder MORE, and
			// the more heavily it were trusted elsewhere the worse the drift.
			s := []NamedAnalyzerResult{
				withScore(makeNamed("sat", 100, 0, "v", 10.0), 1),     // rd = 10
				withScore(makeNamed("ta", 50, 0, "v", 10.0), 2),       // rd = 5
				withScore(makeNamed("lat", 999, 0, "other", 10.0), 5), // sizes a different variant only
			}
			_, ps := initRoleState(s)

			votes := votesFromPickerState(s, ps, domain.RoleBoth, "v")
			Expect(votes).To(HaveLen(2))
			Expect([]int{votes[0].Index, votes[1].Index}).To(Equal([]int{0, 1}))

			got, gotBinder := combineVotes(votes, true)

			want, wantBinder := combineVotes([]replicaVote{
				{Index: 0, Value: 10, Score: 1},
				{Index: 1, Value: 5, Score: 2},
			}, true)

			Expect(got).To(Equal(want))
			Expect(gotBinder).To(Equal(wantBinder))
		})

		It("drops a non-live analyzer from the scale-down ballot", func() {
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			nonLive := makeNamedPD("throughput", 0, 0, 5000, 5000, 10000, 30000, 10000, 10000)
			nonLive.Live = false
			votes := votesFromRoleSpare([]NamedAnalyzerResult{live, nonLive}, "prefill", "pf")
			Expect(votes).To(HaveLen(1))
			Expect(votes[0].Index).To(Equal(0))
		})

		It("drops an analyzer that does not decompose the requested role from the rescale ballot", func() {
			pd := makeNamedPD("sat", 0, 0, 0, 0, 10000, 30000, 10000, 10000)
			flat := makeNamed("ta", 100, 0, "pf", 10.0) // model-level only; no RoleCapacities
			votes := votesFromTotalDemand([]NamedAnalyzerResult{pd, flat}, "prefill", "pf")
			Expect(votes).To(HaveLen(1))
			Expect(votes[0].Index).To(Equal(0))
		})
	})

	// A configured score only ever means one thing: how far the combine pulls the
	// agreed replica count toward that analyzer's own vote. These fixtures drive
	// the two directions through the real call sites, so they fail if the
	// collectors stop reading the ballot's scores -- with uniform weights the
	// scale-up case reads 10 instead of 9 and the scale-down case 5 instead of 6.
	Describe("configured scores reaching the combine", func() {
		It("pulls scale-up sizing toward the better-trusted dissenter without dropping below its vote", func() {
			// Throughput wants 10 replicas and is the default-trusted voter;
			// saturation wants 5 and is trusted twice as much.
			//   e = 10 (throughput binds), s_e = 1, sum s = 3
			//   correction = (10-5)*(2-1)/3 = 1.667  ->  v* = 8.333  ->  ceil = 9
			// Nine is distinguishable from BOTH votes, which is the point: the
			// result is neither analyzer's number and neither analyzer's rounding.
			s := []NamedAnalyzerResult{
				withScore(makeNamed("saturation", 50, 0, "v", 10.0), 2),  // rd = 5
				withScore(makeNamed("throughput", 100, 0, "v", 10.0), 1), // rd = 10
			}
			_, ps := initRoleState(s)

			value, binder := combineVotes(votesFromPickerState(s, ps, domain.RoleBoth, "v"), true)
			Expect(value).To(BeNumerically("~", 8.3333333, 1e-6))
			Expect(binder).To(Equal(1), "throughput's vote binds even though saturation is trusted more")

			Expect(roleBottleneckReplicas(s, ps, domain.RoleBoth, "v")).To(Equal(9))
		})

		It("pulls scale-down removal toward the better-trusted dissenter without exceeding its vote", func() {
			// Mirror image, scores swapped: saturation says 5 replicas are
			// removable (default trust), throughput says 10 and is trusted twice
			// as much.
			//   e = 5 (saturation binds -- the conservative vote), s_e = 1, sum s = 3
			//   correction = (5-10)*(2-1)/3 = -1.667  ->  v* = 6.667  ->  floor = 6
			// Six stays above saturation's own 5 and below throughput's 10: the
			// combine never invents a number outside the votes it was given.
			s := []NamedAnalyzerResult{
				withScore(makeNamed("saturation", 0, 50, "v", 10.0), 1),  // spare = 5 replicas
				withScore(makeNamed("throughput", 0, 100, "v", 10.0), 2), // spare = 10 replicas
			}
			initRoleState(s)

			value, binder := combineVotes(votesFromRoleSpare(s, domain.RoleBoth, "v"), false)
			Expect(value).To(BeNumerically("~", 6.6666667, 1e-6))
			Expect(binder).To(Equal(0), "saturation's conservative vote binds")

			Expect(safeRemovalReplicasForRole(s, "v", domain.RoleBoth)).To(Equal(6))
		})

		It("holds scale-down at the safe extremum when the conservative analyzer is the better-trusted one", func() {
			// Same votes as above with the trust the other way round: saturation
			// says 5 removable and is trusted twice as much as throughput's 10.
			// Every (s_i - s_e)+ is then zero, so no correction is applied and the
			// result stays at the conservative extremum. The direction that
			// matters for safety needs no special case in the formula.
			s := []NamedAnalyzerResult{
				withScore(makeNamed("saturation", 0, 50, "v", 10.0), 2),
				withScore(makeNamed("throughput", 0, 100, "v", 10.0), 1),
			}
			initRoleState(s)

			value, binder := combineVotes(votesFromRoleSpare(s, domain.RoleBoth, "v"), false)
			Expect(value).To(Equal(5.0))
			Expect(binder).To(Equal(0))

			Expect(safeRemovalReplicasForRole(s, "v", domain.RoleBoth)).To(Equal(5))
		})

		It("holds scale-up at the binder's vote when the binder is also the better-trusted one", func() {
			// Throughput wants 10 AND is trusted twice as much as saturation's 5:
			// the less-trusted dissenter has no pull, so sizing stays at 10.
			s := []NamedAnalyzerResult{
				withScore(makeNamed("saturation", 50, 0, "v", 10.0), 1),
				withScore(makeNamed("throughput", 100, 0, "v", 10.0), 2),
			}
			_, ps := initRoleState(s)

			value, binder := combineVotes(votesFromPickerState(s, ps, domain.RoleBoth, "v"), true)
			Expect(value).To(Equal(10.0))
			Expect(binder).To(Equal(1))

			Expect(roleBottleneckReplicas(s, ps, domain.RoleBoth, "v")).To(Equal(10))
		})

		It("treats an unset score as the 1.0 default so a hand-built entry cannot out-weigh a configured one", func() {
			// Only reachable off the config path, which coerces a zero score to
			// 1.0. Left as 0 the entry would contribute nothing to sum s while
			// still appearing in the excess term, quietly over-correcting.
			s := []NamedAnalyzerResult{
				makeNamed("saturation", 50, 0, "v", 10.0), // score unset -> 1.0
				withScore(makeNamed("throughput", 100, 0, "v", 10.0), 1),
			}
			_, ps := initRoleState(s)

			votes := votesFromPickerState(s, ps, domain.RoleBoth, "v")
			Expect(votes[0].Score).To(Equal(1.0))

			// Uniform weights after coercion, so the plain extremum.
			value, _ := combineVotes(votes, true)
			Expect(value).To(Equal(10.0))
		})
	})
})

var _ = Describe("paired helpers", func() {

	Describe("initRoleState", func() {
		It("disaggregated: roles from RoleCapacities; picker-state from RC; RoleSpare from SC", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 15000, 5000, 20000, 10000, 15000, 5000, 10000, 10000)}
			roles, ps := initRoleState(s)
			Expect(roles).To(ConsistOf("prefill", "decode"))
			Expect(ps[0]["prefill"]).To(BeNumerically("~", 15000.0, 1e-9))
			Expect(ps[0]["decode"]).To(BeNumerically("~", 5000.0, 1e-9))
			Expect(s[0].RoleSpare["prefill"]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(s[0].RoleSpare["decode"]).To(BeNumerically("~", 10000.0, 1e-9))
		})

		It("non-disaggregated: synthetic 'both' role using model-level Remaining/Spare", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 20000, 5000, "v", 10.0)}
			roles, ps := initRoleState(s)
			Expect(roles).To(ConsistOf(domain.RoleBoth))
			Expect(ps[0][domain.RoleBoth]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(s[0].RoleSpare[domain.RoleBoth]).To(BeNumerically("~", 5000.0, 1e-9))
		})
	})

	Describe("roleBottleneckReplicas", func() {
		It("computes max cross-analyzer ceil(roleRemaining/PRC)", func() {
			// analyzer0: prefill remaining=10000, PRC=5000 → ceil(10000/5000)=2
			// analyzer1: prefill remaining=15000, PRC=5000 → ceil(15000/5000)=3 (max)
			s := []NamedAnalyzerResult{
				makeNamedPD("sat", 10000, 20000, 0, 0, 10000, 20000, 5000, 8000),
				makeNamedPD("ta", 15000, 15000, 0, 0, 15000, 15000, 5000, 8000),
			}
			_, ps := initRoleState(s)
			Expect(roleBottleneckReplicas(s, ps, "prefill", "pf")).To(Equal(3))
			// decode: max(ceil(20000/8000)=3, ceil(15000/8000)=2) = 3
			Expect(roleBottleneckReplicas(s, ps, "decode", "dc")).To(Equal(3))
		})

		It("returns 0 when PRC=0 (cold-start guard)", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 10000, 20000, 0, 0, 10000, 20000, 0, 0)}
			_, ps := initRoleState(s)
			Expect(roleBottleneckReplicas(s, ps, "prefill", "pf")).To(Equal(0))
		})
	})

	Describe("roleAggRemaining", func() {
		It("compares in replica space, not raw cross-analyzer max (Bug #2)", func() {
			// sat: Remaining=100 (raw), PRC=1  -> replica-space demand = 100/1  = 100 (larger)
			// ta:  Remaining=5000 (raw), PRC=1000 -> replica-space demand = 5000/1000 = 5
			// A raw max would wrongly pick ta's 5000 (tokens vs a different unit's
			// remaining) even though sat's replica-space demand is the real
			// bottleneck. roleAggRemaining must return sat's raw value (100), the
			// entry the combine also identifies as the binder for "v" —
			// commensurable with sat's own PRC in the
			// caller's n*prc/demand formula.
			s := []NamedAnalyzerResult{
				makeNamed("sat", 100, 0, "v", 1.0),
				makeNamed("ta", 5000, 0, "v", 1000.0),
			}
			_, ps := initRoleState(s)
			Expect(roleAggRemaining(s, ps, domain.RoleBoth, "v")).To(Equal(100.0))
		})

		It("is byte-identical to the raw max with a single voter", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 250, 0, "v", 10.0)}
			_, ps := initRoleState(s)
			Expect(roleAggRemaining(s, ps, domain.RoleBoth, "v")).To(Equal(250.0))
		})

		It("returns 0 when no entry has a usable PRC for v", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 100, 0, "other", 1.0)}
			_, ps := initRoleState(s)
			Expect(roleAggRemaining(s, ps, domain.RoleBoth, "v")).To(Equal(0.0))
		})
	})

	Describe("allocateForModelPaired decrement", func() {
		It("decrements each analyzer's remaining by its OWN PRC, not the anchor's uniform PRC (Bug #1)", func() {
			// saturation binds v (rd=100/10=10 beats throughput's rd=500/100=5),
			// so the anchor's PRC for v is saturation's (10). 10 replicas of v
			// fully cover saturation's 100 remaining (10*10=100) AND throughput's
			// 500 remaining (10*100=1000 >= 500) when each analyzer's remaining is
			// decremented by k times its OWN PRC. Before the fix, the decrement
			// used the anchor's uniform PRC (10) for every analyzer, leaving
			// throughput's remaining at 500-10*10=400 -- not cleared, even though
			// its true demand was already satisfied -- which would force spurious
			// extra iterations.
			sat := makeNamed("saturation", 100, 0, "v", 10.0)
			ta := makeNamed("throughput", 500, 0, "v", 100.0)
			s := []NamedAnalyzerResult{sat, ta}
			variants := []domain.VariantCapacity{
				{VariantName: "v", AcceleratorName: "A100", Cost: 1.0, PerReplicaCapacity: 10, ReplicaCount: 0},
			}
			stateMap := map[string]domain.VariantReplicaState{
				"v": {VariantName: "v", CurrentReplicas: 0, GPUsPerReplica: 1},
			}
			targets := map[string]int{"v": 0}
			roles, ps := initRoleState(s)

			allocateForModelPaired(context.Background(), s, variants, stateMap, nil, targets, costGreedyRolePick, ps, roles)

			Expect(ps[0][domain.RoleBoth]).To(Equal(0.0), "saturation's remaining must clear")
			Expect(ps[1][domain.RoleBoth]).To(Equal(0.0), "throughput's remaining must clear using its OWN PRC, not saturation's")
			Expect(targets["v"]).To(Equal(10), "no spurious extra replicas from an under-decremented non-binder")
		})
	})

	Describe("safeRemovalReplicasForRole", func() {
		It("computes removable replicas from RoleSpare for a given role", func() {
			// RoleSpare["prefill"]=20000, PRC_P=10000 → floor(20000/10000)=2
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(2))
			// RoleSpare["decode"]=30000, PRC_D=10000 → floor(30000/10000)=3
			Expect(safeRemovalReplicasForRole(s, "dc", "decode")).To(Equal(3))
		})

		It("returns 0 when RoleSpare for role is 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 0, 30000, 10000, 30000, 10000, 10000)}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(0))
		})

		It("returns 0 when RoleSpare is nil", func() {
			e := makeNamed("sat", 0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(safeRemovalReplicasForRole([]NamedAnalyzerResult{e}, "v", "prefill")).To(Equal(0))
		})

		It("skips a non-live analyzer instead of letting its tiny spare drag the min to 0", func() {
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000) // floor(20000/10000)=2
			nonLive := makeNamedPD("throughput", 0, 0, 5000, 5000, 10000, 30000, 10000, 10000)
			nonLive.Live = false // would compute floor(5000/10000)=0 if counted
			s := []NamedAnalyzerResult{live, nonLive}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(2))
		})
	})

	Describe("applyDeallocationForRole", func() {
		It("decrements RoleSpare[role] by n×PRC", func() {
			// RoleSpare["prefill"]=20000, PRC=10000, n=2 → 20000-20000=0
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			applyDeallocationForRole(s, "pf", "prefill", 2)
			Expect(s[0].RoleSpare["prefill"]).To(Equal(0.0))
			// decode spare unchanged
			Expect(s[0].RoleSpare["decode"]).To(BeNumerically("~", 30000.0, 1e-9))
		})

		It("clamps RoleSpare to 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 5000, 0, 10000, 0, 10000, 10000)}
			applyDeallocationForRole(s, "pf", "prefill", 5) // would subtract 50000
			Expect(s[0].RoleSpare["prefill"]).To(Equal(0.0))
		})
	})

	Describe("needsScaleDownForRole", func() {
		It("returns true when all analyzers have RoleSpare[role] > 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("returns false when any analyzer has RoleSpare[role] = 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 0, 30000, 10000, 30000, 10000, 10000)}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("returns false for nil RoleSpare", func() {
			e := makeNamed("sat", 0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(needsScaleDownForRole([]NamedAnalyzerResult{e}, "prefill")).To(BeFalse())
		})

		It("never-analyzed analyzer does not veto: a non-live analyzer with no spare is skipped", func() {
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			neverAnalyzed := makeNamedPD("throughput", 0, 0, 0, 0, 0, 0, 10000, 10000)
			neverAnalyzed.Live = false
			neverAnalyzed.RoleSpare = nil
			s := []NamedAnalyzerResult{live, neverAnalyzed}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("stale analyzer does not veto: a non-live analyzer with zero spare is skipped", func() {
			// Staleness itself is computed at the engine level (see engine_v2_liveness_test.go);
			// here Live=false stands in for "last good analysis is older than the threshold".
			live := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			stale := makeNamedPD("throughput", 0, 0, 0, 0, 0, 0, 10000, 10000)
			stale.Live = false
			s := []NamedAnalyzerResult{live, stale}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("safety floor: returns false when no live analyzer remains", func() {
			a := makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			a.Live = false
			b := makeNamedPD("throughput", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			b.Live = false
			s := []NamedAnalyzerResult{a, b}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(s, "decode")).To(BeFalse())
		})

		It("a live analyzer with no spare still vetoes (real veto preserved)", func() {
			live := makeNamedPD("sat", 0, 0, 0, 30000, 10000, 30000, 10000, 10000)
			Expect(live.Live).To(BeTrue())
			s := []NamedAnalyzerResult{live}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse())
		})

		It("applies uniformly to saturation: a non-live saturation result does not veto", func() {
			satNonLive := makeNamedPD(domain.SaturationAnalyzerName, 0, 0, 0, 0, 0, 0, 10000, 10000)
			satNonLive.Live = false
			live := makeNamedPD("throughput", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			s := []NamedAnalyzerResult{satNonLive, live}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})
	})

	Describe("variantsForRole", func() {
		It("filters variants by exact role match", func() {
			vcs := []domain.VariantCapacity{
				{VariantName: "pf", Role: "prefill"},
				{VariantName: "dc", Role: "decode"},
				{VariantName: "both", Role: "both"},
			}
			Expect(variantsForRole(vcs, "prefill")).To(HaveLen(1))
			Expect(variantsForRole(vcs, "prefill")[0].VariantName).To(Equal("pf"))
			Expect(variantsForRole(vcs, "decode")[0].VariantName).To(Equal("dc"))
		})

		It("matches 'both' query against both explicit 'both' and empty-role variants", func() {
			vcs := []domain.VariantCapacity{
				{VariantName: "pf", Role: "prefill"},
				{VariantName: "dc", Role: "decode"},
				{VariantName: "all", Role: "both"},
				{VariantName: "also-both"}, // empty Role → canonicalized to "both" by variantsForRole
			}
			result := variantsForRole(vcs, "both")
			Expect(result).To(HaveLen(2))
			names := []string{result[0].VariantName, result[1].VariantName}
			Expect(names).To(ConsistOf("all", "also-both"))
			// querying "" matches nothing (vc empty roles are canonicalized to "both", not "")
			Expect(variantsForRole(vcs, "")).To(BeEmpty())
		})
	})

})
