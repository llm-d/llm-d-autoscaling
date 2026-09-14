package domain

import (
	"context"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DecisionReason categorizes the reason for a scaling decision.
// It is the typed category passed to SetDecisionReason (read back via
// ReasonCategory) and is paired there with a human-readable detail string
// (read back via Reason).
type DecisionReason string

// Defined DecisionReason values for the built-in scaling categories.
const (
	// DecisionReasonV2 indicates a V2 pipeline decision.
	DecisionReasonV2 DecisionReason = "V2"
	// DecisionReasonSaturationOnly indicates decision from saturation-only mode.
	DecisionReasonSaturationOnly DecisionReason = "saturation-only mode"
	// DecisionReasonScaleFromZero indicates scale-up from zero replicas.
	DecisionReasonScaleFromZero DecisionReason = "scale-from-zero"
	// DecisionReasonRescale indicates a priority-weighted rescale reclaim — a
	// deliberate, redistributive scale-down (not flappy demand), which downstream
	// stabilization must not damp as if it were noise.
	DecisionReasonRescale DecisionReason = "rescale"
	// DecisionReasonTest is used for test scenarios.
	DecisionReasonTest DecisionReason = "test"
)

// SaturationAnalyzerName is the canonical name for the saturation analyzer.
const SaturationAnalyzerName = "saturation"

// RoleBoth represents the default role when a variant serves both prefill and decode.
const RoleBoth = "both"

// RolePrefill represents the prefill-only role in a P/D disaggregated deployment.
const RolePrefill = "prefill"

// RoleDecode represents the decode-only role in a P/D disaggregated deployment.
const RoleDecode = "decode"

// ReplicaMetrics holds per-replica metrics used by both the saturation analyzer
// and the queueing model analyzer. Saturation analysis uses KV cache, queue, and
// token-capacity fields, while the queueing model analyzer uses
// ArrivalRate and MaxBatchSize to model queue dynamics and estimate optimal capacity.
// For LWS, ReplicaMetrics only holds leader pods (leaderworkerset.sigs.k8s.io/worker-index=0 label)
// which emit vLLM metrics representing an LWS replica, as such len(ReplicaMetrics) == len(LWS replicas).
type ReplicaMetrics struct {
	PodName         string
	KvCacheUsage    float64 // KV cache utilization (0.0-1.0)
	QueueLength     int     // Number of requests waiting
	VariantName     string  // Name of the variant this replica belongs to
	Namespace       string
	ModelID         string  // Model ID for grouping variants
	AcceleratorName string  // Accelerator type for this variant
	Cost            float64 // Cost per replica (from CRD spec, default 10)
	// Metadata contains freshness information (optional)
	Metadata *ReplicaMetricsMetadata `json:"metadata,omitempty"`

	// --- Fields for Saturation Analyzer V2 and Queueing Model Analyzer ---

	// NumGpuBlocks is the total number of KV cache blocks allocated on GPU.
	// Sourced from vllm:cache_config_info label "num_gpu_blocks".
	// Zero value means cache_config_info metric is not available.
	NumGpuBlocks int64

	// BlockSize is the number of tokens per KV cache block.
	// Sourced from vllm:cache_config_info label "block_size".
	// Zero value means cache_config_info metric is not available.
	BlockSize int64

	// TotalKvCapacityTokens is NumGpuBlocks × BlockSize (total token slots).
	// Computed by the collector after parsing cache_config_info labels.
	// Zero value means capacity data is unavailable.
	TotalKvCapacityTokens int64

	// TokensInUse is the derived current token demand on this replica.
	// Computed as KvCacheUsage × TotalKvCapacityTokens.
	// Zero when TotalKvCapacityTokens is unavailable.
	TokensInUse int64

	// AvgOutputTokens is the average generation tokens per request on this replica.
	// Derived from rate(generation_tokens_sum) / rate(generation_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgOutputTokens float64

	// AvgInputTokens is the average prompt tokens per request on this replica.
	// Derived from rate(prompt_tokens_sum) / rate(prompt_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgInputTokens float64

	// PrefixCacheHitRate is the fraction of prefix cache queries that were hits (0.0-1.0).
	// Derived from rate(vllm:prefix_cache_hits[5m]) / rate(vllm:prefix_cache_queries[5m]).
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Zero when prefix caching is disabled or metrics are unavailable.
	PrefixCacheHitRate float64

	// ArrivalRate is the request arrival rate to this replica in requests per second.
	// Sourced from rate(inference_extension_scheduler_attempts_total{status="success"}[5m]) per pod.
	// This represents requests being dispatched to this replica by the scheduler.
	// Used by queueing model analyzer as Lambda (arrival rate) for queue dynamics estimation.
	// Zero when scheduler metrics are unavailable.
	ArrivalRate float64

	// MaxBatchSize is the maximum number of concurrent inference requests this replica can process.
	// Parsed from the --max-num-seqs flag in the pod's parent Deployment container args.
	// Defaults to 256 (vLLM v0.8+ default) when the flag is not explicitly set.
	// Used by queueing model analyzer.
	MaxBatchSize int64

	// AvgTTFT is the average time-to-first-token on this replica in seconds.
	// Derived from rate(vllm:time_to_first_token_seconds_sum[5m]) / rate(..._count[5m]).
	// Used by queueing model tuner as observed TTFT for Kalman filter parameter learning.
	// Zero when metrics are unavailable.
	AvgTTFT float64

	// AvgITL is the average inter-token latency on this replica in seconds.
	// Derived from rate(vllm:time_per_output_token_seconds_sum[5m]) / rate(..._count[5m]).
	// Used by queueing model tuner as observed ITL for Kalman filter parameter learning.
	// TA notation: ITL_obs — the (k*, ITL_obs) pair drives OLS calibration of ITL(k) = A·k + B.
	// Zero when metrics are unavailable.
	AvgITL float64

	// --- Fields for Throughput Analyzer ---

	// GenerationTokenRate is the observed decode token generation rate on this replica (tokens/sec).
	// Derived from rate(vllm:request_generation_tokens_sum[1m]) per pod.
	// TA notation: μ_dec^obs — directly observable supply proxy; also used as a sanity check
	// against the demand estimate (μ_dec^obs ≈ λ_dec at steady state with no queueing).
	// Zero when metrics are unavailable.
	GenerationTokenRate float64

	// KvUsageInstant is the instantaneous KV cache utilization fraction on this replica (0.0–1.0).
	// Derived from vllm:kv_cache_usage_perc (no max_over_time window).
	// TA notation: k* — the current operating point in the ITL model ITL(k) = A·k + B.
	// Differs from KvCacheUsage which uses max_over_time[1m] for the saturation analyzer.
	// Zero when metrics are unavailable.
	KvUsageInstant float64

	// RequestRate is the engine-side request completion rate on this replica (req/s).
	// Engine-agnostic: derived per pod from rate(vllm:request_generation_tokens_count[1m])
	// for vLLM and rate(sglang:generation_tokens_histogram_count[1m]) for SGLang.
	// TA notation: fallback λ_req — used when ArrivalRate == 0 (EPP not deployed).
	// λ_dec_fallback = sum(RequestRate) × avg(AvgOutputTokens).
	// Measures completed requests only; undercounts when requests queue in the scheduler.
	// Zero when metrics are unavailable.
	RequestRate float64
}

// ReplicaMetricsMetadata contains freshness information for replica metrics
type ReplicaMetricsMetadata struct {
	// CollectedAt is when the metrics were collected
	CollectedAt time.Time
	// Age is the age of the metrics
	Age time.Duration
	// FreshnessStatus indicates freshness: "fresh", "stale", "unavailable"
	FreshnessStatus string
}

// ModelSaturationAnalysis holds saturation analysis results for a model (across all variants)
type ModelSaturationAnalysis struct {
	ModelID    string
	Namespace  string
	AnalyzedAt time.Time // Timestamp when analysis was performed

	// Aggregated metrics across all variants of this model
	TotalReplicas       int
	NonSaturatedCount   int // Replicas below saturation thresholds
	AvgSpareKvCapacity  float64
	AvgSpareQueueLength float64

	// Scale decision recommendations
	ShouldScaleUp bool

	ScaleUpReason string
	ScaleDownSafe bool // Indicates if scale-down simulation passed

	// Detailed variant breakdown
	VariantAnalyses []VariantSaturationAnalysis
}

// VariantSaturationAnalysis holds saturation analysis for a single variant
type VariantSaturationAnalysis struct {
	VariantName         string
	AcceleratorName     string
	Cost                float64 // Cost per replica for this variant
	ReplicaCount        int
	NonSaturatedCount   int
	MaxKvCacheUsage     float64
	AvgKvCacheUsage     float64 // Mean KV cache utilization across all replicas (for V1 Utilization)
	MaxQueueLength      int
	AvgSpareKvCapacity  float64
	AvgSpareQueueLength float64
	SaturatedReplicas   []string // Pod names of saturated replicas
}

// DecisionStep represents a single step in the decision pipeline.
// Each pipeline stage (saturation analysis, resource limiting, etc.) adds its own step.
type DecisionStep struct {
	// Name identifies the pipeline stage (e.g., "saturation", "limiter", "enforcer")
	Name string
	// Action is the action determined by this step
	Action SaturationAction
	// TargetReplicas is the target replicas after this step
	TargetReplicas int
	// Reason explains why this step made its decision
	Reason string
	// WasConstrained is true if this step modified the previous step's target
	WasConstrained bool
	// Timestamp when this step was executed
	Timestamp metav1.Time
}

// VariantDecision represents the scaling decision for a single variant.
//
// This type serves as shared state that flows through the decision pipeline.
// Each pipeline stage (saturation analysis, resource limiting, enforcement)
// reads and modifies the decision, adding its step to DecisionSteps.
//
// Pipeline stages modify the state they own:
//   - Saturation analyzer: sets initial Action, TargetReplicas, SaturationBased
//   - Resource limiter: may constrain TargetReplicas, adds limiting step
//   - Enforcer: applies final constraints (min/max), adds enforcement step
type VariantDecision struct {
	// --- Variant identification ---
	VariantName     string
	Namespace       string
	ModelID         string
	AcceleratorName string
	Cost            float64
	Role            string // "prefill", "decode", "both"

	// --- Scaling state ---
	Action                 SaturationAction
	CurrentReplicas        int
	TargetReplicas         int // Current target (modified by pipeline stages)
	OriginalTargetReplicas int // Original target before resource limiting (for logging)
	DesiredReplicas        int // Original desired replicas from optimizer (from CRD status)

	// --- Resource requirements (for resource limiting) ---
	GPUsPerReplica int // GPUs required per replica
	// SpareCapacity indicates how much spare capacity this variant has.
	// V1: threshold-relative spare KV capacity (AvgSpareKvCapacity), a 0.0-1.0
	//     fraction (0.0 = fully saturated, 1.0 = completely idle).
	// V2: absolute spare in KV-cache tokens, max(0, TotalSupply - TotalDemand/scaleDownBoundary)
	//     from AnalyzerResult — the scale-down companion to RequiredCapacity, in the same
	//     token units (unit is "continuous", matching RequiredCapacityUnit).
	SpareCapacity float64
	// Utilization is the variant-level utilization ratio (0.0-1.0) reported for
	// observability. The exact formula differs by analyzer because V1 and V2
	// reason about saturation differently:
	//   V1: mean of per-replica KvCacheUsage fractions (matches what V1's
	//       per-replica threshold check operates on).
	//   V2: TotalDemand / TotalCapacity from AnalyzerResult (token-demand-based).
	// For uniform-capacity replicas the two are numerically equivalent; for
	// mixed-capacity replicas V2's value is capacity-weighted.
	Utilization float64
	// KvCacheTokensUsed is the sum of TokensInUse across this variant's replicas.
	KvCacheTokensUsed int64
	// KvCacheTokensCapacity is the sum of TotalKvCapacityTokens across this variant's replicas.
	KvCacheTokensCapacity int64
	// RequiredCapacity indicates whether scale-up is needed (>0 means yes).
	// V1: binary (1.0 if shouldScaleUp, else 0.0), model-level.
	// V2: continuous token-based deficit from AnalyzerResult — per-role for P/D
	//     disaggregated models, model-level otherwise.
	// Use RequiredCapacityUnit to disambiguate the units when consuming this field
	// (or its corresponding Prometheus metric).
	RequiredCapacity float64
	// RequiredCapacityUnit describes the unit of RequiredCapacity ("binary" or "continuous").
	// Exposed as the `unit` Prometheus label on wva_required_capacity so dashboards
	// can filter by semantics rather than by which analyzer produced the value.
	//   "binary":     V1 path, value is 0.0 or 1.0
	//   "continuous": V2 path, value is a token-demand magnitude
	RequiredCapacityUnit string
	// ScaleTargetRef references the Deployment/StatefulSet for scheduling constraints
	ScaleTargetRef *autoscalingv2.CrossVersionObjectReference

	// --- Pipeline tracking ---
	// DecisionSteps records each pipeline stage's contribution to the final decision.
	// This replaces the single Reason field with structured multi-step tracking.
	DecisionSteps []DecisionStep

	// decisionReason is the categorized reason used for Prometheus metric labels.
	// Set via SetDecisionReason along with the detailed reason string.
	decisionReason DecisionReason

	// reason contains the detailed human-readable reason for this decision.
	// Used in logs, events, and status updates.
	// Set via SetDecisionReason along with the categorized decisionReason.
	reason string

	// --- Saturation-specific flags ---
	SaturationBased    bool        // True if decision is primarily saturation-driven
	ModelBasedDecision bool        // True if decision considers model-based optimizer
	SafetyOverride     bool        // True if saturation veto overrode model-based decision
	LastRunTime        metav1.Time // Time when decision was made (for status updates)
	SaturationOnly     bool        // True if operating in saturation-only mode (no model-based analysis)

	// --- Allocation state ---
	// CurrentAllocation carries the collected metrics/allocation state
	// This helps the Controller update status without re-collecting metrics
	CurrentAllocation *Allocation

	// --- Resource limiting results ---
	// GPUsAllocated is the number of GPUs allocated by the resource limiter
	GPUsAllocated int
	// WasLimited indicates if the target was constrained by resource limits
	WasLimited bool
	// LimitedBy identifies which limiter constrained the decision (if any)
	LimitedBy string

	// --- Replica bounds ---
	// MinReplicas is the minimum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0).
	MinReplicas *int
	// MaxReplicas is the maximum number of replicas for this variant (from VA spec field).
	// nil means not set (no cap).
	MaxReplicas *int

	// --- Metrics availability ---
	// MetricsAvailable indicates whether saturation metrics were available for this decision
	MetricsAvailable bool
	// MetricsReason is the reason for the MetricsAvailable condition
	MetricsReason string
	// MetricsMessage is the human-readable message for the MetricsAvailable condition
	MetricsMessage string
}

// AddDecisionStep adds a step to the decision pipeline history.
// This should be called by each pipeline stage after modifying the decision.
func (d *VariantDecision) AddDecisionStep(name string, reason string, wasConstrained bool) {
	step := DecisionStep{
		Name:           name,
		Action:         d.Action,
		TargetReplicas: d.TargetReplicas,
		Reason:         reason,
		WasConstrained: wasConstrained,
		Timestamp:      metav1.Now(),
	}
	d.DecisionSteps = append(d.DecisionSteps, step)
}

// LastStep returns the most recent decision step, or nil if none.
func (d *VariantDecision) LastStep() *DecisionStep {
	if len(d.DecisionSteps) == 0 {
		return nil
	}
	return &d.DecisionSteps[len(d.DecisionSteps)-1]
}

// SetDecisionReason sets both the typed reason category and detailed reason string.
// The decisionReason should be one of the DecisionReason* constants.
// The detailedReason provides human-readable context for logs, events, and status.
func (d *VariantDecision) SetDecisionReason(action SaturationAction, decisionReason DecisionReason, detailedReason string) {
	d.Action = action
	d.decisionReason = decisionReason
	d.reason = detailedReason
}

// ReasonCategory returns the categorized reason used for Prometheus metric labels.
func (d *VariantDecision) ReasonCategory() DecisionReason {
	return d.decisionReason
}

// Reason returns the detailed human-readable reason for this decision.
func (d *VariantDecision) Reason() string {
	return d.reason
}

// SaturationAction represents the scaling action
type SaturationAction string

const (
	ActionScaleUp   SaturationAction = "scale-up"
	ActionScaleDown SaturationAction = "scale-down"
	ActionNoChange  SaturationAction = "no-change"
)

// VariantReplicaState holds the current and desired replica counts for a variant
type VariantReplicaState struct {
	VariantName     string
	CurrentReplicas int
	DesiredReplicas int // From optimizer/CRD status, 0 if not set
	// PendingReplicas are pods that exist but are not yet ready to serve traffic
	// (CurrentReplicas - ReadyReplicas). This typically occurs during scale-up when
	// new pods are starting (containers initializing, model loading, health checks).
	// Pod startup can take 2-7 minutes depending on model size and hardware.
	// WVA uses this to prevent cascade scaling - avoiding new scale-up requests
	// while pending pods are still becoming ready.
	PendingReplicas int
	// GPUsPerReplica is the number of GPUs required per replica, extracted from
	// the deployment's container resource requests (nvidia.com/gpu, amd.com/gpu, etc.).
	// Defaults to 1 if no GPU requests are found.
	GPUsPerReplica int
	// Role is the P/D disaggregation role: "prefill", "decode", or "both" (default).
	Role string
	// MinReplicas is the minimum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0, allows scale to zero).
	MinReplicas *int
	// MaxReplicas is the maximum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0, no cap).
	MaxReplicas *int
}

// SaturationAnalyzer analyzes replica saturation metrics and recommends scaling decisions
type SaturationAnalyzer interface {
	// AnalyzeModelSaturation analyzes saturation for all variants of a model
	// Returns saturation analysis with scale-up/scale-down recommendations
	AnalyzeModelSaturation(
		ctx context.Context,
		modelID string,
		namespace string,
		replicaMetrics []ReplicaMetrics,
		config AnalyzerConfig,
	) (*ModelSaturationAnalysis, error)

	// CalculateSaturationTargets determines target replicas per variant based on saturation analysis.
	// Step 1: Pure saturation-based target calculation
	// - Uses ready replica count (those with metrics) to avoid excessive scale-up
	// - Preserves desired replicas when desired ≠ current (from previous optimizer run)
	// - Uses cost-based selection (cheapest for scale-up, most expensive for scale-down)
	// Returns: map[variantName]targetReplicas
	CalculateSaturationTargets(
		saturationAnalysis *ModelSaturationAnalysis,
		variantStates []VariantReplicaState,
	) map[string]int
}
