package throughput

import (
	"math"
	"time"
)

// WorkloadShape captures the stable (IL, OL) characterization for a calibration period.
//
// KVreq = ILeff + OL/2 is the time-averaged per-in-flight-request KV footprint.
// ILeff is used for both N(k*) (current fleet) and N(k_sat) (new replica capacity).
//
// Rationale for using ILeff for new replicas: with cache-aware scheduling (EPP
// prefix routing), a new replica warms up quickly and operates at approximately
// fleet-average ILeff in steady state. Using IL (cold-cache pessimism) would
// chronically over-estimate KV demand per request and lead to over-scaling.
// When prefix caching is disabled, PrefixHitRate=0 and ILeff=IL automatically,
// so the formula is correct in that case without special handling.
type WorkloadShape struct {
	// AvgInputTokens is the average prompt length per request in tokens (IL, tok/req).
	AvgInputTokens float64
	// AvgOutputTokens is the average generation length per request in tokens (OL, tok/req).
	AvgOutputTokens float64
	// PrefixHitRate is the prefix cache hit fraction (0.0–1.0).
	// Zero when prefix caching is disabled or hit-rate metrics are unavailable.
	PrefixHitRate float64
	// ILeff is the effective input length after prefix cache reduction:
	//   ILeff = AvgInputTokens × (1 − PrefixHitRate)
	ILeff float64
	// KVreq is the time-averaged KV token footprint per in-flight request:
	//   KVreq = ILeff + AvgOutputTokens/2
	KVreq float64
}

// newWorkloadShape constructs a WorkloadShape and computes the derived fields.
// hitRate values outside [0, 1] are clamped; NaN is treated as 0.0.
func newWorkloadShape(il, ol, hitRate float64) WorkloadShape {
	if math.IsNaN(hitRate) || hitRate < 0 {
		hitRate = 0
	}
	if hitRate > 1 {
		hitRate = 1
	}
	ileff := il * (1 - hitRate)
	return WorkloadShape{
		AvgInputTokens:  il,
		AvgOutputTokens: ol,
		PrefixHitRate:   hitRate,
		ILeff:           ileff,
		KVreq:           ileff + ol/2,
	}
}

// IsZero returns true when the shape has not been populated (zero IL and OL).
func (s WorkloadShape) IsZero() bool {
	return s.AvgInputTokens == 0 && s.AvgOutputTokens == 0
}

// Within returns true if both IL and OL of s are within ±tolerance of other.
// tolerance is fractional: 0.20 means ±20%.
func (s WorkloadShape) Within(other WorkloadShape, tolerance float64) bool {
	return withinTolerance(s.AvgInputTokens, other.AvgInputTokens, tolerance) &&
		withinTolerance(s.AvgOutputTokens, other.AvgOutputTokens, tolerance)
}

// withinTolerance returns true if |a - b| / b ≤ tolerance, or if both are zero.
func withinTolerance(a, b, tolerance float64) bool {
	if b == 0 {
		return a == 0
	}
	return math.Abs(a-b)/b <= tolerance
}

// ITLObservation is a single (k, ITL_obs) data point collected from one replica
// during one reconcile cycle. Used to calibrate the linear ITL model: ITL(k) = A·k + B.
type ITLObservation struct {
	// K is the KV cache utilization fraction (0.0–1.0) observed on the replica.
	K float64
	// ITLSec is the observed average inter-token latency in seconds/token.
	ITLSec float64
	// Timestamp is when this observation was collected.
	Timestamp time.Time
}

// SanityIssue is a diagnostic tag describing a metric quality problem detected
// during a reconcile cycle.
type SanityIssue string

const (
	// SanityIssueNoReplicas indicates the model has no replica metrics at all.
	SanityIssueNoReplicas SanityIssue = "no_replicas"

	// SanityIssueMissingKV indicates TotalKvCapacityTokens is zero or negative,
	// meaning the KV cache configuration metric (cache_config_info) is unavailable.
	SanityIssueMissingKV SanityIssue = "missing_kv_capacity"

	// SanityIssueKVOutOfRange indicates KvUsageInstant (k*) is outside [0, 1].
	SanityIssueKVOutOfRange SanityIssue = "kv_utilization_out_of_range"

	// SanityIssueITLNonPositive indicates AvgITL is zero, negative, or NaN.
	// This prevents adding any (k, ITL) observations from affected pods.
	SanityIssueITLNonPositive SanityIssue = "itl_non_positive"

	// SanityIssueMissingShape indicates AvgOutputTokens or AvgInputTokens is
	// at or below DefaultMinTokensPerRequest, making shape tracking unreliable.
	SanityIssueMissingShape SanityIssue = "missing_shape_metrics"

	// SanityIssueStaleMetrics indicates the replica's metrics are marked stale
	// (Metadata.FreshnessStatus == "stale"). Stale data should not be used
	// for calibration.
	SanityIssueStaleMetrics SanityIssue = "stale_metrics"
)

// SanityReport summarises metric quality issues detected across a set of replica
// metrics during one reconcile cycle.
type SanityReport struct {
	// Issues lists the distinct issue types found. Empty when all metrics are healthy.
	Issues []SanityIssue
	// AffectedPods lists the pod names that had at least one issue.
	AffectedPods []string
}

// OK returns true when no issues were found.
func (r SanityReport) OK() bool {
	return len(r.Issues) == 0
}

// Has returns true when the report contains the given issue type.
func (r SanityReport) Has(issue SanityIssue) bool {
	for _, i := range r.Issues {
		if i == issue {
			return true
		}
	}
	return false
}

// ThroughputVariantState is a read-only snapshot of per-variant state.
// Returned by ThroughputAnalyzer.VariantState for tests and logging.
type ThroughputVariantState struct {
	// Shape is the current workload shape bucket for this variant.
	Shape WorkloadShape
	// ObservationReady is true when the window has enough data for OLS fitting.
	ObservationReady bool
	// KSpread is max_k - min_k over current observations (0 when window is empty).
	KSpread float64
	// SampleCount is the number of observations currently in the window.
	SampleCount int
	// LastSanityReport is the sanity report from the most recent Observe call.
	LastSanityReport SanityReport

	// ITLModel is the ITL model last resolved during Analyze for this variant
	// (tier-1 OLS or tier-2 constrained OLS). IsZero() when no model has been resolved yet.
	ITLModel ITLModel
	// PerReplicaSupply is mean(μ_dec_sat) across replicas from the last Analyze call
	// in tokens/sec. Zero when no supply could be computed.
	PerReplicaSupply float64
	// TotalSupply is Σ μ_dec_sat across replicas from the last Analyze call
	// in tokens/sec. Zero when no supply could be computed.
	TotalSupply float64
	// Demand is λ_dec (total decode token demand) from the last Analyze call
	// in tokens/sec. Zero when no demand signal was available.
	Demand float64
	// Role is the P/D disaggregation role: "prefill", "decode", "both", or ""
	// (non-disaggregated). Populated from VariantStates in Analyze(); empty until first Analyze call.
	Role string

	// LastFittedB is the B coefficient from the most recent successful Tier-1 OLS fit.
	// Zero when no Tier-1 fit has occurred yet (HasFittedB is false in that case).
	LastFittedB float64
	// HasFittedB is true when at least one Tier-1 OLS fit has succeeded for this variant.
	HasFittedB bool
}
