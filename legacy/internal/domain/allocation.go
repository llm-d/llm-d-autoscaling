package domain

// Allocation describes the current resource allocation for a model variant.
type Allocation struct {
	// Accelerator is the type of accelerator currently allocated.
	Accelerator string `json:"accelerator"`

	// NumReplicas is the number of replicas currently allocated.
	NumReplicas int `json:"numReplicas"`

	// MaxBatch is the maximum batch size currently allocated.
	MaxBatch int `json:"maxBatch"`

	// ITLAverage is the average inter token latency for the current allocation.
	ITLAverage string `json:"itlAverage"`

	// TTFTAverage is the average time to first token for the current allocation
	TTFTAverage string `json:"ttftAverage"`

	// Load describes the workload characteristics for the current allocation.
	Load LoadProfile `json:"load"`
}

// LoadProfile represents the configuration for workload characteristics,
// including the rate of incoming requests (ArrivalRate) and the average
// length of each request (AvgLength). Both fields are specified as strings
// to allow flexible input formats.
type LoadProfile struct {
	// ArrivalRate is the rate of incoming requests in inference server.
	ArrivalRate string `json:"arrivalRate"`

	// AvgInputTokens is the average number of input(prefill) tokens per request in inference server.
	AvgInputTokens string `json:"avgInputTokens"`

	// AvgOutputTokens is the average number of output(decode) tokens per request in inference server.
	AvgOutputTokens string `json:"avgOutputTokens"`
}
