package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// simpleAllocator implements ResourceAllocator with a single pool
type simpleAllocator struct {
	remaining int
}

func (s *simpleAllocator) TryAllocate(_ context.Context, decision *domain.VariantDecision, gpusRequested int) (int, error) {
	if gpusRequested <= 0 {
		return 0, nil
	}
	if s.remaining >= gpusRequested {
		s.remaining -= gpusRequested
		return gpusRequested, nil
	}
	allocated := s.remaining
	s.remaining = 0
	return allocated, nil
}

func (s *simpleAllocator) Remaining() int {
	return s.remaining
}

var _ = Describe("GreedyBySaturation", func() {
	var (
		ctx       context.Context
		algorithm *GreedyBySaturation
		allocator *simpleAllocator
		decisions []*domain.VariantDecision
	)

	BeforeEach(func() {
		ctx = context.Background()
		algorithm = NewGreedyBySaturation()
	})

	Describe("Name", func() {
		It("should return the algorithm name", func() {
			Expect(algorithm.Name()).To(Equal("greedy-by-saturation"))
		})
	})

	Describe("Allocate", func() {
		Context("with single decision that needs scale-up", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 10}
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						CurrentReplicas: 2,
						TargetReplicas:  4, // wants +2
						GPUsPerReplica:  2,
						SpareCapacity:   0.1,
					},
				}
			})

			It("should allocate GPUs for the requested replicas", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				Expect(decisions[0].GPUsAllocated).To(Equal(4)) // 2 replicas * 2 GPUs
				Expect(decisions[0].TargetReplicas).To(Equal(4))
				Expect(allocator.Remaining()).To(Equal(6)) // 10 - 4
			})
		})

		Context("with multiple decisions prioritized by saturation", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 6} // Only enough for 3 replicas at 2 GPUs each
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1-medium",
						CurrentReplicas: 1,
						TargetReplicas:  2, // wants +1 (2 GPUs)
						GPUsPerReplica:  2,
						SpareCapacity:   0.3, // Medium saturation
						Cost:            10,
					},
					{
						VariantName:     "v2-saturated",
						CurrentReplicas: 1,
						TargetReplicas:  2, // wants +1 (2 GPUs)
						GPUsPerReplica:  2,
						SpareCapacity:   0.05, // Most saturated (lowest spare)
						Cost:            15,
					},
					{
						VariantName:     "v3-idle",
						CurrentReplicas: 1,
						TargetReplicas:  2, // wants +1 (2 GPUs)
						GPUsPerReplica:  2,
						SpareCapacity:   0.5, // Least saturated (highest spare)
						Cost:            20,
					},
				}
			})

			It("should allocate to most saturated first", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				// v2-saturated (SpareCapacity=0.05) should get allocation first
				// v1-medium (SpareCapacity=0.3) should get allocation second
				// v3-idle (SpareCapacity=0.5) should get allocation third
				// Only 6 GPUs available, so only first 3 replicas can be allocated

				var v1, v2, v3 *domain.VariantDecision
				for _, d := range decisions {
					switch d.VariantName {
					case "v1-medium":
						v1 = d
					case "v2-saturated":
						v2 = d
					case "v3-idle":
						v3 = d
					}
				}

				// Most saturated gets full allocation
				Expect(v2.GPUsAllocated).To(Equal(2))
				Expect(v2.TargetReplicas).To(Equal(2))
				Expect(v2.WasLimited).To(BeFalse())

				// Second most saturated gets full allocation
				Expect(v1.GPUsAllocated).To(Equal(2))
				Expect(v1.TargetReplicas).To(Equal(2))
				Expect(v1.WasLimited).To(BeFalse())

				// Least saturated gets remaining (2 GPUs)
				Expect(v3.GPUsAllocated).To(Equal(2))
				Expect(v3.TargetReplicas).To(Equal(2))
			})
		})

		Context("when resources are exhausted", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 3} // Not enough for a full replica
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						CurrentReplicas: 1,
						TargetReplicas:  3, // wants +2 (4 GPUs)
						GPUsPerReplica:  2,
						SpareCapacity:   0.1,
					},
				}
			})

			It("should partially allocate and mark as limited", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				// Only 3 GPUs available, 2 GPUs per replica = 1 replica can be added
				Expect(decisions[0].GPUsAllocated).To(Equal(2))  // Only full replicas count
				Expect(decisions[0].TargetReplicas).To(Equal(2)) // 1 + 1 replica
				Expect(decisions[0].WasLimited).To(BeTrue())
			})
		})

		Context("with decisions that don't need scale-up", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 10}
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1-no-change",
						CurrentReplicas: 2,
						TargetReplicas:  2, // no scale-up needed
						GPUsPerReplica:  2,
						SpareCapacity:   0.1,
					},
					{
						VariantName:     "v2-scale-down",
						CurrentReplicas: 3,
						TargetReplicas:  2, // scale-down, not scale-up
						GPUsPerReplica:  2,
						SpareCapacity:   0.5,
					},
				}
			})

			It("should not allocate GPUs", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				Expect(decisions[0].GPUsAllocated).To(Equal(0))
				Expect(decisions[1].GPUsAllocated).To(Equal(0))
				Expect(allocator.Remaining()).To(Equal(10)) // No GPUs consumed
			})
		})

		Context("with equal saturation (tie-breaker by cost)", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 4} // Only enough for 2 replicas
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1-expensive",
						CurrentReplicas: 1,
						TargetReplicas:  2,
						GPUsPerReplica:  2,
						SpareCapacity:   0.1, // Same saturation
						Cost:            20,  // More expensive
					},
					{
						VariantName:     "v2-cheap",
						CurrentReplicas: 1,
						TargetReplicas:  2,
						GPUsPerReplica:  2,
						SpareCapacity:   0.1, // Same saturation
						Cost:            5,   // Cheaper
					},
				}
			})

			It("should prefer cheaper variant when saturation is equal", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				var expensive, cheap *domain.VariantDecision
				for _, d := range decisions {
					if d.VariantName == "v1-expensive" {
						expensive = d
					} else {
						cheap = d
					}
				}

				// Cheaper variant gets full allocation first
				Expect(cheap.GPUsAllocated).To(Equal(2))
				Expect(cheap.TargetReplicas).To(Equal(2))

				// Expensive variant gets remaining
				Expect(expensive.GPUsAllocated).To(Equal(2))
				Expect(expensive.TargetReplicas).To(Equal(2))
			})
		})

		Context("with zero GPUs per replica", func() {
			BeforeEach(func() {
				allocator = &simpleAllocator{remaining: 10}
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						CurrentReplicas: 1,
						TargetReplicas:  2,
						GPUsPerReplica:  0, // Edge case: 0 GPUs
						SpareCapacity:   0.1,
					},
				}
			})

			It("should default to 1 GPU per replica", func() {
				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				// Should allocate 1 GPU for the +1 replica
				Expect(decisions[0].GPUsAllocated).To(Equal(1))
				Expect(decisions[0].TargetReplicas).To(Equal(2))
			})
		})

		Context("with empty decisions", func() {
			It("should return without error", func() {
				allocator = &simpleAllocator{remaining: 10}
				err := algorithm.Allocate(ctx, []*domain.VariantDecision{}, allocator)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with MaxReplicas bound", func() {
			It("should cap scale-up at maxReplicas even when GPUs are available", func() {
				maxReplicas := 3
				allocator = &simpleAllocator{remaining: 100}
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						CurrentReplicas: 1,
						TargetReplicas:  10, // wants to scale to 10
						GPUsPerReplica:  1,
						SpareCapacity:   0.0,
						MaxReplicas:     &maxReplicas,
					},
				}

				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				// Capped at maxReplicas=3
				Expect(decisions[0].TargetReplicas).To(Equal(3))
			})
		})

		Context("with MinReplicas bound", func() {
			It("should enforce minReplicas floor even under GPU scarcity", func() {
				minReplicas := 3
				allocator = &simpleAllocator{remaining: 0} // no GPUs
				decisions = []*domain.VariantDecision{
					{
						VariantName:     "v1",
						CurrentReplicas: 1,
						TargetReplicas:  5,
						GPUsPerReplica:  2,
						SpareCapacity:   0.0,
						MinReplicas:     &minReplicas,
					},
				}

				err := algorithm.Allocate(ctx, decisions, allocator)
				Expect(err).NotTo(HaveOccurred())

				// MinReplicas is a hard floor
				Expect(decisions[0].TargetReplicas).To(Equal(3))
				Expect(decisions[0].WasLimited).To(BeTrue())
			})
		})
	})
})
