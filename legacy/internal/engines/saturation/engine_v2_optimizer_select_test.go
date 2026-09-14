package saturation

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// selectInventory is a minimal pipeline.Inventory whose Refresh result and
// resource pools are configurable, so selectV2Optimizer's handling of the
// inventory limiter can be exercised without a real cluster.
type selectInventory struct {
	refreshErr    error
	pools         map[string]pipeline.ResourcePool
	refreshCalled bool
}

func (s *selectInventory) Name() string {
	return "select-inventory"
}

func (s *selectInventory) Refresh(context.Context) error {
	s.refreshCalled = true
	return s.refreshErr
}

func (s *selectInventory) SetUsed(map[string]int) {}

func (s *selectInventory) CreateAllocator(context.Context) pipeline.ResourceAllocator {
	return nil
}

func (s *selectInventory) GetResourcePools() map[string]pipeline.ResourcePool {
	return s.pools
}

func (s *selectInventory) TotalLimit() int {
	t := 0
	for _, p := range s.pools {
		t += p.Limit
	}
	return t
}
func (s *selectInventory) TotalUsed() int {
	t := 0
	for _, p := range s.pools {
		t += p.Used
	}
	return t
}
func (s *selectInventory) TotalAvailable() int {
	t := 0
	for _, p := range s.pools {
		t += p.Available()
	}
	return t
}

// selectLimiter is a pipeline.Limiter that is NOT the inventory-backed
// DefaultLimiter, used to exercise the "limiter cannot be queried" branch.
type selectLimiter struct{}

func (selectLimiter) Name() string { return "select-limiter" }
func (selectLimiter) Limit(context.Context, []*domain.VariantDecision) error {
	return nil
}

var _ = Describe("selectV2Optimizer", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	newInventoryLimiter := func(refreshErr error, pools map[string]pipeline.ResourcePool) *pipeline.DefaultLimiter {
		return pipeline.NewDefaultLimiter("gpu-limiter",
			&selectInventory{refreshErr: refreshErr, pools: pools},
			pipeline.NewGreedyBySaturation())
	}

	It("falls back to the cost-aware (unlimited) optimizer when inventory refresh fails", func() {
		// Models the cluster-limitation case: node objects cannot be listed, so
		// ComputeConstraints errors. GreedyByScore with empty constraints would
		// silently block all scale-up, so we must run cost-aware instead.
		e := &Engine{
			optimizer:  pipeline.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(errors.New("failed to list nodes"), nil),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
	})

	It("uses greedy-by-score with constraints when the inventory reports GPUs (heterogeneous pools)", func() {
		e := &Engine{
			optimizer: pipeline.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(nil, map[string]pipeline.ResourcePool{
				"A100": {Limit: 8},
				"H100": {Limit: 4},
			}),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("greedy-by-score"))
		Expect(constraints).To(HaveLen(1))
		Expect(constraints[0].Pools).To(HaveKey("A100"))
		Expect(constraints[0].Pools).To(HaveKey("H100"))
	})

	It("keeps greedy-by-score when refresh succeeds but reports zero GPUs (genuine no-capacity)", func() {
		// A present-but-empty constraint is a real "no capacity" signal and must
		// still block scale-up — distinct from "capacity unknown".
		e := &Engine{
			optimizer:  pipeline.NewGreedyByScoreOptimizer(),
			GPULimiter: newInventoryLimiter(nil, map[string]pipeline.ResourcePool{}),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("greedy-by-score"))
		Expect(constraints).To(HaveLen(1))
		Expect(constraints[0].Pools).To(BeEmpty())
	})

	It("falls back to cost-aware when the GPU limiter is not the inventory limiter", func() {
		e := &Engine{
			optimizer:  pipeline.NewGreedyByScoreOptimizer(),
			GPULimiter: selectLimiter{},
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
	})

	It("leaves a non-greedy optimizer untouched and never queries the limiter", func() {
		inv := &selectInventory{refreshErr: errors.New("must not be called")}
		e := &Engine{
			optimizer:  pipeline.NewCostAwareOptimizer(),
			GPULimiter: pipeline.NewDefaultLimiter("gpu-limiter", inv, pipeline.NewGreedyBySaturation()),
		}
		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt.Name()).To(Equal("cost-aware"))
		Expect(constraints).To(BeEmpty())
		Expect(inv.refreshCalled).To(BeFalse(), "the limiter must not be queried for a non-greedy optimizer")
	})
})
