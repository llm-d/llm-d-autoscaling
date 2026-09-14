package saturation_v2

import "time"

const (
	// RollingAverageWindowSize is the number of samples retained for compute
	// capacity (k2) history per workload bucket.
	RollingAverageWindowSize = 10

	// CapacityStalenessTimeout is the duration after which a stored capacity
	// record is considered stale and should be refreshed from live data.
	CapacityStalenessTimeout = 30 * time.Minute

	// CapacityEvictionTimeout is the duration after which unused capacity
	// store records are eligible for removal. This is intentionally long
	// because historical capacity knowledge is valuable for zero-replica
	// estimation and cross-variant matching (e.g., a variant may be at
	// zero replicas over a weekend and scale back up Monday).
	CapacityEvictionTimeout = 7 * 24 * time.Hour

	// HistoryEvictionTimeout is the duration after which unused k2 history
	// entries are eligible for removal. Shorter than capacity eviction
	// because workload patterns shift and stale k2 observations from a
	// very different workload can mislead scaling decisions.
	HistoryEvictionTimeout = 24 * time.Hour

	// BytesPerToken is the approximate number of bytes per LLM token.
	// Used to convert scheduler queue bytes to estimated token count.
	// Based on the OpenAI tiktoken observation that each token corresponds
	// to roughly 4 bytes of text. This is conservative (yields an upper
	// bound on token count) since modern tokenizers achieve 5-6 chars/token.
	BytesPerToken = 4

	// ShortOutputThreshold is the upper bound (exclusive) for the "short"
	// output-length bucket used for k2 history keying.
	ShortOutputThreshold = 100

	// MediumOutputThreshold is the upper bound (exclusive) for the "medium"
	// output-length bucket used for k2 history keying.
	MediumOutputThreshold = 500
)
