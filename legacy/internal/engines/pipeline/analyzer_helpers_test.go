package pipeline

import (
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
		// A two-entry ballot where saturation is the (a)/identity carrier and a
		// live throughput analyzer is the (b)/sizing binder. The merged anchor must
		// take identity (accelerator, cost, replica count, model ID) from
		// saturation and sizing (PRC, reason, model-level RC) from throughput, and
		// recompute TotalCapacity. The fixtures make the anchor differ from
		// ballot[0] (saturation) in both analyzer name and PRC, so an implementation
		// that merely returned the saturation entry would fail — proving the merge.
		It("merges (a) identity from saturation with (b) sizing from the binding analyzer", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // present as the (a) carrier, not voting
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
						PerReplicaCapacity: 100.0, // sat's own (b) — must NOT surface for v1
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
					ModelID:          "ignored", // identity comes from the (a) carrier
					RequiredCapacity: 50,
					VariantCapacities: []domain.VariantCapacity{{
						VariantName:        "v1",
						PerReplicaCapacity: 200.0, // binding (b) — this is what surfaces
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
			// (a) from saturation
			Expect(vc.AcceleratorName).To(Equal("A100"))
			Expect(vc.Cost).To(Equal(10.0))
			Expect(vc.ReplicaCount).To(Equal(2))
			// (b) from binding (throughput)
			Expect(vc.PerReplicaCapacity).To(Equal(200.0))
			Expect(vc.Reason).To(Equal("T1-ols"))
			// TotalCapacity recomputed = ReplicaCount(a) × PerReplicaCapacity(b)
			Expect(vc.TotalCapacity).To(Equal(400.0))
		})

		// Test 2 — per-variant (b)-fallback + ordering.
		// The (a) carrier (saturation) lists v1+v2; the binding analyzer lists only
		// v1. Variant ordering follows the (a) carrier. For v2 (omitted by the
		// binder) the fallback depends on whether saturation votes:
		//   - saturation enabled (but non-binding here because non-live): its own
		//     (b) is a consistent (demand, PRC) source, so v2 resolves from it;
		//   - saturation not enabled (throughput-only): no consistent fallback, so
		//     v2's PRC stays 0 and it is not proactively selectable.
		It("falls back to saturation's own (b) for an omitted variant when saturation votes", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: true,  // votes → fallback source is consistent
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
			// Ordering follows the (a) carrier (saturation): v1 then v2.
			Expect(anchor.VariantCapacities[0].VariantName).To(Equal("v1"))
			Expect(anchor.VariantCapacities[1].VariantName).To(Equal("v2"))
			// v1 sized by the binding analyzer.
			Expect(anchor.VariantCapacities[0].PerReplicaCapacity).To(Equal(200.0))
			Expect(anchor.VariantCapacities[0].Reason).To(Equal("T1-ols"))
			// v2 omitted by the binder → falls back to saturation's own (b).
			Expect(anchor.VariantCapacities[1].PerReplicaCapacity).To(Equal(110.0))
			Expect(anchor.VariantCapacities[1].Reason).To(Equal("P1-obs"))
		})

		It("leaves an omitted variant at PRC=0 under a throughput-only (non-voting saturation) config", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // throughput-only: saturation carries (a) but does not vote
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
		// identity comes from saturation's (a) contribution through the merge. This
		// pins the wiring so a later change that repoints the read at the raw binding
		// result (which has no AcceleratorName) can't silently drop the model from
		// rescale. Throughput-only rescale *correctness* is a later change; this only
		// freezes the read-source.
		It("resolves the accelerator type via the merged anchor when the binding analyzer omits it", func() {
			sat := NamedAnalyzerResult{
				Name:    domain.SaturationAnalyzerName,
				Enabled: false, // throughput-only: saturation carries (a) but does not vote
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
			// Accelerator identity survives via saturation's (a), even though the
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
		// than indexing into an empty or unbindable ballot. These pin the three
		// nil paths that back the "empty / no-live-analyzer ballot is graceful"
		// behaviour: no index panic on an empty ballot, and a deliberate hold
		// when no single analyzer can bind.
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

		It("returns nil for an ambiguous multi-binder (two non-saturation live analyzers)", func() {
			// No saturation entry; two distinct non-saturation analyzers are each
			// enabled+live+informative. This PR does not define which one binds, so
			// bindingAnchor refuses to guess and holds the model.
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
			Expect(bindingAnchor([]NamedAnalyzerResult{ta, lat})).To(BeNil())
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
