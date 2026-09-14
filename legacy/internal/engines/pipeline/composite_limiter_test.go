package pipeline

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// recordingLimiter is a test double that records each invocation and can
// be configured to return an error or to mutate decisions deterministically.
type recordingLimiter struct {
	name        string
	err         error
	cap         int // if > 0, every decision's TargetReplicas is clamped to this value
	invocations int
}

func (r *recordingLimiter) Name() string { return r.name }
func (r *recordingLimiter) Limit(_ context.Context, decisions []*domain.VariantDecision) error {
	r.invocations++
	if r.err != nil {
		return r.err
	}
	if r.cap > 0 {
		for _, d := range decisions {
			if d.TargetReplicas > r.cap {
				d.TargetReplicas = r.cap
				d.WasLimited = true
			}
		}
	}
	return nil
}

var _ = Describe("CompositeLimiter", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("satisfies the Limiter interface", func() {
		var _ Limiter = (*CompositeLimiter)(nil)
	})

	It("returns its configured name", func() {
		c := NewCompositeLimiter("my-composite", nil)
		Expect(c.Name()).To(Equal("my-composite"))
	})

	It("is a no-op when given no decisions", func() {
		a := &recordingLimiter{name: "a"}
		c := NewCompositeLimiter("composite", []Limiter{a})
		Expect(c.Limit(ctx, nil)).To(Succeed())
		Expect(a.invocations).To(Equal(0))
	})

	It("is a no-op when given no constituents", func() {
		c := NewCompositeLimiter("composite", nil)
		decisions := []*domain.VariantDecision{{TargetReplicas: 5}}
		Expect(c.Limit(ctx, decisions)).To(Succeed())
		Expect(decisions[0].TargetReplicas).To(Equal(5))
	})

	It("invokes every constituent in order", func() {
		a := &recordingLimiter{name: "a"}
		b := &recordingLimiter{name: "b"}
		c := &recordingLimiter{name: "c"}
		comp := NewCompositeLimiter("composite", []Limiter{a, b, c})

		decisions := []*domain.VariantDecision{{TargetReplicas: 10}}
		Expect(comp.Limit(ctx, decisions)).To(Succeed())

		Expect(a.invocations).To(Equal(1))
		Expect(b.invocations).To(Equal(1))
		Expect(c.invocations).To(Equal(1))
	})

	It("propagates the most restrictive cap via the shared decisions slice", func() {
		// First constituent clamps to 4; second clamps to 2. The shared slice
		// ends at the tighter cap because each constituent reads & writes
		// TargetReplicas in place.
		a := &recordingLimiter{name: "cluster-quota", cap: 4}
		b := &recordingLimiter{name: "namespace-quota", cap: 2}
		comp := NewCompositeLimiter("composite", []Limiter{a, b})

		decisions := []*domain.VariantDecision{{TargetReplicas: 10}}
		Expect(comp.Limit(ctx, decisions)).To(Succeed())
		Expect(decisions[0].TargetReplicas).To(Equal(2))
		Expect(decisions[0].WasLimited).To(BeTrue())
	})

	It("stops at the first constituent that errors", func() {
		a := &recordingLimiter{name: "a"}
		b := &recordingLimiter{name: "b", err: errors.New("boom")}
		c := &recordingLimiter{name: "c"}
		comp := NewCompositeLimiter("composite", []Limiter{a, b, c})

		err := comp.Limit(ctx, []*domain.VariantDecision{{TargetReplicas: 1}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`constituent[1] ("b")`))
		Expect(err.Error()).To(ContainSubstring("boom"))

		// a ran, b errored, c never executed.
		Expect(a.invocations).To(Equal(1))
		Expect(b.invocations).To(Equal(1))
		Expect(c.invocations).To(Equal(0))
	})

	It("preserves mutations from earlier constituents when a later one errors", func() {
		a := &recordingLimiter{name: "a", cap: 4} // caps TargetReplicas to 4
		b := &recordingLimiter{name: "b", err: errors.New("boom")}
		comp := NewCompositeLimiter("composite", []Limiter{a, b})

		decisions := []*domain.VariantDecision{{TargetReplicas: 10}}
		err := comp.Limit(ctx, decisions)
		Expect(err).To(HaveOccurred())

		// a's cap must survive b's error: CompositeLimiter does not roll back
		// earlier constituents' mutations (the contract — an error aborts the
		// optimize cycle, so the partial mutation is benign).
		Expect(decisions[0].TargetReplicas).To(Equal(4),
			"earlier constituent's cap must persist after a later constituent errors")
	})

	It("exposes its constituents in declaration order", func() {
		a := &recordingLimiter{name: "a"}
		b := &recordingLimiter{name: "b"}
		comp := NewCompositeLimiter("composite", []Limiter{a, b})
		got := comp.Constituents()
		Expect(got).To(HaveLen(2))
		Expect(got[0].Name()).To(Equal("a"))
		Expect(got[1].Name()).To(Equal("b"))
	})
})
