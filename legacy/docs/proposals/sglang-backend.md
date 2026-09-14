# Proposal: SGLang Inference-Engine Backend Support

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-06-26
**Last Updated:** 2026-06-26

---

## Problem Statement

The Workload Variant Autoscaler (WVA) is currently hard-wired to a single inference
engine: **vLLM**. Every signal WVA consumes to make a scaling decision assumes vLLM's
Prometheus metric names and vLLM's deployment CLI flags. There is no abstraction for
"which engine is this variant running" — the engine identity is implicit and everywhere
assumed to be vLLM.

[SGLang](https://github.com/sgl-project/sglang) is a high-performance LLM serving engine
that is a first-class member of the llm-d ecosystem (llm-d supports both `--backend vllm`
and `--backend sglang`). Operators who run SGLang model servers cannot use WVA today: the
collector queries `vllm:*` metric names that SGLang pods never emit, so WVA sees no load
and never scales.

This proposal introduces an **inference-engine backend** dimension to WVA and adds SGLang
as the second supported engine, alongside vLLM, without changing existing vLLM behavior.

## Goals

- Introduce a backend/engine abstraction (`vllm`, `sglang`) that WVA can detect per variant.
- Make metric collection and deployment-argument parsing engine-aware.
- Add SGLang as a fully supported engine for the core autoscaling signal
  (KV-cache utilization, queue depth, token throughput, latency, capacity).
- **Zero behavior change for existing vLLM deployments.** vLLM remains the default and its
  query/registration names are unchanged.

## Non-Goals

- Auto-tuning SGLang-specific server arguments.
- P/D-disaggregation-specific SGLang metrics (prefill/decode transfer queues, etc.).
- Per-pod mixed-engine *within a single variant* (a variant is assumed to run one engine).
- Changing the queueing-model tuner math; only its metric inputs become engine-aware.

## Background: where vLLM is assumed today

WVA bakes vLLM into four distinct layers:

| # | Layer | Location | vLLM-specific detail |
|---|-------|----------|----------------------|
| 1 | Metric name constants | `internal/constants/metrics.go` | All `vllm:*` names |
| 2 | PromQL query templates | `internal/collector/registration/*.go` | `vllm:*` names baked into templates |
| 3 | Deployment arg parser | `internal/engines/analyzers/saturation_v2/deployment_parser.go` | `ParseVLLMArgs`, vLLM CLI flags |
| 4 | KV-cache config gauge | `vllm:cache_config_info` (consumed in `replica_metrics.go` / `capacity_store.go`) | vLLM info-gauge → token capacity |

The query layer already has a clean seam: queries are registered as named
`source.QueryTemplate`s in a `QueryList` and refreshed by name. What is missing is a way to
register and select an *engine-specific* variant of each query.

## Design

### Engine identity and detection

A new package `internal/inferenceengine` defines the engine enum and a detector:

```go
type Engine string

const (
    EngineVLLM   Engine = "vllm"
    EngineSGLang Engine = "sglang"
)

// Detect inspects a scale target's leader pod template (container images, commands,
// and args) and returns the inference engine it runs. Defaults to EngineVLLM when no
// SGLang signal is present, preserving existing behavior.
func Detect(st scaletarget.ScaleTargetAccessor) Engine
```

Detection is **conservative**: a variant is treated as SGLang only when a strong signal is
present (container image contains `sglang`, or the command/args invoke
`sglang.launch_server` / `sglang serve`). Everything else — including pods with no
recognizable signal — is treated as vLLM, so no existing deployment changes behavior.

Engine is a property of the *scale target* (the variant's Deployment/LWS), so it is
detected from the same pod template WVA already parses for deployment args. No new CRD
field is required. (A future explicit override — e.g. an annotation or a field on the
embeddable `VariantAutoscalingConfigSpec` — can be layered on top if auto-detection proves
insufficient.)

### Engine-scoped query registration

Query templates become engine-scoped. The physical registered name is derived from the
logical name and the engine:

```go
// EngineQuery returns the physical registered query name for a logical query under a
// given engine. vLLM keeps the bare logical name (backward compatible); other engines
// get an "<engine>/<logical>" prefix.
func EngineQuery(engine Engine, logical string) string
```

- `EngineQuery(EngineVLLM, "kv_cache_usage")` → `"kv_cache_usage"` (unchanged)
- `EngineQuery(EngineSGLang, "kv_cache_usage")` → `"sglang/kv_cache_usage"`

Each `Register*Queries` function additionally registers the SGLang variant of every
**engine-specific** query. Engine-agnostic queries (the EPP/scheduler flow-control and
dispatch-rate queries, which come from the gateway-api-inference-extension, not the engine)
are registered once under their bare names and shared across engines.

### Engine-aware collection

`CollectReplicaMetrics` determines the set of engines present among a model's scale targets
and refreshes the engine-specific queries for each present engine (plus the agnostic
queries once). Because a vLLM pod only emits `vllm:*` series and an SGLang pod only emits
`sglang:*` series, the per-pod results are naturally disjoint: results from each engine's
query are merged back under the logical query name by instance key, and the existing
per-pod consumer logic is unchanged.

For single-engine models (the overwhelmingly common case) this adds no extra queries for
vLLM and exactly one engine's queries for SGLang.

### Metric mapping (vLLM → SGLang)

SGLang metric names and types were taken from SGLang's metrics collector
(`python/sglang/srt/observability/metrics_collector.py`), not from memory. SGLang labels
its metrics with `model_name`, matching the label WVA already filters on.

| Logical query | vLLM metric(s) | SGLang metric(s) | Notes |
|---------------|----------------|------------------|-------|
| `kv_cache_usage` / `kv_usage_instant` | `vllm:kv_cache_usage_perc` (gauge) | `sglang:token_usage` (gauge) | Both 0.0–1.0; name swap |
| `queue_length` | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` | Name swap |
| `avg_ttft` | `vllm:time_to_first_token_seconds_{sum,count}` | `sglang:time_to_first_token_seconds_{sum,count}` | Histogram; name swap |
| `avg_itl` | `vllm:inter_token_latency_seconds_{sum,count}` | `sglang:inter_token_latency_seconds_{sum,count}` | Histogram; name swap |
| `avg_output_tokens` / `generation_token_rate` / `request_rate` | `vllm:request_generation_tokens_{sum,count}` | `sglang:generation_tokens_histogram_{sum,count}` | Histogram; name swap |
| `avg_input_tokens` | `vllm:request_prompt_tokens_{sum,count}` | `sglang:prompt_tokens_histogram_{sum,count}` | Histogram; name swap |
| `prefix_cache_hit_rate` | `vllm:prefix_cache_hits / vllm:prefix_cache_queries` | `sum by(...)(rate(sglang:cached_tokens_total)) / sum by(...)(rate(sglang:prompt_tokens_total))` | **Structural.** SGLang also exposes `sglang:cache_hit_rate` directly, but its units (0–1 vs 0–100) are version-dependent; the token-counter ratio is unit-safe and parallels the vLLM formula. Each counter is summed to the per-instance key before dividing because `cached_tokens_total` carries an extra `cache_source` label, which would otherwise break one-to-one vector matching |
| `cache_config_info` (→ KV token capacity) | `vllm:cache_config_info{num_gpu_blocks,block_size}` → `num_gpu_blocks × block_size` | `sglang:max_total_num_tokens` (gauge) | **Structural.** SGLang exposes total KV token capacity directly |
| `model_request_count` (scale-to-zero) | `vllm:request_success_total` | `sglang:num_requests_total` | Name swap |
| `scheduler_dispatch_rate`, `scheduler_queue_size`, `scheduler_queue_bytes` | EPP `inference_extension_*` | (same) | Engine-agnostic; unchanged |

### Deployment-argument mapping (vLLM → SGLang)

A new `ParseSGLangArgs` mirrors `ParseVLLMArgs`, populating the same `EngineParams` struct
(formerly `VLLMEngineParams`) so downstream capacity math is unchanged. Flag names and
defaults were taken from SGLang's `server_args.py`.

| `EngineParams` field | vLLM flag | SGLang flag | SGLang default |
|----------------------|-----------|-------------|----------------|
| `GpuMemoryUtilization` | `--gpu-memory-utilization` (0.9) | `--mem-fraction-static` | ~0.9 |
| `BlockSize` | `--block-size` (16) | `--page-size` | 1 |
| `KvCacheDtype` | `--kv-cache-dtype` | `--kv-cache-dtype` | `auto` |
| `TensorParallelSize` | `--tensor-parallel-size` | `--tp-size` / `--tp` / `--tensor-parallel-size` | 1 |
| `MaxNumSeqs` (max batch) | `--max-num-seqs` (256) | `--max-running-requests` | 256 (conservative placeholder when SGLang auto-derives) |
| `MaxModelLen` | `--max-model-len` | `--context-length` | auto |
| `MaxNumBatchedTokens` | `--max-num-batched-tokens` | `--chunked-prefill-size` / `--max-prefill-tokens` | auto |
| `TotalKvTokensOverride` | (n/a) | `--max-total-tokens` | unset |
| `EnforceEager` | `--enforce-eager` | `--disable-cuda-graph` | false |

Engine-arg selection is done via `ParseEngineArgs(engine, scaleTarget)`, which dispatches
to the right parser. The capacity store and the collector's max-batch-size lookup call it
with the detected engine.

## Implementation phases

1. **Foundation (this PR):** `inferenceengine` package (enum + detection), SGLang metric
   constants, SGLang query registration + `EngineQuery` resolver, engine-aware collection
   in `CollectReplicaMetrics`, `ParseSGLangArgs` + `ParseEngineArgs`, capacity-store wiring,
   and unit tests.
2. **Scale-to-zero:** thread the detected engine into `CollectModelRequestCount` so SGLang
   models use `sglang:num_requests_total`.
3. **Validation:** e2e coverage against a real SGLang model server (kind emulator + a
   recorded SGLang metrics fixture), Grafana dashboard labels, and docs in the user guide.

## Alternatives considered

- **Single template with the metric name as a parameter.** Rejected: two queries
  (`cache_config_info`, `prefix_cache_hit_rate`) differ structurally, not just by name, so a
  pure name substitution cannot serve both engines.
- **Explicit engine field on the CRD instead of auto-detection.** Deferred: detection from
  the pod template requires no operator action and no API change. An explicit override can
  be added later without breaking the detection default.

## Backward compatibility

vLLM remains the default engine and all vLLM query/registration names are unchanged. A
deployment with no SGLang signal is detected as vLLM and behaves exactly as before. The new
collection path is identity for vLLM-only models.

## References

- SGLang metrics collector: `python/sglang/srt/observability/metrics_collector.py`
- SGLang server args: `python/sglang/srt/server_args.py`
- SGLang server arguments docs: https://docs.sglang.io/advanced_features/server_arguments.html
