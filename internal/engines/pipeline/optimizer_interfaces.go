package pipeline

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// NamedAnalyzerResult pairs an analyzer's name with its result and mutable
// working counters for the optimizer's allocation loop.
// It is the per-entry type of ModelScalingRequest.AnalyzerResults and is
// only used inside the engine→optimizer contract; it is not a general-purpose
// interfaces type.
//
// Remaining and Spare are initialised from Result.RequiredCapacity and
// Result.SpareCapacity by the engine (model scope) and decremented in place by
// applyAllocation as the optimizer allocates replicas.
// For disaggregated (P/D) models, the optimizer calls initRoleState
// to populate RoleSpare per role and initialize picker-local demand.
// The original Result values are never mutated.
type NamedAnalyzerResult struct {
	Name              string
	Result            *domain.AnalyzerResult
	Score             float64            // belief weight from AnalyzerScoreConfig: how far the per-(variant, role) combine pulls toward this analyzer's replica vote; 0 reads as the 1.0 default. Not a priority and not a budget multiplier
	Remaining         float64            // mutable remaining required capacity; P-scope for disaggregated, model-scope otherwise
	Spare             float64            // mutable remaining spare capacity; model-scope (non-disaggregated only)
	RoleSpare         map[string]float64 // per-role mutable spare; set by initRoleState; nil for non-disaggregated
	ScaleUpThreshold  float64            // resolved scale-up threshold used to compute RC
	ScaleDownBoundary float64            // resolved scale-down boundary used to compute SC

	// Live indicates the analyzer produced a non-error, informative result within the
	// staleness window. Set by the engine each cycle. Non-live analyzers are excluded
	// from the scale-down veto so a registered-but-uninformative analyzer (no metrics,
	// error state, never analyzed) cannot block scale-down. Recovery is automatic: a
	// fresh informative result makes it live again on the next cycle.
	Live bool

	// Enabled indicates the analyzer votes in the combine (RC/SC) math for this cycle.
	// Saturation is present as the identity carrier even when it does not vote
	// (e.g. a throughput-only config), so "present in the ballot" != "votes".
	// Set by the engine each cycle. votingResults prunes the ballot to Enabled &&
	// Live entries before combine math (VG-up); the anchor build (bindingAnchor)
	// reads the full ballot so a non-voting saturation entry can still supply identity.
	Enabled bool
}

// ModelScalingRequest bundles the analyzer result with variant state for one model.
// The optimizer receives a slice of these — one per model — and produces decisions.
type ModelScalingRequest struct {
	ModelID   string
	Namespace string
	// AnalyzerResults is the per-analyzer ballot. votingResults' combine math is
	// order-independent, but bindingAnchor's binder tie-break is not: among
	// qualifying non-saturation entries, the lowest ballot index binds.
	AnalyzerResults []NamedAnalyzerResult
	VariantStates   []domain.VariantReplicaState
	Priority        float64 // Model priority (default 1.0)
	Disaggregated   bool    // true when model has prefill+decode variants
}

// ScalingOptimizer makes final scaling decisions for all models.
//
// Implementations:
//   - CostAwareOptimizer: processes each model independently, minimizes cost (unlimited mode)
//   - GreedyByScoreOptimizer: fair-shares GPUs across models (limited mode)
type ScalingOptimizer interface {
	// Name returns optimizer identifier for logging/metrics.
	Name() string

	// Optimize produces VariantDecisions from analyzer results and optional constraints.
	// constraints may be nil in unlimited mode.
	Optimize(ctx context.Context, requests []ModelScalingRequest, constraints []*ResourceConstraints) []domain.VariantDecision
}
