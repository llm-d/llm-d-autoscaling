package domain

// MetricsValidationResult contains the result of metrics availability check
type MetricsValidationResult struct {
	Available bool
	Reason    string
	Message   string
}

// OptimizerMetrics contains raw metrics collected from Prometheus/EPP for the optimizer.
// These metrics are used by the controller to assemble the Allocation struct.
type OptimizerMetrics struct {
	// ArrivalRate is the arrival rate in requests per minute
	ArrivalRate float64
	// AvgInputTokens is the average number of input tokens per request
	AvgInputTokens float64
	// AvgOutputTokens is the average number of output tokens per request
	AvgOutputTokens float64
	// TTFTSeconds is the average time to first token in seconds (will be converted to milliseconds by controller)
	TTFTSeconds float64
	// ITLSeconds is the average inter-token latency in seconds (will be converted to milliseconds by controller)
	ITLSeconds float64
}
