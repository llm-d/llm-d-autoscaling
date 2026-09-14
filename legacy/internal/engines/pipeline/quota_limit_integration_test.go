package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// These specs exercise the V1 Limit() enforcement path end-to-end: a real
// QuotaInventory driven by the real GreedyBySaturation through DefaultLimiter
// (and CompositeLimiter), asserting that a quota actually reduces a decision's
// TargetReplicas. The per-allocator and per-optimizer behaviors are covered
// elsewhere; this fills the composition gap where a regression in the wiring
// (SetUsed(ByNamespace) → CreateAllocator → algorithm → updateDecisionMetadata)
// would otherwise pass unnoticed.
var _ = Describe("Quota limiter V1 enforcement (integration)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newDecision := func() *domain.VariantDecision {
		return &domain.VariantDecision{
			VariantName:     "v1",
			Namespace:       "team-a",
			AcceleratorName: "H100",
			CurrentReplicas: 2, // 2 * 2 = 4 GPUs already used
			TargetReplicas:  5, // wants +3 replicas = +6 GPUs
			GPUsPerReplica:  2,
		}
	}

	clusterLimiter := func(name string, quota int) *DefaultLimiter {
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": quota},
		})
		return NewDefaultLimiter(name, inv, NewGreedyBySaturation())
	}
	namespaceLimiter := func(name string, quota int) *DefaultLimiter {
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": quota}},
		})
		return NewDefaultLimiter(name, inv, NewGreedyBySaturation())
	}

	It("caps TargetReplicas at the cluster quota headroom", func() {
		d := newDecision()
		// cap 6, used 4 → 2 free = 1 replica; target lands at 2 + 1 = 3.
		Expect(clusterLimiter("cluster-quota", 6).Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(3), "capped at cluster quota headroom")
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("cluster-quota"))
	})

	It("caps TargetReplicas at the namespace quota headroom", func() {
		d := newDecision()
		Expect(namespaceLimiter("namespace-quota", 6).Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(3), "capped at namespace quota headroom")
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("namespace-quota"))
	})

	It("applies the most restrictive of a cluster+namespace composite", func() {
		// cluster is ample (100); namespace (6) binds. Composite runs cluster then
		// namespace against the shared slice.
		comp := NewCompositeLimiter("quota-limiter", []Limiter{
			clusterLimiter("cluster-quota", 100),
			namespaceLimiter("namespace-quota", 6),
		})
		d := newDecision()
		Expect(comp.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(3), "namespace quota is the binding constraint")
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("namespace-quota"),
			"attribution goes to the constituent that actually reduced the target")
	})

	It("attributes a quota cap even when a MinReplicas floor keeps the target from dropping", func() {
		// The quota denies GPUs (only +1 of the +3 requested replicas is
		// schedulable) but a MinReplicas floor restores the target to the
		// requested value. WasLimited still transitions to true, so the cap must
		// be attributed (LimitedBy set, step marked limited) — the before/after
		// target alone would miss this.
		inv := NewQuotaInventory(config.QuotaLimiterConfig{
			Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 6}, // 6 - 4 used = +1 replica
		})
		limiter := NewDefaultLimiter("cluster-quota", inv, NewGreedyBySaturation())

		minReplicas := 5
		d := newDecision()
		d.MinReplicas = &minReplicas
		Expect(limiter.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(5), "MinReplicas floor restores the requested target")
		Expect(d.WasLimited).To(BeTrue(), "the quota still denied GPUs")
		Expect(d.LimitedBy).To(Equal("cluster-quota"), "the cap is attributed despite the floor")
	})

	It("does not let a later non-capping constituent steal LimitedBy", func() {
		// Regression for the sticky-WasLimited misattribution: the namespace
		// quota caps first; the ample cluster quota runs AFTER and must not
		// overwrite LimitedBy (nor re-emit the limited metric) just because
		// WasLimited stays true across constituents.
		comp := NewCompositeLimiter("quota-limiter", []Limiter{
			namespaceLimiter("namespace-quota", 6),
			clusterLimiter("cluster-quota", 100),
		})
		d := newDecision()
		Expect(comp.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(3))
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal("namespace-quota"),
			"the constituent that actually capped keeps attribution even though cluster-quota ran last")
	})
})
