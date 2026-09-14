package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func sumTargets(t map[string]int) int {
	s := 0
	for _, v := range t {
		s += v
	}
	return s
}

var _ = Describe("computeRescaleTargets", func() {
	DescribeTable("priority-weighted water-filling",
		func(in []rescaleInput, budget int, want map[string]int, overBudget bool) {
			got, over := computeRescaleTargets(in, budget)
			Expect(over).To(Equal(overBudget))
			for id, w := range want {
				Expect(got[id]).To(Equal(w), "target[%s]", id)
			}
			if !overBudget {
				// Invariant: the split never over-allocates the budget.
				Expect(sumTargets(got)).To(BeNumerically("<=", budget))
			}
			for _, m := range in {
				// Invariant: no model exceeds its cap.
				Expect(got[m.ID]).To(BeNumerically("<=", m.CapGPUs), "cap for %s", m.ID)
			}
		},
		// Proposal worked example: A holds the pool, B (higher priority) is starved;
		// rescale reclaims from A to give B its priority-weighted share.
		Entry("proposal: A prio1 dem8, B prio3 dem8, budget 8 -> A2 B6",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 8, CapGPUs: 8}, {ID: "B", Priority: 3, Demand: 8, CapGPUs: 8}},
			8, map[string]int{"A": 2, "B": 6}, false),
		// B wants less than its share (demand 4): its 4.8 caps at 4 and the freed
		// excess flows back to A.
		Entry("proposal: B demand 4 caps and re-splits -> A4 B4",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 8, CapGPUs: 8}, {ID: "B", Priority: 3, Demand: 4, CapGPUs: 4}},
			8, map[string]int{"A": 4, "B": 4}, false),
		Entry("minReplicas floors reserved first",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 10, FloorGPUs: 2, CapGPUs: 10}, {ID: "B", Priority: 1, Demand: 10, CapGPUs: 10}},
			6, map[string]int{"A": 4, "B": 2}, false),
		Entry("fractional shares round by largest remainder (tie by id)",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 1, CapGPUs: 7}, {ID: "B", Priority: 1, Demand: 1, CapGPUs: 7}},
			7, map[string]int{"A": 4, "B": 3}, false),
		Entry("zero-demand model gets floor only",
			[]rescaleInput{{ID: "idle", Priority: 1, Demand: 0, FloorGPUs: 1, CapGPUs: 5}, {ID: "busy", Priority: 1, Demand: 8, CapGPUs: 8}},
			6, map[string]int{"idle": 1, "busy": 5}, false),
		Entry("floors exceed budget -> conflict (clamp to floors)",
			[]rescaleInput{{ID: "A", Priority: 1, Demand: 5, FloorGPUs: 4, CapGPUs: 5}, {ID: "B", Priority: 1, Demand: 5, FloorGPUs: 3, CapGPUs: 5}},
			6, map[string]int{"A": 4, "B": 3}, true),
	)

	It("is deterministic across runs", func() {
		in := []rescaleInput{
			{ID: "z", Priority: 1, Demand: 1, CapGPUs: 9},
			{ID: "a", Priority: 1, Demand: 1, CapGPUs: 9},
			{ID: "m", Priority: 1, Demand: 1, CapGPUs: 9},
		}
		first, _ := computeRescaleTargets(in, 7)
		for i := 0; i < 20; i++ {
			got, _ := computeRescaleTargets(in, 7)
			Expect(got).To(Equal(first))
		}
	})
})

var _ = Describe("roundExtras", func() {
	// The water-fill caps at headroom before rounding, so this cap-skip guard is
	// unreachable through computeRescaleTargets; exercise it directly. The model with
	// the largest fractional remainder is already at its cap, so the leftover GPU must
	// skip to the next-largest remainder instead of breaching the cap.
	It("skips a largest-remainder model already at its cap", func() {
		in := []rescaleInput{{ID: "A", CapGPUs: 2}, {ID: "B", CapGPUs: 5}}
		targets := map[string]int{"A": 2, "B": 0} // A already at its cap of 2
		roundExtras(in, map[string]float64{"A": 0.9, "B": 0.4}, targets)
		Expect(targets["A"]).To(Equal(2), "A is at cap; leftover must not push it to 3")
		Expect(targets["B"]).To(Equal(1), "leftover GPU falls to B")
	})
})

var _ = Describe("distributeGPUsByWeight", func() {
	It("splits proportionally to weight, not evenly", func() {
		// 4 GPUs with role demand 6:2 must split 3:1 — a 50/50 split (2:2) or a
		// swapped/ignored weight map would fail here.
		out := distributeGPUsByWeight(4, []string{"decode", "prefill"},
			map[string]int{"prefill": 6, "decode": 2}, nil, nil)
		Expect(out["prefill"]).To(Equal(3))
		Expect(out["decode"]).To(Equal(1))
	})

	It("falls back to the fallback weights when all weights are zero", func() {
		out := distributeGPUsByWeight(4, []string{"decode", "prefill"},
			map[string]int{"prefill": 0, "decode": 0},
			map[string]int{"prefill": 3, "decode": 1}, nil)
		Expect(out["prefill"]).To(Equal(3))
		Expect(out["decode"]).To(Equal(1))
	})

	It("gives a single role the whole total", func() {
		out := distributeGPUsByWeight(5, []string{"both"}, map[string]int{"both": 0}, nil, nil)
		Expect(out["both"]).To(Equal(5))
	})

	It("reserves each role's floor before the weighted split", func() {
		// Cold-start P/D: zero demand and zero current on both roles, but each role has
		// a floor of 1. Without floor reservation the whole total lands on the
		// alphabetically-first role (decode), starving prefill's minReplicas.
		out := distributeGPUsByWeight(2, []string{"decode", "prefill"},
			map[string]int{"prefill": 0, "decode": 0}, // demand
			map[string]int{"prefill": 0, "decode": 0}, // current
			map[string]int{"prefill": 1, "decode": 1}) // floor
		Expect(out["prefill"]).To(Equal(1), "prefill floor honored")
		Expect(out["decode"]).To(Equal(1), "decode floor honored")
	})

	It("gives the weighted remainder on top of reserved floors", func() {
		// 6 GPUs, floors 1:1 (reserve 2), remainder 4 split by demand 3:1 → prefill 3+1,
		// decode 1+1.
		out := distributeGPUsByWeight(6, []string{"decode", "prefill"},
			map[string]int{"prefill": 3, "decode": 1},
			map[string]int{"prefill": 0, "decode": 0},
			map[string]int{"prefill": 1, "decode": 1})
		Expect(out["prefill"]).To(Equal(4))
		Expect(out["decode"]).To(Equal(2))
	})
})

var _ = Describe("roleDemandGPUs", func() {
	It("rounds token demand up to whole replicas (ceil)", func() {
		// 8500 tokens / 1000 per-replica capacity = 8.5 -> ceil = 9 replicas (9 GPUs).
		// Guards against a regression to floor (which would under-count demand at 8).
		sat := &domain.AnalyzerResult{
			ModelID:     "A",
			TotalDemand: 8500,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "A-v", AcceleratorName: "A100", PerReplicaCapacity: 1000},
			},
		}
		stateMap := map[string]domain.VariantReplicaState{
			"A-v": {VariantName: "A-v", GPUsPerReplica: 1},
		}
		Expect(roleDemandGPUs(sat, stateMap, "A100", domain.RoleBoth)).To(Equal(9))
	})
})

var _ = Describe("rescaleModelDecisions", func() {
	// applyRescale drops anchor-less requests before grouping, so production never
	// reaches this function with a nil anchor. The guard exists because the anchor
	// is derived per call rather than read from a stored field, and now comes back
	// nil for a stale, non-informative, or ambiguously bound ballot — a far wider
	// set of inputs than the old "entry absent" case. Pin it directly: without the
	// guard the next line dereferences the anchor and panics the optimize goroutine.
	It("returns no decisions instead of panicking when the ballot yields no anchor", func() {
		o := NewGreedyByScoreOptimizer()
		free := 4

		var decisions []domain.VariantDecision
		Expect(func() {
			// Empty ballot → bindingAnchor returns nil.
			decisions = o.rescaleModelDecisions(
				context.Background(),
				ModelScalingRequest{ModelID: "A", Namespace: "ns"},
				"A100", 8, &free,
			)
		}).NotTo(Panic())
		Expect(decisions).To(BeEmpty())
		Expect(free).To(Equal(4), "a request with no anchor must not consume the cycle's free GPUs")
	})
})
