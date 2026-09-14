package pipeline

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// CompositeLimiter runs a slice of Limiter instances sequentially against the
// same set of decisions. It is the simplest possible composition: each
// constituent caps the decisions in turn, and the most-restrictive constraint
// wins by virtue of the decisions slice being shared mutable state.
//
// CompositeLimiter is the minimal wiring needed to enable an operator to
// deploy a quota config that declares both cluster-scope and namespace-scope
// entries before the full limiter chain (sub-issue #1003) lands. The chain
// will replace it with explicit ordering, allocator-sharing, and
// physical-bound composition (min(physical, quota)).
//
// Behavior:
//   - Empty constituents -> a no-op (Limit returns nil; useful for tests).
//   - One constituent -> behaves identically to that constituent.
//   - On error from any constituent, Limit returns immediately; decisions
//     made by earlier constituents stay applied. TryAllocate's in-memory usage
//     state is per-allocator, so usage accounting never leaks across
//     constituents. The TargetReplicas mutations earlier constituents already
//     wrote to the shared slice DO persist and are NOT rolled back: on a limiter
//     error the V1 caller (saturation engine) logs and proceeds with these
//     partially-capped decisions rather than aborting the cycle, so a Limit
//     error means "decisions may be partially limited." In practice quota
//     constituents never return an error (Refresh is a no-op and the allocation
//     algorithm swallows TryAllocate errors), so this is a robustness note
//     rather than a live path.
//
// LimitedBy semantics: attribution is scoped to the constituent that newly
// constrained a decision (see DefaultLimiter.updateDecisionMetadata) — a
// constituent that runs but does not newly limit a decision leaves its LimitedBy
// and the limited metric untouched. When more than one constituent caps the same
// decision, the FIRST one to constrain it wins on LimitedBy (a later constituent
// sees WasLimited already set and does not re-attribute). The DecisionStep
// history preserves every constituent's contribution, so the per-step trace
// remains accurate.
type CompositeLimiter struct {
	name     string
	limiters []Limiter
}

// NewCompositeLimiter wraps a slice of constituent limiters. The name is used
// in logs and as the outer LimitedBy in single-constituent edge cases.
// Constituents are not copied; the caller retains ownership.
func NewCompositeLimiter(name string, constituents []Limiter) *CompositeLimiter {
	return &CompositeLimiter{
		name:     name,
		limiters: constituents,
	}
}

// Name returns the composite limiter identifier.
func (c *CompositeLimiter) Name() string {
	return c.name
}

// Constituents returns the wrapped limiters in order. Exposed for tests and
// observability; callers must not mutate the returned slice.
func (c *CompositeLimiter) Constituents() []Limiter {
	return c.limiters
}

// Limit applies each constituent's Limit method in order against the shared
// decisions slice. Each constituent recomputes its baseline usage from
// CurrentReplicas (DefaultLimiter.calculateUsedGPUs), but its TryAllocate runs
// against the already-capped TargetReplicas that earlier constituents mutated in
// place — so the most-restrictive cap wins as each successive constituent sees a
// lower target.
func (c *CompositeLimiter) Limit(ctx context.Context, decisions []*domain.VariantDecision) error {
	if len(decisions) == 0 || len(c.limiters) == 0 {
		return nil
	}
	for i, l := range c.limiters {
		if err := l.Limit(ctx, decisions); err != nil {
			return fmt.Errorf("composite %q: constituent[%d] (%q) failed: %w",
				c.name, i, l.Name(), err)
		}
	}
	return nil
}

// Ensure CompositeLimiter implements Limiter.
var _ Limiter = (*CompositeLimiter)(nil)
