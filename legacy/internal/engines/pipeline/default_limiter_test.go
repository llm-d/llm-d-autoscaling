package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

func init() {
	// Initialize metrics for all limiter tests
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		panic("failed to initialize metrics: " + err.Error())
	}
}

// mockInventory implements Inventory for testing
type mockInventory struct {
	name        string
	limitByType map[string]int
	usedByType  map[string]int
	refreshErr  error
}

func newMockInventory(name string, limitByType map[string]int) *mockInventory {
	return &mockInventory{
		name:        name,
		limitByType: limitByType,
		usedByType:  make(map[string]int),
	}
}

func (m *mockInventory) Name() string {
	return m.name
}

func (m *mockInventory) Refresh(ctx context.Context) error {
	return m.refreshErr
}

func (m *mockInventory) SetUsed(usedByType map[string]int) {
	m.usedByType = usedByType
}

func (m *mockInventory) CreateAllocator(ctx context.Context) ResourceAllocator {
	availableByType := make(map[string]int)
	for t, limit := range m.limitByType {
		used := m.usedByType[t]
		availableByType[t] = limit - used
	}
	return &mockTypeAllocator{availableByType: availableByType}
}

func (m *mockInventory) TotalLimit() int {
	total := 0
	for _, v := range m.limitByType {
		total += v
	}
	return total
}

func (m *mockInventory) TotalUsed() int {
	total := 0
	for _, v := range m.usedByType {
		total += v
	}
	return total
}

func (m *mockInventory) TotalAvailable() int {
	return m.TotalLimit() - m.TotalUsed()
}

func (m *mockInventory) GetResourcePools() map[string]ResourcePool {
	pools := make(map[string]ResourcePool, len(m.limitByType))
	for accType, limit := range m.limitByType {
		used := m.usedByType[accType]
		pools[accType] = ResourcePool{Limit: limit, Used: used}
	}
	return pools
}

// mockTypeAllocator implements ResourceAllocator for testing
type mockTypeAllocator struct {
	availableByType map[string]int
}

func (m *mockTypeAllocator) TryAllocate(_ context.Context, decision *domain.VariantDecision, gpusRequested int) (int, error) {
	accelType := decision.AcceleratorName
	if accelType == "" {
		accelType = "default"
	}

	available := m.availableByType[accelType]
	if available >= gpusRequested {
		m.availableByType[accelType] -= gpusRequested
		return gpusRequested, nil
	}

	allocated := available
	m.availableByType[accelType] = 0
	return allocated, nil
}

func (m *mockTypeAllocator) Remaining() int {
	total := 0
	for _, v := range m.availableByType {
		total += v
	}
	return total
}

// mockAlgorithm implements AllocationAlgorithm for testing
type mockAlgorithm struct {
	name         string
	allocateFunc func(ctx context.Context, decisions []*domain.VariantDecision, allocator ResourceAllocator) error
}

func (m *mockAlgorithm) Name() string {
	return m.name
}

func (m *mockAlgorithm) Allocate(ctx context.Context, decisions []*domain.VariantDecision, allocator ResourceAllocator) error {
	if m.allocateFunc != nil {
		return m.allocateFunc(ctx, decisions, allocator)
	}
	return nil
}

var _ = Describe("DefaultLimiter", func() {
	var (
		ctx       context.Context
		limiter   *DefaultLimiter
		inventory *mockInventory
		algorithm *mockAlgorithm
		decisions []*domain.VariantDecision
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Name", func() {
		It("should return the limiter name", func() {
			inventory = newMockInventory("test-inventory", map[string]int{"A100": 10})
			algorithm = &mockAlgorithm{name: "test-algo"}
			limiter = NewDefaultLimiter("test-limiter", inventory, algorithm)

			Expect(limiter.Name()).To(Equal("test-limiter"))
		})
	})

	Describe("Limit", func() {
		Context("with empty decisions", func() {
			It("should return nil without error", func() {
				inventory = newMockInventory("inv", map[string]int{"A100": 10})
				algorithm = &mockAlgorithm{name: "algo"}
				limiter = NewDefaultLimiter("limiter", inventory, algorithm)

				err := limiter.Limit(ctx, []*domain.VariantDecision{})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with scale-up decisions", func() {
			BeforeEach(func() {
				inventory = newMockInventory("type-inv", map[string]int{"A100": 8})
				algorithm = &mockAlgorithm{
					name: "pass-through",
					allocateFunc: func(ctx context.Context, decisions []*domain.VariantDecision, allocator ResourceAllocator) error {
						for _, d := range decisions {
							if d.TargetReplicas > d.CurrentReplicas {
								gpusNeeded := (d.TargetReplicas - d.CurrentReplicas) * d.GPUsPerReplica
								allocated, _ := allocator.TryAllocate(ctx, d, gpusNeeded)
								d.GPUsAllocated = allocated
							}
						}
						return nil
					},
				}
				limiter = NewDefaultLimiter("gpu-limiter", inventory, algorithm)

				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						AcceleratorName: "A100",
						CurrentReplicas: 2,
						TargetReplicas:  4, // wants +2
						GPUsPerReplica:  2,
					},
				}
			})

			It("should calculate used GPUs from current replicas", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				// Current usage: 2 replicas * 2 GPUs = 4 GPUs used
				Expect(inventory.usedByType["A100"]).To(Equal(4))
			})

			It("should allocate GPUs and update decision", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				// Available: 8 - 4 = 4 GPUs, needed: 2 replicas * 2 GPUs = 4 GPUs
				Expect(decisions[0].GPUsAllocated).To(Equal(4))
			})

			It("should add decision step", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				Expect(decisions[0].DecisionSteps).ToNot(BeEmpty())
				lastStep := decisions[0].DecisionSteps[len(decisions[0].DecisionSteps)-1]
				Expect(lastStep.Name).To(Equal("gpu-limiter"))
			})
		})

		Context("when resources are limited", func() {
			BeforeEach(func() {
				// Only 6 GPUs limit
				inventory = newMockInventory("type-inv", map[string]int{"A100": 6})
				algorithm = &mockAlgorithm{
					name: "partial-alloc",
					allocateFunc: func(ctx context.Context, decisions []*domain.VariantDecision, allocator ResourceAllocator) error {
						for _, d := range decisions {
							if d.TargetReplicas > d.CurrentReplicas {
								replicasNeeded := d.TargetReplicas - d.CurrentReplicas
								gpusNeeded := replicasNeeded * d.GPUsPerReplica
								allocated, _ := allocator.TryAllocate(ctx, d, gpusNeeded)
								// Calculate how many replicas we can actually add
								replicasCanAdd := 0
								if d.GPUsPerReplica > 0 {
									replicasCanAdd = allocated / d.GPUsPerReplica
								}
								d.GPUsAllocated = replicasCanAdd * d.GPUsPerReplica
								d.TargetReplicas = d.CurrentReplicas + replicasCanAdd
								// Mark as limited if we couldn't get all requested replicas
								if replicasCanAdd < replicasNeeded {
									d.WasLimited = true
								}
							}
						}
						return nil
					},
				}
				limiter = NewDefaultLimiter("gpu-limiter", inventory, algorithm)

				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						AcceleratorName: "A100",
						CurrentReplicas: 2, // uses 4 GPUs
						TargetReplicas:  5, // wants +3 = 6 more GPUs
						GPUsPerReplica:  2,
					},
				}
			})

			It("should mark decision as limited", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				// Available: 6 - 4 = 2 GPUs, needed 6, can only allocate 2 (1 replica)
				Expect(decisions[0].WasLimited).To(BeTrue())
				Expect(decisions[0].LimitedBy).To(Equal("gpu-limiter"))
			})

			It("should adjust target replicas based on allocation", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				// Can only add 1 replica (2 GPUs available / 2 GPUs per replica)
				Expect(decisions[0].TargetReplicas).To(Equal(3))
			})
		})

		Context("with multiple accelerator types", func() {
			BeforeEach(func() {
				inventory = newMockInventory("type-inv", map[string]int{
					"A100": 8,
					"H100": 4,
				})
				algorithm = &mockAlgorithm{
					name: "multi-type",
					allocateFunc: func(ctx context.Context, decisions []*domain.VariantDecision, allocator ResourceAllocator) error {
						for _, d := range decisions {
							if d.TargetReplicas > d.CurrentReplicas {
								gpusNeeded := (d.TargetReplicas - d.CurrentReplicas) * d.GPUsPerReplica
								allocated, _ := allocator.TryAllocate(ctx, d, gpusNeeded)
								d.GPUsAllocated = allocated
							}
						}
						return nil
					},
				}
				limiter = NewDefaultLimiter("gpu-limiter", inventory, algorithm)

				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1-a100",
						AcceleratorName: "A100",
						CurrentReplicas: 2,
						TargetReplicas:  3,
						GPUsPerReplica:  2,
					},
					{
						VariantName:     "v2-h100",
						AcceleratorName: "H100",
						CurrentReplicas: 1,
						TargetReplicas:  2,
						GPUsPerReplica:  2,
					},
				}
			})

			It("should track usage by accelerator type", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				Expect(inventory.usedByType["A100"]).To(Equal(4)) // 2 replicas * 2 GPUs
				Expect(inventory.usedByType["H100"]).To(Equal(2)) // 1 replica * 2 GPUs
			})

			It("should allocate from correct type pools", func() {
				err := limiter.Limit(ctx, decisions)
				Expect(err).NotTo(HaveOccurred())

				// A100: 8 - 4 = 4 available, needs 2, gets 2
				Expect(decisions[0].GPUsAllocated).To(Equal(2))
				// H100: 4 - 2 = 2 available, needs 2, gets 2
				Expect(decisions[1].GPUsAllocated).To(Equal(2))
			})
		})
	})

	Describe("resolveUnknownAccelerators", func() {
		It("should resolve empty accelerator in homogeneous cluster", func() {
			inventory = newMockInventory("inv", map[string]int{"H100": 8})
			algorithm = &mockAlgorithm{name: "algo"}
			limiter = NewDefaultLimiter("limiter", inventory, algorithm)

			decisions = []*domain.VariantDecision{
				{VariantName: "v1", AcceleratorName: ""},
				{VariantName: "v2", AcceleratorName: "unknown"},
				{VariantName: "v3", AcceleratorName: "H100"},
			}
			limiter.resolveUnknownAccelerators(decisions)

			Expect(decisions[0].AcceleratorName).To(Equal("H100"))
			Expect(decisions[1].AcceleratorName).To(Equal("H100"))
			Expect(decisions[2].AcceleratorName).To(Equal("H100"))
		})

		It("should not resolve in heterogeneous cluster", func() {
			inventory = newMockInventory("inv", map[string]int{"H100": 8, "A100": 4})
			algorithm = &mockAlgorithm{name: "algo"}
			limiter = NewDefaultLimiter("limiter", inventory, algorithm)

			decisions = []*domain.VariantDecision{
				{VariantName: "v1", AcceleratorName: ""},
				{VariantName: "v2", AcceleratorName: "unknown"},
				{VariantName: "v3", AcceleratorName: "A100"},
			}
			limiter.resolveUnknownAccelerators(decisions)

			Expect(decisions[0].AcceleratorName).To(Equal(""))
			Expect(decisions[1].AcceleratorName).To(Equal("unknown"))
			Expect(decisions[2].AcceleratorName).To(Equal("A100"))
		})

		It("should not resolve when inventory is empty", func() {
			inventory = newMockInventory("inv", map[string]int{})
			algorithm = &mockAlgorithm{name: "algo"}
			limiter = NewDefaultLimiter("limiter", inventory, algorithm)

			decisions = []*domain.VariantDecision{
				{VariantName: "v1", AcceleratorName: "unknown"},
			}
			limiter.resolveUnknownAccelerators(decisions)

			Expect(decisions[0].AcceleratorName).To(Equal("unknown"))
		})
	})
})
