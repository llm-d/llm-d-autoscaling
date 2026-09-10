package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// fakeNodeDiscovery is a hand-written discovery.NodeDiscovery returning a fixed
// node set, mutable between Refresh calls to exercise re-evaluation.
type fakeNodeDiscovery struct {
	nodes map[string]discovery.NodeInfo
	err   error
}

func (f *fakeNodeDiscovery) DiscoverNodes(_ context.Context) (map[string]discovery.NodeInfo, error) {
	return f.nodes, f.err
}

func gpuNode(name string, labelSet map[string]string, model string, count int) discovery.NodeInfo {
	return discovery.NodeInfo{
		Name:         name,
		Labels:       labelSet,
		Accelerators: map[string]discovery.AcceleratorModelInfo{model: {Count: count}},
	}
}

func teamSelector(team string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{"team": team})
}

func decisionIn(namespace, accel string) *domain.VariantDecision {
	return &domain.VariantDecision{Namespace: namespace, AcceleratorName: accel}
}

var _ = Describe("NamespaceInventory", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("builds per-namespace pools by intersecting nodes with selectors", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 4),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
			"ns-dev":  teamSelector("dev"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		alloc := inv.CreateAllocator(ctx)
		// prod can take its full 8 H100s.
		got, err := alloc.TryAllocate(ctx, decisionIn("ns-prod", "H100"), 8)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(8))
		// prod is now exhausted; isolation means dev is untouched.
		got, _ = alloc.TryAllocate(ctx, decisionIn("ns-prod", "H100"), 1)
		Expect(got).To(Equal(0))
		got, _ = alloc.TryAllocate(ctx, decisionIn("ns-dev", "H100"), 4)
		Expect(got).To(Equal(4))
	})

	It("falls back to the default selector for unlisted namespaces", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-A100-PCIE-80GB", 6),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		alloc := inv.CreateAllocator(ctx)
		got, err := alloc.TryAllocate(ctx, decisionIn("some-unlisted-ns", "A100"), 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(4))
	})

	It("denies (zero) when a namespace is unlisted and there is no default", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-A100-PCIE-80GB", 6),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		alloc := inv.CreateAllocator(ctx)
		got, _ := alloc.TryAllocate(ctx, decisionIn("ns-other", "A100"), 4)
		Expect(got).To(Equal(0))
	})

	It("bypasses excluded namespaces with no constraint", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-A100-PCIE-80GB", 2),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, sets.New("kube-system"), map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		alloc := inv.CreateAllocator(ctx)
		// Excluded namespace gets everything it asks for, regardless of pools.
		got, _ := alloc.TryAllocate(ctx, decisionIn("kube-system", "A100"), 100)
		Expect(got).To(Equal(100))
	})

	It("re-evaluates selectors against the current node list on Refresh", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.GetResourcePools()["H100"].Limit).To(Equal(8))

		// A node is added to the prod pool; Refresh must pick it up.
		disc.nodes["n2"] = gpuNode("n2", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 4)
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.GetResourcePools()["H100"].Limit).To(Equal(12))

		// A node is relabeled away from prod; its GPUs leave the pool.
		disc.nodes["n1"] = gpuNode("n1", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 8)
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.GetResourcePools()["H100"].Limit).To(Equal(4))
	})

	It("aggregates GetResourcePools cluster-wide per type for accelerator resolution", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 4),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
			"ns-dev":  teamSelector("dev"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		pools := inv.GetResourcePools()
		// Two buckets, one physical type: a single aggregated pool of 12.
		Expect(pools).To(HaveLen(1))
		Expect(pools["H100"].Limit).To(Equal(12))
		Expect(inv.TotalLimit()).To(Equal(12))
	})

	It("subtracts SetUsedByBucket usage from available capacity", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())
		inv.SetUsedByBucket(map[string]map[string]int{"ns-prod": {"H100": 6}})

		alloc := inv.CreateAllocator(ctx)
		got, _ := alloc.TryAllocate(ctx, decisionIn("ns-prod", "H100"), 8)
		Expect(got).To(Equal(2)) // 8 limit - 6 used
		Expect(inv.TotalUsed()).To(Equal(6))
		Expect(inv.TotalAvailable()).To(Equal(2))
	})

	It("assigns each node to a single bucket when selectors overlap (no double-counting)", func() {
		// One physical node matches both named selectors. Its 8 GPUs must count
		// toward exactly one bucket, never both — otherwise the two namespaces
		// could each allocate the same physical GPUs.
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": {
				Name:         "n1",
				Labels:       map[string]string{"team": "prod", "tier": "gold"},
				Accelerators: map[string]discovery.AcceleratorModelInfo{"NVIDIA-H100-SXM5-80GB": {Count: 8}},
			},
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-a": labels.SelectorFromSet(labels.Set{"team": "prod"}),
			"ns-b": labels.SelectorFromSet(labels.Set{"tier": "gold"}),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		// Counted once, not 16.
		Expect(inv.TotalLimit()).To(Equal(8))
		Expect(inv.GetResourcePools()["H100"].Limit).To(Equal(8))

		// The node deterministically goes to the lexicographically-first bucket
		// (ns-a); ns-b gets an empty pool and cannot allocate.
		alloc := inv.CreateAllocator(ctx)
		gotA, _ := alloc.TryAllocate(ctx, decisionIn("ns-a", "H100"), 8)
		gotB, _ := alloc.TryAllocate(ctx, decisionIn("ns-b", "H100"), 8)
		Expect(gotA + gotB).To(Equal(8))
		Expect(gotB).To(Equal(0))
	})

	It("returns zero for an unresolved accelerator in a heterogeneous cluster", func() {
		disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
			"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			"n2": gpuNode("n2", map[string]string{"team": "prod"}, "NVIDIA-A100-PCIE-80GB", 8),
		}}
		inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
			"ns-prod": teamSelector("prod"),
		})
		Expect(inv.Refresh(ctx)).To(Succeed())

		alloc := inv.CreateAllocator(ctx)
		got, _ := alloc.TryAllocate(ctx, decisionIn("ns-prod", ""), 4)
		Expect(got).To(Equal(0))
	})
})
