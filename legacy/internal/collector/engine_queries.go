package collector

import (
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// engineSpecificReplicaQueries are the logical query names collected per replica
// whose metric source is the inference engine (vLLM/SGLang). Each is refreshed
// once per present engine and merged back under its logical name.
//
// This is the per-replica subset of registration.EngineSpecificQueries (the
// authoritative list of engine-specific logical queries). Every entry here MUST
// also appear there — TestEngineSpecificReplicaQueriesSubset enforces it so the
// two lists cannot drift. It intentionally omits registration.QueryModelRequestCount,
// which is engine-specific but a scale-to-zero query (collected on demand by the
// enforcer, not per replica in this collector).
var engineSpecificReplicaQueries = []string{
	registration.QueryKvCacheUsage,
	registration.QueryQueueLength,
	registration.QueryCacheConfigInfo,
	registration.QueryAvgOutputTokens,
	registration.QueryAvgInputTokens,
	registration.QueryPrefixCacheHitRate,
	registration.QueryAvgTTFT,
	registration.QueryAvgITL,
	registration.QueryGenerationTokenRate,
	registration.QueryKvUsageInstant,
	registration.QueryRequestRate,
}

// agnosticReplicaQueries are the logical query names collected per replica whose
// metric source is engine-independent (the EPP inference scheduler). They are
// refreshed once and shared across engines.
var agnosticReplicaQueries = []string{
	registration.QuerySchedulerDispatchRate,
}

// buildEngineQueryList returns the physical query names to refresh for the given
// set of present engines: each agnostic query once, plus each engine-specific
// query for every present engine. For a single-engine vLLM model this is exactly
// the agnostic queries plus the bare engine-specific names (unchanged behavior).
func buildEngineQueryList(engines []inferenceengine.Engine, engineSpecific, agnostic []string) []string {
	queries := make([]string, 0, len(agnostic)+len(engineSpecific)*len(engines))
	queries = append(queries, agnostic...)
	for _, logical := range engineSpecific {
		for _, eng := range engines {
			queries = append(queries, registration.EngineQuery(eng, logical))
		}
	}
	return queries
}

// mergeEngineResults re-keys engine-specific query results under their logical
// names so downstream per-pod processing is engine-agnostic.
//
//   - Single engine: the physical result is aliased to the logical name (a no-op
//     for vLLM, where physical == logical; a rename for SGLang). The physical key
//     is left in place so engine-specific consumers (e.g. the SGLang cache-config
//     pass) can still read it.
//   - Multiple engines: the per-engine series are concatenated into a new result
//     under the logical name. Series are disjoint by pod, so concatenation is safe.
func mergeEngineResults(results map[string]*source.MetricResult, engines []inferenceengine.Engine, logicalNames []string) {
	if len(engines) == 1 {
		eng := engines[0]
		for _, logical := range logicalNames {
			phys := registration.EngineQuery(eng, logical)
			if phys == logical {
				continue // vLLM: already keyed by logical name.
			}
			if r := results[phys]; r != nil {
				r.QueryName = logical
				results[logical] = r
			}
		}
		return
	}

	for _, logical := range logicalNames {
		var merged *source.MetricResult
		var firstErr error
		for _, eng := range engines {
			r := results[registration.EngineQuery(eng, logical)]
			if r == nil {
				continue
			}
			if merged == nil {
				merged = &source.MetricResult{QueryName: logical, CollectedAt: r.CollectedAt}
			}
			merged.Values = append(merged.Values, r.Values...)
			if firstErr == nil && r.Error != nil {
				firstErr = r.Error
			}
			if !r.CollectedAt.IsZero() && (merged.CollectedAt.IsZero() || r.CollectedAt.Before(merged.CollectedAt)) {
				merged.CollectedAt = r.CollectedAt
			}
		}
		if merged != nil {
			// Only surface an error when NO engine produced any series. A partial
			// success — one engine errored (e.g. transient timeout) while another
			// returned values — must not mark the merged result errored, or the
			// downstream HasError() checks would discard the healthy engine's pods
			// and blackhole scaling for the whole mixed-engine model.
			if len(merged.Values) == 0 {
				merged.Error = firstErr
			}
			results[logical] = merged
		}
	}
}

// containsEngine reports whether the engine set includes the given engine.
func containsEngine(engines []inferenceengine.Engine, target inferenceengine.Engine) bool {
	return slices.Contains(engines, target)
}
