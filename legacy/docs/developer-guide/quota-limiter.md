# Quota Limiter

The **quota limiter** enforces operator-declared GPU caps that are independent
of the physical inventory observed in the cluster. It addresses the case where
an administrator wants to cap accelerator consumption regardless of how many
nodes actually exist — for example, to reserve burst capacity, divide a cluster
across tenants, or simulate a smaller budget than the hardware allows.

This document covers the design, configuration, and pipeline integration of
the quota limiter. The implementation closes
[#1002](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1002).

## Enabling

Two settings must both be in place, **both in the saturation-scaling ConfigMap's
`default` entry**, for quota enforcement to take effect:

1. **A `limiters:` list containing a `quota` entry** on the `default` entry. This
   is the sole source that selects the quota limiter (see
   [Selection & lifecycle](#selection--lifecycle)); it is applied **live** — no
   restart. With no `limiters:` list, the physical-inventory limiter is used.
2. **`enableLimiter: true`** on the `default` entry. The limiter is only consulted
   on the limited optimizer path (`GreedyByScoreOptimizer`), which the engine
   selects when `enableLimiter` is true; with the default `enableLimiter: false`
   the engine runs the unlimited `CostAwareOptimizer`, which ignores all
   constraints — so quota caps are **not** enforced. This is the same coupling the
   physical-inventory limiter has.

With a `limiters: quota` entry but `enableLimiter: false`, the limiter is
constructed but never applied; replicas scale unconstrained by quota.

The quota entries use the same schema described below; place them inline under the
saturation `default` entry's `limiters:` list (see
[saturation-scaling-config.md](saturation-scaling-config.md#limiters-cluster-default-only-live)).

## Scope

The quota limiter is one of several limiters in the resource-limiting pipeline.
It is intentionally narrow:

- It does **not** discover physical inventory (that is `TypeInventory`'s job).
- It does **not** make scaling decisions (that is the optimizer's job).
- It does **not** compose itself with other limiters (that is the limiter
  chain's job — see sub-issue
  [#3](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003)).

A `QuotaInventory` instance is tied to exactly one scope. Deployments that need
both cluster-wide and per-namespace caps configure two limiter entries; the
limiter chain consults both before committing a decision.

## Configuration

The limiter is declared via a ConfigMap parsed into `QuotaLimiterEntries`. The
top-level `limiters` key holds a list of entries; each entry has a `scope` of
either `cluster` or `namespace`.

> **Entry `type:`.** Within a quota entry the `type:` field must be `"quota"`.
> (The saturation `limiters:` list additionally accepts a `{type: gpu-inventory}`
> entry to select the physical limiter instead — see
> [Selection & lifecycle](#selection--lifecycle).) The field exists so future
> limiter types (e.g. `reservation`) can share this schema.

### Cluster scope

A cluster-scoped entry caps the total GPUs of a given accelerator type across
all namespaces. The `quotas` map keys are accelerator type names (e.g.,
`H100`); values are caps in GPUs.

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
      A100: 8
      L40S: -1  # unlimited
```

### Namespace scope

A namespace-scoped entry caps per-namespace consumption. The top-level keys of
`namespaceQuotas` are namespace names; inner keys are accelerator types.

```yaml
limiters:
  - name: namespace-quota
    type: quota
    scope: namespace
    exclude:
      - kube-system
      - llm-d-system
    namespaceQuotas:
      team-a:
        H100: 8
      team-priority:
        H100: -1   # unlimited for this tenant
      default:
        H100: 2    # per-namespace fallback for unlisted namespaces
```

> ⚠️ **Reserved key collision.** The string `default` is a reserved key in
> `namespaceQuotas` and selects the per-namespace fallback for unlisted
> namespaces. As a consequence, the *actual* Kubernetes `default` namespace
> cannot be assigned a quota by listing it directly. To enforce a cap on
> workloads in the K8s `default` namespace, either (a) list it in `exclude`
> if you want it to bypass this limiter, or (b) use a strict allowlist
> (omit the reserved key entirely so any unlisted namespace including
> `default` is denied).

### Special values

| Value | Meaning |
|-------|---------|
| Positive integer (up to `MaxQuotaValue` = 1,048,576) | Cap in GPUs |
| `-1` | Unlimited (Kubernetes convention; pass-through in the allocator) |
| `0` or missing entry | Denied (no allocation) |

A finite cap must not exceed `MaxQuotaValue` (`1 << 20` = 1,048,576) — far above any
realistic accelerator count; a larger value is rejected at startup (see Validation).

### Namespace lookup rules

When the allocator evaluates a request for namespace `N` and type `T` in
`namespace` scope:

1. If `N` appears in `exclude`, the limiter **passes through** — the request is
   returned unchanged and no usage is recorded. Other limiters in the chain
   still apply.
2. If `N` is listed in `namespaceQuotas`, that namespace's **whole map is a
   closed allowlist**: `namespaceQuotas[N][T]` is the cap if present, and a type
   `T` that `N` does **not** list is **denied** (granted = 0). A listed namespace
   never falls through to `default` for a missing type — listing a namespace opts
   it into "only the types I name".
3. Otherwise (`N` is not listed), if `namespaceQuotas["default"]` is present, the
   **`default` map** becomes `N`'s per-namespace budget: each unlisted namespace
   gets its own copy of those caps (a type absent from `default` is still
   denied). This matches the Kubernetes `LimitRange` default semantic; it is
   *not* a shared pool across unlisted namespaces.
4. Otherwise, the request is denied (granted = 0).

The fall-through to `default` is therefore at the **namespace** granularity, not
per `(namespace, type)`: it applies only when `N` is wholly unlisted.

### Validation

`QuotaLimiterEntries.Validate()` returns a fatal error for any of:

- Empty or duplicate `name`.
- `type` other than `"quota"`.
- `scope` outside `{cluster, namespace}`.
- `namespaceQuotas` or `exclude` set on a cluster-scoped entry.
- `quotas` set on a namespace-scoped entry.
- Negative quota values other than `-1`.
- A quota value exceeding `MaxQuotaValue` (1,048,576).
- Empty accelerator type or namespace key.

It returns a non-fatal **warning** when a namespace appears in both `exclude`
and `namespaceQuotas`. `exclude` wins; the warning surfaces a likely
configuration mistake without rejecting the ConfigMap.

### Reload lifecycle

The quota configuration lives in the saturation-scaling ConfigMap and is applied
**live**. The controller watches that ConfigMap, and the saturation engine rebuilds
the GPU limiter at the top of the next optimization cycle whenever the effective
limiter mode or quota entries change (`Engine.refreshLimiter`, keyed by
`limiterSignature`). Editing the `limiters:` list therefore takes effect without a
controller restart — no `kubectl rollout restart` required.

## Pipeline integration

`QuotaInventory` implements the `Inventory` interface from
`internal/engines/pipeline/limiter_interfaces.go` plus the
`NamespaceAwareInventory` extension. The lifecycle mirrors `TypeInventory`:

1. The controller constructs `QuotaInventory` from a validated config entry.
2. Before each cycle, `DefaultLimiter.Limit` unconditionally calls
   `inventory.SetUsed(usedByType)` and, when the inventory satisfies
   `NamespaceAwareInventory`, additionally calls
   `SetUsedByNamespace(usedByNS)` via feature detection. Each
   `QuotaInventory` instance ignores the method irrelevant to its scope
   (`SetUsed` is a no-op on namespace-scoped instances; `SetUsedByNamespace`
   is a no-op on cluster-scoped instances), so callers do not branch on
   scope.
3. The controller calls `CreateAllocator` to obtain a per-cycle
   `ResourceAllocator`.
4. The allocation algorithm calls `TryAllocate` once per decision; the
   allocator tracks per-cycle usage in its own snapshot and decrements
   remaining quota in place.

`Refresh` is a no-op: quotas are operator-declared and have no external
source.

### `TryAllocate` semantics

`TryAllocate` returns the number of GPUs that may be allocated, which is
`min(gpusRequested, remaining)`. Specifically:

- **Unresolved accelerator** (empty `AcceleratorName`): **deny — fail closed**
  (granted = 0, with a `WasConstrained` DecisionStep). The allocator cannot match
  a per-type quota without a resolved type, and passing through would let an
  unattributed request bypass the cap. `DefaultLimiter` runs accelerator
  resolution first and resolves single-type inventories; a multi-type quota
  config can still leave the type unresolved here, so denial is the safe default.
- **Unlimited (`-1`)**: pass-through. No usage is recorded because there is no
  finite budget to track.
- **Cluster scope**: cap is `ClusterQuotas[type]`. Missing entry → 0.
- **Namespace scope**: excluded namespace → pass-through; otherwise resolved
  via the lookup rules above. Missing entry (after default fallback) → 0.

### `GetResourcePools` representation

`GetResourcePools` returns `map[string]ResourcePool` keyed by accelerator type.
For the cluster scope it is one pool per configured type. For the namespace
scope it is the per-type **sum** across explicitly-listed (non-excluded,
non-`default`) namespaces — the aggregate cluster budget that the per-namespace
caps then partition. The per-`(namespace, type)` breakdown is exposed separately
via `NamespaceResourcePools` (see *V2 enforcement* below).

Unlimited (`-1`) entries are handled by scope. At **cluster** scope they are
emitted as a sentinel `ResourcePool{Limit == -1}` (like the namespace branch), so
the V2 optimizer can tell "unlimited" (allocate up to demand) apart from a type
with no configured quota (absent → deny). At **namespace** scope an unlimited
`(namespace, type)` is likewise emitted as a sentinel in `NamespaceResourcePools`.
Either way `Limit = 0` stays unambiguous: a pool with `Limit = 0` always means a
real **deny** cap (a configured quota of `0`), never "unlimited". (`TotalLimit`,
`TotalAvailable`, and `Remaining` still exclude unlimited entries from their
totals.)

## V2 enforcement (optimizer path)

The V2 (token-based saturation) analyzer — the default — drives scaling through
the optimizer's `ResourceConstraints`, built from
`DefaultLimiter.ComputeConstraints`. Namespace quota is enforced here with the
**same closed-allowlist semantics as the V1 `Limit()` path** (`tryAllocateNamespace`):

- **Per-type / cluster** caps come from `GetResourcePools` (`ResourceConstraints.Pools`).
- **Per-namespace** caps come from `NamespaceResourcePools`
  (`ResourceConstraints.NamespacePools`, keyed namespace → type). Each non-excluded
  active namespace is a **closed allowlist**: the optimizer may scale a model
  onto a type only if that namespace lists it. For a listed type, the bound is
  `min(per-type budget, that namespace's cap)`, decremented as it allocates.
- A type the namespace does **not** list is **denied** — it does *not* fall
  through to the cluster aggregate, so one namespace can never draw on another's
  quota (no cross-namespace leak). An **unlimited** (`-1`) per-namespace entry is
  carried as a sentinel and bounds the model only by the cluster budget for that
  type (or is unbounded if the cluster does not cap it). An **excluded**
  namespace is omitted entirely, so it is "open" and bound only by the cluster
  constraint — matching V1's pass-through.
- **Multi-entry (composite)** quota configs are fully consulted: the engine
  computes constraints from *each* constituent of a `CompositeLimiter`.

Active namespaces are taken from the optimizer's request set, so a namespace
carrying a quota but currently running zero replicas is still constrained (its
`default` fall-through and `exclude` rules are applied via `QuotaForNamespace`).
A namespace with neither an explicit quota nor a `default` fall-through is a
real **deny-all**: it is present in `NamespacePools` with no listed types, and
the optimizer allocates it nothing.

Remaining boundaries:

- Composing a **physical-inventory** limiter with a quota limiter as
  `min(physical, quota)` within a single constraint set is tracked by the
  limiter chain (sub-issue #1003).
- A **purely unlimited** namespace-quota config with no finite cluster cap for
  the relevant type does not scale under the V2 `GreedyByScore` optimizer (its
  fair-share loop stops when the finite cluster aggregate is zero). This is a
  benign under-provision, not an isolation breach; configure a finite cluster or
  per-namespace cap for any type you expect to scale under V2.
- An **unlimited (`-1`) cluster-scope** quota scales up under V2:
  `GetResourcePools` emits it as a sentinel pool that `mergeConstraints` carries
  through as an unbounded budget, so the type allocates up to demand — matching
  the V1 `Limit` pass-through. (This differs from the namespace-scope
  purely-unlimited case above, whose cluster aggregate is derived from the finite
  namespace caps only.)

### Fair-share interaction

Quotas are **hard ceilings applied on top of** the optimizer's fair-share loop,
not inputs to it. Two properties follow from that:

- The fair-share mean each round is the **average of the active models' remaining
  fair-share metric** (priority × score × unmet demand — see the worked-example
  caveat below) — it is **not** `cluster GPUs / number of active models`, and
  **not** quota-weighted. The cluster GPU total is only the loop's stop condition
  (fair-sharing halts once the aggregate budget is exhausted). Quotas only clamp
  each model after the mean is computed.
- Fairness is **per-model**, not per-namespace. Two models in one namespace each
  get their own fair-share slot; the namespace quota bounds each of them (and
  their running sum) but does not pool one model's allowance for the other.

> **Ceilings, not reservations.** A finite per-namespace cap guarantees a tenant
> will never *exceed* it — it does **not** guarantee the tenant can *reach* it.
> Under V2 the cluster aggregate is the **sum of the finite namespace caps**, and
> an **unlimited (`-1`)** or **excluded** namespace competing for the same
> accelerator type draws from that shared aggregate without contributing to it,
> so it can consume budget a finite-capped peer would otherwise have used (no
> isolation breach — nobody exceeds their own authorization — but a real fairness
> footgun). If you need a tenant's cap to behave like a floor, avoid mixing
> unlimited/excluded namespaces on the same type, and give every type a finite
> cluster-scope cap.

When a model is capped below its fair-share slot it takes only up to its quota;
the unused remainder is **released to subsequent rounds**, where the
still-hungry models split it — it is not handed to other models within the same
round.

Worked example — cluster of 8 GPUs, three models:

| Model | Wants | Quota |
|-------|-------|-------|
| M1    | 3     | 2     |
| M2    | 4     | 4     |
| M3    | 4     | 4     |

- **Round 1:** the mean is the average of the models' remaining demand,
  ≈ (3 + 4 + 4) / 3 ≈ 3.67. M1 is capped at its quota of 2 (below its demand of 3),
  so part of its fair-share slot goes unused; M2 and M3 pull toward the mean under
  their own quotas.
- **Round 2:** with M1 satisfied at 2, the remaining cluster budget (8 − 2 = 6) is
  split between the still-hungry M2 and M3.

Final allocation: **M1 = 2, M2 ≈ 3, M3 ≈ 3** (total 8). The outcome respects every
quota and exhausts the cluster budget; the multi-round path is why a
quota-constrained model's slack flows to later rounds rather than to its
round-mates. (The exact per-round means come from the fair-share metric —
priority × score × demand — so treat the numbers here as an illustration of the
*path*, not an exact trace.)

## Interaction with `TypeInventory`

`QuotaInventory` is composed with `TypeInventory` by the limiter chain. The
intended model:

- `TypeInventory` knows what is **physically available** (nodes × GPUs per
  node).
- `QuotaInventory` knows what is **administratively allowed**.
- The chain takes the minimum: a decision must fit under both.

In the initial implementation the two are **mutually exclusive**, not composed:
the `limiters:` list selects either physical inventory **or** quota (a `quota`
entry wins over a `gpu-inventory` entry). There is no `min(physical, quota)` chain
yet (tracked in sub-issue #1003), so quota mode does **not** enforce physical
bounds at all — a deployment in quota mode will allocate beyond the cluster's
actual capacity if the quota permits (the surplus simply yields `Pending` pods,
not an isolation breach). Set quota caps at or below real capacity until
composition with `TypeInventory` lands.

## DecisionStep trace

When `quotaAllocator.TryAllocate` caps a request (granted < requested), it
appends a `DecisionStep` to the decision via `VariantDecision.AddDecisionStep`
with the reason formatted as:

- Cluster scope: `limited by quota[scope=cluster, type=H100]`
- Namespace scope: `limited by quota[scope=namespace, namespace=team-a, type=H100]`

The step's `Name` field is the limiter's `name` (e.g., `cluster-quota`) and
`WasConstrained` is `true`. Pass-through paths (excluded namespace, unlimited
quota) do **not** record a step because they did not constrain the decision. An
**unresolved accelerator** is the exception among the "did not allocate" paths:
it is a fail-closed **deny** and *does* record a `WasConstrained` step (reason
`denied by quota[...]: accelerator type unresolved`). The chain's
`updateDecisionMetadata` separately sets
`WasLimited` / `LimitedBy` per limiter; the DecisionStep here carries the
finer-grained scope/type detail.

## Selection & lifecycle

The controller selects between physical-inventory and quota enforcement from the
`limiters:` list on the saturation-scaling ConfigMap's **`default`** entry — the
sole source, honored only at that cluster-scope entry (like `enableRescale`):

| `limiters:` on `default` | Behavior |
|--------------------------|----------|
| absent, or a `{type: gpu-inventory}` entry | `TypeInventory` discovers physical GPUs via the GPU operator and caps decisions at `min(physical, requested)`. This is the default. |
| a `{type: quota, ...}` entry | Operator-declared quotas from the inline entries. A `quota` entry wins over any `gpu-inventory` entry in the same list. The two are mutually exclusive in the initial implementation (composing with physical inventory as `min(physical, quota)` is tracked in [#1003](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003)) — quota mode does **not** consult physical inventory. |

The selection is exposed on `*config.Config` as `EffectiveLimiterMode()` and
`EffectiveQuotaEntries()`, both reading the live ConfigMap. The saturation engine
rebuilds the limiter when they change (`Engine.refreshLimiter`), so edits apply
without a restart. The factory in
`internal/engines/pipeline/limiter_factory.go` translates the selection into a
concrete `pipeline.Limiter`:

- **Inventory** — `DefaultLimiter` wrapping `TypeInventoryWithUsage`.
- **Single quota entry** — `DefaultLimiter` wrapping one `QuotaInventory`.
- **Multiple quota entries** (e.g., cluster + namespace combined) —
  `CompositeLimiter` wrapping one `DefaultLimiter` per entry. Each constituent
  applies its cap in declaration order against the shared decisions slice,
  so the most-restrictive bound wins. This is intentionally simple — full
  chain composition with `min(physical, quota)` is sub-issue #1003.

### Per-namespace usage feeding

`DefaultLimiter.Limit` feature-detects `NamespaceAwareInventory` and calls
`SetUsedByNamespace(usedByNS)` in addition to the always-safe `SetUsed`.
`usedByNS` is derived from the decisions slice the same way per-type usage is —
`CurrentReplicas * GPUsPerReplica` summed by `(Namespace, AcceleratorName)`.
No additional discovery or API calls are needed.

### Example configuration

Inventory mode is the default — no `limiters:` list is required. To select quota
mode, add a `quota` entry to the saturation `default` entry's `limiters:` list:

```yaml
# saturation-scaling ConfigMap, data."default":
default: |
  analyzers:
    - type: saturation
  enableLimiter: true          # required for the limiter to actually apply
  limiters:
    - type: quota
      name: cluster-h100
      scope: cluster
      quotas: { H100: 32 }
```

Inline quota entries are validated at ConfigMap parse time
(`SaturationScalingConfig.validateLimiters`): each `quota` entry is checked against
the `QuotaLimiterEntries` schema (name uniqueness, scope, per-type ranges), and a
`gpu-inventory` entry must carry no quota fields. Invalid entries are skipped with
an error log/metric, exactly like any other invalid saturation entry.

## Resource access in quota mode

Quota mode makes the controller fully independent of physical node
discovery. When the effective limiter mode is quota:

- The limiter, inventory, allocator, and factory paths do not call
  `discovery.K8sWithGpuOperator.Discover` / `DiscoverUsage` /
  `DiscoverNodes`.
- No controller-runtime informer for `corev1.NodeList` is instantiated.
- `QuotaInventory.Refresh` is a deliberate no-op; usage is derived from
  the in-memory decisions slice via `DefaultLimiter.calculateUsedGPUs` and
  `calculateUsedGPUsByNamespace`.
- `collector.CollectInventoryK8S` (called from the saturation engine's
  per-cycle `optimize` when `WVA_LIMITED_MODE=true`) is **also** gated on
  the effective limiter mode via `shouldCollectClusterInventory` — it only runs
  when `EffectiveLimiterMode() == inventory`. This keeps the "no Node API access
  in quota mode" contract intact even if an operator combines quota mode with
  `WVA_LIMITED_MODE=true`. When that combination is detected at startup,
  `main.go` emits an informational log so the operator sees that their inventory
  logging is intentionally suppressed.

Combination matrix (limiter mode is the effective mode from the ConfigMap):

| Effective limiter mode | `WVA_LIMITED_MODE` | Node API access? |
|------------------------|--------------------|------------------|
| `inventory` (default) | `false` (default) | Yes, via the limiter's `Refresh` cycle. |
| `inventory` | `true` | Yes, via both the limiter and `CollectInventoryK8S`. |
| `quota` | `false` | **No.** |
| `quota` | `true` | **No.** `CollectInventoryK8S` is skipped; a startup log notes the suppression. |

## Future work

- **Limiter chain composition** (sub-issue #1003) — replace the simple
  `CompositeLimiter` with a smarter chain that:
  - Composes physical (`TypeInventory`) and quota bounds as
    `min(physical, quota)` instead of the current mutually-exclusive choice.
  - Owns DecisionStep ordering / `LimitedBy` selection when multiple caps
    bind the same decision.
- **Reservation-style limiters** — the `type: "quota"` discriminator leaves
  room for additional limiter types (e.g., `reservation`, `priority`) under
  the same ConfigMap schema.
