package throughput

import "time"

const (
	// AnalyzerName is the canonical name for the throughput analyzer.
	AnalyzerName = "throughput"

	// DefaultShapeChangeTolerance is the fractional change in IL or OL that
	// triggers a shape bucket change, clearing the observation window and
	// scheduling a new ITL model fit.
	// A value of 0.20 means a 20% shift in either dimension resets calibration.
	DefaultShapeChangeTolerance = 0.20

	// DefaultWindowMaxSize is the maximum number of (k, ITL) observations
	// retained in the rolling window. When the window is full, the oldest
	// observation is evicted on each new Add call.
	DefaultWindowMaxSize = 20

	// DefaultObservationMaxAge is the maximum age of an observation in the
	// window. Observations older than this are pruned regardless of window
	// fullness, ensuring that stale data from a previous load pattern does
	// not contaminate the current fit.
	DefaultObservationMaxAge = 30 * time.Minute

	// DefaultMinSamples is the minimum number of valid observations required
	// before the window is considered Ready for OLS fitting.
	DefaultMinSamples = 10

	// DefaultMinKSpread is the minimum required spread (max_k - min_k) across
	// observations in the window before the window is considered Ready.
	// A spread of at least 0.30 ensures the linear fit spans a meaningful
	// portion of the KV utilization range and is not extrapolating from a
	// single operating point.
	DefaultMinKSpread = 0.30

	// DefaultMinObservableK is the lower bound on KV utilization for accepted
	// observations. Below this threshold the ITL signal is noisy (few
	// concurrent requests, high variance) and unreliable for fitting.
	DefaultMinObservableK = 0.15

	// DefaultMaxObservableK is the upper bound on KV utilization for accepted
	// observations. Above this threshold the system approaches saturation and
	// the linear ITL model may no longer hold.
	DefaultMaxObservableK = 0.85

	// DefaultMinTokensPerRequest is the minimum plausible value for AvgOutputTokens
	// or AvgInputTokens. Values at or below this threshold indicate the metric
	// is unavailable or zero-padded and are flagged as a sanity issue.
	DefaultMinTokensPerRequest = 1.0

	// fallbackKSat is the k_sat used when the input carries no saturation config to
	// read one from. Per-replica capacity is evaluated at the configured k_sat
	// (resolveKSat); this is only the no-config path.
	//
	// It must equal config.DefaultKvCacheThreshold, which is the value
	// ApplyDefaults writes into KvCacheThreshold when the field is unset, so that
	// every path agrees on the definition of "full". The literal is duplicated
	// rather than imported because internal/config is a lower layer than the
	// analyzers (see throughputAnalyzerName in config.go, the same duplication in
	// the opposite direction); TestFallbackKSatMatchesConfigDefault pins the two
	// together so a change to either fails here.
	//
	// Not to be confused with a watermark: ScaleUpThreshold (0.85) and
	// ScaleDownBoundary (0.70) are margins around the steady state, applied by the
	// engine to required/spare capacity after Analyze returns. This constant
	// previously held 0.85 for exactly that reason and was wrong.
	fallbackKSat = 0.80

	// DefaultBaselineITLSec is the hardware baseline inter-token latency (seconds/token)
	// used in tier-2 estimation when the OLS window is not yet ready.
	// Derived from H100 SXM5 measurements at near-zero KV load; workload-independent.
	DefaultBaselineITLSec = 0.006

	// DefaultQueueDrainFactor controls how aggressively queued requests count as
	// decode demand. The assumed drain time is QueueDrainFactor × ITL(k_sat) × avgOL;
	// after avgOL cancels, queue demand = QueueSize / (QueueDrainFactor × ITL(k_sat)).
	// A factor of 2.0 bounds per-request queueing time to ≤ 2 × ITL(k_sat) × avgOL.
	DefaultQueueDrainFactor = 2.0

	// DefaultMinDecodeOLForLocalDemand is the minimum AvgOutputTokens required
	// before the k*-based local demand estimator is applied. The estimator derives
	// λ_dec = N_dec(k*) / ITL(k*) where N_dec is approximated from KV utilization
	// as k* × KV_max / KVreq. This approximation only holds in the decode-dominated
	// regime (N_pre ≈ 1, TA-supply.md §3.1), which requires sufficiently long OL.
	// When OL ≈ 0, KV usage is from prefill rather than decode; the formula then
	// produces spurious non-zero demand instead of the correct λ_dec = 0.
	DefaultMinDecodeOLForLocalDemand = 20.0

	// DefaultGPSMismatchThresholdPct is the maximum tolerable percentage error between
	// the model-predicted decode rate μ_dec(k*) and the directly observed GPS
	// (GenerationTokenRate). Errors above this threshold at k* ≥ DefaultGPSMinKForVerification
	// indicate the ITL model may be wrong and trigger SpareCapacity suppression.
	DefaultGPSMismatchThresholdPct = 15.0

	// DefaultGPSMinKForVerification is the minimum KV utilization (k*) required before
	// GPS verification is applied. Below this threshold the in-flight count N_dec is
	// small and percentage errors on GPS are unreliable.
	DefaultGPSMinKForVerification = 0.30

	// DefaultNearKSatMargin is the margin below the resolved k_sat at which a
	// replica is considered "near saturation" for GPS sanity diagnostics. At k*
	// above k_sat - DefaultNearKSatMargin the GPS signal is near-oracle quality:
	// any model–GPS discrepancy is a strong indicator of a model error. A genuine
	// margin, unlike the k_sat it is measured from, so it stays a constant.
	DefaultNearKSatMargin = 0.10

	// DefaultNearKSatITLResidualThreshold is the fractional ITL residual above which
	// the observed AvgITL is considered inconsistent with the ITL model near k_sat.
	// A large residual points to bad data points or model drift.
	DefaultNearKSatITLResidualThreshold = 0.20

	// DefaultNearKSatNDecResidualThreshold is the fractional N_dec cross-check threshold
	// used in near-k_sat GPS sanity. If ITL residual is small but GPS × AvgITL disagrees
	// with KV-derived N_dec, the workload shape (KVreq via IL, OL, or hit rate) may be wrong.
	DefaultNearKSatNDecResidualThreshold = 0.20

	// DefaultGPSMismatchClearThreshold is the number of consecutive reconcile cycles
	// with a GPS mismatch before the observation window is cleared for recalibration.
	// Requiring N consecutive mismatches filters transient GPS noise while still
	// breaking persistent calibration lock. The counter resets to zero whenever the
	// window is cleared (shape change or threshold reached) so it is always bound to
	// the current window's lifetime.
	DefaultGPSMismatchClearThreshold = 3

	// itlReason* are the values set on VariantCapacity.Reason. The first four are
	// set by resolveITLModel; itlReasonScaleFromZero is set by the scale-from-zero
	// complement. They appear in the "analyzer-result" structured log line.
	itlReasonT1OLS     = "T1-ols"     // tier-1: OLS fit from live observations
	itlReasonT2Default = "T2-default" // tier-2: constrained OLS with default B baseline
	itlReasonT2Pinned  = "T2-pinned"  // tier-2: constrained OLS with previously fitted B
	itlReasonT2Failed  = "T2-failed"  // all paths exhausted; no model for this cycle
	// itlReasonScaleFromZero marks the PRC-only VariantCapacity emitted for a
	// previously-live variant now at zero replicas: its per-replica capacity is the
	// persisted last-good value, not a fresh fit. Not produced by resolveITLModel.
	itlReasonScaleFromZero = "T-sfz"

	// roleUnmodeledReason marks a RoleCapacity (not a VariantCapacity) whose
	// TotalDemand is structurally zero because this analyzer has no demand
	// model for the role at all -- set on prefill in aggregateRoleCapacities.
	// Duplicated from pipeline.ReasonRoleUnmodeled rather than imported: this
	// package cannot import internal/engines/pipeline (pipeline imports
	// internal/config for its quota/enforcer plumbing, and internal/config's
	// own in-package tests import this package for the k_sat drift guard --
	// the same test-binary import cycle resolveKSat's interface trick works
	// around, but a bare string constant has no equivalent trick). Pinned
	// against drift by TestRoleUnmodeledReasonMatchesPipeline.
	roleUnmodeledReason = "role-unmodeled"
)
