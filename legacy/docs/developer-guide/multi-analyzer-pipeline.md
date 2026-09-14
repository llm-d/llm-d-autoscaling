# Multi-Analyzer Pipeline (developer reference)

The Workload Variant Autoscaler's scaling engine runs multiple **analyzers**
in series each cycle. Each analyzer consumes the same per-replica metrics
and produces an `*interfaces.AnalyzerResult` carrying per-variant capacity,
model-level totals, and (for P/D disaggregated models) per-role capacity.
The engine post-step calibrates `RequiredCapacity` / `SpareCapacity` at
every scope using a uniform threshold formula. The optimizer reads a
per-analyzer slice (`[]NamedAnalyzerResult`) and decides scaling actions
over it via shared free functions in `internal/engines/pipeline/`.

---

## Architecture

### Data flow per optimize cycle

```
┌──────────────────────────────────────────────────────────┐
│ Config (SaturationScalingConfig per model/namespace)     │
│   Priority, Analyzers[]:                                 │
│     name, enabled, Score,                                │
│     ScaleUpThreshold, ScaleDownBoundary                  │
└──────────────────────────┬───────────────────────────────┘
                           │ engine reads per cycle
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: per-model preparation                            │
│   • BuildVariantStates (GPUsPerReplica per variant       │
│     from ScaleTarget / VA labels)                        │
│   • CollectSchedulerQueueMetrics (shared across          │
│     analyzers)                                           │
│   • resolveThresholds(name, cfg) per analyzer            │
│     (per-analyzer override over model-level globals)     │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: run analyzers, build per-analyzer slice          │
│ Saturation V2 (always run — the (a)/identity carrier),   │
│ then each registered non-saturation analyzer:            │
│   • skip if Enabled:false                                │
│   • Analyze(ctx, input) → *AnalyzerResult                │
│   • applyUniversalThreshold(result, scaleUp, scaleDown)  │
│     → writes RC/SC at model scope + each role scope      │
│   • append NamedAnalyzerResult{                          │
│       Name, Result,                                      │
│       Score     ← config.Analyzers[name].Score,          │
│       Remaining ← RC,   Spare ← SC,                      │
│     } to []NamedAnalyzerResult                           │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Engine: build ModelScalingRequest                        │
│   AnalyzerResults  ← per-analyzer slice (above)          │
│   VariantStates    ← prepared above                      │
│   Priority         ← config.Priority                     │
│   Disaggregated    ← any variant has a non-"both" Role   │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ Optimizer (CostAware or GreedyByScore)                   │
│   • initRoleState → RolePairedState + RoleSpare          │
│   • Scale-up: allocateForModelPaired                     │
│       pick(role) → variant; joint Δ_util commit          │
│       applyAllocation → decrement Remaining              │
│   • Scale-down: scaleDownRoleIterated                    │
│       needsScaleDownForRole → veto gate (ALL live agree) │
│       safeRemovalReplicasForRole → min across live       │
│       applyDeallocationForRole → decrement RoleSpare     │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
                       VariantDecisions
```

### Key concepts

| Concept | Definition |
|---|---|
| **Analyzer** | Implementation of `interfaces.Analyzer`. Examples: saturation V2 (kv-token capacity), throughput (RPS/ITL-derived), queueing-model. |
| **`VariantCapacity`** | Per-variant primitives: `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity` (analyzer-specific units), `Cost`, `AcceleratorName`, `Role`, `TotalDemand`. |
| **`AnalyzerResult`** | Per-(model, analyzer) output: `VariantCapacities[]`, model-level `Total*`, `RoleCapacities[role]` (P/D only), `RequiredCapacity` / `SpareCapacity` (engine-written by post-step; analyzers must not populate these). |
| **`RoleCapacity`** | Per-role aggregate within an `AnalyzerResult`: `TotalSupply`, `TotalDemand`, `TotalAnticipatedSupply`, `RequiredCapacity` / `SpareCapacity` (engine-written). Used for P/D disaggregated models only. |
| **`NamedAnalyzerResult`** | Optimizer-side wrapper: `{Name, Result, Score, Remaining, Spare, RoleSpare, Live}`. Working `Remaining`/`Spare`/`RoleSpare` are decremented by helpers during allocation; `Result` is never mutated. `Live` is set by the engine each cycle and gates scale-down participation (see "How results combine"). |
| **Linearity invariant** | Adding *n* replicas of variant *v* reduces analyzer *i*'s working `Remaining` by exactly *n × PRC_i[v]*. Holds at model scope (non-disaggregated) and at role scope (disaggregated). |

### Responsibility table

| Field | Written by | Read by |
|---|---|---|
| Per-variant `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity`, `Cost`, `Role`, `AcceleratorName` | Analyzer | Optimizer (picker + scaling math) |
| Model-level `TotalSupply`, `TotalAnticipatedSupply`, `TotalDemand` | Analyzer (via aggregation helpers) | Engine post-step |
| Per-role `RoleCapacities[role].Total*` | Analyzer (via aggregation helpers) | Engine post-step |
| `RequiredCapacity`, `SpareCapacity` (model + role scope) | **Engine post-step only** — analyzer-written values are overwritten | Optimizer |
| `NamedAnalyzerResult.Remaining`, `Spare`, `RoleSpare` | Optimizer helpers (`applyAllocation`, `applyDeallocationForRole`) | Optimizer allocation loop |
| `NamedAnalyzerResult.Live` | Engine (`runAnalyzersAndScore`, each cycle) | Scale-down veto gate (`needsScaleDownForRole`, `safeRemovalReplicasForRole`) |

---

## Components

- **Registration** — `internal/engines/saturation/engine.go`:
  `RegisterAnalyzer(name, analyzer) error`. `cmd/main.go` registers external
  analyzers (e.g., throughput) before `StartOptimizeLoop`. Saturation V2 is
  pre-registered at slot 0. The registry is snapshotted at `StartOptimizeLoop`;
  late registration returns an error.
- **Engine post-step** — `internal/engines/saturation/engine_v2.go`:
  `applyUniversalThreshold(*AnalyzerResult, scaleUp, scaleDown)` applies the
  formula `RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)` /
  `SC = max(0, TotalSupply − TotalDemand/scaleDown)` at model scope and
  each role in `RoleCapacities`.
- **Aggregation helpers** — `internal/engines/aggregation/`:
  `SumTotalSupply`, `SumTotalAnticipatedSupply`, `SumTotalDemand`,
  `AggregateByRole` over `[]VariantCapacity`. Analyzer authors use these to
  populate per-scope `Total*` fields without reimplementing the math.
- **Optimizer slice flow** — `internal/engines/pipeline/`:
  `NamedAnalyzerResult` slice carries each analyzer's calibrated result plus
  working scratch state for the allocation loop. `CostAwareOptimizer` and
  `GreedyByScoreOptimizer` consume the slice via shared free functions
  (single-variant, paired P/D, and role-iterated helpers).

---

## User configuration

Analyzers are configured via `SaturationScalingConfig.Analyzers` (YAML key
`analyzers`). Each entry is an `AnalyzerScoreConfig` struct
(`internal/config/saturation_scaling.go`):

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | string | required | Must match the name returned by `Analyzer.Name()` |
| `enabled` | bool | true (when the entry is present) | Set false to disable without removing the analyzer |
| `score` | float64 | 1.0 | Weight in the fair-share priority formula |
| `scaleUpThreshold` | float64 | global | Overrides the model-level `scaleUpThreshold` for this analyzer |
| `scaleDownBoundary` | float64 | global | Overrides the model-level `scaleDownBoundary` for this analyzer |

Minimal YAML example:

```yaml
analyzers:
  - name: saturation
    score: 1.0
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
  - name: throughput
    enabled: false   # disable without removing
    score: 2.0
```

When `enabled` is false the analyzer is neither called nor included in the
result slice, so it cannot veto scale-down decisions.

**Participation is opt-in.** An analyzer registered in code
(`Engine.RegisterAnalyzer`) participates in a cycle only when it has an
explicit entry in `analyzers` with `enabled` `true` or unset. An analyzer with
no entry at all does not run and is not included in the result slice,
exactly as if `enabled: false` had been set. This prevents a
registered-but-unconfigured analyzer from returning `SpareCapacity=0` and
silently vetoing scale-down, since the per-role scale-down decision requires
every voting analyzer in the slice to agree. Saturation is always run
regardless of `analyzers` config — the engine identifies it by name and
appends it as the identity/(a) carrier that supplies per-variant metadata
(`Cost`, `AcceleratorName`, `Role`) for every configured variant. Its *vote*,
however, is opt-in like any other analyzer's: saturation votes in the combine
math only in the default single-analyzer config (no explicit `analyzers` list)
or when its name is explicitly enabled. A `[throughput]`-only config leaves
saturation present as a non-voting carrier — it is pruned from the voting
subset (`votingResults`) and neither vetoes nor constrains scale-down.

---

## Analyzer implementor guide

Implement `domain.Analyzer` (`internal/domain/analyzer.go`):

```go
type Analyzer interface {
    Name() string
    Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}
```

### Input

Key `AnalyzerInput` fields:

| Field | Type | Description |
|---|---|---|
| `ModelID` | string | Model being analyzed |
| `Namespace` | string | Kubernetes namespace |
| `ReplicaMetrics` | `[]ReplicaMetrics` | Per-replica metric snapshots |
| `VariantStates` | `[]VariantReplicaState` | Current/desired/pending replica counts per variant |
| `Config` | `AnalyzerConfig` | Resolved config (cast to your config type as needed) |
| `SchedulerQueue` | `*SchedulerQueueMetrics` | Scheduler queue metrics; nil when flow control is off |
| `ArrivalRate` | float64 | Model-level request arrival rate (req/s), no per-pod labels; zero when EPP absent or no traffic yet |

### Output invariants

The **linearity invariant**: `TotalSupply = Σ_v PerReplicaCapacity × ReplicaCount`
across all entries in `VariantCapacities`. Use the aggregation helpers to
populate `VariantCapacities[]`, then call:

```go
result.TotalSupply             = aggregation.SumTotalSupply(result.VariantCapacities)
result.TotalDemand             = aggregation.SumTotalDemand(result.VariantCapacities)
result.TotalAnticipatedSupply  = aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)
```

For P/D disaggregated models, also populate `RoleCapacities` using
`aggregation.AggregateByRole(result.VariantCapacities)`. The engine applies
`applyUniversalThreshold` to every role entry.

**Do NOT populate `RequiredCapacity` or `SpareCapacity`** in the returned
`AnalyzerResult`. The engine overwrites both fields in the post-step; any
analyzer-written values are discarded.

---

## Pipeline flow

1. `cmd/main.go` calls `engine.RegisterAnalyzer(name, a)` for each external
   analyzer before `StartOptimizeLoop`. Saturation V2 is pre-registered at
   slot 0.
2. `StartOptimizeLoop` snapshots the registry into `analyzersSnapshot`
   (frozen, race-safe). The snapshot is the ordered set of analyzers that
   every optimize cycle iterates.
3. Per cycle, for each model: `runAnalyzersAndScore` runs the saturation V2
   analyzer unconditionally (it drives variant metadata), then iterates
   `analyzersSnapshot` in registration order for non-saturation analyzers.
4. Analyzers with `Enabled: false` are skipped entirely — neither called nor
   appended to the result slice.
5. For each analyzer that runs, `applyUniversalThreshold` is applied to its
   result using resolved thresholds (per-analyzer override beats global):
   `RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)`,
   `SC = max(0, TotalSupply − TotalDemand/scaleDown)`.
6. Each result is wrapped in a `NamedAnalyzerResult{Name, Result, Score,
   Remaining, Spare}` and appended to the `[]NamedAnalyzerResult` slice.
   `Remaining = RC` and `Spare = SC` after the post-step.
7. Saturation supplies the (a)/identity fields. Its `VariantCapacities`
   entries carry `Cost`, `AcceleratorName`, and `Role` for every configured
   variant. The optimizer does not read these off the saturation entry
   directly: the anchor it consumes is a per-variant merge (`bindingAnchor`,
   derived on demand) that takes the (a) identity fields from saturation and
   the (b) sizing fields (`PerReplicaCapacity`, demand) from whichever
   analyzer binds — saturation when it votes, otherwise the sole enabled
   non-saturation analyzer. Slice position is not significant.

---

## How results combine

**Scale-down gate** (`needsScaleDownForRole`): ALL **live** analyzers in the
slice must have `Spare > 0` for a role to scale down. One live analyzer with
`RequiredCapacity > 0` (i.e., `Spare == 0`) blocks scale-down for that role.
`safeRemovalReplicasForRole` (the safe-removal-count computation) applies the
same live-only filter.

**Liveness.** An analyzer is live for the current cycle iff it produced a
non-error, capacity-bearing result within the staleness window (a fixed
multiple of the optimization interval, `analyzerLivenessStaleCycles` in
`internal/engines/saturation/engine_v2.go`). The resolved interval falls back
to a 30s default whenever `Config` is absent **or** reports a non-positive
value, so a misconfigured interval can never zero the staleness window and
latch every analyzer non-live. An informative result with a zero-valued
`AnalyzedAt` is treated as current (recorded as "now") rather than
instantly-stale, so a forgotten timestamp on a future analyzer cannot
silently disarm the veto. A non-live analyzer — one that
has never produced a usable result, is currently erroring, or whose last
usable result has aged past the staleness window — is excluded from the
scale-down vote entirely: it neither vetoes nor constrains the safe-removal
minimum. This prevents a registered-but-uninformative analyzer (no metrics
yet, an error state, or a stale result) from silently blocking scale-down
for every model it's registered against. Recovery is automatic: once the
analyzer produces a fresh capacity-bearing result, it becomes live again on
the next cycle. Liveness is tracked per model, not just per analyzer name,
so one model's freshness never masks another's staleness.

An analyzer reporting no usable capacity (`no-data`) does not become
non-live immediately — it becomes non-live only once its last informative
result ages out of the staleness window. This distinguishes three cases: an
analyzer that never had good data (e.g. a mislabelled metric at startup)
never sets its timestamp and is non-live from the start; a transient
no-data blip on an analyzer with a recent good result stays live and still
participates in the vote (the intended "uncertain, err toward not scaling
down" behavior); and an analyzer whose good data has aged past the window
becomes non-live. A mislabelled or broken metrics query is not treated as
an *error* — it still returns a well-formed result, just one with no usable
capacity — so this reason-based check, not an engine-level error signal, is
what actually detects a durably-broken analyzer.

Within the multi-analyzer engine path (`runAnalyzersAndScore`), this
liveness filter applies uniformly to every *voting* analyzer, including
saturation's own token-capacity signal — there is no name-based exemption
inside the scale-down gate. The gate runs over the voting subset
(`votingResults`, the ballot entries whose analyzer is enabled this cycle):
saturation is subject to it exactly when it votes (default single-analyzer
config, or when its name is explicitly enabled), and a non-voting saturation
carrier is excluded from the vote just like any other disabled analyzer.
(Saturation's separate role as the shared metrics-collection layer — cache
size, replica cost, etc., feeding every analyzer and the cost optimizer — is
unaffected; that collection either succeeds for everyone or, if it fails,
every analyzer ends up non-live and the safety floor below applies.) The
queueing-model optimize path is no longer dispatched: when a queueing-model
ConfigMap is present the engine refuses that path — it logs an error and holds
each model at its last-good replica count for the cycle rather than running
the older, un-tracked optimizer. The path's code is retained but parked;
re-enabling it is a separate follow-up that would make the queueing model a
first-class, liveness-tracked multi-analyzer participant.

Liveness reflects whether an analyzer has a current *capacity* (supply-side)
signal — it does not gate on the *demand* signal. A falsely-low demand value
only biases toward scale-down, never toward a spurious veto, so it never
affects the veto gate; demand robustness is handled upstream by other
mechanisms (metric sanity checks on calibration inputs, request-rate /
local-demand fallbacks).

**Demand-liveness telemetry (warn-only).** As an observability aid, the engine
separately watches for the throughput analyzer having a live capacity signal
while reporting no demand (`TotalDemand == 0`) for at least the staleness
window. This usually means the request-arrival query is misconfigured or EPP
is not reporting arrivals — supply is being measured but no load is observed,
so scale-up will never trigger. When detected, the engine logs a warning; it
never sets `Live`, never touches `RoleSpare`, and never gates any scaling
decision. The signal is a timestamp gap rather than a boolean so a cold-start
scrape lag (supply resolving a cycle or two before the first arrival scrape)
does not false-positive: the gap only reaches the staleness window after
demand has genuinely been absent for that long.

**Safety floor.** If every analyzer in the slice is non-live for a role,
`needsScaleDownForRole` returns false rather than falling through to "no
vetoes, so scale down" — with zero live analyzers there is no current basis
to scale down. This also makes leader failover safe: a freshly-elected
leader starts with no liveness history, so scale-down for every role is
withheld until at least one analyzer produces a fresh result (typically
within a cycle or two).

**Scale-up gate** (`anyRoleNeedsScaleUp`): ANY analyzer having `Remaining > 0`
triggers scale-up for the corresponding role. The liveness gate does not
apply to scale-up — a non-live analyzer contributes `Remaining == 0`
(from `RequiredCapacity == 0`), which is already harmless to the max-across-
analyzers formula.

The optimizer never reads per-variant metadata straight off the saturation
entry. It consumes an **anchor** built on demand by `bindingAnchor`: a fresh,
per-variant `AnalyzerResult` merged by `VariantName` from the (a)/identity
fields (`Cost`, `AcceleratorName`, `Role`, replica counts) that saturation
supplies for every configured variant, plus the (b)/sizing fields
(`PerReplicaCapacity`, demand) from whichever analyzer binds. Nothing is
stored — the merge is recomputed each time it is needed — and when nothing
can bind (an empty ballot, no enabled-and-live-and-informative analyzer, or an
ambiguous set of binding candidates) `bindingAnchor` returns `nil` and the
optimizer holds that model unchanged for the cycle.

### Scale-from-zero and zero-replica variants

The throughput analyzer computes per-replica capacity from *live* replica
metrics, so a variant that has scaled to zero produces no capacity row and
would drop out of the anchor merge — leaving it unselectable for a proactive
scale-up. To keep a returning variant selectable, the throughput analyzer
emits a per-replica-capacity-only fallback (`Reason: "T-sfz"`) for any variant
it observed live earlier, carrying that variant's persisted last-good
per-replica supply. It emits only the (b)/sizing field: `Cost`,
`AcceleratorName`, and `Role` remain saturation's (a)/identity, supplied
through the merge. A variant the throughput analyzer has never seen gets no
fallback — its `PerReplicaCapacity` stays zero and it is not proactively
selectable; the reactive `scalefromzero` engine still covers genuine
cold-starts. The persisted supply self-expires on the analyzer's idle window
(the observation-max-age eviction, ~60 min), so a long-idle variant degrades
back to the never-seen case on its own.

**Known limitation.** `Cost` always comes from saturation's (a)/identity, and
saturation reports `Cost = 0` for a variant currently at zero replicas. A
returning variant therefore has a cost-efficiency of `0 / PerReplicaCapacity`
and ranks cheapest, so the cost optimizer picks it first on scale-up. This is
a pre-existing saturation behavior — it affects every config with a returning
zero-replica variant, and resolving it means fixing that separate saturation
`Cost = 0` behavior, which is out of scope here. Scale-from-zero still
functions (the variant is selected, if eagerly); only cost *priority* is
affected. Because no cooldown or grace period exists in the cost optimizer,
the choice can flap while load oscillates across the scale-up/scale-down
boundary, or persist as a costlier-than-ideal allocation while load stays
high. That flapping gap is pre-existing and not introduced by this mechanism.

---

## Data model: AnalyzerResult → NamedAnalyzerResult

Understanding what transforms where prevents the most common mistake: treating
`Result.*` counters as live state during allocation.

**`interfaces.AnalyzerResult`** is the immutable record an analyzer returns.
The engine owns its calibration:

1. The analyzer populates `VariantCapacities[]`, `TotalSupply`, `TotalDemand`,
   `TotalAnticipatedSupply` (and `RoleCapacities` for P/D models). It must NOT
   populate `RequiredCapacity` or `SpareCapacity`.
2. `applyUniversalThreshold` overwrites `RequiredCapacity` / `SpareCapacity` at
   model scope, and each `RoleCapacities[role].RequiredCapacity` /
   `SpareCapacity`. The analyzer's view of supply and demand is fixed here.
3. The engine wraps the calibrated result in a `NamedAnalyzerResult` and never
   mutates `Result` again. `Result.*` values are stable read-only data for the
   rest of the cycle.

**`pipeline.NamedAnalyzerResult`** is the working unit the optimizer operates on.
Its fields fall into three categories:

| Field | Category | Description |
|---|---|---|
| `Name`, `Score`, `Result` | Immutable | Set by engine; never written by optimizer |
| `Remaining`, `Spare` | Mutable scalars | Model-scope working counters; decremented by `applyAllocation` during scale-up |
| `RoleSpare` | Mutable per-role map | Populated by `initRoleState`; decremented by `applyDeallocationForRole` during scale-down |

`Remaining` and `Spare` are seeded from `Result.RequiredCapacity` and
`Result.SpareCapacity`. `RoleSpare` is seeded from
`Result.RoleCapacities[role].SpareCapacity`. None of this flows back into
`Result`.

**`RolePairedState`** (`[]map[string]float64`, indexed as
`[analyzer-index][role]`) is picker-local demand created per call to
`initRoleState`. It holds per-role required capacity for the scale-up loop and
is decremented by the joint-commit step inside `allocateForModelPaired`. It is
not stored on `NamedAnalyzerResult` and is discarded after each model's
allocation pass.

---

## Optimizer internals and helper composition

Both optimizers share the same allocation and scale-down primitives from
`internal/engines/pipeline/analyzer_helpers.go` and
`internal/engines/pipeline/cost_aware_optimizer.go`. The optimizers own the
*when* and *which model*; the helpers own the *how*.

### Scale-up path

All scale-up goes through `allocateForModelPaired`:

```
initRoleState(s)               → roles, RolePairedState (per-role demand + RoleSpare)
anyRoleNeedsScaleUp(ps, roles) → loop gate: any role still has demand?
  pick(role, ...)              → (variant, capN): optimizer-specific variant selector
  roleBottleneckReplicas       → max_i ceil(state[i][role] / PRC_i[v]): cross-analyzer replica sizing
  roleAggRemaining             → max demand across analyzers for this role
  Δ_util = min_role util_role  → joint commit bound: trim to the least-served role
  applyAllocation(s, v, k)     → decrement Remaining on all NamedAnalyzerResults
```

`pick` is a `RolePickFn` — the only part that differs between optimizers:

- `costGreedyRolePick`: picks the cheapest cost-efficient variant; no GPU budget
  cap (unlimited mode).
- `fairShareRolePick`: picks the cheapest variant within available GPU budget;
  caps `capN` to the fair-share target (limited mode).

For non-disaggregated models, `initRoleState` synthesizes a single `"both"` role
from the model-level scalars, so `allocateForModelPaired` handles both the
disaggregated and non-disaggregated cases through the same loop.

### Scale-down path

Both optimizers call `scaleDownRoleIterated`, which handles both disaggregated
and non-disaggregated models through the same role loop (`"both"` is the
synthetic role for non-disaggregated):

```
for each role (sorted for determinism):
  needsScaleDownForRole(s, role)           → gate: ALL live analyzers have RoleSpare > 0
                                              (no live analyzer → false; see "How results combine")
  sortVariantsForScaleDown(s, vcs)         → cost-desc; tie-break: Score-weighted PRC asc
  scaleDownVariantSet(...)
    safeRemovalReplicasForRole(s, v, role) → min over live i of floor(RoleSpare[i][role] / PRC_i[v])
    applyDeallocationForRole(s, v, role, n)→ decrement RoleSpare on all entries
```

`sortVariantsForScaleDown` uses a Score-weighted PRC tie-break. With a single
analyzer (Score=1) this reduces to plain cost-descending / PRC-ascending order.

### Fair-share iteration (GreedyByScoreOptimizer only)

`fairShareScaleUp` uses iterative mean equalization rather than fixed fractions:

1. Compute `mean` = average `remaining` (fair-share priority value) across active
   models.
2. Sort by `remaining` descending; take the highest.
3. Call `allocateForModel` with budget `target = remaining − mean`: allocates
   replicas via `allocateForModelPaired` until the model's priority value drops
   to or below `mean`.
4. Recompute `remaining = fairShareValue(priority, s, ps, roles)` from the
   post-allocation working state.
5. Repeat until no active models remain or no GPUs are left.

`fairShareValue = priority × Σᵢ Score_i × Σ_role pickerState[i][role]`.
A higher `Score` on a high-demand analyzer increases a model's priority value
and therefore how many GPUs it attracts in a constrained environment.

---

## Optimizer consumption

The `[]NamedAnalyzerResult` slice is passed to one of two optimizers depending
on the `enableLimiter` flag in `SaturationScalingConfig`:

- **`CostAwareOptimizer`** (unlimited mode, `enableLimiter: false`): operates
  on the saturation entry's `VariantCapacities` for cost and role data; scales
  up the cheapest variant that covers the required capacity, scales down the
  most expensive variant with spare capacity.
- **`GreedyByScoreOptimizer`** (limited mode, `enableLimiter: true`): respects
  `ResourceConstraints` (GPU budgets per accelerator type). Models are ordered
  by fair-share priority value:
  `fsv = Priority × Σᵢ Score_i × Σ_role pickerState[i][role]`,
  where the sum over `i` runs across all `NamedAnalyzerResult` entries and
  `pickerState` is seeded from each entry's `Remaining`. Higher `Score` on a
  high-demand analyzer increases a model's allocation priority in constrained
  environments.

Both optimizers are stateless and selected per-cycle from the engine's
`optimizer` field.

## Observability

The engine emits two structured INFO log lines per reconcile cycle per model —
one per analyzer (after the threshold post-step) and one after the optimizer
returns. See [cycle-log.md](cycle-log.md) for field schemas, grep patterns,
and an explanation of the `reason` values set by each analyzer.
