package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// namespaceAwareMockInventory extends mockInventory with SetUsedByNamespace
// and records every invocation so the Ginkgo spec can verify the
// DefaultLimiter feature-detection path.
type namespaceAwareMockInventory struct {
	mockInventory
	usedByNS                map[string]map[string]int
	setUsedByNamespaceCalls int
	nsPools                 map[string]map[string]ResourcePool // returned by NamespaceResourcePools
	lastActiveNamespaces    []string                           // arg captured from the last NamespaceResourcePools call
}

func newNamespaceAwareMockInventory(limitByType map[string]int) *namespaceAwareMockInventory {
	return &namespaceAwareMockInventory{
		mockInventory: mockInventory{
			name:        "nai",
			limitByType: limitByType,
			usedByType:  make(map[string]int),
		},
	}
}

func (m *namespaceAwareMockInventory) SetUsedByNamespace(usedByNS map[string]map[string]int) {
	m.setUsedByNamespaceCalls++
	m.usedByNS = usedByNS
}

func (m *namespaceAwareMockInventory) NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool {
	m.lastActiveNamespaces = activeNamespaces
	return m.nsPools
}

// Compile-time check that the mock satisfies NamespaceAwareInventory.
var _ NamespaceAwareInventory = (*namespaceAwareMockInventory)(nil)

var _ = Describe("DefaultLimiter namespace-aware feature detection", func() {
	var (
		ctx       context.Context
		algorithm *mockAlgorithm
	)

	BeforeEach(func() {
		ctx = context.Background()
		algorithm = &mockAlgorithm{name: "noop"}
	})

	It("calls SetUsedByNamespace on inventories that implement NamespaceAwareInventory", func() {
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 100})
		limiter := NewDefaultLimiter("test", inv, algorithm)

		decisions := []*domain.VariantDecision{
			{VariantName: "v1", Namespace: "team-a", AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 4},
			{VariantName: "v2", Namespace: "team-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 2},
			{VariantName: "v3", Namespace: "team-b", AcceleratorName: "A100", CurrentReplicas: 3, GPUsPerReplica: 1},
		}
		Expect(limiter.Limit(ctx, decisions)).To(Succeed())

		Expect(inv.setUsedByNamespaceCalls).To(Equal(1))
		Expect(inv.usedByNS).To(HaveKey("team-a"))
		Expect(inv.usedByNS["team-a"]).To(HaveKeyWithValue("H100", 10), "2*4 + 1*2 = 10")
		Expect(inv.usedByNS).To(HaveKey("team-b"))
		Expect(inv.usedByNS["team-b"]).To(HaveKeyWithValue("A100", 3), "3*1 = 3")
	})

	It("skips decisions with empty namespace or accelerator in the namespace bucketing", func() {
		// Heterogeneous inventory (>1 type) so resolveUnknownAccelerators cannot
		// auto-resolve the empty-accelerator decision to a single type — it stays
		// unresolved and must be skipped by the namespace bucketing.
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 100, "A100": 100})
		limiter := NewDefaultLimiter("test", inv, algorithm)

		decisions := []*domain.VariantDecision{
			{Namespace: "team-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			{Namespace: "", AcceleratorName: "H100", CurrentReplicas: 99, GPUsPerReplica: 99},   // skipped (empty ns)
			{Namespace: "team-a", AcceleratorName: "", CurrentReplicas: 99, GPUsPerReplica: 99}, // skipped (empty type, unresolvable)
		}
		Expect(limiter.Limit(ctx, decisions)).To(Succeed())

		Expect(inv.usedByNS).To(HaveLen(1))
		Expect(inv.usedByNS["team-a"]).To(HaveKeyWithValue("H100", 1))
	})

	It("does NOT call SetUsedByNamespace on plain Inventory implementations", func() {
		// The existing mockInventory does not implement NamespaceAwareInventory.
		inv := newMockInventory("plain", map[string]int{"H100": 100})
		limiter := NewDefaultLimiter("test", inv, algorithm)

		decisions := []*domain.VariantDecision{
			{Namespace: "team-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		}
		Expect(limiter.Limit(ctx, decisions)).To(Succeed())
		// usedByType is still populated by SetUsed (verified indirectly by inv.TotalUsed).
		Expect(inv.TotalUsed()).To(Equal(1))
	})

	It("ComputeConstraints feeds namespace usage and exposes NamespacePools", func() {
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 10})
		inv.nsPools = map[string]map[string]ResourcePool{"team-a": {"H100": {Limit: 4, Used: 1}}}
		limiter := NewDefaultLimiter("test", inv, algorithm)

		rc, err := limiter.ComputeConstraints(ctx,
			map[string]int{"H100": 3},
			map[string]map[string]int{"team-a": {"H100": 1}})
		Expect(err).NotTo(HaveOccurred())
		Expect(inv.setUsedByNamespaceCalls).To(Equal(1), "per-namespace usage fed to the inventory")
		Expect(rc.NamespacePools["team-a"]).To(HaveKeyWithValue("H100", ResourcePool{Limit: 4, Used: 1}))
		Expect(inv.lastActiveNamespaces).To(ConsistOf("team-a"),
			"active-namespace set is derived from the keys of usageByNamespace")
	})

	It("ComputeConstraints derives the active-namespace set from usageByNamespace keys", func() {
		// The configured inventory knows about team-a and team-b, but only
		// team-a has current usage; team-b must NOT be queried/materialized.
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 10})
		inv.nsPools = map[string]map[string]ResourcePool{"team-a": {"H100": {Limit: 4}}}
		limiter := NewDefaultLimiter("test", inv, algorithm)

		_, err := limiter.ComputeConstraints(ctx,
			map[string]int{"H100": 1},
			map[string]map[string]int{"team-a": {"H100": 1}})
		Expect(err).NotTo(HaveOccurred())
		Expect(inv.lastActiveNamespaces).To(ConsistOf("team-a"))
		Expect(inv.lastActiveNamespaces).NotTo(ContainElement("team-b"),
			"a namespace absent from usageByNamespace is not in the active set")
	})

	It("ComputeConstraints derives the cluster aggregate from the active namespace pools", func() {
		// GetResourcePools would report H100:999, but with namespace caps present
		// the aggregate must be the sum across active namespaces (incl. a default
		// fall-through namespace like team-z), not the static per-type value.
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 999})
		inv.nsPools = map[string]map[string]ResourcePool{
			"team-a": {"H100": {Limit: 4, Used: 1}},
			"team-z": {"H100": {Limit: 2, Used: 0}},
		}
		limiter := NewDefaultLimiter("test", inv, algorithm)

		rc, err := limiter.ComputeConstraints(ctx,
			map[string]int{"H100": 1},
			map[string]map[string]int{"team-a": {"H100": 1}, "team-z": {}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.Pools).To(HaveKeyWithValue("H100", ResourcePool{Limit: 6, Used: 1}), "aggregate of team-a(4)+team-z(2), not GetResourcePools(999)")
		Expect(rc.TotalLimit).To(Equal(6))
		Expect(rc.NamespacePools).To(HaveKey("team-z"))
	})

	It("ComputeConstraints falls back to GetResourcePools when no namespace pools are produced", func() {
		// e.g. a cluster-scoped quota, or an active set that is entirely excluded:
		// NamespaceResourcePools returns empty, so Pools/Totals stay the static
		// per-type GetResourcePools result and NamespacePools is nil.
		inv := newNamespaceAwareMockInventory(map[string]int{"H100": 8})
		inv.nsPools = nil // NamespaceResourcePools returns empty
		limiter := NewDefaultLimiter("test", inv, algorithm)

		rc, err := limiter.ComputeConstraints(ctx,
			map[string]int{"H100": 2},
			map[string]map[string]int{"team-a": {"H100": 2}})
		Expect(err).NotTo(HaveOccurred())
		Expect(inv.setUsedByNamespaceCalls).To(Equal(1), "still namespace-aware: usage is fed")
		Expect(rc.NamespacePools).To(BeNil(), "no namespace caps materialized")
		Expect(rc.Pools).To(HaveKeyWithValue("H100", ResourcePool{Limit: 8, Used: 2}), "static GetResourcePools fallback")
	})
})
