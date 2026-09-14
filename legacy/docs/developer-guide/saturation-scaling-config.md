# Saturation Scaling Configuration

## Overview

The Workload Variant Autoscaler supports saturation-based scaling using KV cache utilization and queue length metrics. This feature is enabled by default and configured via a ConfigMap.

**Key features:**
- ✅ ConfigMap-based configuration with global defaults and per-model overrides
- ✅ **Efficient caching** with single read on startup (zero API calls during reconciliation)
- ✅ **Automatic reload** via ConfigMap watch (immediate response to changes)
- ✅ **Thread-safe** concurrent access with RWMutex
- ✅ Graceful degradation if ConfigMap missing (V2 has hardcoded defaults; V1 requires ConfigMap — see [Default Configuration](#default-configuration))

## Analyzer Selection (V1 vs V2)

There are two saturation analyzers:

- **V2** (token/capacity-based) — **the default since v0.9.0**. Selected whenever
  the config carries a non-empty `analyzers:` list (or `analyzerName: "saturation"`).
- **V1** (percentage/spare-capacity-based) — legacy. Selected when no analyzer is
  specified (empty `analyzers:` **and** empty `analyzerName`).

The shipped `default` entry (`deploy/configmap-saturation-scaling.yaml` and
`config/base/manager/saturation-scaling-configmap.yaml`) includes an `analyzers:`
section, so a fresh install runs V2. **To opt out to V1, remove the `analyzers:`
section** (and the V2-only thresholds) from the `default` entry. No code change or
image rebuild is required — selection is driven entirely by config.

> This is a behavioral change introduced in v0.9.0. See the repository README
> "Upgrading to v0.9.0" note for migration guidance.

> **Analyzer selection is global; thresholds are per-model/namespace.** Selection
> is resolved once from the **global** `default` entry, while thresholds are
> resolved from the per-model override and namespace-local ConfigMap when present.
> Because these can disagree, the engine defaults the V2-only `scaleUpThreshold` /
> `scaleDownBoundary` on the **final resolved config** (after merging the override
> onto the base), so a per-model override or namespace-local entry written V1-style
> (no `analyzers:`) still runs **calibrated** when the global selection routes it to
> V2. Defaulting happens post-merge — not on the stored entries — so a V1-style
> override does not clobber a tuned global threshold. To use non-default V2
> thresholds for a specific model or namespace, set `scaleUpThreshold` /
> `scaleDownBoundary` explicitly in that entry; the explicit value is honored
> regardless of whether the entry carries an `analyzers:` section.

### Threshold ownership

Not every threshold applies to both analyzers. When switching between V1 and V2,
keep the fields that the target analyzer actually reads:

| Threshold | V1 | V2 | Notes |
|-----------|----|----|-------|
| `kvCacheThreshold` | ✅ | ✅ | Shared — replica saturated when KV cache ≥ threshold |
| `queueLengthThreshold` | ✅ | ✅ | Shared — replica saturated when queue length ≥ threshold |
| `kvSpareTrigger` | ✅ | — | **V1-only** — ignored by V2 |
| `queueSpareTrigger` | ✅ | — | **V1-only** — ignored by V2 |
| `scaleUpThreshold` | — | ✅ | **V2-only** — engine post-step (default 0.85) |
| `scaleDownBoundary` | — | ✅ | **V2-only** — engine post-step (default 0.70) |

## ScalingPolicy Schema (Phase 1)

Phase 1 of the [ScalingPolicy proposal](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1245)
extends this ConfigMap — additively, with no CRD, migration, or new install steps —
with a plugin envelope on each entry. Existing flat entries keep working unchanged;
adoption is optional per entry.

### Analyzer plugin envelope (`type` / `parameters`)

Each `analyzers:` entry accepts a `type` and a free-form `parameters` map:

```yaml
analyzers:
  - type: saturation
    parameters: { scaleUpThreshold: 0.95 }
```

- `type` selects the analyzer plugin (`saturation`, `queueing-model`, `throughput`).
  It falls back to `name` when omitted, so the legacy `- name: saturation` form still
  works and is treated as `type: saturation`.
- `parameters` carries plugin configuration. The well-known keys `scaleUpThreshold`,
  `scaleDownBoundary`, `score`, and `enabled` are equivalent to the corresponding
  top-level/typed fields (an explicit typed field wins over the same key in
  `parameters`). Unknown keys are tolerated (forward-compatible); a wrongly-typed
  known key fails validation and the entry is skipped.

**Opting out to V1 is unchanged:** delete the `analyzers:` section (and don't set
`analyzerName: saturation`) — see [Analyzer Selection](#analyzer-selection-v1-vs-v2).

### `scaleToZero`

An entry may set scale-to-zero inline:

```yaml
scaleToZero: { enabled: false }
```

When present, it **overrides** the separate `wva-model-scale-to-zero-config` ConfigMap
for that model/namespace. When omitted, that ConfigMap (and the `WVA_SCALE_TO_ZERO`
env fallback) governs, exactly as before. The retention period is unaffected — it
always comes from the scale-to-zero ConfigMap.

### `limiters` (cluster-default only, live)

The `default` entry selects the GPU limiter for the scaling pipeline. This is the
**sole source** — there is no `--limiter-type` flag or quota config file — and it is
applied **live**: editing the ConfigMap switches the limiter without a controller
restart (the engine rebuilds it on the next optimization cycle).

A `limiters:` list selects a **single mode**, so declare **one** of these forms:

```yaml
# (a) physical-capacity limiter (also the default when no limiters: is declared):
default: |
  analyzers:
    - type: saturation
  limiters:
    - type: gpu-inventory            # alias: inventory

# (b) operator-declared quota limiter:
default: |
  analyzers:
    - type: saturation
  limiters:
    - type: quota                    # inline quota entry — same schema as QuotaLimiterConfig
      name: cluster-h100
      scope: cluster
      quotas: { H100: 32 }
```

- **Single mode.** If a list contains both a `quota` and a `gpu-inventory` entry, the
  quota entry wins and the `gpu-inventory` entry is ignored — only one limiter is built.
  (Composition of physical + quota caps is tracked separately in #1003.)
- `type: quota` uses the **same schema** as `QuotaLimiterConfig`, so `scope`, `quotas`,
  `namespaceQuotas`, and `exclude` all apply; multiple quota entries are composed.
- **Cluster-default only.** The list is read only from the global (system-namespace)
  `default` entry — a budget-scope setting, like `enableRescale` — so a tenant cannot
  widen a cap via a per-model or namespace-local entry. A `limiters:` block placed on any
  other entry parses and validates but is **silently ignored** at runtime.
- With no `limiters:` block, the physical-inventory limiter is used.

## Configuration

### ConfigMap Structure

The saturation scaling configuration is stored in a ConfigMap named `wva-saturation-scaling-config` in the Workload Variant Autoscaler controller's namespace.

**Location:** `deploy/configmap-saturation-scaling.yaml`

### Parameters

| Parameter | Type | Description | Recommended |
|-----------|------|-------------|-------------|
| `kvCacheThreshold` | float64 | Replica is considered saturated if KV cache utilization ≥ threshold (0.0-1.0) | 0.80 |
| `queueLengthThreshold` | float64 | Replica is considered saturated if queue length ≥ threshold | 5 |
| `kvSpareTrigger` | float64 | Scale-up signal if average spare KV capacity < trigger (0.0-1.0) | 0.10 |
| `queueSpareTrigger` | float64 | Scale-up signal if average spare queue capacity < trigger | 3 |
| `scaleUpThreshold` | float64 | Model-level utilization threshold above which scale-up is triggered (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.85 |
| `scaleDownBoundary` | float64 | Model-level utilization boundary below which scale-down is safe (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.70 |

### V2 Analyzer Parameters

These parameters apply when `analyzerName: "saturation"` is set or when the `analyzers:` list is populated.

| Parameter | Type | Description | Default |
|-----------|------|-------------|---------|
| `analyzerName` | string | Legacy selector for the V2 token-based analyzer: set to `"saturation"`. Empty string uses V1. Prefer the `analyzers:` list, which the shipped default uses. | `""` |
| `priority` | float64 | Multiplier for this model's scaling urgency in fair-share GPU allocation | 1.0 |
| `analyzers` | list | Multi-analyzer pipeline registration — see [Multi-Analyzer Registration](#multi-analyzer-registration) | `[{name: "saturation", score: 1.0}]` |

> `scaleUpThreshold` and `scaleDownBoundary` are honored only for saturation on this branch; see the `multi-analyzer-threshold` PR for the universal post-step that calibrates RC/SC across all analyzers.

### Default Configuration

Since v0.9.0 the shipped `default` entry selects **V2** (token/capacity-based) via
the `analyzers:` list:

```yaml
analyzers:
  - name: saturation
    score: 1.0
# V2-only thresholds:
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
# Shared by V1 and V2:
kvCacheThreshold: 0.80
queueLengthThreshold: 5
# V1-only (ignored by V2):
kvSpareTrigger: 0.1
queueSpareTrigger: 3
```

To opt out to the legacy **V1** (percentage-based) analyzer, remove the `analyzers:`
section (and the V2-only thresholds); the remaining shared + V1-only thresholds drive V1:

```yaml
kvCacheThreshold: 0.80
queueLengthThreshold: 5
kvSpareTrigger: 0.1
queueSpareTrigger: 3
```

> **Important:** The V1 threshold values are **not hardcoded** in the analyzer code.
> If you opt out to V1 and the ConfigMap is missing or has no `default` entry, all V1
> thresholds default to zero, which will cause every replica to appear saturated and
> trigger continuous scale-up. Always deploy the ConfigMap with a `default` entry
> containing valid thresholds. (V2 has hardcoded fallbacks for its own thresholds.)

### How Scale-Up Triggers Work

> **Applies to V1 only.** The spare-capacity / `kvSpareTrigger` / `queueSpareTrigger`
> model described here is the legacy **V1** analyzer's logic. The default **V2**
> analyzer uses a token/capacity model driven by `scaleUpThreshold` /
> `scaleDownBoundary` and ignores the spare triggers (see
> [Analyzer Selection](#analyzer-selection-v1-vs-v2)).

The V1 saturation analyzer uses a **spare capacity model** to determine when to scale up. Instead of waiting for replicas to become fully saturated, WVA proactively scales when the average spare capacity across non-saturated replicas falls below configured thresholds.

**Scale-up logic:**

1. **Calculate spare capacity** for each non-saturated replica:
   - Spare KV capacity = `kvCacheThreshold - current_kv_usage`
   - Spare queue capacity = `queueLengthThreshold - current_queue_length`

2. **Average across non-saturated replicas**:
   - WVA computes the average spare capacity across all healthy (non-saturated) replicas

3. **Trigger scale-up when spare capacity is low**:
   - If `avg_spare_kv < kvSpareTrigger` **OR** `avg_spare_queue < queueSpareTrigger`
   - Scale-up is triggered to add capacity before existing replicas saturate

4. **Cascade scaling prevention**:
   - Variants with pending replicas (pods that exist but aren't ready yet) are skipped during scale-up
   - This prevents repeatedly scaling the same variant while previous scale-up operations complete
   - Pod startup can take 2-7 minutes (model loading, health checks)

**Example scenario:**
- `kvCacheThreshold = 0.80`, `kvSpareTrigger = 0.10`
- Replica A: 65% KV cache usage → Spare capacity: 0.15
- Replica B: 72% KV cache usage → Spare capacity: 0.08
- Average spare KV: (0.15 + 0.08) / 2 = **0.115**
- Since 0.115 ≥ 0.10, no scale-up yet (trigger uses strict `<`)
- If Replica B increases to 76%: Average spare = (0.15 + 0.04) / 2 = **0.095** → 0.095 < 0.10 → **Scale-up triggered**

This proactive approach ensures adequate headroom and prevents request drops by scaling before saturation occurs.

**For detailed implementation, see:** [Saturation Analyzer Documentation](user-guide/saturation-analyzer.md)

## Multi-Analyzer Registration

The V2 engine carries an analyzer registry so external analyzers (e.g.
throughput, SLO) can be plugged in without the engine package knowing the
concrete type. Each cycle the engine iterates every registered analyzer in
registration order and invokes its `Analyze` method.

> **Scope note.** Saturation drives scaling decisions only when it votes. The
> engine runs every enabled analyzer and derives the decision anchor on demand
> from the ballot, so three configurations follow from the `analyzers` list
> alone:
> - *default* (no `analyzers` list) → saturation is the sole voter and binder;
>   behavior is unchanged from single-analyzer operation.
> - `[saturation, throughput]` → both vote; saturation binds the anchor.
> - `[throughput]` only → throughput binds the anchor; saturation still runs as
>   the identity/(a) carrier but does not vote.
>
> Saturation always runs to supply per-variant identity metadata regardless of
> which config is active; what the `analyzers` list changes is whether it
> *votes*.

### Registering Analyzers

Pre-registered:

- The V2 saturation analyzer is pre-registered by `NewEngine` under
  `interfaces.SaturationAnalyzerName`. It always runs — supplying the
  identity/(a) carrier — but drives the optimizer only when it votes (the
  default config, or when its name is enabled).

External analyzers are registered from `cmd/main.go` via:

```go
if err := engine.RegisterAnalyzer(name, analyzer); err != nil {
    // handle: duplicate name or called after StartOptimizeLoop
}
```

`RegisterAnalyzer` appends to the registry and returns an `error` for
two misuse conditions: calling it after `StartOptimizeLoop` or
re-registering an existing name. Callers must check the error. Registration
order defines processing order in the engine loop and should match the
operator's `analyzers:` config order.

`RegisterAnalyzer` must be called before `StartOptimizeLoop`.

### Configuring Analyzers

The `analyzers:` list shapes the configuration for the registered analyzers:

```yaml
analyzerName: saturation
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzers:
  - name: saturation
    score: 1.0
  - name: throughput
    score: 1.0
```

When `analyzers:` is omitted **and** `analyzerName: "saturation"` is set, the list
defaults to `[{name: "saturation", score: 1.0}]`. When both are omitted, no analyzer
is selected and the engine falls back to V1 (see
[Analyzer Selection](#analyzer-selection-v1-vs-v2)).

### AnalyzerScoreConfig Fields

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `name` | string | Analyzer name (must match a `RegisterAnalyzer` call) | required |
| `enabled` | bool | Reserved — placeholder for future combine logic | `true` |
| `score` | float64 | Reserved — placeholder for future combine logic | `1.0` |
| `scaleUpThreshold` | float64 | Per-analyzer override for the scale-up threshold; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleUpThreshold` |
| `scaleDownBoundary` | float64 | Per-analyzer override for the scale-down boundary; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleDownBoundary` |

### Analyzer responsibilities and the universal threshold post-step

#### Per-variant data is canonical

`interfaces.VariantCapacity` is the single source of truth for per-variant primitives. Analyzers populate it; the engine and optimizer read it.

| Field | Written by | Read by |
|---|---|---|
| `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity`, `Cost`, `Role`, `AcceleratorName` | Analyzer | Optimizer (per-variant scaling math + picker) |
| `TotalCapacity`, `TotalDemand`, `Utilization` | Analyzer | Sat_v2 internal aggregation; `Utilization` passed through to `VariantDecision.Utilization` for metric emission |
| `r.TotalSupply`, `r.TotalAnticipatedSupply`, `r.TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RoleCapacities[role].TotalSupply/TotalAnticipatedSupply/TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RequiredCapacity`, `r.SpareCapacity` | **Engine post-step only** | Optimizer |
| `r.RoleCapacities[role].RequiredCapacity/SpareCapacity` | **Engine post-step only** | Optimizer (P/D disaggregation) |

#### Analyzer inputs

`interfaces.AnalyzerInput` carries the shared inputs every analyzer reads:
replica metrics, variant states, the model's resolved config, and
`SchedulerQueue`. None of these are analyzer-specific.

`SchedulerQueue` represents requests queued upstream of any pod (in the
llm-d flow control layer). Queue items are model-scoped and **not yet
attributed to any variant or role**. Any analyzer with a demand model may
use it — sat_v2 does today; the throughput analyzer will when it lands.

**Demand extraction from the queue is per-analyzer.** Each analyzer
converts queue depth/bytes into demand in its own unit (sat_v2:
kv-tokens; throughput: tokens/sec). Each analyzer also decides how to
attribute that demand across roles or variants — sat_v2 splits it among
active roles.

#### Per-replica demand and the role-aware waiting-queue charge

Distinct from `SchedulerQueue` above, each replica also reports its own **local
engine queue** (`vllm:num_requests_waiting` / `sglang:num_queue_reqs`) — requests
already admitted to that pod but not yet generating. sat_v2 charges them to the
replica, so per-replica demand has two terms:

```
replicaDemand = <resident KV tokens> + QueueLength × <per-request charge>
```

The resident term is `TokensInUse` on the main path, or
`KvCacheUsage × effectiveCapacity` on the fallback path (no
`vllm:cache_config_info`). The per-request charge depends on the variant's P/D
role, because a replica only pays for KV it actually materializes:

| Role | Per-request charge | Rationale |
|------|--------------------|-----------|
| `prefill` | `AvgInputTokens` | Prompt KV only; output is generated on a decode pod |
| `decode` | `AvgInputTokens + AvgOutputTokens` | Holds prompt KV and grows it per generated token |
| `both` (default) | `AvgInputTokens + AvgOutputTokens` | Handles the full request lifecycle |

An absent or empty role canonicalizes to `both`, so **non-disaggregated
deployments take the decode-style charge** — correct for a replica serving the
whole request, but it means the majority of deployments include output tokens.

`AvgInputTokens + AvgOutputTokens` is the footprint at a request's *final* decode
step, chosen as a peak / no-preemption planning charge: KV grows monotonically
once generation starts and cannot be shed without preemption and recompute.

**`I + O` is the saturation analyzer's demand-side unit; `ILeff + O/2` is the
throughput analyzer's.** The two are separate analyzers with separate
demand/supply models, so the difference is by design, not an inconsistency:

| Analyzer | Per-request KV unit | Meaning |
|----------|--------------------|---------|
| Saturation V2 (demand) | `I + O` | Peak footprint — what a replica must be able to hold |
| Throughput (`WorkloadShape.KVreq`) | `ILeff + O/2` | Time-averaged residency (`ILeff = I × (1 − PrefixHitRate)`) |

One asymmetry *is* internal to saturation V2 and should not be confused with the
above: its own k2 derivation (`estimateCapacityFromParams`) prices a *concurrent*
request at `I + O/2`. So a queued request is charged more than it occupies on
average, biasing this term toward scale-up — accepted deliberately, since
under-provisioning decode capacity causes preemption thrash. Changing it means
moving the demand/capacity pair together. See the `waitingQueueDemand` doc
comment for the full trade-off.

Note both demand terms derive from 1-minute maxima (`max_over_time` on
`kv_cache_usage_perc` and `num_requests_waiting`), whose peaks need not coincide,
so their sum can exceed any single instant's demand. That is a property of the
collector's queries, not of the role charge.

> **Operational note.** Because the default role is `both`, enabling or upgrading
> into this behavior raises computed demand wherever replicas have a non-empty
> local queue and report token metrics. `wva_saturation_utilization` and
> `wva_required_capacity{unit="continuous"}` step up, `wva_spare_capacity` steps
> down, and `wva_desired_replicas` may take a one-time step.
> `wva_kv_cache_tokens_used` is unaffected — it sums raw `TokensInUse` and does
> not include the queue projection. Utilization-based alerts (such as the sample
> `wva_saturation_utilization > 0.85` in
> [prometheus.md](prometheus.md)) may need re-baselining. The resolved role is
> emitted per variant on the `analyzer-result` log line.

#### Linearity invariant

The optimizer's per-variant scaling math (`bottleneckReplicas`, `safeRemovalReplicas`, `applyAllocation`) assumes that `n` replicas of variant `v` reduce model-level RC by exactly `n × PRC[v]`. That means `Total*` must equal the canonical sum over variants:

```
r.TotalSupply            == Σ_v vc.ReplicaCount × vc.PerReplicaCapacity
r.TotalAnticipatedSupply == Σ_v (vc.ReplicaCount + vc.PendingReplicas) × vc.PerReplicaCapacity
r.TotalDemand            == Σ_v vc.TotalDemand
r.RoleCapacities[role].* == same sums filtered by vc.Role == role
```

Use the shared helpers in `internal/engines/aggregation/` to compute these. An analyzer that doesn't use the helpers takes responsibility for producing identical math — otherwise the optimizer's per-variant allocation silently breaks.

#### Shared aggregation helpers

```go
import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"

r.TotalSupply            = aggregation.SumTotalSupply(variantCapacities)
r.TotalAnticipatedSupply = aggregation.SumTotalAnticipatedSupply(variantCapacities)
r.TotalDemand            = aggregation.SumTotalDemand(variantCapacities)

// For P/D disaggregated models:
totals := aggregation.AggregateByRole(variantCapacities)
// → map[role]aggregation.ScopeTotals{TotalSupply, TotalAnticipatedSupply, TotalDemand}
```

`AggregateByRole` canonicalizes empty role to `interfaces.RoleBoth`.

#### Engine post-step formula

After each analyzer's `Analyze()` returns, the engine applies the universal threshold formula at **every scope** — model level and each `RoleCapacity` entry:

```
RC = max(0, TotalDemand / scaleUpThreshold − TotalAnticipatedSupply)
SC = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)
```

`TotalAnticipatedSupply` is read as-is — **zero is a literal value, not a sentinel**. For a model scaled to zero with positive demand, RC = TotalDemand/scaleUp (the correct "this much capacity needed" answer). Analyzers must populate `TotalAnticipatedSupply` via `SumTotalAnticipatedSupply`; the engine does not walk `VariantCapacities` as a fallback.

The asymmetry — anticipated supply for scale-up, steady-state `TotalSupply` for scale-down — preserves the conservative "don't double-scale while replicas are launching, don't count pending as removable" stance.

**P/D disaggregation.** The same formula and the same `(scaleUp, scaleDown)` values are applied to every `RoleCapacity` entry. Threshold configuration is common regardless of role — there are no per-role overrides. The optimizer receives fully calibrated per-role RC/SC and uses them for P/D replica allocation.

**Per-analyzer threshold overrides.** `analyzers[].scaleUpThreshold` / `analyzers[].scaleDownBoundary` are resolved per analyzer: a per-entry override takes precedence over the model-level global. The resolved pair is applied uniformly at model level and every role for that analyzer. An opt-out flag for analyzers with non-universal calibration math is deferred to a follow-up.

### Saturation as the Identity Carrier

The saturation analyzer executes on every cycle regardless of the `analyzers`
config — its `VariantCapacities` carry the (a)/identity fields (`Cost`,
`AcceleratorName`, `Role`, replica counts) the optimizer needs for variant
selection and GPU accounting, for every configured variant including those at
zero replicas. Running is unconditional; *voting* is opt-in. Saturation
contributes its `RequiredCapacity` / `SpareCapacity` to the combine math only
when it votes — the default single-analyzer config, or when its name is
enabled. In a `[throughput]`-only config it is present purely as the identity
carrier: it supplies (a) for the anchor merge but is pruned from the voting
subset and neither drives scale-up nor vetoes scale-down.

### Resilience

Each registered non-saturation analyzer runs in isolation: errors are
logged and discarded, and panics are recovered. A faulty plugin analyzer
cannot take down the optimize goroutine or block saturation from driving
the scaling decision.

## Best Practices: Coordinating with InferenceScheduler (End Point Picker)

### What is End Point Picker (EPP)?

The **End Point Picker (EPP)** is an intelligent request routing component in the InferenceScheduler that selects the optimal inference server replica to handle each incoming request. EPP monitors replica capacity metrics (KV cache utilization, queue depth), as well as other replica metrics and uses scoring algorithms to route requests to replicas.

### Deployment Architecture

**EPP Deployment Model**: Each model has a **1-to-1 relationship** with its EPP instance. Every model served by the inference infrastructure has a dedicated EPP component that routes requests specifically to that model's replicas.

**Example deployment pattern:**
- Model: `Qwen/Qwen3-0.6B` in namespace `llm-d-autoscaler` → Dedicated EPP instance `gaie-workload-autoscaler-epp`
- Model: `ibm/granite-13b` in namespace `production` → Dedicated EPP instance `gaie-production-epp`
- Each model deployment has its own EPP instance (naming follows namespace/workload convention)

This 1-to-1 architecture means that saturation detection and request routing decisions are **model-specific**, with each EPP instance monitoring only its associated model's replicas.

### Threshold Alignment Recommendation

**For optimal cluster performance, we strongly recommend using the same threshold values for both WVA (Workload Variant Autoscaler) and InferenceScheduler (End Point Picker) for each model deployment.**

Using aligned thresholds ensures consistent capacity management across the cluster and prevents request drop situations.

**Why threshold alignment matters:**

1. **Reduced Request Drop Rates**: When WVA and EPP use the same saturation thresholds, the scheduler will avoid routing requests to replicas that WVA already considers saturated. This prevents the scheduler from overloading replicas that are about to trigger scale-up.

2. **Consistent Capacity Assessment**: Both components evaluate replica capacity using the same criteria (KV cache utilization and queue length), ensuring coordinated behavior across the entire inference stack.

3. **Improved GPU Utilization**: Aligned thresholds allow the cluster to maintain optimal GPU utilization without oversaturation. The scheduler respects the same capacity boundaries that drive autoscaling decisions.

4. **Faster Response to Load Changes**: When both components agree on saturation thresholds, the system responds more quickly to load changes with coordinated routing and scaling actions.

### Configuration Comparison

#### WVA Saturation Scaling Configuration

```yaml
# WVA Configuration (wva-saturation-scaling-config ConfigMap)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:                    # selects V2 (default since v0.9.0)
      - name: saturation
        score: 1.0
    kvCacheThreshold: 0.80        # Should match EPP kvCacheUtilThreshold
    queueLengthThreshold: 5       # Should match EPP queueDepthThreshold
    kvSpareTrigger: 0.10          # WVA-specific scale-up trigger (V1-only, ignored by V2)
    queueSpareTrigger: 3          # WVA-specific scale-up trigger (V1-only, ignored by V2)
```

#### EPP Saturation Detector Configuration

The InferenceScheduler EPP component uses the [gateway-api-inference-extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/epp-configuration/config-text.md) saturation detector to identify cluster overload.

**Per-Model Configuration**: Since each model has its own dedicated EPP instance, saturation detection is configured **per model deployment**. This allows different models to have different saturation thresholds based on their specific characteristics and SLO requirements.

```yaml
# EPP Saturation Detector Configuration (per-model EPP instance)
saturationDetector:
  ...
  queueDepthThreshold: 5          # Default: 5 - Backend waiting queue size threshold
  kvCacheUtilThreshold: 0.8       # Default: 0.8 - KV cache utilization threshold (0.0-1.0)
  ...
```

**Configuration Notes**:
- All parameters are optional; omitting them applies the documented defaults
- EPP configuration is **read only on startup** - changes require EPP pod restart
- Unlike WVA, EPP does not currently support live ConfigMap updates
- **Each EPP instance** (one per model) can have different threshold values

### Parameter Mapping and Alignment

| Concept | WVA Field | EPP Field | Aligned Default | Description |
|---------|-----------|-----------|-----------------|-------------|
| **KV Cache Saturation** | `kvCacheThreshold` | `kvCacheUtilThreshold` | **0.80** (80%) | Replica is saturated when KV cache ≥ threshold |
| **Queue Saturation** | `queueLengthThreshold` | `queueDepthThreshold` | **5** | Replica is saturated when queue length ≥ threshold |
| **Scale-Up Trigger (KV)** | `kvSpareTrigger` | *(not applicable)* | **0.10** (10%) | WVA-only: Trigger scale-up when spare KV < threshold |
| **Scale-Up Trigger (Queue)** | `queueSpareTrigger` | *(not applicable)* | **3** | WVA-only: Trigger scale-up when spare queue < threshold |

### Configuration Workflow

#### Step 1: Define Thresholds

Choose thresholds based on your workload characteristics and SLO requirements:

| Workload Type | kvCacheThreshold | queueLengthThreshold | Rationale |
|---------------|------------------|----------------------|-----------|
| **Conservative** (Default) | 0.80 | 5 | Balanced performance and utilization |
| **Aggressive** (High GPU utilization) | 0.90 | 15 | Maximize GPU usage, higher latency variance |
| **Strict** (Low latency SLO) | 0.70 | 3 | Prioritize responsiveness, lower utilization |

#### Step 2: Apply to WVA

Update `wva-saturation-scaling-config` ConfigMap:

```bash
kubectl edit cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
```

Changes take effect **immediately** (WVA watches ConfigMap and auto-reloads).

#### Step 3: Apply to EPP

**Important**: Since each model has its own dedicated EPP instance (1-to-1 relationship), you must configure the EPP instance for **each specific model deployment** separately.

**Current approach:**

1. Identify the EPP instance for your target model:
   ```bash
   # Example: Find EPP deployment for a specific model in namespace
   kubectl get deployments -n llm-d-autoscaler | grep epp
   ```

2. Update the EPP instance's environment variables or configuration file for that specific model

3. Restart the EPP pod for that model:
   ```bash
   # Restart the specific model's EPP instance
   kubectl rollout restart deployment/gaie-<model-name>-epp -n <namespace>
   ```

**Example for multiple models:**
```bash
# Model 1: granite-13b in production
kubectl rollout restart deployment/gaie-granite-13b-epp -n production

# Model 2: llama-70b in lab
kubectl rollout restart deployment/gaie-llama-70b-epp -n lab
```

#### Step 4: Verify Configuration

**WVA verification:**
```bash
kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace> -o yaml
```

**EPP verification (per-model instance):**
```bash
# Check specific model's EPP pod logs for loaded configuration
kubectl logs -n <namespace> deployment/gaie-<model-name>-epp | grep -i "saturation\|threshold"

# Example: Verify EPP configuration for granite-13b model in production
kubectl logs -n production deployment/gaie-granite-13b-epp | grep -i "saturation\|threshold"
```

### Alignment Best Practices

1. **Core Thresholds Must Match Per Model**:
   - `kvCacheThreshold` (WVA) = `kvCacheUtilThreshold` (EPP)
   - `queueLengthThreshold` (WVA) = `queueDepthThreshold` (EPP)
   - **Important**: Since each model has its own EPP instance, ensure thresholds align for **each model deployment** individually

2. **Per-Model Configuration Strategy**:
   - Use WVA's per-model override feature to set model-specific thresholds
   - Configure the corresponding EPP instance with matching thresholds
   - Document the threshold mapping for each model deployment
   - Example: If `ibm/granite-13b` uses `kvCacheThreshold: 0.85` in WVA, its dedicated EPP must use `kvCacheUtilThreshold: 0.85`

3. **WVA-Specific Parameters** (`kvSpareTrigger`, `queueSpareTrigger`):
   - These control WVA's scale-up aggressiveness
   - Should be set **lower** than saturation thresholds
   - Provide headroom before replicas become saturated
   - Recommended: `kvSpareTrigger = kvCacheThreshold - 0.1 to 0.2`

4. **Testing Threshold Changes**:
   - Test in development environment first
   - Monitor impact on request drop rate and latency for the specific model
   - Adjust based on observed behavior
   - Remember to update both WVA and the model's EPP instance

## Usage

### 1. Using Default Configuration

> **Warning:** For the V1 (percentage-based) analyzer, deploying without a ConfigMap will
> result in zero-valued thresholds, causing all replicas to be marked as saturated.
> Always deploy the ConfigMap with a `default` entry for V1.

If the ConfigMap is missing, the system will log a warning:
```
WARN Saturation scaling ConfigMap not found
```

### 2. Customizing Global Defaults

Edit `deploy/configmap-saturation-scaling.yaml`. Keep the `analyzers:` section to
stay on the default **V2** analyzer — a `default` entry **without** it selects the
legacy V1 analyzer (see [Analyzer Selection](#analyzer-selection-v1-vs-v2)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85      # V2-only
    scaleDownBoundary: 0.70     # V2-only
    kvCacheThreshold: 0.75      # shared by V1 and V2
    queueLengthThreshold: 10    # shared by V1 and V2
    kvSpareTrigger: 0.15        # V1-only (ignored by V2)
    queueSpareTrigger: 5        # V1-only (ignored by V2)
```

Apply the ConfigMap:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

**Note:** Changes take effect immediately! The controller watches the ConfigMap and automatically:
1. Reloads the cache when changes are detected
2. Triggers reconciliation of all VariantAutoscaling resources
3. Applies the new configuration without requiring pod restart

### 3. Per-Model Overrides

Add model-specific configuration entries to override defaults for specific model/namespace pairs.

The saturation engine resolves per-model config using a lookup key in the format
`{modelID}#{namespace}` (see `internal/engines/saturation/engine.go` — `resolveSaturationConfig()`).
The ConfigMap data key **must** match this format for overrides to take effect.
Lookup order: `modelID#namespace` → `default` → zero-value with defaults applied.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    analyzers:              # selects V2 (default since v0.9.0)
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3

  # Override for granite model in production namespace. Overrides inherit the
  # default's analyzer selection (V2) via field-level merge, so they only list the
  # thresholds they change.
  "ibm/granite-13b#production": |
    kvCacheThreshold: 0.85
    kvSpareTrigger: 0.15

  # Override for llama model in lab namespace
  "meta/llama-70b#lab": |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 20
    kvSpareTrigger: 0.1
    queueSpareTrigger: 10
```

**Key points:**
- Override keys **must** use the format `{modelID}#{namespace}` to match the engine's lookup
- The `model_id` and `namespace` YAML fields inside the entry are parsed but **not used for lookup**
- Overrides use **field-level merge**: only non-zero fields in the override replace the corresponding values from `default`; any field you omit (or set to its zero value) inherits from `default`. See `Merge()` in `internal/config/saturation_scaling.go`.
- Multiple overrides can exist for different model/namespace combinations

### 4. Per-Model Overrides — Field-Level Inheritance

Overrides are merged onto `default` field by field: only non-zero fields in the override
replace values from `default`, and any field you omit inherits from `default`. To change
just one threshold, specify only that field:

```yaml
  # Inherits queueLengthThreshold, kvSpareTrigger, queueSpareTrigger from `default`
  "my-org/my-model#my-namespace": |
    kvCacheThreshold: 0.90
```

> **Gotcha:** Because `Merge()` only overlays non-zero values, an override cannot force
> a field to its zero value — the override is treated as "unset" and inherits from
> `default` instead. This affects every field type, not just numeric thresholds:
>
> - `kvSpareTrigger: 0` → inherits from `default` (cannot zero out a numeric threshold)
> - `enableLimiter: false` → inherits from `default` (cannot disable a limiter that's enabled by default)
> - `analyzers: []` → inherits from `default` (cannot clear a non-empty analyzer list)
>
> See `Merge()` in `internal/config/saturation_scaling.go`.

## Validation

The controller validates all configuration entries on load. Invalid entries are logged and skipped:

### Validation Rules

1. **KvCacheThreshold:** Must be between 0.0 and 1.0
2. **QueueLengthThreshold:** Must be ≥ 0
3. **KvSpareTrigger:** Must be between 0.0 and 1.0
4. **QueueSpareTrigger:** Must be ≥ 0
5. **Consistency:** `kvCacheThreshold` must be ≥ `kvSpareTrigger`
6. **Priority:** Must be ≥ 0
7. **V2 thresholds** (when set): `scaleUpThreshold` and `scaleDownBoundary` must be in (0, 1],
   and `scaleUpThreshold` must be > `scaleDownBoundary`. Per-analyzer overrides are range-checked too.
8. **Limiters:** each `type` must be `gpu-inventory`/`inventory` or `quota`; a `gpu-inventory` entry
   must carry no quota fields; `quota` entries are validated end-to-end against the `QuotaLimiterConfig`
   schema (name uniqueness, scope, per-type ranges).

> Analyzer `type`/`name` values are **not** restricted to a fixed set — an unrecognized analyzer is
> simply ignored at runtime, so externally-registered analyzers and newer built-ins do not fail validation.

### Example Validation Errors

**Invalid entry (logged and skipped):**
```yaml
  invalid-config: |
    model_id: test/model
    namespace: test
    kvCacheThreshold: 1.5  # ERROR: Must be ≤ 1.0
```

**Log output:**
```
WARN Invalid saturation scaling config entry, skipping key=invalid-config error=kvCacheThreshold must be between 0 and 1, got 1.50
```

## Integration with Controller

### Caching Architecture

The controller uses an **efficient caching mechanism** with ConfigMap watch for optimal performance:

**Initialization (on controller startup):**

The ConfigMap reconciler watches the `wva-saturation-scaling-config` ConfigMap and loads
configuration into the shared `Config` object. On startup, the controller bootstraps the
config cache (see `internal/controller/configmap_bootstrap.go`).

**Reconciliation (zero API calls):**

During the optimization loop, the engine reads config from the in-memory cache:
```go
// In optimize() - reads cached config (no API call)
saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)

// Resolve per-model config using "{modelID}#{namespace}" lookup
saturationConfig := resolveSaturationConfig(saturationConfigMap, modelID, namespace)

// Use saturationConfig for saturation-based scaling decisions
// (thresholds drive the analyzer's saturation detection)
```

### Automatic Cache Updates

The `ConfigMapReconciler` watches the `wva-saturation-scaling-config` ConfigMap for changes
(see `internal/controller/configmap_reconciler.go`):

1. **ConfigMap change detected** → Watch event triggered
2. **Cache automatically reloaded** → New configuration parsed and stored in `Config`
3. **Next optimization cycle** picks up the new config automatically

### Performance Characteristics

| Operation | Before (Without Cache) | After (With Cache) |
|-----------|------------------------|-------------------|
| Startup | N/A | Single ConfigMap read |
| Per Reconciliation | ConfigMap API call | Memory read only |
| Config Change | Manual pod restart needed | Automatic reload + reconcile |
| Latency Impact | Network round-trip per reconcile | Zero (memory access) |
| Concurrency | Serial API calls | Thread-safe concurrent reads |

**Cache benefits:**
- ✅ **Single read on startup** instead of per-reconciliation
- ✅ **Zero API calls during reconciliation** (cached access)
- ✅ **Event-driven updates** (immediate response to changes)
- ✅ **Thread-safe concurrent access** (RWMutex)
- ✅ **Defensive copying** prevents external modification

## Troubleshooting

### ConfigMap Not Found

**Symptom:** Warning log message
```
WARN Saturation scaling ConfigMap not found, using hardcoded defaults configmap=wva-saturation-scaling-config namespace=<workload-variant-autoscaler-namespace>
```

**Solution:** Deploy the ConfigMap:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

### Invalid Configuration Entry

**Symptom:** Warning log message
```
WARN Invalid saturation scaling config entry, skipping key=my-config error=...
```

**Solution:** Fix the validation error in the ConfigMap entry and reapply.

### Missing Default Entry

**Symptom:** Warning log message
```
WARN No 'default' entry in saturation scaling ConfigMap, using hardcoded defaults
```

**Solution:** Add a `default` entry to the ConfigMap (keep the `analyzers:` section
to stay on the default V2 analyzer; omitting it selects legacy V1):
```yaml
data:
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

### Override Not Applied

**Symptom:** Model-specific override is not being used

**Checklist:**
1. Verify the ConfigMap data key uses the format `{modelID}#{namespace}` (e.g., `"ibm/granite-13b#production"`)
2. Verify `modelID` exactly matches `va.Spec.ModelID`
3. Verify `namespace` exactly matches the VariantAutoscaling resource namespace
4. Check controller logs for validation errors
5. Ensure entry passed validation (check for WARN logs)

### Config Changes Not Taking Effect

**Symptom:** Updated ConfigMap but controller still uses old values

**Solution:** The controller watches for ConfigMap changes and automatically reloads. Check:

1. **Verify ConfigMap was updated:**
   ```bash
   kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace> -o yaml
   ```

2. **Check controller logs for reload confirmation:**
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller | grep "Saturation scaling"
   ```

   Expected logs:
   ```
   INFO  Saturation scaling ConfigMap changed, reloading cache
   INFO  Saturation scaling config cache updated entries=2 has_default=true
   INFO  Triggering reconciliation for all VariantAutoscaling resources
   ```

3. **If no logs appear, verify watch is working:**
   - Check controller pod is running: `kubectl get pods -n <workload-variant-autoscaler-namespace>`
   - Check for errors: `kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller --tail=100`

4. **Manual restart (last resort):**
   ```bash
   kubectl rollout restart deployment/wva-controller -n <workload-variant-autoscaler-namespace>
   ```

### Cache Initialization Failed

**Symptom:** Warning on controller startup
```
WARN Failed to load initial saturation scaling config, will use defaults
```

**Solution:** This is non-fatal. The controller continues with zero-valued V1 thresholds (V2 has hardcoded defaults). To fix:

1. Deploy the ConfigMap:
   ```bash
   kubectl apply -f deploy/configmap-saturation-scaling.yaml
   ```

2. The watch mechanism will automatically reload the cache once ConfigMap is available

3. Verify cache loaded:
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller | grep "Saturation scaling configuration loaded"
   ```

## Example: Production Setup

**deploy/configmap-saturation-scaling.yaml:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  # Conservative defaults for most workloads (V2 analyzer — the default since v0.9.0)
  default: |
    analyzers:
      - name: saturation
        score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3

  # High-priority production workload - scale aggressively.
  # Per-model overrides inherit the default's analyzer selection (V2 here) via
  # field-level merge, so they need only the thresholds they change.
  "ibm/granite-13b#production": |
    kvCacheThreshold: 0.70
    queueLengthThreshold: 3
    kvSpareTrigger: 0.20
    queueSpareTrigger: 5

  # Development workload - allow higher saturation
  "meta/llama-70b#development": |
    kvCacheThreshold: 0.90
    queueLengthThreshold: 15
    kvSpareTrigger: 0.05
    queueSpareTrigger: 2
```

Apply the configuration:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

Verify deployment:
```bash
kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
kubectl describe cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
```

## API Reference

### Go Structs

**SaturationScalingConfig** (defined in `internal/config/saturation_scaling.go`):
```go
type SaturationScalingConfig struct {
    ModelID              string                `yaml:"model_id,omitempty"`
    Namespace            string                `yaml:"namespace,omitempty"`
    KvCacheThreshold     float64               `yaml:"kvCacheThreshold"`
    QueueLengthThreshold float64               `yaml:"queueLengthThreshold"`
    KvSpareTrigger       float64               `yaml:"kvSpareTrigger"`
    QueueSpareTrigger    float64               `yaml:"queueSpareTrigger"`
    EnableLimiter        bool                  `yaml:"enableLimiter,omitempty"`
    EnableRescale        bool                  `yaml:"enableRescale,omitempty"`
    AnalyzerName         string                `yaml:"analyzerName,omitempty"`
    ScaleUpThreshold     float64               `yaml:"scaleUpThreshold,omitempty"`   // default 0.85
    ScaleDownBoundary    float64               `yaml:"scaleDownBoundary,omitempty"`  // default 0.70
    Priority             float64               `yaml:"priority,omitempty"`           // default 1.0
    Analyzers            []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`
    ScaleToZero          *ScaleToZeroEnvelope  `yaml:"scaleToZero,omitempty"`        // Phase 1: inline scale-to-zero
    Limiters             []QuotaLimiterConfig  `yaml:"limiters,omitempty"`           // Phase 1: cluster-default-only GPU limiters
}

// AnalyzerScoreConfig configures one analyzer in the multi-analyzer pipeline.
// Type/Parameters implement the Phase 1 plugin envelope; well-known parameter
// keys are folded into the typed fields by Normalize(). Type falls back to Name.
type AnalyzerScoreConfig struct {
    Type              string         `yaml:"type,omitempty"`              // plugin type; falls back to Name
    Name              string         `yaml:"name"`
    Enabled           *bool          `yaml:"enabled,omitempty"`           // default true
    Score             float64        `yaml:"score,omitempty"`             // default 1.0
    ScaleUpThreshold  *float64       `yaml:"scaleUpThreshold,omitempty"`  // overrides global (saturation only today)
    ScaleDownBoundary *float64       `yaml:"scaleDownBoundary,omitempty"` // overrides global (saturation only today)
    Parameters        map[string]any `yaml:"parameters,omitempty"`        // plugin params; folded by Normalize
}

// ScaleToZeroEnvelope is the inline scale-to-zero setting on an entry.
type ScaleToZeroEnvelope struct {
    Enabled *bool `yaml:"enabled,omitempty"` // nil = inherit from the scale-to-zero ConfigMap
}
```

`Limiters` reuses `QuotaLimiterConfig` (`internal/config/quota_limiter.go`); a
`gpu-inventory` entry carries no quota fields, a `quota` entry uses the full quota
schema. See [ScalingPolicy Schema (Phase 1)](#scalingpolicy-schema-phase-1).

**Methods:**
- `Normalize() error` - Folds each analyzer's `parameters` into the typed fields; call at parse time before `ApplyDefaults()`/`Validate()`
- `ApplyDefaults()` - Fills in zero-valued V2 fields with their defaults and seeds the `Analyzers` list when empty (V1 fields have no hardcoded defaults)
- `Validate() error` - Validates configuration values (thresholds in range, consistency checks, per-analyzer overrides, analyzer/limiter types)
- `EffectiveType()` (on `AnalyzerScoreConfig`) - Returns `Type`, or `Name` when `Type` is empty

## Architecture Notes

### Caching Implementation Details

The caching mechanism uses the following components:

**Thread Safety:**
- Uses `sync.RWMutex` for concurrent access control
- Multiple reconciliation loops can read cache simultaneously
- Write operations (cache reload) are exclusive

**Defensive Copy:**
- `SaturationConfigForNamespace()` returns a deep copy
- Prevents external code from modifying cached configuration
- Each caller gets an independent copy

**Watch Mechanism:**
- Kubernetes watch on `wva-saturation-scaling-config` ConfigMap
- Predicate filters to only relevant ConfigMap events
- Event handler reloads cache and triggers reconciliation

**Graceful Degradation:**
- Controller starts successfully even if ConfigMap missing
- V2 analyzer uses hardcoded defaults (`scaleUpThreshold: 0.85`, `scaleDownBoundary: 0.70`)
- V1 analyzer has **no hardcoded defaults** — all thresholds will be zero without ConfigMap
- Automatically loads config once ConfigMap becomes available

