package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// mockDiscovery implements discovery.CapacityDiscovery for testing.
type mockDiscovery struct {
	inventory map[string]map[string]discovery.AcceleratorModelInfo
	err       error
}

func (m *mockDiscovery) Discover(ctx context.Context) (map[string]map[string]discovery.AcceleratorModelInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.inventory, nil
}

// mockFullDiscovery implements discovery.FullDiscovery for testing.
type mockFullDiscovery struct {
	inventory map[string]map[string]discovery.AcceleratorModelInfo
	usage     map[string]int
	discErr   error
	usageErr  error
}

func (m *mockFullDiscovery) Discover(ctx context.Context) (map[string]map[string]discovery.AcceleratorModelInfo, error) {
	if m.discErr != nil {
		return nil, m.discErr
	}
	return m.inventory, nil
}

func (m *mockFullDiscovery) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	if m.usageErr != nil {
		return nil, m.usageErr
	}
	return m.usage, nil
}

// DiscoverNodes is required by FullDiscovery. TypeInventory doesn't use per-node
// info, so this mock returns an empty map.
func (m *mockFullDiscovery) DiscoverNodes(ctx context.Context) (map[string]discovery.NodeInfo, error) {
	if m.discErr != nil {
		return nil, m.discErr
	}
	return map[string]discovery.NodeInfo{}, nil
}

var _ = Describe("TypeInventory", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Refresh", func() {
		DescribeTable("should aggregate GPU capacity from nodes",
			func(nodeInventory map[string]map[string]discovery.AcceleratorModelInfo, expectedLimits map[string]int, expectedTotal int) {
				disc := &mockDiscovery{inventory: nodeInventory}
				inv := NewTypeInventory("test", disc)

				err := inv.Refresh(ctx)
				Expect(err).NotTo(HaveOccurred())

				Expect(inv.TotalLimit()).To(Equal(expectedTotal))

				for accType, expected := range expectedLimits {
					Expect(inv.LimitByType(accType)).To(Equal(expected))
				}

				// With no usage set, available should equal limits
				Expect(inv.TotalAvailable()).To(Equal(expectedTotal))
				for accType, expected := range expectedLimits {
					Expect(inv.AvailableByType(accType)).To(Equal(expected))
				}
			},
			Entry("single node single type",
				map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
				},
				map[string]int{"H100": 8},
				8,
			),
			Entry("single node multiple types",
				map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {
						"H100": {Count: 4, Memory: "80GB"},
						"A100": {Count: 4, Memory: "40GB"},
					},
				},
				map[string]int{"H100": 4, "A100": 4},
				8,
			),
			Entry("multiple nodes same type",
				map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					"node-2": {"H100": {Count: 8, Memory: "80GB"}},
				},
				map[string]int{"H100": 16},
				16,
			),
			Entry("heterogeneous cluster",
				map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					"node-2": {"A100": {Count: 8, Memory: "40GB"}},
					"node-3": {"L40S": {Count: 4, Memory: "48GB"}},
				},
				map[string]int{"H100": 8, "A100": 8, "L40S": 4},
				20,
			),
			Entry("empty cluster",
				map[string]map[string]discovery.AcceleratorModelInfo{},
				map[string]int{},
				0,
			),
		)
	})

	Describe("SetUsed", func() {
		It("should track GPU usage and update available capacity", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 16, Memory: "80GB"}},
					"node-2": {"A100": {Count: 8, Memory: "40GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Initially: limit=24, used=0, available=24
			Expect(inv.TotalLimit()).To(Equal(24))
			Expect(inv.TotalUsed()).To(Equal(0))
			Expect(inv.TotalAvailable()).To(Equal(24))

			// Set some usage
			inv.SetUsed(map[string]int{"H100": 4, "A100": 2})

			// Now: limit=24, used=6, available=18
			Expect(inv.TotalLimit()).To(Equal(24))
			Expect(inv.TotalUsed()).To(Equal(6))
			Expect(inv.TotalAvailable()).To(Equal(18))

			// Per-type checks
			Expect(inv.LimitByType("H100")).To(Equal(16))
			Expect(inv.UsedByType("H100")).To(Equal(4))
			Expect(inv.AvailableByType("H100")).To(Equal(12))

			Expect(inv.LimitByType("A100")).To(Equal(8))
			Expect(inv.UsedByType("A100")).To(Equal(2))
			Expect(inv.AvailableByType("A100")).To(Equal(6))
		})
	})

	Describe("OverAllocation", func() {
		It("should handle usage exceeding limits gracefully", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Set usage greater than limit (shouldn't happen but handle gracefully)
			inv.SetUsed(map[string]int{"H100": 12})

			// Available should be 0, not negative
			Expect(inv.TotalLimit()).To(Equal(8))
			Expect(inv.TotalUsed()).To(Equal(12))
			Expect(inv.TotalAvailable()).To(Equal(0))
			Expect(inv.AvailableByType("H100")).To(Equal(0))
		})
	})

	Describe("RefreshAll", func() {
		Context("with usage discovery configured", func() {
			It("should refresh both capacity and usage", func() {
				disc := &mockFullDiscovery{
					inventory: map[string]map[string]discovery.AcceleratorModelInfo{
						"node-1": {"H100": {Count: 16, Memory: "80GB"}},
						"node-2": {"A100": {Count: 8, Memory: "40GB"}},
					},
					usage: map[string]int{"H100": 4, "A100": 2},
				}

				inv := NewTypeInventoryWithUsage("test", disc)
				err := inv.RefreshAll(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Check limits
				Expect(inv.TotalLimit()).To(Equal(24))
				Expect(inv.LimitByType("H100")).To(Equal(16))
				Expect(inv.LimitByType("A100")).To(Equal(8))

				// Check usage (auto-discovered)
				Expect(inv.TotalUsed()).To(Equal(6))
				Expect(inv.UsedByType("H100")).To(Equal(4))
				Expect(inv.UsedByType("A100")).To(Equal(2))

				// Check available
				Expect(inv.TotalAvailable()).To(Equal(18))
				Expect(inv.AvailableByType("H100")).To(Equal(12))
				Expect(inv.AvailableByType("A100")).To(Equal(6))
			})
		})

		Context("without usage discovery configured", func() {
			It("should fail with appropriate error", func() {
				disc := &mockDiscovery{
					inventory: map[string]map[string]discovery.AcceleratorModelInfo{
						"node-1": {"H100": {Count: 8, Memory: "80GB"}},
					},
				}

				inv := NewTypeInventory("test", disc)
				err := inv.RefreshAll(ctx)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("usage discovery not configured"))
			})
		})
	})

	Describe("CreateAllocator", func() {
		It("should create allocator with available GPUs", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 16, Memory: "80GB"}},
					"node-2": {"A100": {Count: 8, Memory: "40GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Set current usage
			inv.SetUsed(map[string]int{"H100": 4, "A100": 4})

			// Verify inventory state: limit=24, used=8, available=16
			Expect(inv.TotalLimit()).To(Equal(24))
			Expect(inv.TotalUsed()).To(Equal(8))
			Expect(inv.TotalAvailable()).To(Equal(16))

			// Create allocator - should get available (limit - used)
			allocator := inv.CreateAllocator(ctx)

			// Allocator should have available GPUs (not limits)
			Expect(allocator.Remaining()).To(Equal(16)) // H100: 16-4=12, A100: 8-4=4

			// Allocate from H100 pool
			allocated, err := allocator.TryAllocate(ctx, &domain.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			}, 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocated).To(Equal(4))
			Expect(allocator.Remaining()).To(Equal(12)) // 16 - 4 = 12

			// Original inventory should be unchanged
			Expect(inv.TotalAvailable()).To(Equal(16))
			Expect(inv.AvailableByType("H100")).To(Equal(12))
		})

		It("should handle partial allocation when resources exhausted", func() {
			disc := &mockDiscovery{
				inventory: map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"H100": {Count: 8, Memory: "80GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Set high usage - only 2 GPUs available
			inv.SetUsed(map[string]int{"H100": 6})

			allocator := inv.CreateAllocator(ctx)
			Expect(allocator.Remaining()).To(Equal(2))

			// Request more than available - should get partial allocation
			allocated, err := allocator.TryAllocate(ctx, &domain.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			}, 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocated).To(Equal(2)) // Only 2 available
			Expect(allocator.Remaining()).To(Equal(0))
		})
	})
})

var _ = Describe("TypeAllocator", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("TryAllocate", func() {
		DescribeTable("should allocate GPUs correctly",
			func(initialByType map[string]int, decision *domain.VariantDecision, gpusRequested int, expectedAllocated int, expectedRemaining map[string]int, expectError bool) {
				// Calculate initial total
				total := 0
				for _, count := range initialByType {
					total += count
				}

				allocator := &typeAllocator{
					remainingByType: copyMap(initialByType),
					totalRemaining:  total,
				}

				allocated, err := allocator.TryAllocate(ctx, decision, gpusRequested)

				if expectError {
					Expect(err).To(HaveOccurred())
					return
				}

				Expect(err).NotTo(HaveOccurred())
				Expect(allocated).To(Equal(expectedAllocated))

				for accType, expected := range expectedRemaining {
					Expect(allocator.RemainingForType(accType)).To(Equal(expected))
				}
			},
			Entry("allocate from available pool",
				map[string]int{"H100": 16, "A100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "H100"},
				4, 4,
				map[string]int{"H100": 12, "A100": 8},
				false,
			),
			Entry("allocate entire pool",
				map[string]int{"H100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "H100"},
				8, 8,
				map[string]int{"H100": 0},
				false,
			),
			Entry("partial allocation when pool exhausted",
				map[string]int{"H100": 4},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "H100"},
				8, 4,
				map[string]int{"H100": 0},
				false,
			),
			Entry("no allocation when type not available",
				map[string]int{"A100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "H100"},
				4, 0,
				map[string]int{"A100": 8},
				false,
			),
			Entry("resolve empty accelerator in homogeneous cluster",
				map[string]int{"H100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default"},
				4, 4,
				map[string]int{"H100": 4},
				false,
			),
			Entry("return zero for empty accelerator in heterogeneous cluster",
				map[string]int{"H100": 8, "A100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default"},
				4, 0,
				map[string]int{"H100": 8, "A100": 8},
				false,
			),
			Entry("zero request returns zero",
				map[string]int{"H100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "H100"},
				0, 0,
				map[string]int{"H100": 8},
				false,
			),
			Entry("types are isolated",
				map[string]int{"H100": 4, "A100": 8},
				&domain.VariantDecision{VariantName: "model-a", Namespace: "default", AcceleratorName: "A100"},
				6, 6,
				map[string]int{"H100": 4, "A100": 2},
				false,
			),
		)
	})

	Describe("Multiple Allocations", func() {
		It("should track state across multiple allocations", func() {
			allocator := &typeAllocator{
				remainingByType: map[string]int{"H100": 16, "A100": 8},
				totalRemaining:  24,
			}

			ctx := context.Background()

			// First allocation: 4 H100 GPUs
			allocated, err := allocator.TryAllocate(ctx, &domain.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			}, 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocated).To(Equal(4))
			Expect(allocator.RemainingForType("H100")).To(Equal(12))
			Expect(allocator.RemainingForType("A100")).To(Equal(8))
			Expect(allocator.Remaining()).To(Equal(20))

			// Second allocation: 6 A100 GPUs
			allocated, err = allocator.TryAllocate(ctx, &domain.VariantDecision{
				VariantName:     "model-b",
				Namespace:       "default",
				AcceleratorName: "A100",
			}, 6)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocated).To(Equal(6))
			Expect(allocator.RemainingForType("H100")).To(Equal(12))
			Expect(allocator.RemainingForType("A100")).To(Equal(2))
			Expect(allocator.Remaining()).To(Equal(14))

			// Third allocation: more H100 than available
			allocated, err = allocator.TryAllocate(ctx, &domain.VariantDecision{
				VariantName:     "model-c",
				Namespace:       "default",
				AcceleratorName: "H100",
			}, 20)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocated).To(Equal(12)) // Only 12 remaining
			Expect(allocator.RemainingForType("H100")).To(Equal(0))
			Expect(allocator.RemainingForType("A100")).To(Equal(2))
			Expect(allocator.Remaining()).To(Equal(2))
		})
	})
})

// copyMap creates a copy of a map[string]int
func copyMap(m map[string]int) map[string]int {
	result := make(map[string]int, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

var _ = Describe("TypeInventory normalization", func() {
	Context("with accelerator name normalization", func() {
		It("should normalize discovered GPU types", func() {
			ctx := context.Background()
			disc := &mockDiscovery{
				inventory: map[string]map[string]discovery.AcceleratorModelInfo{
					"node-1": {"NVIDIA-A100-PCIE-80GB": {Count: 4, Memory: "80GB"}},
					"node-2": {"NVIDIA-H100-SXM5-80GB": {Count: 8, Memory: "80GB"}},
				},
			}

			inv := NewTypeInventory("test", disc)
			err := inv.Refresh(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify that types are normalized to short names
			Expect(inv.LimitByType("A100")).To(Equal(4))
			Expect(inv.LimitByType("H100")).To(Equal(8))
			Expect(inv.TotalLimit()).To(Equal(12))

			// Full names should not be accessible
			Expect(inv.LimitByType("NVIDIA-A100-PCIE-80GB")).To(Equal(0))
		})
	})
})
