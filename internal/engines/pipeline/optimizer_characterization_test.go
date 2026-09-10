package pipeline

// Shared harness for the optimizer characterization goldens.
//
// This file used to hold the sat-only goldens themselves: a freeze suite that
// captured main's single-analyzer decisions as literals, written to prove the
// anchor refactor changed nothing on the one-voter path and to ride onto this
// branch as its ship gate. It did that job. The goldens are REMOVED here rather
// than left on main, because they were scoped to a specific refactor and were
// never meant to become a permanent optimizer contract -- freezing today's
// allocation arithmetic forever would make ordinary future tuning look like a
// regression. What replaces them is not less coverage but strictly more:
// optimizer_multivote_characterization_test.go asserts every one of those
// scenarios across three ballot shapes, and its [sat]-only shape is the same
// ballot asserted through this same harness. The removal commit maps each
// scenario individually.
//
// What stays here is the harness, because it is shared: the multi-vote goldens
// and optimizer_combine_characterization_test.go both build on it. The filename
// is deliberately unchanged so the removal reads as a deletion in one place
// rather than as a rename.

import (
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// goldenDecision is the subset of domain.VariantDecision fields the anchor
// refactor repoints and this branch freezes: target replica count,
// RequiredCapacity, SpareCapacity, and Utilization.
type goldenDecision struct {
	Replicas         int
	RequiredCapacity float64
	SpareCapacity    float64
	Utilization      float64
}

// expectDecisionSet asserts got matches want as a SET keyed by VariantName,
// never by slice order or slice equality: Optimize's per-decision content is
// deterministic but its output slice order is not (map iteration in
// buildDecisionsWithOptimizer, unstable sort in sortByRemainingDesc).
func expectDecisionSet(got []domain.VariantDecision, want map[string]goldenDecision) {
	gm := make(map[string]domain.VariantDecision, len(got))
	gotNames := make([]string, 0, len(got))
	for _, d := range got {
		gm[d.VariantName] = d
		gotNames = append(gotNames, d.VariantName)
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	Expect(gotNames).To(ConsistOf(wantNames), "decision-set variant names must match the golden")

	for name, w := range want {
		d := gm[name]
		Expect(d.TargetReplicas).To(Equal(w.Replicas), "variant %q: TargetReplicas", name)
		Expect(d.RequiredCapacity).To(BeNumerically("~", w.RequiredCapacity, 1e-9), "variant %q: RequiredCapacity", name)
		Expect(d.SpareCapacity).To(BeNumerically("~", w.SpareCapacity, 1e-9), "variant %q: SpareCapacity", name)
		Expect(d.Utilization).To(BeNumerically("~", w.Utilization, 1e-9), "variant %q: Utilization", name)
	}
}

// unlimitedConstraints emulates "no GPU limit" for GreedyByScoreOptimizer,
// which treats absent/empty constraints as zero (deny), not unlimited —
// mirrors the pattern in optimizer_equivalence_test.go.
func unlimitedConstraints(types ...string) []*ResourceConstraints {
	pools := map[string]ResourcePool{}
	for _, t := range types {
		pools[t] = ResourcePool{Limit: 1_000_000}
	}
	return []*ResourceConstraints{{Pools: pools}}
}
