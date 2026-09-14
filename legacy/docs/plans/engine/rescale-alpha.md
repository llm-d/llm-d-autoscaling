# Plan: Priority-Weighted Rescale — Alpha stage

Implements the **Alpha** milestone of the rescale proposal (`docs/proposals/design-rescale.md`, PR #1238):
a redistributive, priority-weighted allocation in the V2 GPU-constrained optimizer.

## Goal

Under contention, reallocate the **whole group budget** by each model's `priority × demand`, reclaiming
GPUs from models holding more than their share (lower-priority, even if still hungry) so higher-priority
work can run — instead of today's additive fair-share that hands out only *free* GPUs and freezes
allocation by arrival order once the budget is full.

## Scope

**In Alpha**
- Opt-in flag; behavior **unchanged when off, or when a group is uncontended**.
- V2 `GreedyByScoreOptimizer` only (cost-aware/unlimited path untouched).
- **All models, including P/D** — role-aware reclaim/fill (dependency #1237 *role-aware scale-down* is
  merged; the machinery — `scaleDownRoleIterated`, `safeRemovalReplicasForRole`,
  `allocateForModelPaired` — is in `main`).
- Competition group = **`(accelerator type, budget scope)`** where budget scope is `cluster`
  (physical #1129 / cluster-scope quota) or `namespace-N` (namespace quota #1162). Namespace-quota
  budgets come from `availableByNS` (already plumbed: `QuotaInventory.NamespaceResourcePools` →
  `ResourceConstraints.NamespacePools` → `mergeNamespaceConstraints` → `availableByNS`).
- Water-filling targets, reclaim-then-fill, **free-capacity-gated fill**, `DecisionReasonRescale` on
  reclaims.

**Deferred**
- Physical∧quota **namespace partition** of the cluster budget → needs #1003.
- **Multi-accelerator** models (variants spanning GPU types) → future scope.
- **Observability** (`Rescaled` status condition, surfacing a reclaim **stall** when a role can't
  shed a whole replica) + **hysteresis** (min share-gap / cool-down) → Beta.

## Flag semantics — scope-coupled to the budget

Rescale runs on a group **iff** a budget exists at the group's scope **and** the rescale flag is set at
that **same** scope. No cross-level effect.

| Budget at scope | Flag at same scope | Result |
|---|---|---|
| cluster budget | cluster flag on | cluster rescale (redistribute the cluster budget) |
| namespace-N quota | namespace-N flag on | namespace-N rescale (redistribute N's quota) |
| both | both on | both, independently, on their own budgets |
| namespace-N quota | only cluster flag on | **no effect** (cluster flag doesn't govern N's quota) |
| cluster budget | only namespace flag on | **no effect** (namespace flag doesn't govern the cluster budget) |

A flag with no same-scope budget is a no-op.

### Config source
- **cluster flag** = the **global** saturation config `default` entry's `enableRescale`.
- **namespace-N flag** = **namespace-N's own** saturation config `default`.`enableRescale`, read
  *namespace-local only* (never the global fallback), so the cluster flag can't leak onto a namespace
  quota. Requires a config accessor that distinguishes "namespace has its own config" from "falls back
  to global".
- Read only from the **`default`** entry — per-model override entries (`modelID#namespace`) never carry
  it (it is budget-scope, not per-model / per-pool).

## Algorithm (per contended group)

1. **Group** models by `(accType, scope)`. `scope` = `namespace-N` if N is a closed quota allowlist in
   `availableByNS`, else `cluster`.
2. **Budget** = free-available for the group (`effectiveAvailable` / `availableByNS`) **+ Σ current GPU
   usage** of the group's models on that type.
3. **Enabled?** budget exists at scope **and** same-scope flag on. If not, or budget unlimited
   (`math.MaxInt`), fall through to today's additive `fairShareScaleUp`.
4. **Contended?** `Σ demandGPUs > budget`. If not, fall through (no reclaim needed).
5. **Per model:** `floorᵢ = Σ minReplicas·gpusPerReplica`; `demandGPUsᵢ` = demand→GPUs (per-role for
   P/D), capped at `maxReplicas`; `weightᵢ = priorityᵢ × demandᵢ` (demand enters only as a ratio, so
   token units cancel).
6. **Water-fill:** `shareᵢ = floorᵢ + (budget − Σfloor)·weightᵢ/Σweight`, iteratively capping any model
   above `min(demandGPUsᵢ, maxGPUsᵢ)` and re-splitting the freed excess; quantize to whole replicas at
   each variant's `gpusPerReplica` (round down). Surfacing a reclaim **stall** (a role that can't shed
   a whole replica) is deferred to Beta observability.
   `Σfloor > budget` → over-budget **Conflict** (log, clamp shares to floors).
7. **Apply:**
   - **reclaim** (`targetGPUsᵢ < currentGPUsᵢ`): `scaleDownRoleIterated` (role-aware per-role spare,
     `minReplicas`, cheapest-at-1); tag decisions `DecisionReasonRescale`.
   - **fill** (`> currentGPUsᵢ`): `allocateForModelPaired` / cost-eff pick, but only up to
     `min(target, free-this-cycle)`. Reclaim does not free budget this cycle (usage is
     `currentReplicas`-based), so fills consume only pre-existing free → the group is **never
     over-budget**; the remaining fill lands next cycle as reclaims actuate.
8. **Build decisions** via the existing `buildDecisionsWithOptimizer`.

### Invariants (enforced by tests)
- `Σ targets ≤ budget` every cycle (free-capacity-gated fill).
- Reclaim never below `minReplicas`; P/D roles independently served (a slack role never trims a
  saturated one).
- Whole-replica quantization (round down at each variant's `gpusPerReplica`).

## Files

| File | Change |
|---|---|
| `internal/config/saturation_scaling.go` | `EnableRescale bool` (`yaml:"enableRescale,omitempty"`) + `Merge`; read from `default` only; no default/validation |
| `internal/interfaces/saturation_analyzer.go` | add `DecisionReasonRescale` |
| `config/base/manager/saturation-scaling-configmap.yaml`, `deploy/configmap-saturation-scaling.yaml` | document flag (commented, off; cluster-default / per-namespace) |
| `internal/config/config.go` (or accessor) | namespace-local-only saturation lookup (distinguish own-config vs global fallback) |
| `internal/engines/saturation/engine.go` | resolve `{cluster, byNamespace}` rescale flags; pass to optimizer |
| `internal/engines/pipeline/greedy_score_optimizer.go` | group by `(type, scope)`; branch to rescale on enabled+contended groups, else existing path |
| new `internal/engines/pipeline/rescale.go` | water-fill (`computeRescaleTargets`, pure) + reclaim/fill wiring |
| new `internal/engines/pipeline/rescale_test.go` | unit tests |

## Tests (proposal's worked examples)
- Water-fill: budget 8 — A(prio 1, dem 8, holds 8) & B(prio 3, dem 8, holds 0) → **A=2, B=6**; B dem 4 →
  excess re-splits **A=4, B=4**; `minReplicas` floors reserved first; `gpusPerReplica=2` quantization
  (A sheds a whole replica; A=4/B=4 = 2 replicas each).
- Reclaim carries `DecisionReasonRescale`; fill respects free-this-cycle.
- P/D: reclaim sheds per-role by spare; ratio preserved.
- Namespace-quota group: reclaim confined to the namespace.
- Scope-coupling: cluster flag does not rescale a namespace quota, and vice-versa.
- **Flag off / uncontended → decisions byte-identical to the current path.**

## Commits (each: build + `go test` + golangci-lint via WSL, DCO-signed)
1. **Config flag + reason** — `EnableRescale` (+ `Merge`) + `DecisionReasonRescale` + configmap docs.
   Behavior-neutral.
2. **Water-fill core** — pure `computeRescaleTargets(...)` + unit tests (the worked examples).
3. **Wiring** — namespace-local flag accessor + engine resolves `{cluster, byNamespace}` + optimizer
   grouping / contention / reclaim / fill (non-P/D and P/D via the role helpers) + free-capacity
   gating + tests.
