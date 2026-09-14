// Package registration provides query registration for metrics sources.
// This file registers queries used by the throughput analyzer (ThroughputAnalyzer).
package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// Query name constants for throughput analyzer metrics.
//
// Only four queries are registered here — those that are genuinely new and not
// provided by other analyzer registrations. The remaining TA inputs are already
// collected and exposed via domain.ReplicaMetrics; the TA reads those fields
// directly instead of re-registering duplicate PromQL templates.
//
// TA notation → ReplicaMetrics field (query / registration):
//
//	KV_max  (total KV token capacity) → TotalKvCapacityTokens  (QueryCacheConfigInfo       / RegisterSaturationQueries)
//	ITL_obs (observed ITL, seconds)   → AvgITL                 (QueryAvgITL                / RegisterQueueingModelQueries)
//	OL      (avg output tokens)       → AvgOutputTokens        (QueryAvgOutputTokens       / RegisterSaturationQueries)
//	IL      (avg input tokens)        → AvgInputTokens         (QueryAvgInputTokens        / RegisterSaturationQueries)
//	H%      (prefix cache hit rate)   → PrefixCacheHitRate     (QueryPrefixCacheHitRate    / RegisterSaturationQueries)
//	λ_req   (per-pod arrival rate)    → ArrivalRate            (QuerySchedulerDispatchRate / RegisterQueueingModelQueries)
//	Λ_req   (model-level arrival)     → AnalyzerInput.ArrivalRate (QueryModelArrivalRate   / RegisterThroughputAnalyzerQueries, this file)
//	         λ_dec = Λ_req × avgOL, combined with the queue-drain term (model level, see Commit 2)
const (
	// QueryGenerationTokenRate is the query name for the observed generation
	// (decode) token rate per pod (tokens/sec).
	// This is the direct observable proxy for μ_dec^obs — how many tokens each
	// replica is currently generating per second.
	// Source: vllm:request_generation_tokens_sum (histogram _sum counter)
	QueryGenerationTokenRate = "generation_token_rate"

	// QueryKvUsageInstant is the query name for the instantaneous KV cache utilization
	// fraction per pod (0.0–1.0). Used as k* (current operating point) in the ITL
	// model: ITL(k) = A·k + B.
	//
	// Same underlying metric as QueryKvCacheUsage (vllm:kv_cache_usage_perc), but
	// without max_over_time. QueryKvCacheUsage wraps the gauge in max_over_time[1m]
	// to give the saturation analyzer a conservative peak. This query reads the raw
	// gauge so the throughput analyzer sees the current operating point, not a
	// 1-minute high-water mark that could overestimate load and trigger premature
	// scale-up after a transient spike.
	//
	// max by (instance, pod, llm_d_ai_variant): deduplication only. vllm:kv_cache_usage_perc
	// is a single scalar gauge per vLLM process; there is one series per pod in normal
	// deployment. The max by (...) collapses any duplicate series that arise when a pod
	// is scraped by multiple targets (e.g., PodMonitor + ServiceMonitor). Since duplicates
	// carry the same value, max = avg — the choice has no effect on correctness.
	// Source: vllm:kv_cache_usage_perc (gauge)
	QueryKvUsageInstant = "kv_usage_instant"

	// QueryRequestRate is the query name for the engine-side request completion
	// rate per pod (req/s), derived from the generation tokens histogram count.
	// It is engine-agnostic: the vLLM variant reads
	// vllm:request_generation_tokens_count and the SGLang variant reads
	// sglang:generation_tokens_histogram_count.
	//
	// Used as a fallback for λ_dec estimation when EPP/scheduler metrics are
	// unavailable (ArrivalRate == 0 for all pods). Per variant V, the analyzer computes:
	//   λ_dec_fallback = Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r)
	//
	// Note: measures completed requests (served demand), not arriving requests.
	// It undercounts when requests are queued in the scheduler. Use
	// ArrivalRate (via QuerySchedulerDispatchRate) as the primary demand source.
	QueryRequestRate = "request_rate"

	// QueryModelArrivalRate is the query name for the model-level request arrival
	// rate (requests/sec), summed across the whole model with no per-pod labels to
	// reconcile — unlike QuerySchedulerDispatchRate (RegisterQueueingModelQueries),
	// which groups by pod_name/port and belongs to QM. This is a TA-exclusive query
	// on the same underlying source metric; it is registered here, not in
	// queueing_model.go, because no other analyzer consumes it.
	//
	// No model_name fallback: inference_extension_scheduler_attempts_total has
	// never carried a model_name label on any EPP version examined (only
	// target_model_name) — unlike the flow-control queue metric, which does.
	QueryModelArrivalRate = "model_arrival_rate"
)

// RegisterThroughputAnalyzerQueries registers the four TA-exclusive queries.
// It must be called once at engine startup alongside other analyzer registrations.
//
// Registered queries:
//   - QueryGenerationTokenRate — μ_dec^obs: observed decode token rate per pod
//   - QueryKvUsageInstant      — k*: instantaneous KV cache utilization per pod
//   - QueryRequestRate     — fallback λ_req: completion rate per pod when EPP absent
//   - QueryModelArrivalRate    — Λ_req: model-level request arrival rate
//
// Additional TA inputs are read from domain.ReplicaMetrics fields populated by
// RegisterSaturationQueries (TotalKvCapacityTokens, AvgOutputTokens, AvgInputTokens,
// PrefixCacheHitRate) and RegisterQueueingModelQueries (AvgITL, ArrivalRate).
// See the package-level constant block for the full TA notation → field mapping.
//
// μ_dec is computed using a linear ITL model:
//
//	ITL(k)   = A·k + B            (calibrated from AvgITL × k* pairs over time)
//	IL_eff   = IL × (1 - H%)
//	KV_req   = IL_eff + OL/2
//	N_dec(k) = k × KV_max / KV_req
//	μ_dec    = N_dec(k_sat) / ITL(k_sat)
//
// Per variant V (summed over that variant's replicas only):
// λ_dec primary:  Σ_{r∈V}(ArrivalRate_r × AvgOutputTokens_r)     [EPP deployed]
// λ_dec fallback: Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r) [EPP absent]
func RegisterThroughputAnalyzerQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping throughput analyzer query registration")
		return
	}
	registry := metricsSource.QueryList()

	// Per-pod observed generation (decode) token rate (tokens/sec).
	// Computed as the rate of the _sum histogram counter over 1m.
	// Preserves instance (IP:port for composite key), pod (for pod lookup),
	// and llm_d_ai_variant (for direct pod-to-VA mapping).
	// llm_d_ai_variant will be dropped from this groupby once PR #1260
	// (pod→VA derivation) and issue #1263 (remove label from all groupbys) land.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryGenerationTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Observed generation (decode) token rate per pod (tokens/sec), proxy for μ_dec^obs",
	})

	// Per-pod instantaneous KV cache utilization (0.0–1.0).
	// Does NOT use max_over_time: the throughput analyzer needs the current
	// operating point k*, not the worst-case peak used by the saturation analyzer.
	// Preserves instance (IP:port for composite key), pod (for pod lookup),
	// and llm_d_ai_variant (for direct pod-to-VA mapping).
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0), used as k* in the ITL model",
	})

	// Per-pod vLLM request completion rate (req/s).
	// Derived from the generation tokens histogram _count (increments once per
	// completed request). Used as a fallback for λ_dec when EPP/scheduler metrics
	// are unavailable; per variant V, the analyzer falls back to:
	//   λ_dec_fallback = Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r)
	// Preserves instance (IP:port for composite key), pod (for pod lookup),
	// and llm_d_ai_variant (for direct pod-to-VA mapping).
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "vLLM request completion rate per pod (req/s); fallback for λ_dec when EPP metrics are unavailable",
	})

	// Model-level request arrival rate (requests/sec), summed across the whole
	// model. Same status="success" filter as QuerySchedulerDispatchRate, but
	// grouped only by namespace — no pod_name/port labels, so none of that
	// query's per-instance attribution fragility. Engine-agnostic (sourced from
	// EPP, not vLLM/SGLang), so — like QuerySchedulerDispatchRate — it is not
	// duplicated in registerSGLangThroughputAnalyzerQueries.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryModelArrivalRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (namespace) (rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}",target_model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Model-level request arrival rate (requests/sec) from scheduler, summed across the whole model with no per-pod labels to reconcile",
	})

	registerSGLangThroughputAnalyzerQueries(registry)
}

// registerSGLangThroughputAnalyzerQueries registers the SGLang variants of the
// throughput-analyzer queries. SGLang exposes generation tokens via the
// generation_tokens_histogram series and KV utilization via token_usage.
func registerSGLangThroughputAnalyzerQueries(registry *source.QueryList) {
	// Per-pod observed generation (decode) token rate (tokens/sec), 1m rate.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryGenerationTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(sglang:generation_tokens_histogram_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Observed generation (decode) token rate per pod (tokens/sec), proxy for μ_dec^obs (SGLang)",
	})

	// Per-pod instantaneous KV cache utilization (0.0-1.0), no max_over_time.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (sglang:token_usage{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0-1.0), used as k* in the ITL model (SGLang)",
	})

	// Per-pod request completion rate (req/s) from the generation histogram count.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request completion rate per pod (req/s); fallback for λ_dec when EPP metrics are unavailable (SGLang)",
	})
}
