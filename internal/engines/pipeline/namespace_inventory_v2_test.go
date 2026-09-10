package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

// nsUsage is a shorthand for the namespace -> accelerator type -> GPUs map that
// the V2 path feeds to SetUsedByNamespace / ComputeConstraints. Its keys define
// the active-namespace set.
type nsUsage map[string]map[string]int

// activeOf returns the active-namespace slice for a usage map, mirroring how
// ComputeConstraints derives it from the map's keys.
func activeOf(u nsUsage) []string {
	out := make([]string, 0, len(u))
	for ns := range u {
		out = append(out, ns)
	}
	return out
}

var _ = Describe("NamespaceInventory V2 constraint path", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	Describe("NamespaceResourcePools closed-allowlist contract", func() {
		It("maps a named bucket 1:1 onto its namespace", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
				"n2": gpuNode("n2", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 4),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
				"ns-dev":  teamSelector("dev"),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-prod": {"H100": 2}, "ns-dev": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools).To(HaveKey("ns-prod"))
			Expect(pools["ns-prod"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 8, Used: 2}))
			Expect(pools["ns-dev"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 0}))
		})

		It("omits an excluded namespace, signalling open rather than deny", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, sets.New("ns-infra"), map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-prod": {}, "ns-infra": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools).NotTo(HaveKey("ns-infra"), "excluded namespaces are omitted (= open)")
			Expect(pools).To(HaveKey("ns-prod"))
		})

		It("materializes a namespace with no selector and no default as deny-all", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-prod": {}, "ns-orphan": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools).To(HaveKey("ns-orphan"), "present marks a closed allowlist")
			Expect(pools["ns-orphan"]).To(BeEmpty(), "empty inner map is a real deny-all")
		})

		It("materializes an active namespace that has a pool but no current usage", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "dev"}, "NVIDIA-H100-SXM5-80GB", 4),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-dev": teamSelector("dev"),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-dev": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools["ns-dev"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 0}))
		})

		It("denies a type the namespace's nodes do not hold by omitting it", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
				"n2": gpuNode("n2", map[string]string{"team": "dev"}, "NVIDIA-A100-SXM4-80GB", 4),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
				"ns-dev":  teamSelector("dev"),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-prod": {}, "ns-dev": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools["ns-prod"]).NotTo(HaveKey("A100"), "absent type = denied, no cluster fall-through")
			Expect(pools["ns-dev"]).NotTo(HaveKey("H100"))
		})

		It("never emits the unlimited sentinel, since physical inventory is finite", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
				"n2": gpuNode("n2", map[string]string{"pool": "shared"}, "NVIDIA-H100-SXM5-80GB", 6),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod":          teamSelector("prod"),
				DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-prod": {}, "ns-a": {}, "ns-b": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			for ns, perType := range pools {
				for t, p := range perType {
					Expect(p.Limit).To(BeNumerically(">=", 0),
						"namespace %s type %s must carry a finite cap", ns, t)
				}
			}
		})
	})

	Describe("shared default bucket", func() {
		// The default bucket is one physical pool shared by every namespace
		// without an explicit selector, so its capacity is partitioned rather
		// than handed to each namespace whole.
		newSharedInventory := func(gpus int) *NamespaceInventory {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-H100-SXM5-80GB", gpus),
			}}
			return NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
			})
		}

		It("splits remaining capacity so the shares cannot overcommit the pool", func() {
			inv := newSharedInventory(8)
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-a": {"H100": 2}, "ns-b": {}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			// 8 total - 2 used = 6 remaining, split evenly.
			Expect(pools["ns-a"]["H100"].Available()).To(Equal(3))
			Expect(pools["ns-b"]["H100"].Available()).To(Equal(3))
			Expect(pools["ns-a"]["H100"].Available()+pools["ns-b"]["H100"].Available()).
				To(Equal(6), "shares sum to exactly the pool's remaining capacity")
			// Each namespace still reports its own usage, so the aggregate is honest.
			Expect(pools["ns-a"]["H100"].Used).To(Equal(2))
			Expect(pools["ns-b"]["H100"].Used).To(Equal(0))
		})

		It("distributes an uneven remainder deterministically", func() {
			inv := newSharedInventory(8)
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-a": {}, "ns-b": {}, "ns-c": {}}
			inv.SetUsedByNamespace(usage)

			first := inv.NamespaceResourcePools(activeOf(usage))
			second := inv.NamespaceResourcePools(activeOf(usage))
			Expect(first).To(Equal(second), "same inputs produce the same split")

			total := 0
			for _, ns := range []string{"ns-a", "ns-b", "ns-c"} {
				total += first[ns]["H100"].Available()
			}
			Expect(total).To(Equal(8), "8 GPUs split 3 ways still sums to 8")
		})

		It("charges an excluded namespace's usage against the pool it draws from", func() {
			// The V1 path charges excluded namespaces so a shared pool cannot hand
			// out GPUs that are already occupied; V2 must match.
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"pool": "shared"}, "NVIDIA-H100-SXM5-80GB", 8),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, sets.New("ns-infra"), map[string]labels.Selector{
				DefaultSelectorKey: labels.SelectorFromSet(labels.Set{"pool": "shared"}),
			})
			Expect(inv.Refresh(ctx)).To(Succeed())

			usage := nsUsage{"ns-a": {}, "ns-infra": {"H100": 5}}
			inv.SetUsedByNamespace(usage)
			pools := inv.NamespaceResourcePools(activeOf(usage))

			Expect(pools).NotTo(HaveKey("ns-infra"), "excluded stays open")
			Expect(pools["ns-a"]["H100"].Available()).To(Equal(3),
				"the excluded namespace's 5 in-use GPUs still debit the shared pool")
		})
	})

	Describe("NamespaceLimiter as a ConstraintProvider", func() {
		It("derives the cluster aggregate from the active namespace pools", func() {
			// ns-idle holds capacity no active namespace can draw from, so it must
			// not inflate the aggregate the optimizer partitions.
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
				"n2": gpuNode("n2", map[string]string{"team": "idle"}, "NVIDIA-H100-SXM5-80GB", 99),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
				"ns-idle": teamSelector("idle"),
			})
			lim := NewNamespaceLimiter(inv, NewGreedyBySaturation())

			rc, err := lim.ComputeConstraints(ctx, map[string]int{"H100": 2}, nsUsage{"ns-prod": {"H100": 2}})
			Expect(err).NotTo(HaveOccurred())

			Expect(rc.NamespacePools).To(HaveKey("ns-prod"))
			Expect(rc.NamespacePools).NotTo(HaveKey("ns-idle"), "not active this cycle")
			Expect(rc.Pools).To(HaveKeyWithValue("H100", ResourcePool{Limit: 8, Used: 2}),
				"aggregate is the active namespace's pool, not the static 107-GPU cluster sum")
			Expect(rc.TotalLimit).To(Equal(8))
			Expect(rc.TotalAvail).To(Equal(6))
		})

		It("reports the full cluster aggregate when every active namespace is excluded", func() {
			// With nothing to partition this limiter constrains nothing, so the
			// binding limit really is physical cluster capacity.
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, sets.New("ns-infra"), map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
			})
			lim := NewNamespaceLimiter(inv, NewGreedyBySaturation())

			rc, err := lim.ComputeConstraints(ctx, nil, nsUsage{"ns-infra": {}})
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.NamespacePools).To(BeEmpty())
			Expect(rc.Pools).To(HaveKeyWithValue("H100", ResourcePool{Limit: 8, Used: 0}))
		})

		It("propagates a discovery failure instead of reporting empty capacity", func() {
			disc := &fakeNodeDiscovery{err: context.DeadlineExceeded}
			inv := NewNamespaceInventory("ns-inv", disc, nil, map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
			})
			lim := NewNamespaceLimiter(inv, NewGreedyBySaturation())

			_, err := lim.ComputeConstraints(ctx, nil, nsUsage{"ns-prod": {}})
			Expect(err).To(HaveOccurred())
		})

		It("keeps V1 and V2 agreeing on which namespaces are constrained", func() {
			disc := &fakeNodeDiscovery{nodes: map[string]discovery.NodeInfo{
				"n1": gpuNode("n1", map[string]string{"team": "prod"}, "NVIDIA-H100-SXM5-80GB", 8),
			}}
			inv := NewNamespaceInventory("ns-inv", disc, sets.New("ns-infra"), map[string]labels.Selector{
				"ns-prod": teamSelector("prod"),
			})
			lim := NewNamespaceLimiter(inv, NewGreedyBySaturation())

			rc, err := lim.ComputeConstraints(ctx, nil, nsUsage{
				"ns-prod": {}, "ns-infra": {}, "ns-orphan": {},
			})
			Expect(err).NotTo(HaveOccurred())

			// V1: excluded passes through, orphan gets nothing, prod is capped.
			Expect(rc.NamespacePools).NotTo(HaveKey("ns-infra"))
			Expect(rc.NamespacePools["ns-orphan"]).To(BeEmpty())
			Expect(rc.NamespacePools["ns-prod"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 8, Used: 0}))
		})
	})
})
