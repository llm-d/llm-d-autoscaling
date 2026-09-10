package pipeline

// Per-iteration dynamic refresh: refreshAnchorSizing recomputes the
// (role,variant) binder from the current pickerState — allocateForModelPaired
// re-invokes it at the head of every allocation iteration, so the anchor's
// sizing reflects the *current* remaining demand, not a one-time pick. This
// test drives refreshAnchorSizing directly with two pickerState snapshots
// (an "early" one and a "late" one, standing in for allocation progress
// within a single water-fill) rather than threading a full multi-iteration
// scenario through the optimizer, since the loop's own commit sizing (k) can
// clear an unconstrained single-role model's demand in one shot — the
// snapshots isolate exactly what the refresh changes, independent of that.
//
// What the refresh buys, stated as the contrast it is built against rather
// than as a claim about history: without it the sizing source is chosen once,
// and every read of the anchor's VariantCapacities sees that same pick no
// matter how far remaining demand has since moved. The two snapshots below
// differ only in remaining demand, so an assertion that tells them apart is
// telling apart exactly that.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("per-iteration dynamic refresh of the anchor's binder", func() {
	It("flips the (role,variant) binder as remaining demand shifts, and the cost ranking with it", func() {
		// v1 is cheap only while saturation binds (Cost/PRC = 1/100);
		// v2 is cheap only once throughput binds (Cost/PRC = 1/100 vs
		// saturation's 1/10 for v2, i.e. 10x worse under saturation).
		sat := &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 1, PerReplicaCapacity: 100, Reason: "P1-obs"},
				{VariantName: "v2", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 1, PerReplicaCapacity: 10, Reason: "P1-obs"},
			},
		}
		ta := &domain.AnalyzerResult{
			AnalyzerName: "throughput",
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", PerReplicaCapacity: 10, Reason: "T1-ols"},
				{VariantName: "v2", PerReplicaCapacity: 100, Reason: "T1-ols"},
			},
		}
		s := []NamedAnalyzerResult{
			{Name: domain.SaturationAnalyzerName, Result: sat, Enabled: true, Live: true},
			{Name: "throughput", Result: ta, Enabled: true, Live: true},
		}
		anchor := bindingAnchor(s)
		Expect(anchor).NotTo(BeNil())

		// Early water-fill: saturation has far more remaining relative to its
		// PRC on both variants (ceil(1000/100)=10, ceil(1000/10)=100) than
		// throughput (ceil(50/10)=5, ceil(50/100)=1) — saturation binds both,
		// and v1 (Cost/PRC_sat=0.01) is cheaper than v2 (Cost/PRC_sat=0.1).
		early := RolePairedState{
			{domain.RoleBoth: 1000},
			{domain.RoleBoth: 50},
		}
		refreshAnchorSizing(anchor.VariantCapacities, s, early)
		Expect(sortByCostEfficiencyAsc(anchor.VariantCapacities)[0].VariantName).To(Equal("v1"),
			"early water-fill: saturation binds both variants, and v1 is cheaper under saturation's PRC")

		// Late water-fill: saturation's remaining has shrunk far more than
		// throughput's (a plausible outcome of committing replicas over
		// several iterations) — now throughput binds both variants
		// (ceil(250/10)=25 > ceil(5/100)=1; ceil(250/100)=3 > ceil(5/10)=1),
		// and v2 (Cost/PRC_ta=0.01) is now cheaper than v1 (Cost/PRC_ta=0.1).
		late := RolePairedState{
			{domain.RoleBoth: 5},
			{domain.RoleBoth: 250},
		}
		refreshAnchorSizing(anchor.VariantCapacities, s, late)
		Expect(sortByCostEfficiencyAsc(anchor.VariantCapacities)[0].VariantName).To(Equal("v2"),
			"late water-fill: throughput now binds both variants, and v2 is cheaper under throughput's PRC")
	})

	It("is a no-op with a single voter, upholding the single-vote invariant", func() {
		sat := &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", AcceleratorName: "A100", Cost: 1.0, ReplicaCount: 1, PerReplicaCapacity: 100, Reason: "P1-obs"},
			},
		}
		s := []NamedAnalyzerResult{
			{Name: domain.SaturationAnalyzerName, Result: sat, Enabled: true, Live: true},
		}
		anchor := bindingAnchor(s)
		Expect(anchor).NotTo(BeNil())
		before := anchor.VariantCapacities[0]

		refreshAnchorSizing(anchor.VariantCapacities, s, RolePairedState{{domain.RoleBoth: 1}})

		Expect(anchor.VariantCapacities[0]).To(Equal(before), "len(s) <= 1 must not invoke the refresh at all")
	})
})
