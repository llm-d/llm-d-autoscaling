package pipeline

import (
	"context"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// rescaleReq builds a single-variant, non-disaggregated model request on the
// A100 accelerator (1 GPU/replica) for the rescale integration tests. Cost and
// per-replica capacity are fixed fixtures — the reclaim/fill assertions turn on
// priority, demand and GPU counts, not on the cost score.
func rescaleReq(id, ns string, priority, demand float64, current int) ModelScalingRequest {
	v := id + "-v"
	r := &domain.AnalyzerResult{
		ModelID:     id,
		Namespace:   ns,
		TotalDemand: demand,
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: v, AcceleratorName: "A100", Cost: 5, ReplicaCount: current, PerReplicaCapacity: 1000},
		},
	}
	return withSatEntry(r, ModelScalingRequest{
		ModelID:   id,
		Namespace: ns,
		Priority:  priority,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: v, CurrentReplicas: current, GPUsPerReplica: 1},
		},
	})
}

// nsModelReq builds a single-variant request with an explicit ModelID, namespace, and
// variant name — so a caller can place the same ModelID in two namespaces with distinct
// variants (the real multi-tenant shape) to exercise (namespace, ModelID) keying.
func nsModelReq(modelID, ns, variant string, priority, demand float64, current int) ModelScalingRequest {
	r := &domain.AnalyzerResult{
		ModelID:     modelID,
		Namespace:   ns,
		TotalDemand: demand,
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: variant, AcceleratorName: "A100", Cost: 5, ReplicaCount: current, PerReplicaCapacity: 1000},
		},
	}
	return withSatEntry(r, ModelScalingRequest{
		ModelID:   modelID,
		Namespace: ns,
		Priority:  priority,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: variant, CurrentReplicas: current, GPUsPerReplica: 1},
		},
	})
}

func hasRescaleReason(dm map[string]domain.VariantDecision) bool {
	for _, d := range dm {
		if d.ReasonCategory() == domain.DecisionReasonRescale {
			return true
		}
	}
	return false
}

var _ = Describe("GreedyByScoreOptimizer rescale", func() {
	// Budget 8 fully used by A (holds 8); B (higher priority) is starved. Water-fill
	// yields A2/B6, but reclaim frees nothing this cycle, so B cannot fill yet:
	// A reclaims to 2, B stays 0 (paced convergence, never over-budget).
	It("paces reclaim when no GPUs are free this cycle", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}
		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 8),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		da := dm["A-v"]
		Expect(da.TargetReplicas).To(Equal(2), "A reclaims to 2")
		Expect(da.Action).To(Equal(domain.ActionScaleDown))
		Expect(da.ReasonCategory()).To(Equal(domain.DecisionReasonRescale))
		Expect(dm["B-v"].TargetReplicas).To(Equal(0), "B fill gated: no free GPUs this cycle")
	})

	// Budget 8 with 3 free (A holds 5). A reclaims 5->2; B fills only up to the 3 free
	// GPUs this cycle -> B=3. Total never exceeds the budget.
	It("gates fill on the GPUs freed this cycle", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}
		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 5),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 5}}}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["A-v"].TargetReplicas).To(Equal(2))
		Expect(dm["B-v"].TargetReplicas).To(Equal(3), "free-gated")
		Expect(dm["B-v"].Action).To(Equal(domain.ActionScaleUp))
		Expect(dm["A-v"].TargetReplicas + dm["B-v"].TargetReplicas).To(BeNumerically("<=", 8))
	})

	// Rescale off: additive path only, no reclaim, no rescale reason; A stays at 8.
	It("leaves the additive path untouched when off", func() {
		o := NewGreedyByScoreOptimizer() // Rescale zero value = off
		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 8),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["A-v"].TargetReplicas).To(Equal(8), "no reclaim when off")
		Expect(hasRescaleReason(dm)).To(BeFalse())
	})

	// Plenty of budget (sum demand <= budget): no rescale, no reclaim.
	It("does not rescale an uncontended budget", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}
		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 2000, 2),
			rescaleReq("B", "default", 3, 2000, 2),
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 20, Used: 4}}}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(hasRescaleReason(dm)).To(BeFalse())
		Expect(dm["A-v"].TargetReplicas).To(BeNumerically(">=", 2), "A not reclaimed while uncontended")
	})

	// Scope-coupling: models under a namespace quota with only the cluster flag set are
	// NOT rescaled (the cluster flag doesn't govern a namespace quota).
	It("does not let the cluster flag rescale a namespace quota", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true} // no ByNamespace["team"]
		reqs := []ModelScalingRequest{
			rescaleReq("A", "team", 1, 8000, 8),
			rescaleReq("B", "team", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{{
			Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}},
			NamespacePools: map[string]map[string]ResourcePool{
				"team": {"A100": {Limit: 8, Used: 8}},
			},
		}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(hasRescaleReason(dm)).To(BeFalse(), "cluster flag rescaled a namespace-quota group")
	})

	// Same setup, but the namespace flag is on -> the namespace quota IS rescaled.
	It("rescales a namespace quota when the namespace flag is on", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"team": true}}
		reqs := []ModelScalingRequest{
			rescaleReq("A", "team", 1, 8000, 8),
			rescaleReq("B", "team", 3, 8000, 0),
		}
		cons := []*ResourceConstraints{{
			Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}},
			NamespacePools: map[string]map[string]ResourcePool{
				"team": {"A100": {Limit: 8, Used: 8}},
			},
		}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		da := dm["A-v"]
		Expect(da.TargetReplicas).To(Equal(2))
		Expect(da.ReasonCategory()).To(Equal(domain.DecisionReasonRescale))
	})

	// A P/D (disaggregated) low-priority model holding prefill 4 + decode 4 is reclaimed
	// to 2 total GPUs; the reclaim is split across roles by role demand (equal here), so
	// prefill and decode each drop to 1 — a slack role never trims the other.
	It("splits a P/D reclaim across roles by role demand", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		a := &domain.AnalyzerResult{
			ModelID:     "A",
			Namespace:   "default",
			TotalDemand: 8000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {TotalDemand: 4000},
				"decode":  {TotalDemand: 4000},
			},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "A-prefill", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "prefill", ReplicaCount: 4},
				{VariantName: "A-decode", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "decode", ReplicaCount: 4},
			},
		}
		reqA := withSatEntry(a, ModelScalingRequest{
			ModelID: "A", Namespace: "default", Priority: 1, Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "A-prefill", CurrentReplicas: 4, GPUsPerReplica: 1, Role: "prefill"},
				{VariantName: "A-decode", CurrentReplicas: 4, GPUsPerReplica: 1, Role: "decode"},
			},
		})
		reqB := rescaleReq("B", "default", 3, 8000, 0)

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		pf, dc := dm["A-prefill"], dm["A-decode"]
		Expect(pf.TargetReplicas).To(Equal(1), "prefill reclaimed to 1")
		Expect(dc.TargetReplicas).To(Equal(1), "decode reclaimed to 1")
		Expect(pf.ReasonCategory()).To(Equal(domain.DecisionReasonRescale))
		Expect(dc.ReasonCategory()).To(Equal(domain.DecisionReasonRescale))
	})

	// Unequal role demand (prefill 6000 / decode 2000) must split the reclaim 3:1, not
	// evenly. A holds prefill 4 + decode 4 (8 GPUs) and is reclaimed to 4 total: prefill
	// -> 3, decode -> 1. A 50/50 split would give 2/2 and fail.
	It("splits a P/D reclaim proportionally to unequal role demand", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		a := &domain.AnalyzerResult{
			ModelID:     "A",
			Namespace:   "default",
			TotalDemand: 8000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {TotalDemand: 6000},
				"decode":  {TotalDemand: 2000},
			},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "A-prefill", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "prefill", ReplicaCount: 4},
				{VariantName: "A-decode", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "decode", ReplicaCount: 4},
			},
		}
		reqA := withSatEntry(a, ModelScalingRequest{
			ModelID: "A", Namespace: "default", Priority: 1, Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "A-prefill", CurrentReplicas: 4, GPUsPerReplica: 1, Role: "prefill"},
				{VariantName: "A-decode", CurrentReplicas: 4, GPUsPerReplica: 1, Role: "decode"},
			},
		})
		// Equal-priority, equal-demand peer holding nothing → water-fill gives A a
		// target of 4 (half the budget of 8).
		reqB := rescaleReq("B", "default", 1, 8000, 0)

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		Expect(dm["A-prefill"].TargetReplicas).To(Equal(3), "prefill keeps 3 of 4 (demand 6:2)")
		Expect(dm["A-decode"].TargetReplicas).To(Equal(1), "decode drops to 1 of 4 (demand 6:2)")
	})

	// A namespace-scoped rescale FILL must debit BOTH the namespace quota AND the shared
	// cluster budget (a namespace quota draws from the same physical pool), or a later
	// cluster-scope allocation on the additive path could double-spend those GPUs.
	// applyRescale mutates the budget maps in place, so assert both are decremented.
	It("debits both the namespace and cluster budgets on a namespace fill", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"team": true}}

		reqs := []ModelScalingRequest{
			rescaleReq("A", "team", 1, 8000, 8), // low prio, holds the team quota
			rescaleReq("B", "team", 3, 8000, 0), // high prio, starved → fills the 2 free GPUs
		}
		available := map[string]int{"A100": 5}                          // cluster free
		availableByNS := map[string]map[string]int{"team": {"A100": 2}} // team free = 2
		_, handled := o.applyRescale(context.Background(), reqs, available, availableByNS)

		Expect(handled).To(HaveKeyWithValue("team/B", true))
		Expect(availableByNS["team"]["A100"]).To(Equal(0), "team quota debited by the 2-GPU fill")
		Expect(available["A100"]).To(Equal(3), "cluster budget must also drop by 2, not stay at 5")
	})

	// The physical cluster pool, not the namespace quota, bounds how many GPUs a
	// namespace fill may take this cycle. Here team's quota has 5 free but the cluster
	// physically has only 1 free, so B may fill at most 1 — and the cluster budget must
	// land at 0, never negative (which would corrupt the additive path this cycle).
	It("bounds a namespace fill by the cluster physical free, not the quota", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"team": true}}

		reqs := []ModelScalingRequest{
			rescaleReq("A", "team", 1, 8000, 8), // low prio, holds the quota
			rescaleReq("B", "team", 3, 8000, 0), // high prio, starved
		}
		available := map[string]int{"A100": 1}                          // cluster physically tight
		availableByNS := map[string]map[string]int{"team": {"A100": 5}} // quota looser than physical
		_, handled := o.applyRescale(context.Background(), reqs, available, availableByNS)

		Expect(handled).To(HaveKeyWithValue("team/B", true))
		Expect(available["A100"]).To(Equal(0), "fill bounded by the 1 physical GPU; budget not driven negative")
		Expect(availableByNS["team"]["A100"]).To(Equal(4), "quota debited by the 1 GPU actually filled")
	})

	// Two groups on the same accelerator type share one physical pool. Groups are
	// processed in a deterministic (accType, scope) order — cluster scope ("") sorts
	// before any namespace — so the cluster group consumes the 2 physical GPUs first and
	// the namespace group's fill is then gated to 0. This pins the deterministic ordering:
	// X (cluster) wins, A (team) is starved even though its quota still shows 5 free.
	It("processes groups in a deterministic order across a shared pool", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true, ByNamespace: map[string]bool{"team": true}}

		reqs := []ModelScalingRequest{
			rescaleReq("X", "solo", 1, 8000, 0), // cluster-scope filler
			rescaleReq("A", "team", 1, 8000, 0), // namespace-scope filler
		}
		available := map[string]int{"A100": 2}                          // shared physical free
		availableByNS := map[string]map[string]int{"team": {"A100": 5}} // team quota looser
		decisions, _ := o.applyRescale(context.Background(), reqs, available, availableByNS)
		dm := decisionMap(decisions)

		Expect(dm["X-v"].TargetReplicas).To(Equal(2), "cluster group processed first, takes the 2 physical GPUs")
		Expect(dm["A-v"].TargetReplicas).To(Equal(0), "namespace group gated to 0 by the depleted shared pool")
		Expect(available["A100"]).To(Equal(0))
		Expect(availableByNS["team"]["A100"]).To(Equal(5), "team quota untouched: it never got a physical GPU")
	})

	// A filler's MaxReplicas caps how far rescale grows it, even when more GPUs are free.
	// B (high prio) would take ~6 of the 6 free GPUs by priority weight, but MaxReplicas=2
	// caps its target at 2. Fails if the MaxReplicas-derived cap is dropped.
	It("caps a fill at the model's MaxReplicas", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		max2 := 2
		bAnalyzer := &domain.AnalyzerResult{
			ModelID: "B", Namespace: "default", TotalDemand: 8000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "B-v", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, ReplicaCount: 0},
			},
		}
		reqB := withSatEntry(bAnalyzer, ModelScalingRequest{
			ModelID: "B", Namespace: "default", Priority: 3,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "B-v", CurrentReplicas: 0, GPUsPerReplica: 1, MaxReplicas: &max2},
			},
		})
		reqA := rescaleReq("A", "default", 1, 8000, 4) // holds 4, absorbs the rest

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 10, Used: 4}}}} // free = 6
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		Expect(dm["B-v"].TargetReplicas).To(Equal(2), "capped at MaxReplicas despite 6 free GPUs and high priority")
	})

	// Two models in one contended group both want to fill, but only 1 GPU is free this
	// cycle. The higher-priority filler must win it regardless of request order — so with
	// input order [C, A(low), B(high)], B (not the earlier-listed A) gets the GPU. This
	// fails if the priority sort before the fill loop is removed.
	It("serves the higher-priority filler first when free GPUs are scarce", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			rescaleReq("C", "default", 1, 4000, 4), // over-share, reclaims (frees nothing this cycle)
			rescaleReq("A", "default", 2, 4000, 0), // lower-priority filler, listed first
			rescaleReq("B", "default", 4, 4000, 0), // higher-priority filler
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 5, Used: 4}}}} // free = 1
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["B-v"].TargetReplicas).To(Equal(1), "high-priority filler wins the single free GPU")
		Expect(dm["A-v"].TargetReplicas).To(Equal(0), "lower-priority filler starved despite being listed first")
		Expect(dm["C-v"].TargetReplicas).To(Equal(1), "over-share model reclaimed 4 -> 1")
	})

	// A multi-accelerator model (variants spanning A100 + H100) is out of Alpha scope and
	// must be skipped by rescale — never tagged DecisionReasonRescale — while a co-running
	// single-accelerator contended pair still rescales normally.
	It("skips multi-accelerator models but still rescales single-type groups", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		m := &domain.AnalyzerResult{
			ModelID:     "M",
			Namespace:   "default",
			TotalDemand: 8000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "M-a100", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, ReplicaCount: 4},
				{VariantName: "M-h100", AcceleratorName: "H100", Cost: 8, PerReplicaCapacity: 2000, ReplicaCount: 0},
			},
		}
		reqM := withSatEntry(m, ModelScalingRequest{
			ModelID: "M", Namespace: "default", Priority: 1,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "M-a100", CurrentReplicas: 4, GPUsPerReplica: 1},
				{VariantName: "M-h100", CurrentReplicas: 0, GPUsPerReplica: 1},
			},
		})
		// Single-accelerator contended pair on A100 that rescale as usual.
		reqA := rescaleReq("A", "default", 1, 8000, 8)
		reqB := rescaleReq("B", "default", 3, 8000, 0)

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}, "H100": {Limit: 4, Used: 0}}}}
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqM, reqA, reqB}, cons))

		ma, mh, av := dm["M-a100"], dm["M-h100"], dm["A-v"]
		Expect(ma.ReasonCategory()).ToNot(Equal(domain.DecisionReasonRescale), "multi-accel model must not be rescaled")
		Expect(mh.ReasonCategory()).ToNot(Equal(domain.DecisionReasonRescale), "multi-accel model must not be rescaled")
		// Proof that rescale did run this cycle on the single-type group.
		Expect(av.ReasonCategory()).To(Equal(domain.DecisionReasonRescale))
	})

	// The same ModelID served in two namespaces must not collide in rescale's bookkeeping
	// (a model's identity is (namespace, ModelID)). Both "llama" models land in the same
	// cluster-scope group; they must get independent priority-weighted targets, not be
	// driven to a shared one. With bare-ModelID keying, team/llama would inherit
	// prod/llama's target and never be reclaimed.
	It("keeps same-ModelID models in different namespaces independent within a group", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			nsModelReq("llama", "team", "llama-team-v", 1, 8000, 6), // low prio, over-share
			nsModelReq("llama", "prod", "llama-prod-v", 3, 8000, 0), // high prio, starved
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 6}}}} // free = 2
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["llama-team-v"].TargetReplicas).To(Equal(2), "team/llama reclaimed to its own share, not left at prod/llama's target")
		Expect(dm["llama-prod-v"].TargetReplicas).To(Equal(2), "prod/llama fills the 2 free GPUs")
	})

	// A model handled by rescale in one namespace must not freeze the same ModelID in
	// another namespace out of the additive path. team/llama is rescaled (namespace flag);
	// prod/llama is cluster-scope with the cluster flag off, so it must still get a normal
	// additive decision. With bare-ModelID keying, handled["llama"] would skip prod/llama
	// entirely, producing no decision for it.
	It("does not freeze a same-ModelID model in another namespace", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"team": true}} // cluster flag off

		reqs := []ModelScalingRequest{
			nsModelReq("llama", "team", "llama-team-v", 1, 10000, 4), // rescaled (contended namespace quota)
			nsModelReq("llama", "prod", "llama-prod-v", 1, 3000, 3),  // cluster-scope, additive path
		}
		cons := []*ResourceConstraints{{
			Pools: map[string]ResourcePool{"A100": {Limit: 20, Used: 7}},
			NamespacePools: map[string]map[string]ResourcePool{
				"team": {"A100": {Limit: 6, Used: 4}}, // team free = 2, contended
			},
		}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm).To(HaveKey("llama-prod-v"), "prod/llama must not be frozen out by team/llama being handled")
		Expect(dm["llama-prod-v"].TargetReplicas).To(Equal(3), "prod/llama gets its normal additive decision")
	})

	// Reclaim quantizes to whole replicas at the variant's GPUsPerReplica. A holds 4
	// replicas x 2 GPUs = 8 GPUs; its water-fill target is 3 GPUs, but with 2 GPUs/replica
	// it can only shed whole replicas, so it rounds down to 2 replicas (4 GPUs) rather
	// than a fractional 3-GPU target. Exercises the gpusPerReplica>1 quantization path.
	It("quantizes a reclaim to whole replicas at GPUsPerReplica > 1", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		aAnalyzer := &domain.AnalyzerResult{
			ModelID: "A", Namespace: "default", TotalDemand: 8000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "A-v", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, ReplicaCount: 4},
			},
		}
		reqA := withSatEntry(aAnalyzer, ModelScalingRequest{
			ModelID: "A", Namespace: "default", Priority: 1,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "A-v", CurrentReplicas: 4, GPUsPerReplica: 2}, // 8 GPUs
			},
		})
		reqB := nsModelReq("B", "default", "B-v", 2, 8000, 0) // higher prio, 1 GPU/replica

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		Expect(dm["A-v"].TargetReplicas).To(Equal(2), "8 GPUs -> 4 GPUs (2 replicas); the odd 3rd GPU can't be shed")
	})

	// An unlimited (math.MaxInt) cluster budget for the type is skipped — there is nothing
	// to redistribute, so rescale leaves the group to the additive path (no rescale reason,
	// no panic/overflow from water-filling against MaxInt).
	It("skips a group whose budget is unlimited", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 8),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		available := map[string]int{"A100": math.MaxInt} // unlimited budget for this type
		decisions, handled := o.applyRescale(context.Background(), reqs, available, nil)

		Expect(handled).To(BeEmpty(), "unlimited budget: nothing rescaled")
		Expect(hasRescaleReason(decisionMap(decisions))).To(BeFalse())
		Expect(available["A100"]).To(Equal(math.MaxInt), "unlimited budget untouched")
	})

	// An already-over-subscribed pool (Used > Limit -> negative free) is skipped: there is
	// no budget to redistribute, and water-filling a negative budget would be nonsensical.
	It("skips a group whose budget is already over-subscribed", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			rescaleReq("A", "default", 1, 8000, 8),
			rescaleReq("B", "default", 3, 8000, 0),
		}
		available := map[string]int{"A100": -2} // Used exceeds Limit
		decisions, handled := o.applyRescale(context.Background(), reqs, available, nil)

		Expect(handled).To(BeEmpty(), "over-subscribed budget: nothing rescaled")
		Expect(hasRescaleReason(decisionMap(decisions))).To(BeFalse())
		Expect(available["A100"]).To(Equal(-2), "over-subscribed budget untouched")
	})

	// A P/D role whose fill is capped by MaxReplicas must free the unused budget for later
	// fillers this cycle — freeThisCycle is decremented by the GPUs actually spent, not the
	// amount wanted. B (high prio) has prefill capped at 1 replica; the GPU its prefill
	// couldn't use must flow to C (lower prio). With a want-based decrement, C would get 0.
	It("returns a MaxReplicas-capped role's unused fill budget to later models", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		prefMax := 1
		bAnalyzer := &domain.AnalyzerResult{
			ModelID: "B", Namespace: "default", TotalDemand: 8000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {TotalDemand: 3000},
				"decode":  {TotalDemand: 1000},
			},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "B-prefill", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "prefill", ReplicaCount: 0},
				{VariantName: "B-decode", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "decode", ReplicaCount: 0},
			},
		}
		reqB := withSatEntry(bAnalyzer, ModelScalingRequest{
			ModelID: "B", Namespace: "default", Priority: 3, Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "B-prefill", CurrentReplicas: 0, GPUsPerReplica: 1, Role: "prefill", MaxReplicas: &prefMax},
				{VariantName: "B-decode", CurrentReplicas: 0, GPUsPerReplica: 1, Role: "decode"},
			},
		})
		reqA := rescaleReq("A", "default", 1, 8000, 5) // over-share, reclaims (frees nothing this cycle)
		reqC := rescaleReq("C", "default", 2, 8000, 0) // lower-priority filler that should get the leftover

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 5}}}} // free = 3
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB, reqC}, cons))

		Expect(dm["B-prefill"].TargetReplicas).To(Equal(1), "prefill capped at MaxReplicas")
		Expect(dm["C-v"].TargetReplicas).To(Equal(1), "C gets the GPU prefill couldn't use (spent-, not want-, accounting)")
	})

	// Fill ties are broken by the full (namespace, ModelID) identity, not bare ModelID.
	// Two equal-priority "llama" fillers in different namespaces compete for one free GPU;
	// the winner must be deterministic (modelKey order: "prod/llama" < "team/llama"),
	// regardless of team being listed first. With a bare-ModelID tie-break, the winner
	// would follow randomized request order.
	It("breaks fill ties deterministically by namespace, not just ModelID", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		reqs := []ModelScalingRequest{
			rescaleReq("C", "default", 1, 8000, 4),                  // over-share, reclaims
			nsModelReq("llama", "team", "llama-team-v", 2, 8000, 0), // equal-priority filler, listed first
			nsModelReq("llama", "prod", "llama-prod-v", 2, 8000, 0), // equal-priority filler
		}
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 5, Used: 4}}}} // free = 1
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(dm["llama-prod-v"].TargetReplicas).To(Equal(1), "prod/llama wins the tie (lower modelKey)")
		Expect(dm["llama-team-v"].TargetReplicas).To(Equal(0), "team/llama loses despite being listed first")
	})

	// P/D fill: a starved disaggregated model fills across roles proportional to role
	// demand (6:2 -> 3:1), sharing one freeThisCycle counter. With 8 free GPUs its target
	// of 8 fills fully as prefill 6 / decode 2 — not evenly (4/4) and not dumped on one role.
	It("splits a P/D fill proportionally to role demand", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		b := &domain.AnalyzerResult{
			ModelID: "B", Namespace: "default", TotalDemand: 8000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {TotalDemand: 6000},
				"decode":  {TotalDemand: 2000},
			},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "B-prefill", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "prefill", ReplicaCount: 0},
				{VariantName: "B-decode", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, Role: "decode", ReplicaCount: 0},
			},
		}
		reqB := withSatEntry(b, ModelScalingRequest{
			ModelID: "B", Namespace: "default", Priority: 3, Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "B-prefill", CurrentReplicas: 0, GPUsPerReplica: 1, Role: "prefill"},
				{VariantName: "B-decode", CurrentReplicas: 0, GPUsPerReplica: 1, Role: "decode"},
			},
		})
		reqA := rescaleReq("A", "default", 1, 8000, 6) // low prio, holds part of the budget

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 14, Used: 6}}}} // free = 8
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		Expect(dm["B-prefill"].TargetReplicas).To(Equal(6), "prefill gets 6 of 8 (demand 6:2)")
		Expect(dm["B-decode"].TargetReplicas).To(Equal(2), "decode gets 2 of 8 (demand 6:2)")
	})

	// Scope-coupling no-op row: a namespace whose own flag is on but that has NO quota
	// (no availableByNS entry) is classified cluster-scope; with the cluster flag off, the
	// namespace flag governs nothing, so rescale must not run.
	It("does not rescale when the namespace flag has no same-scope budget", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{ByNamespace: map[string]bool{"ns": true}} // cluster flag off

		reqs := []ModelScalingRequest{
			rescaleReq("A", "ns", 1, 8000, 8),
			rescaleReq("B", "ns", 3, 8000, 0),
		}
		// No NamespacePools for "ns" -> models are cluster-scoped; the namespace flag has
		// no budget at its scope to govern.
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 8, Used: 8}}}}
		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))

		Expect(hasRescaleReason(dm)).To(BeFalse())
	})

	// Fill-side whole-replica quantization at GPUsPerReplica > 1: a starved 2-GPU/replica
	// model with a 5-GPU water-fill target and only 3 GPUs free this cycle fills 1 replica
	// (2 GPUs) — floor(3/2) — not a fractional or rounded-up amount.
	It("quantizes a fill to whole replicas at GPUsPerReplica > 1", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		bAnalyzer := &domain.AnalyzerResult{
			ModelID: "B", Namespace: "default", TotalDemand: 8000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "B-v", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, ReplicaCount: 0},
			},
		}
		reqB := withSatEntry(bAnalyzer, ModelScalingRequest{
			ModelID: "B", Namespace: "default", Priority: 3,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "B-v", CurrentReplicas: 0, GPUsPerReplica: 2},
			},
		})
		reqA := rescaleReq("A", "default", 1, 8000, 4) // holds 4, reclaims

		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 7, Used: 4}}}} // free = 3
		dm := decisionMap(o.Optimize(context.Background(), []ModelScalingRequest{reqA, reqB}, cons))

		Expect(dm["B-v"].TargetReplicas).To(Equal(1), "3 free GPUs at 2 GPUs/replica -> 1 whole replica, odd GPU unfilled")
	})

	// Conflict path (Σ floors > budget) driven through Optimize: two models both below
	// their minReplicas=4 (floors sum 8 > budget 7). The clamp-to-floors path must complete
	// without panic and never reclaim a model below its current holdings.
	It("handles the floors-exceed-budget Conflict path without violating floors", func() {
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}

		min4 := 4
		mk := func(id string, prio float64, cur int) ModelScalingRequest {
			a := &domain.AnalyzerResult{
				ModelID: id, Namespace: "default", TotalDemand: 8000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: id + "-v", AcceleratorName: "A100", Cost: 5, PerReplicaCapacity: 1000, ReplicaCount: cur},
				},
			}
			return withSatEntry(a, ModelScalingRequest{
				ModelID: id, Namespace: "default", Priority: prio,
				VariantStates: []domain.VariantReplicaState{
					{VariantName: id + "-v", CurrentReplicas: cur, GPUsPerReplica: 1, MinReplicas: &min4},
				},
			})
		}
		reqs := []ModelScalingRequest{mk("A", 1, 2), mk("B", 3, 2)}                                   // both under minReplicas
		cons := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 7, Used: 4}}}} // free 3, budget 7 < Σfloor 8

		dm := decisionMap(o.Optimize(context.Background(), reqs, cons))
		Expect(dm["A-v"].TargetReplicas).To(BeNumerically(">=", 2), "not reclaimed below current in Conflict")
		Expect(dm["B-v"].TargetReplicas).To(BeNumerically(">=", 2), "not reclaimed below current in Conflict")
	})
})
