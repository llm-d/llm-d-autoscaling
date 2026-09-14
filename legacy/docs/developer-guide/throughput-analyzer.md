# Throughput Analyzer

## Overview

The Throughput Analyzer is a **model-driven, proactive scaling analyzer** that estimates decode
token throughput supply (μ_dec) and compares it against decode token demand (λ_dec) to drive scaling decisions.

Where the Saturation Analyzer reacts to observed capacity exhaustion, the Throughput Analyzer
predicts how much decode throughput the current replica fleet can sustain at a given KV cache
operating point, and scales before demand exceeds that supply.

**Key concepts:**
- **μ_dec** — decode token supply: how many tokens/sec the fleet can generate, estimated from
  KV cache occupancy and a calibrated inter-token latency (ITL) model
- **λ_dec** — decode token demand: how many tokens/sec the scheduler is dispatching to this model
- **ITL(k)** — inter-token latency as a function of KV utilization k: fitted as `A·k + B` via OLS

> **Status:** Implementation complete and wired into the engine's multi-analyzer pipeline.
> Enable via the `analyzers:` field in `wva-saturation-scaling-config` — see [Configuration](#configuration).
>
> **Enablement:** The ThroughputAnalyzer is **opt-in**. At startup, the controller checks whether
> any saturation config entry lists `throughput` with `enabled != false`. If none does, the
> analyzer is not registered and never participates in scaling. The default config ships with
> saturation only, so throughput is off by default.
>
> **Runtime toggling requires a restart.** Registration is frozen after `StartOptimizeLoop`;
> editing the configmap to add `throughput` takes effect only after a controller restart.
> This restart requirement is about registration, not participation: the per-cycle
> `effectiveEnabled` gate (see the [multi-analyzer pipeline guide](multi-analyzer-pipeline.md))
> governs whether an already-registered analyzer participates in a given cycle, and is
> independent of this startup-time registration gate.
>
> **Editing the ConfigMap without restarting is surfaced, not silent.** When the
> `ConfigMapReconciler` processes an edit to the **global** `wva-saturation-scaling-config`
> ConfigMap and the live throughput-analyzer enablement now differs from the registration
> decision frozen at startup, it emits a Kubernetes Warning event on the ConfigMap (reason
> `ThroughputAnalyzerRestartRequired`, visible via `kubectl describe configmap
> wva-saturation-scaling-config -n <system-namespace>`) and logs a message telling the operator
> to restart `wva-controller-manager` to apply the change. This fires in both directions —
> enabling or disabling `throughput` at runtime. **Namespace-local saturation ConfigMaps are not
> covered**: startup registration reads only the global config, so a namespace-local edit can
> never diverge from it and this check does not run for that path.

## Table of Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Metrics](#metrics)
  - [Throughput Analyzer Queries](#throughput-analyzer-queries)
  - [Shared Fields from Collector](#shared-fields-from-collector)
  - [Query Design Decisions](#query-design-decisions)
- [Architecture](#architecture)
  - [Package Structure](#package-structure)
  - [Components](#components)
  - [Data Flow](#data-flow)
- [ITL Model Calibration](#itl-model-calibration)
  - [Tier 1 — OLS Fit](#tier-1--ols-fit)
  - [Tier 2 — Constrained OLS](#tier-2--constrained-ols)
- [Supply Estimation](#supply-estimation)
- [Demand Estimation](#demand-estimation)
  - [Priority Chain](#priority-chain)
  - [Scheduler Queue Demand](#scheduler-queue-demand)
- [Scaling Signal](#scaling-signal)
  - [Model-Level Aggregation](#model-level-aggregation)
  - [Role-Aware Aggregation](#role-aware-aggregation)
- [Constants and Tuning](#constants-and-tuning)
- [References](#references)

## Configuration

The Throughput Analyzer is enabled by adding it to the `analyzers:` list in the
`wva-saturation-scaling-config` ConfigMap alongside the saturation analyzer:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzerName: saturation
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    analyzers:
      - name: saturation
        score: 1.0
      - name: throughput
        score: 1.0
```

With this config:
- **Scale-up** fires when either saturation OR throughput signals overload (any-up).
- **Scale-down** fires only when both agree there is spare capacity (all-down).

To run the Throughput Analyzer in isolation (without the saturation signal):

```yaml
analyzers:
  - name: saturation
    enabled: false   # provides Cost/AcceleratorName metadata but no RC/SC signal
  - name: throughput
    score: 1.0
```

See [saturation-scaling-config.md — Multi-Analyzer Pipeline](../saturation-scaling-config.md#multi-analyzer-pipeline)
for the full `analyzers:` field reference and combine algorithm.

## Metrics

`RegisterThroughputAnalyzerQueries` (in `internal/collector/registration/throughput_analyzer.go`)
registers three queries that are genuinely new and not covered by other analyzer registrations.
All other TA inputs are read from `interfaces.ReplicaMetrics` fields populated by the saturation
and queueing model registrations.

### Throughput Analyzer Queries

#### QueryGenerationTokenRate (`generation_token_rate`)

```promql
sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_sum{namespace="...",model_name="..."}[1m]))
```

**What it measures:** Observed generation (decode) token rate per pod in tokens/sec.

**TA notation:** μ_dec^obs — the directly observable supply proxy. Used for supply model
verification: the analyzer compares the ITL-model-predicted rate μ_dec(k*) against GPS_obs per
replica. A deviation > 15% at k* ≥ 0.30 suppresses SpareCapacity for the cycle. See
[GPS Verification](#gps-verification).

**ReplicaMetrics field:** `GenerationTokenRate`

---

#### QueryKvUsageInstant (`kv_usage_instant`)

```promql
max by (instance, pod, llm_d_ai_variant) (vllm:kv_cache_usage_perc{namespace="...",model_name="..."})
```

**What it measures:** Instantaneous KV cache utilization fraction per pod (0.0–1.0).

**TA notation:** k* — the current operating point in the ITL model `ITL(k) = A·k + B`.

**ReplicaMetrics field:** `KvUsageInstant`

**Same underlying metric as `QueryKvCacheUsage`:** Both queries hit `vllm:kv_cache_usage_perc`.
`QueryKvCacheUsage` (saturation) wraps it in `max_over_time[1m]` to get the 1-minute peak —
a conservative bound for capacity guardrails. `QueryKvUsageInstant` reads the raw gauge so the
Throughput Analyzer sees the current operating point k*, not a high-water mark from a transient
spike that has since subsided. Using the peak would overestimate load and cause premature
scale-up. Both fields coexist on `ReplicaMetrics` for their respective purposes.

**Why `max by (instance, pod, llm_d_ai_variant)` and not `avg by (...)`:** `vllm:kv_cache_usage_perc`
is a scalar gauge per vLLM process, so there is one Prometheus series per pod in normal
deployment. The `max by (...)` clause is purely deduplication: if the same pod is scraped by
multiple targets (e.g., a PodMonitor and a ServiceMonitor), duplicate series with identical
values appear under the same pod label. `max` collapses them. Since duplicates carry the same
value, `max = avg` — the choice has no effect on correctness. This follows the convention used
by every other per-pod query in this codebase.

---

#### QueryRequestRate (`request_rate`)

```promql
sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_count{namespace="...",model_name="..."}[1m]))
```

**What it measures:** Engine-side request completion rate per pod (req/s), derived from the
generation tokens histogram `_count` counter (increments once per completed request). Engine-
agnostic — the vLLM example is shown above; the SGLang variant reads
`sglang:generation_tokens_histogram_count`.

**TA notation:** fallback λ_req — used when `ArrivalRate == 0` for all pods (EPP not deployed).
The analyzer computes `λ_dec_fallback = Σ RequestRate_r × AvgOutputTokens_r`.

**ReplicaMetrics field:** `RequestRate`

**Note:** This also serves as a throughput proxy weight for histogram averaging. When computing
variant-average IL, OL, and prefix hit rate across replicas, each replica is weighted by its
`RequestRate` to prevent low-throughput replicas from distorting the shape estimate.

**Important:** This measures *completed* (served) requests, not *arriving* requests. It
undercounts when requests are queued in the scheduler. Use `ArrivalRate` (primary) first;
fall back to this only when the EPP is not deployed.

---

### Shared Fields from Collector

The following TA inputs are already collected via other analyzer registrations. The TA reads
these fields directly rather than registering duplicate queries.

| TA notation | Field | Query | Registration |
|---|---|---|---|
| KV_max (token capacity) | `ReplicaMetrics.TotalKvCapacityTokens` | `QueryCacheConfigInfo` | `RegisterSaturationQueries` |
| ITL_obs (observed ITL) | `ReplicaMetrics.AvgITL` | `QueryAvgITL` | `RegisterQueueingModelQueries` |
| OL (avg output tokens) | `ReplicaMetrics.AvgOutputTokens` | `QueryAvgOutputTokens` | `RegisterSaturationQueries` |
| IL (avg input tokens) | `ReplicaMetrics.AvgInputTokens` | `QueryAvgInputTokens` | `RegisterSaturationQueries` |
| H％ (prefix hit rate) | `ReplicaMetrics.PrefixCacheHitRate` | `QueryPrefixCacheHitRate` | `RegisterSaturationQueries` |
| λ_req (per-pod, req/s) | `ReplicaMetrics.ArrivalRate` | `QuerySchedulerDispatchRate` | `RegisterQueueingModelQueries` |
| Q (scheduler queue size) | `SchedulerQueueMetrics.QueueSize` (model-level) | `QuerySchedulerQueueSize` | `RegisterSaturationQueries` |

**λ_dec primary:** `Σ ArrivalRate_r × AvgOutputTokens_r` across all replicas (EPP deployed).  
**λ_dec fallback:** `Σ RequestRate_r × AvgOutputTokens_r` (EPP absent, all ArrivalRate == 0).

**Note on arrival rate:** `ArrivalRate` comes from `QuerySchedulerDispatchRate` which is per-pod,
namespaced, and model-scoped — correctly isolating traffic to a specific variant. The TA sums
per-replica λ_dec in the analyzer rather than using a model-level query, which avoids the
namespace filtering limitation of the scheduler metric.

**Note on metric freshness:** the collector derives each replica's
`ReplicaMetricsMetadata.Age`/`FreshnessStatus` from the same scrape timestamps used for the
aggregate `wva_metrics_freshness_status` gauge — the least-fresh status across a pod's *present*
metrics, and the age of the oldest one. Metrics that are absent by design (e.g. the EPP arrival
rate when no EPP is deployed, or the prefix-cache metrics when prefix caching is off) leave a zero
timestamp and are skipped rather than reported as `missing`, so a healthy replica is not
mislabelled and a genuinely stale metric is not masked. `CheckModelMetrics` (`sanity.go`) flags a
replica as `SanityIssueStaleMetrics` when `FreshnessStatus == "stale"`. This is currently detection
only: the stale-metrics sanity issue is reported but not currently used to gate a scaling decision.

---

### Query Design Decisions

| Query / Field | Source | Aggregation | Window | Purpose in TA |
|---|---|---|---|---|
| `QueryGenerationTokenRate` | vLLM | `sum by (instance, pod, llm_d_ai_variant)` | 1m rate | μ_dec^obs per pod (observability) |
| `QueryKvUsageInstant` | vLLM | `max by (instance, pod, llm_d_ai_variant)` | instant | k* (no max_over_time) |
| `QueryRequestRate` | vLLM | `sum by (instance, pod, llm_d_ai_variant)` | 1m rate | Fallback λ_req; histogram weight |
| `TotalKvCapacityTokens` | `KvCacheConfigInfo` labels | derived | static | KV_max = blocks × block_size |
| `AvgITL` | `QueryAvgITL` | `max by (instance, pod, llm_d_ai_variant)` | 1m rate | ITL_obs for OLS calibration |
| `AvgOutputTokens` | `QueryAvgOutputTokens` | `max by (instance, pod, llm_d_ai_variant)` | 5m rate | OL for KV_req and λ_dec |
| `AvgInputTokens` | `QueryAvgInputTokens` | `max by (instance, pod, llm_d_ai_variant)` | 5m rate | IL for IL_eff = IL × (1−H％) |
| `PrefixCacheHitRate` | `QueryPrefixCacheHitRate` | `max by (instance, pod, llm_d_ai_variant)` | 5m rate | H％ for IL_eff |
| `ArrivalRate` | `QuerySchedulerDispatchRate` | `sum by (pod_name, port, namespace)` | 1m rate | λ_req per pod (primary) |

## Architecture


### Package Structure

```
internal/engines/analyzers/throughput/
├── constants.go               thresholds, window params, tuning defaults
├── types.go                   WorkloadShape, ITLObservation, SanityIssue, SanityReport,
│                              ThroughputVariantState
├── shape_tracker.go           ShapeTracker: (IL,OL) bucket + change detection
├── observation_window.go      ObservationWindow: rolling (k,ITL) pairs, Ready flag
├── sanity.go                  CheckModelMetrics: 6 SanityIssue types
├── itl_model.go               ITLModel{A,B}, FitITLModel (OLS), ITLAt(k)
└── analyzer.go                ThroughputAnalyzer: Observe() + full Analyze()
```

### Components

**Query Registration (`internal/collector/registration/throughput_analyzer.go`)**  
Registers three PromQL templates exclusive to the throughput analyzer:
`QueryGenerationTokenRate`, `QueryKvUsageInstant`, `QueryRequestRate`.
`RegisterThroughputAnalyzerQueries` must be called once at startup alongside
`RegisterSaturationQueries` and `RegisterQueueingModelQueries`.

**Metrics Collector (`internal/collector/replica_metrics.go`)**  
Populates all `interfaces.ReplicaMetrics` fields in a single `Refresh()` call covering all
12 registered queries. The three TA-exclusive fields are:
`GenerationTokenRate`, `KvUsageInstant`, `RequestRate`.
The remaining TA fields (`TotalKvCapacityTokens`, `AvgITL`, `AvgOutputTokens`, `AvgInputTokens`,
`PrefixCacheHitRate`, `ArrivalRate`) are populated by saturation and queueing model queries.

**ShapeTracker (`shape_tracker.go`)**  
Maintains the current workload shape bucket `(IL, OL, IL_eff, KVreq)`. Detects shape changes
(>20% shift in IL or OL) and triggers observation window reset.

- `IL_eff = IL × (1 − PrefixCacheHitRate)` — effective input length after prefix cache
- `KVreq = IL_eff + OL/2` — time-averaged KV footprint per decode request

**ObservationWindow (`observation_window.go`)**  
Rolling window of `(k*, ITL_obs)` pairs collected per replica per cycle. Filters observations
to `k ∈ [0.15, 0.85]` (reliable linear-model range). Reports `Ready()` when ≥ 10 samples with
≥ 0.30 k-spread are accumulated within the 30-minute default window.

**ITLModel (`itl_model.go`)**  
Two-tier calibration of `ITL(k) = A·k + B`. See [ITL Model Calibration](#itl-model-calibration).

**ThroughputAnalyzer (`analyzer.go`)**  
Implements `interfaces.Analyzer`. Groups replicas by `VariantName`, runs sanity checks,
updates per-variant shape tracker and observation window in `Observe()`, then computes
supply and demand signals in `Analyze()`, publishing raw `Total*` fields.
The engine's universal threshold post-step writes `RequiredCapacity` and `SpareCapacity`.

### State and High Availability

`ThroughputAnalyzer` is stateful across reconcile cycles: it accumulates `(k*, ITL)` observations until the window is ready to fit the ITL model. The state is **in-memory only** — a `map[string]*variantState` held inside the analyzer instance, with no persistence to etcd or Kubernetes.

Per-variant state is minimal:

| Field | What it holds |
|---|---|
| `ShapeTracker` | Current workload shape snapshot (IL, OL, hit rate); overwritten each cycle |
| `ObservationWindow` | Rolling slice of ≤ 20 `(k*, ITL_obs)` pairs + timestamps |
| `lastSanityReport` | Most recent sanity check result |
| `lastObservedAt` | Timestamp of last observation |

**In HA mode**, the engine reconciliation loops run only on the elected leader (gated in `main.go`). The `ThroughputAnalyzer` instance lives inside that loop — state is local to the leader process and is never shared across replicas.

On leader failover the incoming leader starts with an empty analyzer. During warm-up (until the observation window re-accumulates ≥ 10 samples with ≥ 0.30 k-spread), the TA emits no scaling signal (`TotalDemand = 0, TotalSupply = 0`). The saturation analyzer runs unaffected and provides coverage throughout. Warm-up completes within a few minutes at normal traffic levels.

**No external state store is needed.** State loss on failover is equivalent to a workload shape change (which already clears the window by design). The gap is bounded and temporary; adding a ConfigMap or lease annotation to persist calibration state would not be worth the added complexity at this stage.


### Analysis Pipeline

```
  ┌──────────────────── Per-Variant Processing (each variant v) ──────────────────┐
  │                                                                               │
  │  []ReplicaMetrics (replicas of variant v)                                     │
  │        │                                                                      │
  │        ├─(IL, OL, H%) [RequestRate-weighted]──► ShapeTracker              │
  │        │                                               │                      │
  │        │                                         KVreq, IL_eff                │
  │        │                                         shape change──► Window.Clear │
  │        │                                                                      │
  │        ├─(k*, ITL_obs per replica)──────────────► ObservationWindow           │
  │        │                                               │                      │
  │        │                                        Ready? yes──► OLS fit         │
  │        │                                               │    no──► constrained │
  │        │                                               └──► ITLModel{A, B}    │
  │        │                                                         │            │
  │        ├─(KV_max)─────────────────────────────► computeVariantSupply          │
  │        │                                        [ITL(k_sat) = A·k_sat + B]    │
  │        │                                        → μ_dec_sat, perReplicaSupply │
  │        │                                                                      │
  │        ├─(ArrivalRate / RequestRate)─────► computeDemand                  │
  │        │                                         → λ_dec, isEPP               │
  │        │                                                                      │
  │        └─(GPS_obs, k*, KV_max)──────────────► checkVariantGPSMismatch         │
  │                                               μ_model = N_dec(k*) / ITL(k*)   │
  │                                               err = |μ_model − GPS_obs|       │
  │                                                     / GPS_obs × 100           │
  │                                               if err > 15% at k* ≥ 0.30:     │
  │                                                 anyGPSMismatch = true         │
  └──────────────────────────────────┬────────────────────────────────────────────┘
                                     │ per-variant outputs accumulated
  ┌──────────────────────────────────▼────────────────────────────────────────────┐
  │  Model-Level Aggregation (TA publishes; engine post-step writes RC/SC)        │
  │                                                                               │
  │  TotalSupply            = Σ μ_dec_sat                                         │
  │  TotalDemand            = Σ λ_dec  +  QueueSize / (factor × ITL(k_sat))       │
  │  TotalAnticipatedSupply = Σ (current + pending) × perReplicaSupply            │
  │                                                                               │
  │  RequiredCapacity = 0  ← engine writes after Analyze() returns                │
  │  SpareCapacity    = 0  ← engine writes after Analyze() returns                │
  │  RoleCapacities  [if P/D roles: TotalSupply/TotalDemand/TotalAnticipated]     │
  └───────────────────────────────────────────────────────────────────────────────┘
```

### Data Flow

```
┌────────────┐
│ Prometheus │
└──────┬─────┘
       │ vllm:request_generation_tokens_sum      (QueryGenerationTokenRate   → GenerationTokenRate)
       │ vllm:kv_cache_usage_perc                (QueryKvUsageInstant          → KvUsageInstant)
       │ vllm:request_generation_tokens_count    (QueryRequestRate           → RequestRate)
       │ vllm:cache_config_info                  (QueryCacheConfigInfo       → TotalKvCapacityTokens)
       │ vllm:inter_token_latency_seconds_*      (QueryAvgITL               → AvgITL)
       │ vllm:request_generation_tokens_*        (QueryAvgOutputTokens       → AvgOutputTokens)
       │ vllm:request_prompt_tokens_*            (QueryAvgInputTokens        → AvgInputTokens)
       │ vllm:prefix_cache_hits/queries          (QueryPrefixCacheHitRate    → PrefixCacheHitRate)
       │ inference_extension_scheduler_*         (QuerySchedulerDispatchRate → ArrivalRate)
       │ inference_extension_flow_control_*      (QuerySchedulerQueueSize    → QueueSize)
       ↓
┌─────────────────────────┐
│ ReplicaMetricsCollector │  ← internal/collector/replica_metrics.go
│ CollectReplicaMetrics() │     single Refresh() call, 12 queries
└──────┬──────────────────┘
       │ []interfaces.ReplicaMetrics + SchedulerQueueMetrics
       ↓
┌──────────────────────────────────────────────────────────┐
│ ThroughputAnalyzer.Analyze()                             │
│                                                          │
│  per variant:                                            │
│    ShapeTracker → KVreq                                  │
│    ObservationWindow → (k*, ITL) pairs                   │
│    ITLModel (tier-1 OLS or tier-2 constrained)           │
│    supply: μ_dec_sat = k_sat×KV_max / KVreq / ITL(k_sat) │
│    demand: EPP primary → engine fallback → k*-local      │
│                                                          │
│  model-level:                                            │
│    demand: ArrivalRate×avgOL + queue demand              │
│    TotalSupply, TotalDemand, TotalAnticipatedSupply       │
│    RequiredCapacity = 0, SpareCapacity = 0 (engine fills) │
└──────┬───────────────────────────────────────────────────┘
       │ AnalyzerResult{TotalSupply, TotalDemand, TotalAnticipatedSupply,
       │                VariantCapacities, RoleCapacities}
       ↓
┌──────────────────────────────────────────────┐
│ Engine universal threshold post-step         │
│   RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply) │
│   SC = max(0, TotalSupply − TotalDemand/scaleDown)          │
└──────────────────┬───────────────────────────┘
                   │ AnalyzerResult with RC/SC populated
                   ↓
┌──────────────────────────────────────┐
│ Multi-analyzer optimizer             │  ← slice-predicate any-up/all-down
│ (internal/engines/pipeline)          │
└──────────────────┬───────────────────┘
                   │ VariantDecisions → Controller
                   ↓
```

## ITL Model Calibration

The ITL model `ITL(k) = A·k + B` captures how inter-token latency grows with KV cache
utilization k. It is calibrated independently per variant (different hardware → different A, B).

### Tier 1 — OLS Fit

When `ObservationWindow.Ready()` is true (≥ 10 samples spanning ≥ 30% of the k range),
`FitITLModel` fits A and B by ordinary least squares, minimizing `Σ(ITL_i − A·k_i − B)²`.
The fit is accepted only when the resulting `(A, B)` passes `validITLModel` — finite, a
meaningfully positive slope, and positive ITL at saturation (physically required: more
concurrent requests → higher latency). On success, the fitted model is used for both supply
and demand estimation this cycle.

### Tier 2 — Constrained OLS

When the window is not ready, A is estimated with B pinned and only A fitted:

```
A = Σ((ITL_i − B) · k_i) / Σ(k_i²)
```

This is least-squares with B fixed, applied to all replicas with k* > 0. For a single replica
it reduces to the single-point formula `A = (ITL − B) / k*`. For multiple replicas it is
strictly better — same OLS criterion as tier-1 but with one fewer degree of freedom. The
resulting `(A, B)` is accepted through the same `validITLModel` predicate as Tier 1, so Tier 2
cannot accept a model Tier 1 would reject.

**B selection:** B is taken from `variantState.lastFittedB` when a prior successful Tier-1 fit
exists for this variant. B reflects hardware/model characteristics (not workload shape), so it
survives shape-change window resets. When no prior Tier-1 fit has occurred (`hasFittedB` is
false), B falls back to `DefaultBaselineITLSec` (0.006 s — H100 baseline at near-zero load).
`lastFittedB` and `hasFittedB` are exposed in `ThroughputVariantState` for observability.

**Tier 3 (not implemented):** a zero-replica fallback — reusing the last successful Tier-1 fit
when a variant has scaled to zero replicas — is not implemented. The current `Analyze()` loop
only iterates variants with active replica metrics, so a scaled-to-zero variant has no ITL
model to fall back to.

## Supply Estimation

Per replica `r`:

```
IL_eff    = AvgInputTokens × (1 − PrefixCacheHitRate)
KVreq     = IL_eff + AvgOutputTokens / 2      # time-averaged KV footprint per request
N_dec_sat = DefaultKSat × KV_max / KVreq      # in-flight requests at k_sat
μ_dec_sat = N_dec_sat / ITL(k_sat)            # decode tokens/sec at saturation operating point
```

Per-variant totals: `totalSupply = Σ μ_dec_sat`, `perReplicaSupply = totalSupply / n`, where
`n` is the count of KV-capable replicas (`nKV`) — replicas actually reporting KV metrics, not
a raw spec/status replica count. This `n` becomes `ReplicaCount_v` in the Scaling Signal
formulas below, mirroring the saturation analyzer's `readyCount`; still-booting KV=0 replicas
are excluded here and counted separately via `PendingReplicas_v` below instead.

`DefaultKSat = 0.85` — the KV utilization at which μ_dec_sat is evaluated. This is a
per-analyzer constant pending alignment with the EPP system-wide k_sat (see open items).

## Demand Estimation

### Model-Level Decode Demand

TA's decode demand is a **model-level** quantity, not a sum of per-variant
contributions:

```
avgOL               = Σ_v (nKV_v × shape_v.AvgOutputTokens) / Σ_v nKV_v
                      over non-prefill variants v (tracked WorkloadShape,
                      weighted by each variant's replica count — see below)
arrivalDecodeDemand = AnalyzerInput.ArrivalRate × avgOL
```

`AnalyzerInput.ArrivalRate` is collected from a single model-level
`sum by (namespace) (rate(inference_extension_scheduler_attempts_total{...}))` query with no
`pod`/`port`/`instance` labels — unlike the per-pod `ReplicaMetrics.ArrivalRate`
(`QuerySchedulerDispatchRate`), it cannot partially mis-attribute: it either matches the model
filter (correct) or returns zero (filter/EPP absent). No witness metric is needed.

`ReplicaMetrics.ArrivalRate` (per-pod) is **retained** — `queueingmodel` and
`internal/utils/allocation` still depend on it — but no longer drives TA's `TotalDemand`. The
per-instance EPP↔vLLM key merge that previously fed it into TA's demand (and could orphan and
drop it when ports differed) is no longer on TA's critical path.

### Warm-Up Safety and Weighting

`avgOL` is computed from each variant's **tracked** `WorkloadShape.AvgOutputTokens`
(`state.shapeTracker.Current()`), never a fresh average over the current cycle's live
`ReplicaMetrics`. This matters during EPP warm-up: a replica can report `ArrivalRate > 0` (EPP
dispatching) but `AvgOutputTokens == 0` this cycle (no completions yet). Averaging live data
would zero `avgOL` and, with it, `TotalDemand` — reintroducing the exact spurious-scale-down bug
that the per-variant EPP-warm-up fix (`computeDemand` falling through to `computeLocalDemand`
when `AvgOutputTokens == 0`, see below) was written to prevent. The tracked shape is robust to
a single zero-OL cycle.

Combining each variant's tracked OL into one model-level `avgOL` must be **weighted by replica
count** (`nKV`), not an unweighted mean-of-variant-means — otherwise a variant with one replica
and a variant with ten replicas would contribute equally to `avgOL`, diverging from the true
per-replica/request-weighted model-level average whenever non-prefill variants have different OL
profiles and/or different replica counts. With a single non-prefill variant the distinction is
invisible; it only matters with 2+ (e.g. canary/blue-green, multiple GPU tiers).

### Per-Variant Demand (Introspection Only)

Each variant still resolves its own demand via the same three-tier priority chain as before —
the cascade falls through to the next source whenever the current source yields zero — but the
result now only populates `VariantCapacity.TotalDemand` / `Utilization` (surfaced via
`VariantState()`); it no longer feeds model-level `TotalDemand`.

**1. EPP primary** — when any replica has `ArrivalRate > 0`:
`λ_dec = Σ ArrivalRate_r × AvgOutputTokens_r`. If EPP is present but all `AvgOutputTokens == 0`
(warm-up), this yields zero and the cascade falls through; `isEPP` still reflects EPP presence.

**2. Engine-rate fallback** — when EPP is absent or yielded zero, and `RequestRate > 0`:
`λ_dec = Σ RequestRate_r × AvgOutputTokens_r`. Counts only served (completed) requests.

**3. k\*-based local** (scale-up only) — when both above yield zero:
`λ_local = Σ_r k_r* × KV_max_r / KVreq / ITL(k_r*)`. A replica is skipped from the sum (rather
than counted as zero) when its `KvUsageInstant` or the model's `ITLAt(KvUsageInstant)` is
missing, non-positive, or NaN, or when `KvUsageInstant` is out of the valid `[0, 1]` range — so a
single bad metric cannot poison the total.

**Deferred:** with demand model-level, `AnalyzerInput.ArrivalRate` is the sole driver of
`TotalDemand`'s arrival component. When EPP is absent model-wide, `ArrivalRate` is legitimately
0 (no signal — not "zero traffic"), and unlike before this cascade's k\*-local fallback no
longer backfills model-level `TotalDemand` in that case; it remains per-variant only. This is a
deliberate simplification, not a bug — revisit when `k_knee` (an arrival-driven
operating knee) is implemented.

### Scheduler Queue Demand

The scheduler queue term is combined **alongside** the arrival decode term at model level —
not folded into it, not double-counted:

```
avgDecodeITLSat  = mean(ITL(k_sat)) over decode/both variants
queueDemand      = QueueSize / (DefaultQueueDrainFactor × avgDecodeITLSat)
TotalDemand      = arrivalDecodeDemand + queueDemand
```

`DefaultQueueDrainFactor = 2.0` bounds per-request queueing time to
≤ 2 × ITL(k_sat) × avgOL. The output-length factor cancels in the derivation, so the result
is independent of OL.

Both `arrivalDecodeDemand` and `queueDemand` are split across active non-prefill roles by the
same helper (`distributeDemandByRole`) and folded into `RoleCapacities[*].TotalDemand` — see
Role-Aware Aggregation. Neither is attributed back to individual variants:
`Σ VariantCapacity.TotalDemand` no longer sums to `result.TotalDemand`; the per-role sum does.

**Note:** `SchedulerQueueMetrics` is passed via `AnalyzerInput.SchedulerQueue`. The TA handles
nil correctly (queue demand = 0 when absent). The engine currently always passes nil due to a
known bug (`engine_v2.go` never calls `CollectSchedulerQueueMetrics`); fixing this is tracked
as a separate engine PR and will not require changes to the TA.

## Scaling Signal

### Model-Level Aggregation

TA publishes raw `Total*` fields on `AnalyzerResult`; the engine's universal threshold
post-step writes `RequiredCapacity` and `SpareCapacity` after `Analyze()` returns.

```
TotalSupply            = aggregation.SumTotalSupply(VariantCapacities)
                       = Σ_v ReplicaCount_v × PerReplicaCapacity_v
TotalAnticipatedSupply = aggregation.SumTotalAnticipatedSupply(VariantCapacities)
                       = Σ_v (ReplicaCount_v + PendingReplicas_v) × PerReplicaCapacity_v
TotalDemand            = (AnalyzerInput.ArrivalRate × avgOL)
                       + QueueSize / (DefaultQueueDrainFactor × ITL(k_sat))
```
`TotalDemand` no longer derives from `aggregation.SumTotalDemand(VariantCapacities)` — see
Demand Estimation above.

`PendingReplicas` (booting replicas not yet in service) are included in anticipated supply
to suppress redundant scale-up requests while pods are starting.

The engine post-step formula (using the model's configured `scaleUpThreshold` / `scaleDownBoundary`):
```
RC = max(0, TotalDemand / scaleUpThreshold  − TotalAnticipatedSupply)
SC = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)
```

See [`saturation-scaling-config.md`](../saturation-scaling-config.md) § Universal Threshold Post-Step
for the authoritative formula and per-analyzer threshold override configuration.

### Known Regression

**PR-5 drops the EPP-presence and GPS-mismatch gates that previously suppressed `SpareCapacity`.**
Under the old behavior, TA set `SpareCapacity = 0` when EPP was not deployed or when a GPS
mismatch flagged the ITL model as unreliable. Under the new contract, TA leaves `SpareCapacity`
zero and the engine post-step computes it unconditionally from `TotalSupply` and `TotalDemand`.

This means:
- In EPP-absent deployments, TA's model-level `SC > 0` will be forwarded to the optimizer,
  potentially triggering scale-down on a supply estimate that may be less reliable.
- When the GPS mismatch flag is active (ITL model suspect), TA no longer blocks scale-down.

These safety properties will be restored in a follow-up PR once the analyzer→engine contract
supports an SC opt-out signal (`AnalyzerResult.SuppressSpareCapacity` or equivalent).
TA-only deployments with EPP and without persistent GPS mismatches are unaffected.

### GPS Verification

`GenerationTokenRate` (GPS_obs = μ_dec^obs) is the directly observed decode token rate per
replica from `rate(vllm:request_generation_tokens_sum[1m])`. Each cycle, `Analyze()` compares
this against the ITL model's prediction:

```
μ_model(k*) = N_dec(k*) / ITL(k*)
            = (k* × KV_max / KVreq) / (A·k* + B)

gpsErrPct = |μ_model(k*) − GPS_obs| / GPS_obs × 100
```

When any replica in any variant shows `gpsErrPct > 15%` at `k* ≥ 0.30`, the mismatch is
recorded and logged. TA sets `consecutiveGPSMismatches` on the variant state; if this reaches
`DefaultGPSMismatchClearThreshold` (3) consecutive cycles, the observation window is cleared to
force ITL model recalibration.

**Note:** Prior to PR-5, TA suppressed `SpareCapacity` when a GPS mismatch was active. Under the
multi-analyzer engine contract, TA leaves `RequiredCapacity` and `SpareCapacity` at zero and the
engine post-step computes both unconditionally. The GPS-mismatch SC gate is no longer applied.
See the Known Regression section above.

The `k* ≥ 0.30` guard prevents false positives at low load where GPS is noisy and N_dec is small.

**Near-saturation diagnostics.** When `k* ≥ DefaultKSat − 0.10` (i.e. k* ≥ 0.75), GPS is
near-oracle quality: a discrepancy between μ_model and GPS_obs is a strong indicator of a
model error. In this case, `checkVariantGPSMismatch` logs additional root-cause diagnostics:

- **ITL residual high** (`|AvgITL − ITL(k*)| / AvgITL > 20%`): the observed ITL deviates from
  the model's prediction at k*. Cause: bad data points in the observation window, or the workload
  has shifted and the model has not yet recalibrated.
- **N_dec mismatch** (ITL residual small, but `|N_dec_model − GPS_obs × AvgITL| / N_dec_model > 20%`):
  the ITL model fits observed ITL but GPS × ITL disagrees with KV-derived N_dec. Cause: the
  workload shape (IL, OL, or prefix-hit-rate) used to compute KVreq is wrong.

GPS mismatch is logged at INFO so operators see it without enabling debug logging.

### Role-Aware Aggregation

Roles are read from `AnalyzerInput.VariantStates` and stored in per-variant state. All roles
use the same decode-rate framework.

- TA publishes `TotalSupply`, `TotalAnticipatedSupply`, and `TotalDemand` per role.
  `RequiredCapacity` and `SpareCapacity` are left zero — the engine post-step fills them.
- **Prefill role:** `RoleCapacities["prefill"].TotalDemand` is 0 by construction —
  `distributeDemandByRole` excludes the prefill role from both the model-level arrival decode
  term and the queue term (both decode-rate-denominated), so neither contributes a prefill
  share. (Per-variant `VariantCapacity.TotalDemand` for a prefill variant is separately
  negligible via the OL guard in `computeLocalDemand`, but that value no longer feeds
  `RoleCapacities` — see Demand Estimation.) The engine post-step therefore produces RC ≈ 0 for
  the prefill role naturally.
- **Demand attribution:** both the model-level arrival decode term and the scheduler queue term
  are decode-rate-denominated and split evenly across active non-prefill roles by the same
  helper (`distributeDemandByRole`).
- `RoleCapacities` is nil when all variants have role `""` or `"both"` (non-disaggregated model).

## Constants and Tuning

| Constant | Default | Description |
|---|---|---|
| `DefaultKSat` | 0.85 | KV utilization at which μ_dec_sat is evaluated |
| `DefaultBaselineITLSec` | 0.006 | B in tier-2 ITL model (H100 near-zero-load baseline) |
| `DefaultQueueDrainFactor` | 2.0 | Bounds queueing time to ≤ factor × ITL(k_sat) × OL |
| `DefaultWindowMaxSize` | 20 | Max (k*, ITL) pairs in ObservationWindow |
| `DefaultObservationMaxAge` | 30m | Observations older than this are pruned |
| `DefaultMinSamples` | 10 | Minimum samples for OLS Ready flag |
| `DefaultMinKSpread` | 0.30 | Minimum k-spread for OLS Ready flag |
| `DefaultMinObservableK` | 0.15 | Lower k* filter for ObservationWindow |
| `DefaultMaxObservableK` | 0.85 | Upper k* filter for ObservationWindow |
| `DefaultShapeChangeTolerance` | 0.20 | IL or OL shift that triggers window reset |
| `DefaultGPSMismatchThresholdPct` | 15.0 | GPS error % above which SpareCapacity is suppressed |
| `DefaultGPSMinKForVerification` | 0.30 | Minimum k* for GPS check to apply |
| `DefaultNearKSatMargin` | 0.10 | k* within this margin of k_sat triggers deeper diagnostics |
| `DefaultNearKSatITLResidualThreshold` | 0.20 | ITL residual above which model drift is flagged |
| `DefaultNearKSatNDecResidualThreshold` | 0.20 | N_dec cross-check residual above which shape mismatch is flagged |

**Open items:**
- `DefaultKSat = 0.85` is per-analyzer; needs alignment with EPP system-wide k_sat
- `DefaultBaselineITLSec = 0.006` is H100-specific; may need hardware-aware defaults

## References

- Related: [Saturation Analyzer](../user-guide/saturation-analyzer.md)
- Design: `plans/planning/TA-Plan.md`, `plans/planning/TA-PR4-plan.md`
