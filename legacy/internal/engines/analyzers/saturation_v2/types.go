package saturation_v2

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"

// learnedFromLive indicates a capacity record was derived from live metrics.
const learnedFromLive = "live"

// k2Source identifies which priority level produced the compute-bound capacity
// estimate for a replica.
type k2Source int

const (
	k2SrcObserved   k2Source = iota + 1 // queue saturated: tokensInUse
	k2SrcHistorical                     // rolling average from prior observations
	k2SrcDerived                        // estimated from deployment args
	k2SrcFallback                       // fallback to k1 (memory-bound)
)

var k2Labels = map[k2Source]string{
	k2SrcObserved:   "P1-obs",
	k2SrcHistorical: "P2-hist",
	k2SrcDerived:    "P3-k2",
	k2SrcFallback:   "P4-k1",
}

const (
	satReasonP0Store = "P0-store" // capacity from store or compatible-variant record; no live replicas
	// satReasonNoData marks a variant with no live replicas and no store record.
	// It aliases the shared pipeline sentinel so this producer and the engine's
	// liveness gate (pipeline.ResultIsInformative) cannot drift apart.
	satReasonNoData = pipeline.ReasonNoData
)

// ReplicaCapacity holds the per-replica capacity breakdown computed by
// the V2 saturation analyzer. It is internal to the analyzer and not
// part of the public interfaces package.
type ReplicaCapacity struct {
	PodName               string
	VariantName           string
	AcceleratorName       string
	TokensInUse           int64
	TotalKvCapacityTokens int64
	MemoryBoundCapacity   int64    // k1: KV-cache-limited capacity
	ComputeBoundCapacity  int64    // k2: compute/scheduling-limited capacity
	K2Priority            k2Source // how k2 was computed
	EffectiveCapacity     int64    // min(k1, k2)
	IsSaturated           bool
	// ReplicaDemand is the replica's resident KV tokens — TokensInUse on the main
	// path, kvCacheUsage * effectiveCapacity on the fallback path — plus the
	// role-aware waiting-queue footprint: queueLength * avgInputTokens for
	// prefill replicas, and queueLength * (avgInputTokens + avgOutputTokens) for
	// decode/"both". See waitingQueueDemand.
	ReplicaDemand int64
}

// classifyOutputLength returns a workload bucket name based on average
// output token length. The buckets are used to key compute-capacity (k2)
// history, since k2 depends heavily on generation length.
//
// Buckets:
//
//	"short"  — avgOutput in [0, 100)
//	"medium" — avgOutput in [100, 500)
//	"long"   — avgOutput >= 500
func classifyOutputLength(avgOutputTokens float64) string {
	switch {
	case avgOutputTokens < ShortOutputThreshold:
		return "short"
	case avgOutputTokens < MediumOutputThreshold:
		return "medium"
	default:
		return "long"
	}
}
