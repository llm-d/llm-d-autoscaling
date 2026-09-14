# Coordinator Component — Introduction

## Context

The WVA engine produces a `wva_desired_replicas` metric per InferencePool variant. When HPA is configured to consume this metric, it scales replicas according to the WVA engine's cluster-wide view of GPU supply and per-pool demand.

When HPA is **not** configured to consume `wva_desired_replicas` — either because the operator wired HPA to a different metric (CPU, custom, queue depth) or because no HPA exists at all on a managed Deployment/LWS — WVA has no path to influence replica counts. Several capabilities that depend on a cluster-wide view of GPU supply and per-pool demand (among otherthings) simply do not exist in that mode.

This proposal introduces a new top-level component, the **Coordinator**, whose job is to close that gap.

## Glossary

**WVA** - Workload Variant Autoscaler, the project as a whole.
**WVA engine** - the existing control loop that produces `wva_desired_replicas`. Code mostly lives in `internal/engine/`.
**Coordinator** - the new component proposed here, which runs a controller-wide loop and acts on scale targets (HPAs and KEDA ScaledObjects) that don't consume `wva_desired_replicas`.
**Scale target** - either a `HorizontalPodAutoscaler` (`autoscaling/v2`) or a KEDA `ScaledObject` (`keda.sh/v1alpha1`). The Coordinator treats these uniformly as the objects whose replica bounds it may write.

## Use Cases

The Coordinator is motivated by a small set of cluster-wide concerns that no per-variant loop can address. This is by no means an exhaustive list of what the Coordinator *could* do, but these are the most pressing use cases that motivated its creation:

### UC1 — GPU allocation rebalancing under constrained supply

When the sum of per-pool maximum replicas (multiplied by GPUs/replica) exceeds the GPU inventory, HPAs and KEDA ScaledObjects can independently raise replicas that don't fit. The result: Pending pods, evictions, or noisy-neighbor failures.

The Coordinator rebalances the upper replica bound on managed scale targets — `spec.maxReplicas` on HPAs and `spec.maxReplicaCount` on ScaledObjects — so that the cluster-wide or namespace-wide allocation respects total GPU inventory, with each pool getting a share proportional to a pluggable load signal.


### UC2 — Replica buffer management for burst handling

For each variant or InferencePool, operators may want to keep a small buffer of **idle replicas** — replicas that are running but receive no traffic — so that an incoming traffic burst is absorbed without waiting for cold-start.

A buffer policy is inherently cluster-wide: it competes with UC1 for the same GPU inventory, and the size of each pool's buffer depends on burst characteristics, GPU cost, and remaining headroom after live demand is satisfied.

The Coordinator is the natural place for this policy because it already holds (a) per-pool demand signals and (b) the cluster GPU budget.

The detailed design for this use case will be addressed in a future proposal.

## What the Coordinator Is (and Is Not)

**Is:**
- A new, independent component in the WVA control plane.
- Leader-elected, with its own loop interval and its own goroutine.
- A **plugin host**: it computes the set of scale targets (HPAs and ScaledObjects) that are "under Coordinator control" each tick and dispatches that set to every registered plugin. Each plugin is treated as an opaque, self-contained unit; the Coordinator does not look inside one or share state across them.


**Is not:**
- A replacement for the existing WVA engine that produces `wva_desired_replicas`. Pools that *do* feed HPA via that metric continue to work unchanged.
- A scheduler or admission controller. It does not pre-empt pods, taint nodes, or interact with the kube-scheduler beyond setting replica counts.

## Relationship to Existing WVA

```
┌────────────────────────────────────────────────────────────────────┐
│                        WVA control plane                           │
│                                                                    │
│  ┌─────────────────────┐         ┌─────────────────────────────┐   │
│  │ WVA engine          │         │ Coordinator (new)           │   │
│  │ (per-pool, existing)│         │ (cluster-wide, this spec)   │   │
│  │                     │         │                             │   │
│  │ emits               │         │ writes                      │   │
│  │ wva_desired_replicas│         │ spec.maxReplicas (HPA) or   │   │
│  │                     │         │ spec.maxReplicaCount (SO)   │   │
│  │                     │         │ (+ future: Deployment/LWS   │   │
│  │                     │         │   replicas for UC2)         │   │
│  └──────────┬──────────┘         └──────────────┬──────────────┘   │
│             │                                   │                  │
└─────────────┼───────────────────────────────────┼──────────────────┘
              │                                   │
              ▼                                   ▼
        ┌──────────┐                      ┌────────────────────┐
        │   HPA    │ (when wired to       │ Managed HPAs and   │
        │          │  wva_desired_repls)  │ KEDA ScaledObjects │
        └──────────┘                      └────────────────────┘
```

Both components observe the same pools, but they act on disjoint surfaces and serve disjoint use cases.

## Loop Algorithm

The Coordinator runs a single periodic loop (leader-only, default interval `15s`) that drives all use-case modules registered against it. The loop itself is fixed and lives in `internal/coordinator/coordinator.go`. This proposal specifies *only* the loop skeleton and the set of scale targets the Coordinator considers under its control; what each tick *does* is the responsibility of the modules wired in.

```
every Coordinator.Interval, on the leader:

  1. Compute the set of scale targets under Coordinator control (see
     "Scale-target selection" below). The set is a single mixed slice
     containing HorizontalPodAutoscaler and ScaledObject objects. If
     listing fails, skip the cycle, log, and increment
     wva_coordinator_cycle_errors_total{kind="discovery"}.

  2. For each registered plugin, in declared order:
       call plugin.Tick(ctx, selected)
     The Coordinator passes the same selected slice to every plugin.
     A plugin that has nothing to do simply returns nil.

  3. (Plugin-driven, not loop-driven) any writes, reconciliation,
     damping, conflict handling, metrics, and events are owned by
     the plugin that produced them. The loop itself does not write
     to the cluster.
```

### Scale-target selection — which objects the Coordinator considers under its control

A scale target is **under Coordinator control** in a given tick iff it satisfies the rule for its kind. Both rules require the canonical `llm-d.ai/managed: "true"` annotation (see `internal/annotations/annotations.go` and `internal/controller/hpa_reconciler.go`) and additionally exclude objects already steered by the WVA engine via `wva_desired_replicas`.

**HorizontalPodAutoscaler — under control iff all of:**

1. The HPA carries `llm-d.ai/managed: "true"`.
2. The HPA's `spec.metrics` does **not** include the `wva_desired_replicas` external metric. When that metric is present, WVA's per-pool engine is already steering this HPA via the metric pipeline, and the Coordinator must not also act on it.
3. The HPA is **not** owned by a `ScaledObject`. KEDA generates an HPA per ScaledObject and reconciles it from the SO; writes to such an HPA are reverted by KEDA on its next reconcile. Detected via `metav1.GetControllerOf(hpa)` returning an OwnerReference with `apiVersion: keda.sh/v1alpha1, kind: ScaledObject`. KEDA-generated HPAs reach plugins only via their parent ScaledObject.

**ScaledObject — under control iff both of:**

1. The ScaledObject carries `llm-d.ai/managed: "true"`.
2. The ScaledObject's `spec.triggers` does **not** include a Prometheus trigger whose `metadata.query` references `wva_desired_replicas`. Same intent as the HPA rule — if the WVA engine is already steering this target via that metric, the Coordinator stays out.

ScaledObject discovery is conditional on the KEDA CRD being installed at startup, mirroring the existing `ScaledObjectReconciler` registration guard in `cmd/main.go`. When the CRD is absent the Coordinator simply produces no ScaledObjects in the selected set.

Objects failing the rule for their kind are excluded from the set passed to plugins. Each plugin then decides for itself whether it applies to a given selected object — for example, the gpu-rebalance plugin further requires that the target's pool resolves and that GPUs/replica is known. The Coordinator does not pre-filter beyond the conditions above.

**Safety invariants enforced by the loop itself (not delegated to plugins):**
- Leader-only: registered via `mgr.Add(...)` so non-leaders never enter the loop body.
- Empty-input fail-safe: if no scale target satisfies the selection conditions, or no plugins are registered, the loop is a no-op for that tick.
- The loop never writes to the cluster directly; all cluster writes are issued by plugins.

## Plugin Contract

Each Coordinator capability is implemented as a **plugin** — a self-contained Go type that registers with the Coordinator at startup and is invoked once per tick. Plugins are the only place writes happen.

The minimum interface a plugin must satisfy:

```go
// internal/coordinator/plugin.go

import (
    "context"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// Plugin is the contract every Coordinator capability satisfies. The
// Coordinator does not inspect anything inside a plugin beyond this
// interface and the plugin's name.
type Plugin interface {
    // Name uniquely identifies the plugin (e.g. "gpu-rebalance").
    // Used in metrics labels, event reasons, and the per-plugin
    // damping cache key.
    Name() string

    // Tick is called once per Coordinator interval, on the leader, with
    // the set of scale targets the loop has decided are under
    // Coordinator control this tick. Each element is one of:
    //   - *autoscalingv2.HorizontalPodAutoscaler
    //   - *kedav1alpha1.ScaledObject
    //
    // Plugins type-switch on the concrete kind and ignore kinds they do
    // not handle.
    //
    // Plugins MUST be deterministic given the same input.
    // Plugins MUST return nil for "no work this cycle" and only return a
    //   non-nil error for failures the operator should see in the
    //   Coordinator's cycle-error counter.
    Tick(ctx context.Context, selected []client.Object) error
}
```

**Plugin isolation rules** (these define what "treated as a plugin" means here):

- **Independent surface area.** Each plugin owns its own package under `internal/coordinator/plugins/<name>/`.
- **No shared state.** Plugins MUST NOT read or mutate state owned by another plugin. If two plugins need to coordinate, that coordination is added at the Coordinator level by an explicit future proposal — not by plugins reaching into each other.
- **Per-plugin damping and metrics.** A plugin's damping cache, Prometheus metrics, and Kubernetes events carry its `Name()` so writes are attributable. The damping cache key is `(plugin-name, kind, namespaced-name)` — `kind` is part of the key because an HPA and a ScaledObject can share a name in the same namespace.
- **Per-plugin configuration.** Each plugin owns one entry under `coordinator.plugins.<name>:` in the unified config. Plugins MUST NOT read configuration from another plugin's sub-block.
- **Cluster writes via standard API patches with `resourceVersion` precondition.** Plugins use optimistic concurrency so concurrent writes from another plugin (or a human operator) race-detect rather than silently overwrite.


## Scope of This Proposal

What this proposal *does* establish:

1. The Coordinator exists as a distinct, leader-elected component.
2. It lives under a new package: `internal/coordinator/`.
3. It is registered as an `mgr.Add(...)` runnable in `cmd/main.go`, mirroring the saturation engine pattern (see `cmd/main.go:431-458`).
4. It is opt-in: a top-level `coordinator.enabled` flag in the unified config, off by default in v0.
5. It defines the rule for which scale targets (HPAs and ScaledObjects) are "under Coordinator control" (the conditions above).
6. It runs the per-tick loop and dispatches the selected scale-target set to every registered plugin.
7. It defines the plugin contract (`Plugin` interface) and the isolation rules every plugin must follow.

What this proposal *does not* establish (deferred to plugin proposals):

- Cluster writes, reconciliation, damping caches, conflict handling, and per-plugin metrics or events.
- Pool grouping, demand signals, allocation strategies, buffer policies, or any other plugin-specific data.
- Configuration beyond `coordinator.enabled`, the loop interval, and the `coordinator.plugins.*` map shape.

## Out of Scope (for any Coordinator proposal)

- Replacing or modifying the WVA engine.
- Per-accelerator partitioning of GPU inventory (treated as fungible in v0).
- Scheduling decisions normally handled by kube-scheduler or descheduler.
- Cross-cluster or multi-cluster coordination.
- Touching `VariantAutoscaling` resources (CRD is deprecated).

## Verification

This proposal is structural; verification is limited to confirming the component is wired in correctly, the scale-target selection rule behaves as specified, and no cluster writes happen from the loop itself:

1. With `coordinator.enabled: false` (default), the Coordinator goroutine is not started; existing WVA engine behavior is unchanged.
2. With `coordinator.enabled: true` but no plugins registered, the Coordinator starts, computes the selected scale-target set each tick, logs it at debug level, and performs no cluster writes.
3. HPA selection unit tests:
   - HPA without `llm-d.ai/managed` → excluded.
   - HPA with `llm-d.ai/managed: "true"` and no `wva_desired_replicas` external metric → included.
   - HPA with `llm-d.ai/managed: "true"` *and* `wva_desired_replicas` listed in `spec.metrics` → excluded.
   - HPA with `llm-d.ai/managed: "true"` whose controller OwnerReference points at a `keda.sh/v1alpha1, ScaledObject` → excluded (KEDA-generated).
4. ScaledObject selection unit tests (run only when KEDA CRD is present):
   - SO without `llm-d.ai/managed` → excluded.
   - SO with `llm-d.ai/managed: "true"` and no Prometheus trigger referencing `wva_desired_replicas` → included.
   - SO with `llm-d.ai/managed: "true"` whose `spec.triggers` contains a Prometheus trigger whose `metadata.query` references `wva_desired_replicas` → excluded.
   - With KEDA CRD absent, SO discovery is skipped entirely; the selected set contains only HPAs.
5. Plugin contract test: a stub plugin that records calls to `Tick` is registered; verify the Coordinator passes the same `selected` slice (mixed HPAs and ScaledObjects) to every plugin and continues the loop when a plugin returns an error.

Detailed behavioral verification (writes, allocation, damping, etc.) belongs to the plugin proposals.
