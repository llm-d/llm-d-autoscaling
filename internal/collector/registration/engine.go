// This file makes query registration engine-aware. Each engine-specific logical
// query (one whose metric source is the inference engine itself) has a per-engine
// template variant registered under an engine-scoped name. Engine-agnostic
// queries (e.g. the EPP scheduler flow-control queries) are registered once under
// their bare logical name and shared across engines.

package registration

import (
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// EngineSpecificQueries lists the logical query names whose metric source is the
// inference engine (vLLM/SGLang), and which therefore have a per-engine template
// variant. Any query not in this set is engine-agnostic and shared across engines.
//
// This is the authoritative list. The collector keeps a per-replica subset of it
// (collector.engineSpecificReplicaQueries), which omits QueryModelRequestCount
// (a scale-to-zero query). The two are kept from drifting by
// TestEngineSpecificReplicaQueriesSubset: every entry in the collector's list
// must appear here.
var EngineSpecificQueries = []string{
	QueryKvCacheUsage,
	QueryQueueLength,
	QueryCacheConfigInfo,
	QueryAvgOutputTokens,
	QueryAvgInputTokens,
	QueryPrefixCacheHitRate,
	QueryAvgTTFT,
	QueryAvgITL,
	QueryGenerationTokenRate,
	QueryKvUsageInstant,
	QueryRequestRate,
	QueryModelRequestCount,
}

var engineSpecificSet = func() map[string]bool {
	m := make(map[string]bool, len(EngineSpecificQueries))
	for _, q := range EngineSpecificQueries {
		m[q] = true
	}
	return m
}()

// IsEngineSpecific reports whether a logical query name has per-engine variants.
func IsEngineSpecific(logical string) bool {
	return engineSpecificSet[logical]
}

// EngineQuery returns the physical registered query name for a logical query
// under a given engine. vLLM — and any engine-agnostic query — keeps the bare
// logical name for backward compatibility; other engines get an
// "<engine>/<logical>" prefix (e.g. "sglang/kv_cache_usage").
func EngineQuery(engine inferenceengine.Engine, logical string) string {
	if engine == inferenceengine.EngineVLLM || !IsEngineSpecific(logical) {
		return logical
	}
	return engine.String() + "/" + logical
}

// registerForEngine registers a query template under the engine-scoped name for
// the given engine. The supplied template's Name field must hold the logical
// query name; it is rewritten to the physical name before registration.
// engine is part of the engine-aware registration API: callers pass an explicit
// engine so any future backend can register its own variant. Only SGLang uses
// this path today (vLLM keeps bare logical names), hence unparam is suppressed.
func registerForEngine(registry *source.QueryList, engine inferenceengine.Engine, tmpl source.QueryTemplate) { //nolint:unparam // see comment above
	tmpl.Name = EngineQuery(engine, tmpl.Name)
	registry.MustRegister(tmpl)
}

// queryTypeByLogicalName maps each logical query name to the query_type label
// used by the metrics-collection error counter. Grouped queries share a label
// (e.g. the token-shape queries) to keep the counter's label cardinality low.
var queryTypeByLogicalName = map[string]string{
	QueryKvCacheUsage:          constants.QueryTypeKVCache,
	QueryQueueLength:           constants.QueryTypeQueueLength,
	QueryCacheConfigInfo:       constants.QueryTypeCacheConfig,
	QueryAvgOutputTokens:       constants.QueryTypeTokenMetrics,
	QueryAvgInputTokens:        constants.QueryTypeTokenMetrics,
	QueryPrefixCacheHitRate:    constants.QueryTypeTokenMetrics,
	QuerySchedulerDispatchRate: constants.QueryTypeDispatchRate,
	QueryAvgTTFT:               constants.QueryTypeLatency,
	QueryAvgITL:                constants.QueryTypeLatency,
	QueryGenerationTokenRate:   constants.QueryTypeThroughput,
	QueryKvUsageInstant:        constants.QueryTypeThroughput,
	QueryRequestRate:           constants.QueryTypeThroughput,
	QueryModelArrivalRate:      constants.QueryTypeArrivalRate,
	QueryModelRequestCount:     constants.QueryTypeRequestCount,
	QuerySchedulerQueueSize:    constants.QueryTypeSchedulerQueue,
	QuerySchedulerQueueBytes:   constants.QueryTypeSchedulerQueue,
}

// QueryTypeFor returns the query_type label for a logical or engine-scoped
// physical query name (e.g. "kv_cache_usage" or "sglang/kv_cache_usage").
// Unknown names fall back to the bare name so the error counter still records
// them instead of dropping the failure silently.
func QueryTypeFor(queryName string) string {
	if qt, ok := queryTypeByLogicalName[queryName]; ok {
		return qt
	}
	// Strip an engine prefix ("sglang/…") and retry with the logical name.
	if idx := strings.Index(queryName, "/"); idx >= 0 {
		if qt, ok := queryTypeByLogicalName[queryName[idx+1:]]; ok {
			return qt
		}
	}
	return queryName
}
