package pipeline

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// NoOpLimiter is a Limiter whose Limit method returns nil without
// touching the decisions slice. Useful as the zero value of the Limiter
// interface — pipelines that want to disable limiting without rewiring,
// and tests that construct an engine without exercising the limiter
// path.
type NoOpLimiter struct {
	name string
}

// NewNoOpLimiter constructs a NoOpLimiter with the given name. The name
// surfaces in logs and decision traces.
func NewNoOpLimiter(name string) *NoOpLimiter {
	return &NoOpLimiter{name: name}
}

// Name returns the no-op limiter identifier.
func (l *NoOpLimiter) Name() string { return l.name }

// Limit is a no-op; decisions are returned unchanged.
func (l *NoOpLimiter) Limit(_ context.Context, _ []*domain.VariantDecision) error {
	return nil
}

// Ensure NoOpLimiter implements Limiter.
var _ Limiter = (*NoOpLimiter)(nil)
