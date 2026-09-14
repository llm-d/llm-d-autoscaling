package queueingmodel

const (
	// Set the default values when we can't get a server configuration
	// TODO: collect values from servers

	DefaultMaxBatchSize = 256
	DefaultMaxQueueSize = 100

	// DefaultSLOMultiplier is the queueing delay multiplier for inferred SLOs.
	// The SLO allows iteration time (queueing delay) to inflate by k× the idle
	// baseline α, while keeping deterministic work components at their true cost.
	// k=3 corresponds to utilization ρ = 1 - 1/k = 0.67.
	DefaultSLOMultiplier = 3.0

	// DefaultMaxFallbackTTFT caps the observation-based fallback TTFT SLO (ms).
	DefaultMaxFallbackTTFT = 10000.0

	// DefaultMaxFallbackITL caps the observation-based fallback ITL SLO (ms).
	DefaultMaxFallbackITL = 500.0

	// DefaultFallbackHeadroom is the multiplier applied to observed latencies
	// when learned parameters are unavailable and we fall back to observations.
	DefaultFallbackHeadroom = 1.5

	// TuningByAggregatingPodsForVariant is a selection switch used when
	// tuning the queueing model for a variant.
	// true: run the tuner once for an aggregate server of all pods
	// false: run the tuner for all pods of the variant
	TuningByAggregatingPodsForVariant = false
)
