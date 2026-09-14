package collector

import (
	"errors"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestBuildEngineQueryList_VLLMOnly(t *testing.T) {
	q := buildEngineQueryList([]inferenceengine.Engine{inferenceengine.EngineVLLM}, engineSpecificReplicaQueries, agnosticReplicaQueries)

	// Agnostic queries appear once, engine-specific queries use their bare names.
	if !contains(q, registration.QuerySchedulerDispatchRate) {
		t.Errorf("expected agnostic scheduler dispatch query in %v", q)
	}
	if !contains(q, registration.QueryKvCacheUsage) {
		t.Errorf("expected bare vLLM kv_cache_usage in %v", q)
	}
	if contains(q, "sglang/"+registration.QueryKvCacheUsage) {
		t.Errorf("did not expect SGLang variant for vLLM-only model in %v", q)
	}
}

func TestBuildEngineQueryList_SGLangOnly(t *testing.T) {
	q := buildEngineQueryList([]inferenceengine.Engine{inferenceengine.EngineSGLang}, engineSpecificReplicaQueries, agnosticReplicaQueries)

	if !contains(q, "sglang/"+registration.QueryKvCacheUsage) {
		t.Errorf("expected SGLang kv_cache_usage in %v", q)
	}
	if contains(q, registration.QueryKvCacheUsage) {
		t.Errorf("did not expect bare vLLM kv_cache_usage for SGLang-only model in %v", q)
	}
	// Agnostic query is still shared (bare).
	if !contains(q, registration.QuerySchedulerDispatchRate) {
		t.Errorf("expected agnostic scheduler dispatch query in %v", q)
	}
}

func TestBuildEngineQueryList_Mixed(t *testing.T) {
	q := buildEngineQueryList(
		[]inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang},
		engineSpecificReplicaQueries, agnosticReplicaQueries)

	if !contains(q, registration.QueryKvCacheUsage) || !contains(q, "sglang/"+registration.QueryKvCacheUsage) {
		t.Errorf("expected both vLLM and SGLang kv_cache_usage in %v", q)
	}
	// Agnostic query appears exactly once even with multiple engines.
	count := 0
	for _, name := range q {
		if name == registration.QuerySchedulerDispatchRate {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected agnostic query exactly once, got %d", count)
	}
}

func TestMergeEngineResults_VLLMIdentity(t *testing.T) {
	results := map[string]*source.MetricResult{
		registration.QueryKvCacheUsage: {QueryName: registration.QueryKvCacheUsage, Values: []source.MetricValue{{Value: 0.5}}},
	}
	mergeEngineResults(results, []inferenceengine.Engine{inferenceengine.EngineVLLM}, engineSpecificReplicaQueries)

	r := results[registration.QueryKvCacheUsage]
	if r == nil || len(r.Values) != 1 || r.Values[0].Value != 0.5 {
		t.Fatalf("vLLM result should be unchanged, got %+v", r)
	}
}

func TestMergeEngineResults_SGLangRename(t *testing.T) {
	physical := "sglang/" + registration.QueryKvCacheUsage
	results := map[string]*source.MetricResult{
		physical: {QueryName: physical, Values: []source.MetricValue{{Value: 0.7}}},
	}
	mergeEngineResults(results, []inferenceengine.Engine{inferenceengine.EngineSGLang}, engineSpecificReplicaQueries)

	r := results[registration.QueryKvCacheUsage]
	if r == nil || len(r.Values) != 1 || r.Values[0].Value != 0.7 {
		t.Fatalf("SGLang result should be aliased to the logical name, got %+v", r)
	}
	if r.QueryName != registration.QueryKvCacheUsage {
		t.Errorf("QueryName should be re-keyed to logical, got %q", r.QueryName)
	}
}

func TestMergeEngineResults_MixedConcatenates(t *testing.T) {
	physical := "sglang/" + registration.QueryKvCacheUsage
	results := map[string]*source.MetricResult{
		registration.QueryKvCacheUsage: {QueryName: registration.QueryKvCacheUsage, Values: []source.MetricValue{{Value: 0.5, Labels: map[string]string{"pod": "vllm-0"}}}},
		physical:                       {QueryName: physical, Values: []source.MetricValue{{Value: 0.7, Labels: map[string]string{"pod": "sglang-0"}}}},
	}
	mergeEngineResults(results,
		[]inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang},
		engineSpecificReplicaQueries)

	r := results[registration.QueryKvCacheUsage]
	if r == nil || len(r.Values) != 2 {
		t.Fatalf("expected 2 merged values, got %+v", r)
	}
}

// TestEngineSpecificReplicaQueriesSubset guards against drift between the
// collector's per-replica engine-specific query list and the authoritative
// registration.EngineSpecificQueries. Every per-replica query must be a known
// engine-specific query; otherwise mergeEngineResults would silently fail to
// re-key it under its logical name for SGLang.
func TestEngineSpecificReplicaQueriesSubset(t *testing.T) {
	authoritative := make(map[string]bool, len(registration.EngineSpecificQueries))
	for _, q := range registration.EngineSpecificQueries {
		authoritative[q] = true
	}
	for _, q := range engineSpecificReplicaQueries {
		if !authoritative[q] {
			t.Errorf("engineSpecificReplicaQueries contains %q which is not in registration.EngineSpecificQueries — lists have drifted", q)
		}
		if !registration.IsEngineSpecific(q) {
			t.Errorf("engineSpecificReplicaQueries contains %q but registration.IsEngineSpecific(%q) is false", q, q)
		}
	}
	// QueryModelRequestCount is engine-specific but intentionally excluded from the
	// per-replica list (it is a scale-to-zero query, not a per-replica metric).
	if contains(engineSpecificReplicaQueries, registration.QueryModelRequestCount) {
		t.Errorf("engineSpecificReplicaQueries should not contain the scale-to-zero query %q", registration.QueryModelRequestCount)
	}
}

// TestMergeEngineResults_MixedPartialError verifies that when one engine errors
// but another returns values, the merged result is NOT marked errored — otherwise
// the downstream HasError() checks would discard the healthy engine's pods and
// blackhole scaling for the whole mixed-engine model.
func TestMergeEngineResults_MixedPartialError(t *testing.T) {
	physical := "sglang/" + registration.QueryKvCacheUsage
	results := map[string]*source.MetricResult{
		// vLLM succeeded with a value.
		registration.QueryKvCacheUsage: {QueryName: registration.QueryKvCacheUsage, Values: []source.MetricValue{{Value: 0.5, Labels: map[string]string{"pod": "vllm-0"}}}},
		// SGLang errored with no values (e.g. transient scrape timeout).
		physical: {QueryName: physical, Error: errors.New("scrape timeout")},
	}
	mergeEngineResults(results,
		[]inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang},
		engineSpecificReplicaQueries)

	r := results[registration.QueryKvCacheUsage]
	if r == nil {
		t.Fatal("expected a merged result")
	}
	if r.HasError() {
		t.Errorf("partial success must not be marked errored; got error %v", r.Error)
	}
	if len(r.Values) != 1 || r.Values[0].Value != 0.5 {
		t.Errorf("the healthy engine's value should survive, got %+v", r.Values)
	}
}

// TestMergeEngineResults_MixedAllErrored verifies the complement: when every
// engine errors and no values exist, the merged result still surfaces an error.
func TestMergeEngineResults_MixedAllErrored(t *testing.T) {
	physical := "sglang/" + registration.QueryKvCacheUsage
	results := map[string]*source.MetricResult{
		registration.QueryKvCacheUsage: {QueryName: registration.QueryKvCacheUsage, Error: errors.New("vllm down")},
		physical:                       {QueryName: physical, Error: errors.New("sglang down")},
	}
	mergeEngineResults(results,
		[]inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang},
		engineSpecificReplicaQueries)

	r := results[registration.QueryKvCacheUsage]
	if r == nil || !r.HasError() {
		t.Errorf("when all engines error and no values exist, merged result should carry an error; got %+v", r)
	}
}

func TestContainsEngine(t *testing.T) {
	engines := []inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang}
	if !containsEngine(engines, inferenceengine.EngineSGLang) {
		t.Error("expected SGLang to be present")
	}
	if containsEngine([]inferenceengine.Engine{inferenceengine.EngineVLLM}, inferenceengine.EngineSGLang) {
		t.Error("did not expect SGLang to be present")
	}
}
