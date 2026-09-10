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
│ Saturation V2 (always run — the identity carrier),       │
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
│       refresh anchor sizing per iter (multi-vote only)   │
│       pick(role) → variant; joint Δ_util commit          │
│       applyAllocation → decrement Remaining              │
│   • Scale-down: scaleDownRoleIterated                    │
│       needsScaleDownForRole → veto gate (ALL live agree) │
│       safeRemovalReplicasForRole → combine min over live │
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
| `score` | float64 | 1.0 | Belief weight over this analyzer's replica votes, applied in the combine (see [How results combine](#how-results-combine)); `1.0` everywhere gives the plain max/min |
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
appends it as the identity carrier that supplies per-variant metadata
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
7. Saturation supplies the identity fields. Its `VariantCapacities`
   entries carry `Cost`, `AcceleratorName`, and `Role` for every configured
   variant. The optimizer does not read these off the saturation entry
   directly: the anchor it consumes is a per-variant merge (`bindingAnchor`,
   derived on demand) that takes the identity fields from saturation and
   the sizing fields (`PerReplicaCapacity`, demand) from whichever
   analyzer binds — saturation when it votes, otherwise the lowest-ballot-index
   qualifying non-saturation analyzer (see "How results combine" below for the
   tie-break when more than one qualifies). The combine math itself
   (`votingResults`) is order-independent; only the binder tie-break reads
   ballot position.

---

## How results combine

**One combine core.** Every cross-analyzer quantity — scale-up sizing,
safe-removal counts, rescale demand — reduces one `(variant, role)` ballot to a
single replica count through the same function:

```go
func combineVotes(votes []replicaVote, up bool) (value float64, binder int)
```

`up=true` takes the max (scale-up demand), `up=false` the min (scale-down safe
removal). It returns **both** the count and the ballot index of the binding
analyzer from a single evaluation, so "how many replicas" and "which analyzer
decided that" can no longer disagree — the binder is what the per-iteration
anchor refresh writes onto the anchor's sizing fields. Ties keep the lowest
ballot index. An empty ballot returns `(0, -1)`, which callers read as "no basis
to act".

Rounding happens **once, at the caller** — `ceil` for scale-up, `floor` for
scale-down — never per vote.

**Who participates.** Three thin collectors, one per state source, decide
membership:

| Collector | Feeds | Vote value |
|---|---|---|
| `votesFromPickerState` | scale-up (`roleBottleneckReplicas`, `roleAggRemaining`) | `pickerState[i][role] / PRC_i[v]` |
| `votesFromRoleSpare` | scale-down (`safeRemovalReplicasForRole`) | `RoleSpare[i][role] / PRC_i[v]` |
| `votesFromTotalDemand` | rescale (`roleDemandGPUs`) | `demand_i[role] / PRC_i[v]` |

All three require a `Result` and a positive `PerReplicaCapacity` for that
variant: with no conversion factor there is no vote to cast.
`votesFromRoleSpare` additionally drops non-live entries. Keeping the filter in
the collectors rather than in `combineVotes` is deliberate — it means an
analyzer that says nothing about a `(variant, role)` is structurally absent from
the combine, so it cannot influence the outcome by staying silent.

The three differ on a *missing role*. `votesFromTotalDemand` drops an entry that
doesn't decompose the role; the other two read the map-miss as `0`, so the entry
votes zero rather than abstaining. On the scale-down path that difference is
visible and deliberate — see the gate discussion below.

**Abstaining is not the same as being exempt.** An analyzer that cannot price a
variant contributes no claim for it — that is the participation filter above —
and it must also spend nothing on it. Those are two halves of one property, and
they are enforced in two different places: the claim side by the collectors, the
spend side by the `continue` at `allocateForModel`'s per-role clamp. It is
tempting to read the clamp's `continue` as merely skipping a harmless entry.
That reading is wrong twice over. An entry that escapes the clamp still holds
whatever demand it reported, and demand that was never converted into the
model's claim but is still available to draw against the model's entitlement is
an unpriced draw on a shared budget — the analyzer spends from a fair share it
did not contribute to. The wording matters for the same reason: "skipped"
describes what the code does, "abstains" describes what it means, and only the
second makes the missing spend look like part of the contract.

The reason this is easy to miss is that the two filters are keyed on **different
variants**. The clamp keys on the role's *reference* variant
(`referenceVariantForRole`); the vote keys on the variant the picker actually
landed on. When they coincide, an entry that escapes the clamp is also absent
from the vote, and nothing is observable. When they diverge — which
`referenceVariantForRole`'s own doc comment says is expected once the cheaper
variant is at its replica ceiling — the entry can escape the clamp and still
vote for a variant it *can* price. Whether that is observable then depends on
whether the entitlement or the vote bottleneck binds first, so the safety here
is a coincidence of two independent filters rather than a guarantee. There is a
measured case where it does not hold; see [Fair-share
iteration](#fair-share-iteration-greedybyscoreoptimizer-only).

**How much each vote counts.** Read the extremum first: with every analyzer at
the default `score: 1.0` — which is what every shipped config uses — the combine
*is* the plain cross-analyzer max (scale-up) or min (scale-down), and the rest of
this paragraph is inert. A non-uniform score turns on a dominance correction:

```
v_i = replicas analyzer i implies   s_i = analyzer i's score  (> 0)
e   = max v_i (up) | min v_i (down)  -- the binder's own vote,  s_e its score

v*  = e  -  SUM_i (e - v_i)*(s_i - s_e)+ / SUM_j s_j        ((x)+ = max(x, 0))
```

Only the *excess* over the binder's score pulls, which is what keeps `v*` inside
`[min v_i, max v_i]`: the combine can never invent a replica count no analyzer
asked for, and raising every score together changes nothing. One expression
serves both directions — on scale-down `e` is the min, so `(e - v_i) <= 0` and
the subtraction adds. `SUM_j s_j` runs over participating votes only, per the
filter above.

Worked example: throughput's demand implies 10 replicas at `score: 1.0`,
saturation's implies 5 at `score: 2.0`. Throughput binds (`e = 10`, `s_e = 1`,
`SUM s = 3`), the correction is `(10-5)*(2-1)/3 = 1.67`, so `v* = 8.33` and the
caller's single `ceil` gives **9 replicas** — still driven by the larger demand,
pulled down because the dissenter is trusted more. Note 9 is neither analyzer's
number and neither analyzer's rounding; this is why rounding once at the caller
matters. Swap the scores and every `(s_i - s_e)+` is zero, leaving the extremum
untouched: trusting the conservative voter more never moves the result away from
safety.

`score` is consumed here and nowhere else. It is a belief weight over votes, not
a priority and not a budget multiplier — model `priority` is the only fair-share
weight. See
[AnalyzerScoreConfig Fields](saturation-scaling-config.md#analyzerscoreconfig-fields)
for the operator-facing description.

`needsScaleDownForRole` keeps its own boolean all-agree shape rather than
delegating: it is a veto, not a magnitude.

**Scale-down gate** (`needsScaleDownForRole`): every **live** analyzer that
has an opinion on a role must report `Spare > 0` for that role to scale down.
"Has an opinion" excludes a live voter whose own `RoleSpare` simply doesn't
decompose that role (e.g. a non-disaggregated analyzer's single `RoleBoth`
entry, asked about `prefill`) — that voter **abstains** rather than reading
the map-miss as `Spare == 0`: a coarser voter has no basis to veto a
role it never sized. A live analyzer that DOES have an opinion and reports
`RequiredCapacity > 0` (i.e., `Spare == 0`) still blocks scale-down for that
role.

The gate and the per-variant enforcement point share one predicate,
`roleSpareVetoed`, so the gate is an early-out for the whole role and
`safeRemovalReplicasForRole` is where the objection is actually enforced — see
["Scale-down path"](#scale-down-path) for why one check is not enough. The
*ballot* keeps a weaker filter than either: a live voter whose `RoleSpare`
doesn't decompose the role abstains from the gate and the veto, yet still votes
`0.0` on the count. That `0` is not itself a block — with non-uniform scores the
dominance correction pulls the combined value positive — so the three filters
still do not agree, and the count can read low for a voter that had no opinion to
give. Nothing unsafe follows from it: a spurious `0` can only under-remove.

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
saturation's own token-capacity signal — there is no name-based exemption.
`votingResults` prunes the ballot to entries that are both Enabled and Live
(VG-up) — the single gate feeding both scale-up and scale-down, so a stale
Enabled analyzer can no longer force a spurious scale-up any more than it
could veto scale-down: saturation is subject to it exactly when it votes
(default single-analyzer config, or when its name is explicitly enabled),
and a non-voting or non-live saturation carrier is excluded from the vote
just like any other disabled or stale analyzer.
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
triggers scale-up for the corresponding role. `anyRoleNeedsScaleUp` and the
`roleBottleneckReplicas`/`roleAggRemaining` combine it feeds only ever see
`votingResults`' pruned (Enabled && Live) slice, so a stale Enabled analyzer's
last `Remaining` value is never read at all (VG-up) — scale-up no longer
depends on the external invariant "a dead analyzer's `Remaining` happens to
be 0"; it is structurally excluded from the combine regardless of what value
it holds.

The optimizer never reads per-variant metadata straight off the saturation
entry. It consumes an **anchor** built on demand by `bindingAnchor`: a fresh,
per-variant `AnalyzerResult` merged by `VariantName` from the identity
fields (`Cost`, `AcceleratorName`, `Role`, replica counts) that saturation
supplies for every configured variant, plus the sizing fields
(`PerReplicaCapacity`, demand) from whichever analyzer binds. Nothing is
stored — the merge is recomputed each time it is needed.

**Binder selection** is deterministic, never a guess: saturation binds
whenever it is enabled, live, and informative. Otherwise, the binder is the
lowest-ballot-index non-saturation entry that is enabled, live, and
informative — with two or more analyzers enabled (`[sat, TA]` and beyond), a
tie among qualifying non-saturation entries resolves to the earliest one in
the ballot rather than holding the model; the later entry still votes in the
quantity combine, it just does not become the binder. `bindingAnchor` returns
`nil` — and the optimizer holds that model unchanged for the cycle — only when
literally nothing qualifies: an empty ballot, or no analyzer that is both
enabled+live and informative.

### Scale-from-zero and zero-replica variants

The throughput analyzer computes per-replica capacity from *live* replica
metrics, so a variant that has scaled to zero produces no capacity row and
would drop out of the anchor merge — leaving it unselectable for a proactive
scale-up. To keep a returning variant selectable, the throughput analyzer
emits a per-replica-capacity-only fallback (`Reason: "T-sfz"`) for any variant
it observed live earlier, carrying that variant's persisted last-good
per-replica supply. It emits only the sizing field: `Cost`,
`AcceleratorName`, and `Role` remain saturation's identity, supplied
through the merge. A variant the throughput analyzer has never seen gets no
fallback, in any config — its `PerReplicaCapacity` stays zero and it is not
proactively selectable; the reactive `scalefromzero` engine still covers
genuine cold-starts. This holds uniformly whether or not saturation is also
voting: the anchor never borrows saturation's own sizing for a variant the
binder omits — a binder-unknown variant abstains rather than mixing
metric scales across variants within one anchor. The persisted supply
self-expires on the analyzer's idle window (the observation-max-age eviction,
~60 min), so a long-idle variant degrades back to the never-seen case on its
own.

**Known limitation.** `Cost` always comes from saturation's identity, and
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

**Proactive admission of an unpriced variant: built, not enabled.** The rule
above — a variant no analyzer can price is not proactively selectable — has a
narrow intended exception. The exception's *guard* ships; its *trigger* does
not, so the rule above is still the whole of today's behavior. Read this
subsection for what the guard is for; do not read it as a description of what
happens on a live cluster.

The intended shape is three claims. **One:** a zero-replica variant that no
analyzer on the ballot can price is *admitted* with a per-replica capacity of one,
tagged by its own reason constant (`ReasonFromZeroAdmission`), so that the
eligibility gates the optimizers already apply — every one of which rejects a
non-positive per-replica capacity — stop excluding it. **Two:** that admission is
not a capacity estimate and must never be spent as one, so the variant's target is
ceilinged at a single replica. The phrase to carry is *unpriced capacity, bounded
spend*. **Three:** the ceiling is on the variant's **target**, not on one
iteration, and a picker that cannot grant the replica **skips the variant** rather
than returning a cap of zero. That third part is the non-obvious one, and it is
worth stating why: a returned zero makes the caller compute a utilization delta of
zero and break out of the *whole model's* allocation loop, so a bounded-out variant
would deny every variant behind it as well.

Claims two and three ship. `maxTargetReplicas` merges the variant's configured
`MaxReplicas` with the admission ceiling and returns the tighter of the two, and
all three granting sites — `costGreedyRolePick`, `fairShareRolePick`, `fillRole` —
consult it and skip rather than zero-cap. Claim one does **not** ship: nothing in
production code writes the tag, so no variant carries it, and the ceiling's
admission clause is reachable only from tests. For an untagged variant
`maxTargetReplicas` is the `MaxReplicas` check verbatim, so nothing else moves.
The admitting write is held because an anchor-only sentinel makes a variant
*selectable* without making it *sizable*, and the two are sourced differently:
selection reads the anchor, but the replica count comes from the ballot, via
`votesFromPickerState` → `combineVotes` → `roleBottleneckReplicas`, which abstains
for a variant no voting entry prices and so yields zero. The optimizer then sees a
utilization delta of zero and breaks — the same collapse claim three guards
against, arriving by a different route and costing the model every variant behind
the admitted one. That is a regression rather than a missed feature. Whether the
sentinel may instead be written on the binding analyzer's own ballot entry is an
open question; the reasoning is
recorded at `ReasonFromZeroAdmission` in `analyzer_helpers.go`, beside the constant
it applies to. Contrast the returning variant above, which works precisely because
the throughput analyzer emits its persisted per-replica supply into the **ballot**
rather than into the anchor.

One property to correct while the mechanism is dormant, because it is easy to
assume the reverse: a per-replica capacity of one does degenerate cost-per-unit
ordering to raw cost, but that does **not** make an admitted variant sort last. Its
`Cost` arrives as `0` from the same zero-replica lookup the limitation above
describes, so its cost-efficiency is `0 / 1 = 0` and it sorts **first**; its
never-measured peers all tie at `0` under an unstable sort, so the choice among
them is arbitrary rather than cost-ordered. No sentinel value repairs this —
`Cost = 0` zeroes the ratio for any positive capacity — and the ordering recovers
only when the `Cost = 0` behavior itself is fixed, which is out of scope here. The
one-replica ceiling is therefore the *only* guard on an admitted variant, not one
of two.

### A structurally unmodeled role does not vote

The throughput analyzer has no demand model for the prefill role at all: its
role split excludes prefill by construction, so `RoleCapacities["prefill"]`
always carries `TotalDemand = 0` — not because prefill needs nothing this
cycle, but because the map key was never computed. Left unmarked, that zero
reads to every ballot function exactly like a real measurement of "nothing
needed", and on the scale-down side it is actively dangerous: with no demand
to weigh against, the whole prefill fleet reads as spare, and a single-voter
ballot drains it to its floor.

**The fix is to abstain, not to vote a real-looking zero.** `RoleCapacity`
carries a `Reason` field for exactly this: an analyzer that has no demand
model for a role tags that role's entry with `ReasonRoleUnmodeled`
(`internal/engines/pipeline/analyzer_helpers.go`), and every ballot-collector
function that reads `RoleCapacities` -- `votesFromPickerState`,
`votesFromRoleSpare`, `votesFromTotalDemand` -- skips a tagged entry instead
of counting its value. An entry that abstains casts no vote at all, which is
a different statement from casting a vote of zero: `combineVotes` on an empty
ballot returns "no basis to act", while a real zero vote still participates
and can be outweighed. Saturation is role-complete for every role its
variants declare, so it never sets this tag.

**What this closes: the drain.** When decode has no scale-up demand and the
model is on the scale-down path, an analyzer with a real per-role model
(saturation) still votes its measured prefill spare; one with no model for
the role now abstains instead of voting the whole fleet as removable. The
single-voter case -- the analyzer with no prefill model is the only one
sizing it -- stops draining prefill toward its floor.

**What this does not close: the freeze.** When decode needs scale-up, the
whole model takes the scale-up path, and prefill's own demand is still zero
by construction -- abstaining changes nothing when there is no second voter
to un-suppress. Prefill stays frozen at its current replica count, including
zero, exactly as before. Closing that side needs a real demand model for the
role, which is future analyzer work, not a ballot-participation fix.

**Observability follows the same rule.** A decision built from a role tagged
`ReasonRoleUnmodeled` does not publish that role's `RequiredCapacity`/
`SpareCapacity` -- both would misrepresent a structural non-answer as a
measurement -- and falls back to the model-level totals instead, the same
fallback already used when no per-role entry exists at all.

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
  refreshAnchorSizing(variants, s, ps) → re-select each variant's (role,v) binder (multi-vote only)
  pick(role, ...)              → (variant, capN): optimizer-specific variant selector
  roleBottleneckReplicas       → ceil(combineVotes(votesFromPickerState(...), up)): cross-analyzer replica sizing
  roleAggRemaining             → the binding entry's own raw demand (same combine, second return value)
  Δ_util = min_role util_role  → joint commit bound: trim to the least-served role
  pickerState[i][role] -= k*PRC_i[v] → per-analyzer decrement (each analyzer's OWN PRC, not the anchor's)
  applyAllocation(s, v, k)     → decrement Remaining on all NamedAnalyzerResults
```

`pick` is a `RolePickFn` — the only part that differs between optimizers:

- `costGreedyRolePick`: picks the cheapest cost-efficient variant; no GPU budget
  cap (unlimited mode).
- `fairShareRolePick`: picks the cheapest variant within available GPU budget;
  caps `capN` to what is *left* of the model's fair-share entitlement after the
  roles that drew before it (limited mode) — not to the whole entitlement, which
  each role would otherwise clamp against independently. See
  [Fair-share iteration](#fair-share-iteration-greedybyscoreoptimizer-only).

For non-disaggregated models, `initRoleState` synthesizes a single `"both"` role
from the model-level scalars, so `allocateForModelPaired` handles both the
disaggregated and non-disaggregated cases through the same loop.

**Per-iteration anchor refresh.** `refreshAnchorSizing` re-invokes the (role,
variant) binder selection — the second return value of the same `combineVotes`
call that sizes the role — at the head of every iteration, mutating the anchor's
`VariantCapacities` in place. With a single voter it is not called at all —
the anchor's one-time pick from `bindingAnchor` already equals the sole
voter's, and calling it would just reproduce the same values (see "How
results combine" for the binder tie-break itself). With two or more voters,
each analyzer's remaining demand shrinks at its own rate as replicas commit,
so the binder for a given `(role, variant)` — and the cost-efficiency ranking
that follows from its `PerReplicaCapacity` — can change partway through a
single water-fill, not just once per optimize cycle.

**Per-analyzer decrement.** Committing `k` replicas of the picked variant `v`
decrements each voting entry's `pickerState[i][role]` by `k` times *that
entry's own* `PerReplicaCapacity[v]` — not the anchor's (the binder's) PRC
applied uniformly. With a single voter the two are the same value, so this is
byte-identical there; once a second analyzer votes, its PRC for `v` can
differ from the binder's, and decrementing by the wrong PRC would leave its
remaining demand over- or under-stated for the next iteration's bottleneck
and binder calculations.

### Scale-down path

Both optimizers call `scaleDownRoleIterated`, which handles both disaggregated
and non-disaggregated models through the same role loop (`"both"` is the
synthetic role for non-disaggregated):

```
for each role (sorted for determinism):
  needsScaleDownForRole(s, role)           → early-out: no live analyzer objects at role level
                                              (no live analyzer → false; see "How results combine")
  sortVariantsForScaleDown(s, vcs, states) → cost-desc; tie-break: coverage per GPU freed, asc
                                              (an ordering key; nothing here is spent)
  scaleDownVariantSet(...)
    safeRemovalReplicasForRole(s, v, role) → roleSpareVetoed → 0, else
                                             floor(combineVotes(votesFromRoleSpare(...), down)) over live i
    applyDeallocationForRole(s, v, role, n)→ decrement each reported RoleSpare balance
```

**The role-level objection is re-checked per variant, not only at role entry.**
`safeRemovalReplicasForRole` returns 0 whenever any live analyzer holds an
explicit non-positive `RoleSpare[role]`, before it combines anything. The
role-entry gate is not sufficient on its own: `applyDeallocationForRole` decrements
every analyzer's role balance by `n × PRC_i[v]` after each variant sheds, so a
spare that was positive when the gate ran can be exhausted **mid-loop**, and the
gate is never re-checked. From that point on, two things would otherwise discard
the objection. The objector may not size the *next* variant, and
`votesFromRoleSpare` drops entries with no per-variant capacity — so the veto is
**PRC-blind**. And a `0` vote is not a veto once scores are non-uniform: the
dominance correction pulls the combined value positive whenever a higher-scored
voter reports spare — so the veto is also **score-blind**. A veto is not a
magnitude; there is nothing to convert and nothing to weigh.

An **absent** key is a different statement from a **present** zero. A live
analyzer whose `RoleSpare` does not decompose this role never sized it and so
**abstains**; one whose key is present and `≤ 0` did size it and reports
there is nothing left to give back, which vetoes. That distinction has to survive
the whole loop, which is why `applyDeallocationForRole` draws down only balances an
analyzer actually reported: a bare decrement on a missing key would materialize it
at zero and manufacture a veto out of a silence.

**Shed order.** `sortVariantsForScaleDown` is cost-descending, and ties break on
*coverage per GPU freed* ascending — `maxᵢ PRC_i[v] ÷ GPUsPerReplica[v]`. Shedding
one replica of `v` returns `GPUsPerReplica[v]` GPUs and gives up `PRC[v]` of
serving capacity, so the ratio is what one freed GPU costs in coverage, and
ascending order sheds whatever gives up the least per GPU it returns. Dividing by
`GPUsPerReplica` is what makes two differently-sized variants comparable; raw
capacity alone prefers shedding the smaller variant even when the larger one is
strictly less efficient per GPU. The combine across analyzers is a **maximum,
never a sum** — no single removal gives up the total of every analyzer's separate
estimate, and a sum grows with the number of configured analyzers, so adding a
voter would reorder the shed with no observation having changed. `Score` does not
appear: it is a belief weight consumed by the sizing combine and it stops there,
and this key is a comparator input that reduces no budget. With a single analyzer,
and for a role whose variants share a `GPUsPerReplica`, this reduces to plain
cost-descending / PRC-ascending order.

The scale-down path never refreshes the anchor's sizing. `refreshAnchorSizing` is
a scale-up-loop step only; nothing here re-reads a binder to overwrite
`VariantCapacities`, so the anchor a scale-down decision is built from is the one
the ballot produced.

### Fair-share iteration (GreedyByScoreOptimizer only)

`fairShareScaleUp` uses iterative mean equalization rather than fixed fractions:

1. Compute `mean` = average `remaining` (the model's fair-share claim, in
   priority-scaled GPUs) across active models.
2. Sort by `remaining` descending; take the highest.
3. Call `allocateForModel` with budget `target = remaining − mean`: allocates
   replicas via `allocateForModelPaired` until the model's claim drops to or
   below `mean`, or until `target` is spent — whichever comes first.
4. Recompute `remaining = fairShareValue(priority, s, ps, roles, variants, stateMap)`
   from the post-allocation working state.
5. Repeat until no active models remain or no GPUs are left.

```
fairShareValue = priority × Σ_role maxᵢ toGPUs(pickerState[i][role], PRC_i[v_role], GPUsPerReplica[v_role])
```

**The currency is GPUs**, and that is what makes the rest of the loop
well-formed. The sum across a model's roles is meaningful only because prefill
and decode compete for the same physical GPUs — the same sum in tokens per
second, or in replicas of two differently-sized variants, adds quantities that
are not interchangeable. The mean is a common water level only because every
model's claim is in that same unit.

Within one role the combine across analyzers is a **maximum, never a sum**: a
role needs as many GPUs as its most demanding analyzer says it does, not the
total of their separate opinions. The maximum is **unweighted**. `Score` does
not appear anywhere in this formula — it is a belief weight about how much a
variant serves, it is consumed by the sizing combine (`combineVotes`), and it
stops there. The fair-share claim is *spent*: it becomes the model's budget for
the round, and a ranking weight must not scale a quantity that is later spent.

`priority` is the only fair-share weight. It is folded into the claim rather
than applied to the comparison alone, so `target` carries it too and is
priority-scaled GPUs rather than a plain GPU count; separating the two is a
change this section does not describe. On the way back out, `allocateForModel`
converts that bound into each analyzer's *own* metric through that entry's own
per-replica capacity — the one place a per-replica capacity is applied leaving
GPU space, and it converts a bound, never a quantity.

**One entitlement per model, spent jointly across its roles.** `target` is a
single balance, not a per-role allowance: prefill and decode draw it down in
sequence, so the sum of what the roles spend is bounded by the model's `target`.
Concretely, a P/D model no longer makes two full-budget draws — a 7-GPU
entitlement used to let prefill commit 7 GPUs and decode commit 7 more. Both
clamps read the *remaining* balance rather than `target`: the demand clamp in
`allocateForModel`, which converts the balance into each entry's own metric one
role at a time, and `fairShareRolePick`'s `capN` cap.

The draw is **sequenced**, not split into fixed per-role shares — a static split
under-serves whichever role is cheaper to satisfy, since a role needing less than
its share cannot hand the difference back. Two floors keep the sequence from
starving whoever draws last, and they are not the same rule. Sizing a role
**holds back** one GPU for each role still to draw; that applies on every draw,
because it only moves room between roles and so can never inflate the spend. On
the model's **first** draw only, a role may additionally take one indivisible
replica even when the balance no longer covers it: `allocateForModelPaired`'s
pick loop is all-roles-or-nothing, so before anything is committed an empty pick
makes the caller abandon the model outright, whereas once the model holds a
commitment an exhausted balance merely ends the loop with that commitment intact.
Kept on past the first draw, that floor would be a per-iteration drip the
entitlement never bounds.

Ending the loop mid-model is what makes `debitCommittedDemand` necessary.
`allocateForModel` re-seeds picker-local demand on every call, and nothing writes
an allocation back to `RoleCapacities[role].RequiredCapacity` — `applyAllocation`
refreshes only the model-level scalar. So the per-role seed is subtracted by what
the model has already been given: target replicas against observed current, per
variant, priced at each entry's *own* `PerReplicaCapacity` for that variant —
the same quantity the allocation loop charges when it commits. Without it the
next round would serve the original demand a second time.

**What was masking this.** Every commit is also bounded by the downstream GPU
pool, so a model drawing its entitlement twice still could not conjure hardware
that does not exist, and the doubled draw surfaced as a fair-share violation
rather than as a failure: the pool was enforced, the fair share was not. It moved
no golden either, because with a single active model `allocationMean` is forced to
`0` — the water level `mean` itself is unchanged, and still governs the
above-the-level drop check — so `target == claim` and the entitlement can only
bind under contention. It is stronger than that: `claimGPUs` sums the role claims
in the same GPU currency the roles are then charged in, so with one active model
the entitlement equals the combined spend exactly. No single-model golden *can*
move on this.

**A claim is priced through one variant and spent through another.** `claimGPUs`
converts a role's demand to GPUs through `referenceVariantForRole` — the role's
cost-efficiency winner — using *that* variant's `GPUsPerReplica`. The entitlement
is then spent through whichever candidate `fairShareRolePick` lands on, using
*that* variant's `GPUsPerReplica`. The two agree only when the two variants agree
on GPUs per replica. When the reference variant is the more GPU-hungry one, the
claim — and therefore the model's entitlement and its ranking position — is
inflated by the ratio between them.

This is reachable, and it is what makes the abstention gap in [How results
combine](#how-results-combine) observable. Reference selection filters only on
`PerReplicaCapacity > 0` and does **not** check headroom, so it can price a whole
role through a variant the picker provably cannot buy — one already at its
`MaxReplicas`. Two measured consequences, both with the pool never binding:

- With a second analyzer that cannot price the reference variant, that analyzer
  escapes the clamp and votes past the claiming analyzer's bottleneck to fill the
  inflated entitlement. A reference variant at 3 GPUs/replica pinned at its
  ceiling, against a picked variant at 1 GPU/replica, turns `+3` replicas into
  `+9` — the whole 9-GPU claim, where the true need was 3 GPUs.
- With a **single** analyzer and two contending models, the inflated claim wins a
  larger share. Two models each truly needing 3 GPUs, a 4-GPU pool: changing only
  the `GPUsPerReplica` of a variant the first model *cannot buy* moves it from an
  even 2/2 split of the additions to 3/1 in its favour.

Note what the second case implies for testing. The pool is honoured in both runs
— it is a pure redistribution between models — so no pool check catches it, and
no single-model golden can. The claim-pricing question is open with the
analyzer-design owner; the abstention fixtures in
`greedy_score_optimizer_test.go` deliberately cover only the regime where the
reference and picked variants agree, and say so.

### Rescale pre-pass (GreedyByScoreOptimizer only)

When rescale is enabled for a budget scope, `applyRescale` groups contended
models by `(accType, budget-scope)` and priority-weighted water-fills each
group's GPU budget across them (`computeRescaleTargets`) before the additive
fair-share path runs. Two demand quantities feed that water-fill, both
combined across every voting entry rather than read off the anchor alone:

- **`roleDemandGPUs`** converts a role's demand to a GPU count via the cheapest
  variant `v*` on the accelerator type, through the shared combine (see
  [How results combine](#how-results-combine)): `votesFromTotalDemand` builds
  the ballot from each entry's own demand (model-level `TotalDemand` for the
  synthetic `"both"` role, or its own `RoleCapacities[role].TotalDemand` for a
  P/D role) over its own PRC for `v*`, `combineVotes` takes the scale-up
  extremum, and `roleDemandGPUs` rounds up once. Reading only the anchor's (the
  binder's) demand and PRC would miss a non-binding analyzer whose demand for
  that role is larger.
- **The water-fill weight** (`rescaleInput.Demand`, consumed as `priority ×
  demand` in `computeRescaleTargets`) uses the model's combined demand-in-GPUs
  (`modelDemandGPUs`, which sums `roleDemandGPUs` across roles) rather than
  the anchor's `TotalDemand` in its own natural unit. The water-fill compares
  weights *across models* directly, so two models bound by different
  analyzers (one in tokens, one in request-rate) would be incommensurable
  otherwise — unlike a same-model ratio, the unit does not cancel here.

With a single voter both reduce to that voter's own demand and PRC —
byte-identical to reading the anchor alone. `fillRole`'s cost-efficiency sort
and `reclaimRole`'s scale-down tie-break already read the anchor's
(binder's) `VariantCapacities` directly, so they need no separate combine.

`rescaleModelDecisions` nil-guards its own `bindingAnchor` call, matching
every sibling topology helper (`modelCurrentGPUs`, `rescaleInputsForGroup`,
`roleCurrentGPUs`, `roleFloorGPUs`) — safe today only via `applyRescale`'s
pre-filter, but the local guard removes that fragile coupling.

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
  `fsv = Priority × Σ_role maxᵢ toGPUs(pickerState[i][role], PRC_i[v_role], GPUsPerReplica[v_role])`,
  where `pickerState` is seeded from each entry's `Remaining` and the maximum
  over `i` runs across every `NamedAnalyzerResult` entry that can price the
  role's reference variant. The unit is **GPUs**: an entry's own per-replica
  capacity converts its metric to replicas, and the variant's `GPUsPerReplica`
  converts replicas to GPUs. A higher `Score` does **not** increase a model's
  allocation — see
  [Fair-share iteration](#fair-share-iteration-greedybyscoreoptimizer-only).

Both optimizers are stateless and selected per-cycle from the engine's
`optimizer` field.

## Observability

The engine emits two structured INFO log lines per reconcile cycle per model —
one per analyzer (after the threshold post-step) and one after the optimizer
returns. See [cycle-log.md](cycle-log.md) for field schemas, grep patterns,
and an explanation of the `reason` values set by each analyzer.

**Known limitation: the `wva_required_capacity` and `wva_spare_capacity` gauges
do not say whose currency they are in.** Each decision's `RequiredCapacity` and
`SpareCapacity` are copied from the anchor, which means from **whichever
analyzer bound it** — per-role when the binder has an entry for the variant's
role, model-level otherwise (`buildDecisionsWithOptimizer`). The `unit` label on
those two gauges, however, is stamped as the continuous/token unit for every V2
decision unconditionally (`enrichDecisionsWithKvTokenData`), and the gauge help
text names the KV-token analyzer as the source. On a model with one enabled
analyzer those agree. On a multi-analyzer model they need not: a throughput
binder's per-replica capacity is a decode token *rate*, saturation's is a
batched-token *level*, and the two are not comparable numbers. Worse, the binder
can change between cycles — saturation binds whenever it votes and is live, so a
staleness lapse hands binding to another analyzer — which moves the series'
meaning with **no label change to signal it**.

Treat the two gauges as a scaling-pressure indicator, not a token measurement,
on any model with more than one enabled analyzer: the sign and the trend are
meaningful, the absolute value is only comparable against itself while the binder
holds. What is *not* affected is the per-analyzer `analyzer-result` log line —
it carries each analyzer's own `rc`/`sc` under that analyzer's name, so it stays
unambiguous, and [cycle-log.md](cycle-log.md) needs no qualification. No fix is
proposed here; distinguishing the currencies would mean a new label or a renamed
field, and that decision is out of scope for this change.
