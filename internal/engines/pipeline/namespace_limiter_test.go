package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// scaleUpDecision builds a scale-up VariantDecision (one GPU per replica).
func scaleUpDecision(namespace, accel string, current, target int) *domain.VariantDecision {
	return &domain.VariantDecision{
		VariantName:     namespace + "-variant",
		Namespace:       namespace,
		AcceleratorName: accel,
		CurrentReplicas: current,
		TargetReplicas:  target,
		GPUsPerReplica:  1,
	}
}

var _ = Describe("NamespaceLimiter", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	newLimiter := func(disc discovery.NodeDiscovery, exclude sets.Set[string], selectors map[string]labels.Selector) *NamespaceLimiter {
		inv := NewNamespaceInventory("ns-inv", disc, exclude, selectors)
		return NewNamespaceLimiter(inv, NewGreedyBySaturation())
	}

	It("caps a scale-up at the namespace pool and records the limiter", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{"ns-prod": teamSelector("prod")})

		d := scaleUpDecision("ns-prod", "H100", 2, 12) // wants +10, pool has 8 (2 used)
		Expect(l.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())

		Expect(d.TargetReplicas).To(Equal(8)) // 2 current + 6 remaining
		Expect(d.WasLimited).To(BeTrue())
		Expect(d.LimitedBy).To(Equal(NamespaceLimiterName))
	})

	It("isolates pools: one namespace's exhaustion does not consume another's", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 4),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
			"ns-dev":  teamSelector("dev"),
		})

		prod := scaleUpDecision("ns-prod", "H100", 2, 100)
		dev := scaleUpDecision("ns-dev", "H100", 1, 100)
		Expect(l.Limit(ctx, []*domain.VariantDecision{prod, dev})).To(Succeed())

		Expect(prod.TargetReplicas).To(Equal(8)) // 2 + (8-2)
		Expect(dev.TargetReplicas).To(Equal(4))  // 1 + (4-1)
	})

	It("aggregates default-fallback usage so the shared pool is not over-allocated", func() {
		// Both namespaces fall to the default bucket; their existing usage must
		// jointly debit the shared pool (regression guard for default-bucket
		// over-allocation).
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-A100-PCIE-80GB", 10),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{
			DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
		})

		a := scaleUpDecision("ns-a", "A100", 3, 20) // 3 used
		b := scaleUpDecision("ns-b", "A100", 4, 20) // 4 used; default used total = 7
		Expect(l.Limit(ctx, []*domain.VariantDecision{a, b})).To(Succeed())

		newA := a.TargetReplicas - 3
		newB := b.TargetReplicas - 4
		// Only 10 - 7 = 3 GPUs remain in the shared pool for new replicas.
		Expect(newA + newB).To(Equal(3))
	})

	It("does not constrain excluded namespaces", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-A100-PCIE-80GB", 2),
		}}
		l := newLimiter(disc, sets.New("kube-system"), map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})

		d := scaleUpDecision("kube-system", "A100", 0, 50)
		Expect(l.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())
		Expect(d.TargetReplicas).To(Equal(50))
		Expect(d.WasLimited).To(BeFalse())
	})

	It("charges excluded-namespace usage against the shared pool so it is not overcommitted", func() {
		// An excluded namespace bypasses the cap, but its running replicas still
		// occupy GPUs in the shared default pool; that usage must be subtracted
		// so a default-pool tenant cannot be granted GPUs that are physically
		// gone (regression guard for excluded-namespace overcommit).
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-A100-PCIE-80GB", 10),
		}}
		l := newLimiter(disc, sets.New("kube-system"), map[string]labels.Selector{
			DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
		})

		// Excluded namespace running 6 GPUs on the shared pool (not capped, but charged).
		excluded := &domain.VariantDecision{
			VariantName: "kube-system-x", Namespace: "kube-system", AcceleratorName: "A100",
			CurrentReplicas: 6, TargetReplicas: 6, GPUsPerReplica: 1,
		}
		tenant := scaleUpDecision("ns-a", "A100", 0, 20) // default-pool tenant wants +20
		Expect(l.Limit(ctx, []*domain.VariantDecision{excluded, tenant})).To(Succeed())

		// 10 pool - 6 charged to the excluded namespace = 4 left for the tenant.
		Expect(tenant.TargetReplicas).To(Equal(4))
		Expect(tenant.WasLimited).To(BeTrue())
	})

	It("charges an excluded namespace whose accelerator arrives unresolved", func() {
		// Same overcommit, reached through the unresolved-accelerator path:
		// accelerator resolution must run for excluded namespaces too, otherwise
		// the decision stays unresolved, usage accounting drops it, and its GPUs
		// are never charged against the shared pool.
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-A100-PCIE-80GB", 10),
		}}
		l := newLimiter(disc, sets.New("kube-system"), map[string]labels.Selector{
			DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
		})

		excluded := &domain.VariantDecision{
			VariantName: "kube-system-x", Namespace: "kube-system",
			AcceleratorName: constants.DefaultAcceleratorName, // unresolved
			CurrentReplicas: 6, TargetReplicas: 6, GPUsPerReplica: 1,
		}
		tenant := scaleUpDecision("ns-a", "A100", 0, 20)
		Expect(l.Limit(ctx, []*domain.VariantDecision{excluded, tenant})).To(Succeed())

		Expect(excluded.AcceleratorName).To(Equal("A100"),
			"excluded namespaces are resolved so their usage can be charged")
		Expect(tenant.TargetReplicas).To(Equal(4),
			"the excluded namespace's 6 GPUs still debit the shared pool")
		Expect(tenant.WasLimited).To(BeTrue())
	})

	It("charges an excluded namespace that grows within the same batch", func() {
		// Usage accounting only charges pre-cycle CurrentReplicas, so an excluded
		// namespace scaling up alongside a capped tenant on the same pool would
		// otherwise let both take the same GPUs for one cycle.
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-A100-PCIE-80GB", 10),
		}}
		l := newLimiter(disc, sets.New("kube-system"), map[string]labels.Selector{
			DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
		})

		// Excluded namespace grows 0 -> 6 in this very batch (not pre-existing).
		excluded := scaleUpDecision("kube-system", "A100", 0, 6)
		tenant := scaleUpDecision("ns-a", "A100", 0, 20)
		Expect(l.Limit(ctx, []*domain.VariantDecision{excluded, tenant})).To(Succeed())

		Expect(excluded.TargetReplicas).To(Equal(6), "excluded namespaces are never capped")
		Expect(tenant.TargetReplicas).To(Equal(4),
			"the excluded namespace's 6 new GPUs must debit the shared pool")
		Expect(excluded.TargetReplicas+tenant.TargetReplicas).To(Equal(10),
			"combined allocation never exceeds the pool")
	})

	It("denies scale-up for an unlisted namespace when no default exists", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-A100-PCIE-80GB", 8),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{"ns-prod": teamSelector("prod")})

		d := scaleUpDecision("ns-orphan", "A100", 1, 10)
		Expect(l.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())
		Expect(d.TargetReplicas).To(Equal(1)) // no pool → no new replicas
		Expect(d.WasLimited).To(BeTrue())
	})

	It("resolves unknown accelerators per bucket so heterogeneous clusters don't over-allocate", func() {
		// Cluster is heterogeneous overall (H100 + A100), but the team-a bucket
		// is single-type (H100). An unresolved accelerator in team-a must resolve
		// to H100 so its existing replicas debit the H100 pool; otherwise a
		// sibling scale-up would be granted GPUs already in use.
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "a"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "b"}, "NVIDIA-A100-PCIE-80GB", 8),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{
			"ns-a": teamSelector("a"),
			"ns-b": teamSelector("b"),
		})

		// Unresolved accelerator, 6 replicas already running (no scale-up).
		running := &domain.VariantDecision{
			VariantName: "ns-a-running", Namespace: "ns-a", AcceleratorName: "",
			CurrentReplicas: 6, TargetReplicas: 6, GPUsPerReplica: 1,
		}
		grow := scaleUpDecision("ns-a", "H100", 0, 10)
		Expect(l.Limit(ctx, []*domain.VariantDecision{running, grow})).To(Succeed())

		Expect(running.AcceleratorName).To(Equal("H100")) // resolved per bucket
		// 8 H100 - 6 already used = 2 available for the sibling scale-up.
		Expect(grow.TargetReplicas).To(Equal(2))
		Expect(grow.WasLimited).To(BeTrue())
	})

	It("skips unresolved-accelerator replicas in a multi-type bucket and denies them new GPUs", func() {
		// team-a's pool spans two types, so an "unknown"-accelerator variant
		// cannot be attributed to a type: it is skipped in usage accounting (no
		// phantom pool entry) and gets no new allocation. A resolved sibling
		// still allocates from its own type pool.
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "a"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "a"}, "NVIDIA-A100-PCIE-80GB", 8),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{"ns-a": teamSelector("a")})

		unknown := &domain.VariantDecision{
			VariantName: "ns-a-unknown", Namespace: "ns-a", AcceleratorName: constants.DefaultAcceleratorName,
			CurrentReplicas: 2, TargetReplicas: 6, GPUsPerReplica: 1,
		}
		typed := scaleUpDecision("ns-a", "H100", 0, 4)
		Expect(l.Limit(ctx, []*domain.VariantDecision{unknown, typed})).To(Succeed())

		Expect(unknown.TargetReplicas).To(Equal(2)) // denied: cannot pick a pool
		Expect(unknown.WasLimited).To(BeTrue())
		Expect(typed.TargetReplicas).To(Equal(4)) // resolved sibling allocates from H100 pool
	})

	It("resolves an unknown accelerator name in a homogeneous cluster", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
		}}
		l := newLimiter(disc, nil, map[string]labels.Selector{"ns-prod": teamSelector("prod")})

		d := scaleUpDecision("ns-prod", "", 0, 4) // unresolved accelerator
		Expect(l.Limit(ctx, []*domain.VariantDecision{d})).To(Succeed())
		Expect(d.AcceleratorName).To(Equal("H100"))
		Expect(d.TargetReplicas).To(Equal(4))
	})
})
