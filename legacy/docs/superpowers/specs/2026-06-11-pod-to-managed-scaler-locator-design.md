# Pod → Managed Scaler Locator

## Context

vLLM pods expose Prometheus metrics. The Prometheus pod scraper attaches Kubernetes-derived labels to those metrics, including `pod_name` (and `namespace`). WVA's internal pipeline (saturation analyzer, future Coordinator plugins) needs to bucket those metrics by the **managed scaler** that controls the pod's replica count — either a `HorizontalPodAutoscaler` or a KEDA `ScaledObject` annotated with `llm-d.ai/managed: "true"`.

Today this association relies on operator-supplied wiring at the metrics layer: WVA requires `llm-d.ai/variant: <variant-name>` on the pod template of every scale target plus a target-relabeling rule on the ServiceMonitor / PodMonitor that propagates the label into each metric series as `llm_d_ai_variant` (see `docs/design/controller-behavior.md`, "Prerequisites"). That contract is two extra obligations on every operator and is easy to get wrong.

For the common deployment shapes — `apps/v1.Deployment` and `leaderworkerset.x-k8s.io/v1.LeaderWorkerSet` — the association can be derived without operator help, because the vLLM pod's `ownerReferences` chain reaches the scaler's `scaleTargetRef` directly. The Locator does that derivation. With the Locator in place, **the `llm-d.ai/variant` label and the relabeling rule become unnecessary** for Deployment and LWS scale targets.

The label remains the only viable linkage in the **shadow pod** deployment shape: the HPA-scaled Deployment runs a front pod, and vLLM runs in a separate pod managed by a different controller, not linked by a shared owner-reference chain. The metric's `pod_name` is the vLLM pod, not the front pod, so owner-walking fails. For shadow pods only, operators continue to stamp `llm-d.ai/variant` and the Locator's label fast path resolves it directly — exploiting the fact that `llm_d_ai_variant` *is* the managed scaler's name (the synthetic VA shares the scaler's name; see "Resolving the internal VariantAutoscaling" below).

This spec defines a `PodLocator` with two entry points: `Locate(ctx, namespace, podName)` for owner-walk on Deployment / LWS layouts, and `LocateByVariant(ctx, namespace, variantName)` for shadow pods.

## Glossary

- **Managed scaler** — an HPA or KEDA ScaledObject carrying `llm-d.ai/managed: "true"`.
- **Scale-target chain** — the chain of `ownerReferences` upward from a Pod, terminating at the first object with no controller owner.
- **Chain node** — one element of a scale-target chain, identified by `(namespace, apiVersion, kind, name)`.
- **Shadow pod** — a deployment shape where the HPA-scaled Deployment runs a placeholder pod and vLLM runs in a separate pod managed by a different controller, with no shared `ownerReferences` chain between them.
- **Variant name** — the value of the `llm-d.ai/variant` pod label (relabeled to the `llm_d_ai_variant` metric label). Equal to the managed scaler's `metadata.name`, which is also the synthetic VariantAutoscaling's name. Required only for shadow pods; see `docs/design/controller-behavior.md`.

## Scope

- Scale targets: `apps/v1.Deployment` and `leaderworkerset.x-k8s.io/v1.LeaderWorkerSet`. These are the only kinds wired by `HPAReconciler` / `ScaledObjectReconciler` today.
- Cardinality: 1 pod → exactly 1 managed scaler (enforced upstream by operator policy; locator detects and errors on violations).
- Consumer: WVA-internal callers only (saturation analyzer, Coordinator plugins). Not exposed to prometheus-adapter or to HPA via custom-metrics.
- Call profile: per-metric, on-demand point lookup. No batch API.

Out of scope:

- StatefulSet, ReplicaSet-as-root, DaemonSet scale targets.
- Walking through arbitrary unknown CR kinds in the chain.
- Returning the KEDA-generated HPA underneath a managed ScaledObject. The locator returns the ScaledObject; consumers that need the HPA can resolve it themselves.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Caller (saturation analyzer, Coordinator plugin)                │
│                                                                  │
│  Default — Deployment / LWS layouts:                             │
│   metric{pod_name="vllm-x", namespace="ns"}                      │
│      │                                                           │
│      ▼                                                           │
│   PodLocator.Locate(ctx, "ns", "vllm-x") → *ManagedScaler        │
│      │  1. Get Pod (cached client)                               │
│      │  2. walkOwnersUp → []chainNode                            │
│      │  3. for each node: lookup HPAByScaleTargetKey,            │
│      │     ScaledObjectByScaleTargetKey; first hit wins;         │
│      │     both → error                                          │
│                                                                  │
│  Shadow pod — caller has the relabeled metric series:            │
│   metric{llm_d_ai_variant="v", namespace="ns", pod_name="…"}     │
│      │                                                           │
│      ▼                                                           │
│   PodLocator.LocateByVariant(ctx, "ns", "v") → *ManagedScaler    │
│      │  Get HPA "ns/v" and ScaledObject "ns/v"; verify managed   │
└──────────────────────────────────────────────────────────────────┘

Field indexes (controller-runtime, populated by HPA/SO reconcilers):
  HPAByScaleTargetKey:          managed HPA → its scaleTargetRef key
  ScaledObjectByScaleTargetKey: managed SO  → its scaleTargetRef key
```

## Public API

```go
// internal/collector/locator/locator.go
package locator

// ManagedScaler is one of the managed scaler kinds WVA recognizes.
// Exactly one of HPA / ScaledObject is non-nil on a successful Locate.
type ManagedScaler struct {
    HPA          *autoscalingv2.HorizontalPodAutoscaler
    ScaledObject *kedav1alpha1.ScaledObject
}

type PodLocator interface {
    // Locate finds the managed scaler whose scale-target chain contains the
    // given pod. This is the default entry point for Deployment and LWS
    // scale targets, where the vLLM pod is in the scaler's ownerReferences
    // chain. Returns (nil, nil) when the pod is unmanaged or when the chain
    // does not reach a managed scaler (e.g. shadow pod — see LocateByVariant).
    // Returns an error only on infrastructure failures or invariant
    // violations (multiple managed scalers in chain, cycle, depth exceeded).
    Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error)

    // LocateByVariant resolves the managed scaler by variant name (the value
    // of the llm_d_ai_variant metric label, equal to the scaler's name).
    // Use this for shadow-pod layouts, where the vLLM pod is not in the
    // scaler's ownerReferences chain and Locate by pod_name cannot find it.
    // Returns (nil, nil) when no managed HPA / ScaledObject of that name
    // exists in the namespace.
    LocateByVariant(ctx context.Context, namespace, variantName string) (*ManagedScaler, error)
}
```

Constructed once in `cmd/main.go` and passed to consumers. Safe for concurrent use.

The constructor takes two readers from the manager:

```go
func New(cached client.Reader, apiReader client.Reader) PodLocator
```

- **`cached`** — `mgr.GetClient()`. Used only for the field-indexed list of managed HPAs / ScaledObjects. Backed by the per-GVK informer; HPA + ScaledObject watch fan-out is bounded by variant count.
- **`apiReader`** — `mgr.GetAPIReader()`. Used for every Pod / ReplicaSet / Deployment / LWS read in the owner-chain walk and for `LocateByVariant`. No informer, no in-memory mirror, no per-GVK cache. Each read is an API call; the locator's own LRU is the only memory the locator owns.

This split keeps the controller-runtime cache footprint bounded by the number of variants (small) rather than the number of pods / ReplicaSets / Deployments in the cluster (potentially large).

### Resolving the internal VariantAutoscaling

The v2 engine consumes an in-memory `VariantAutoscaling` struct, not the HPA / ScaledObject directly. With the VA CRD deprecated, the engine synthesizes the struct from the scaler. Callers turn a `ManagedScaler` into a synthetic VA using the existing helpers in `internal/utils/variant_fromannotations.go`:

```go
ms, err := locator.Locate(ctx, namespace, podName)
if err != nil || ms == nil {
    // (nil, nil) → unmanaged pod; skip metric this tick
    return
}

var va *wvav1alpha1.VariantAutoscaling
switch {
case ms.HPA != nil:
    va, err = utils.VariantAutoscalingFromHPA(ms.HPA)
case ms.ScaledObject != nil:
    va, err = utils.VariantAutoscalingFromScaledObject(ms.ScaledObject)
}
```

The synthetic VA shares the scaler's `name`/`namespace`, carries the `llm-d.ai/synthetic: "true"` annotation, and is in-memory only — never written to the API server (`utils.IsSynthetic`). The Locator does not hold a VA index because no separate VA object exists in annotation-driven discovery; identity is established by synthesis, not lookup.

### Compatibility with the `llm-d.ai/variant` label

The pre-existing operator contract (pod-template label `llm-d.ai/variant` plus the `llm-d.ai/variant` → `llm_d_ai_variant` target-relabeling rule, documented in `docs/design/controller-behavior.md`, "Prerequisites") is preserved by this design — this spec does not introduce or change any operator-facing convention. What changes is *when* the contract is required:

- **Deployment / LWS scale targets.** The label and relabeling rule are no longer required. The Locator derives the association by walking `ownerReferences` from the vLLM pod up to the scaler's `scaleTargetRef`. Operators may still stamp the label (it is harmless), but WVA does not depend on it.
- **Shadow pods.** The label and the relabeling rule remain required and are the only viable linkage. `LocateByVariant` resolves the metric series directly to the managed scaler.

Three properties make `LocateByVariant` correct without any extra plumbing:

1. **Identity coincidence.** The label value is the `VariantAutoscaling` name. With the VA CRD deprecated, the synthetic VA's name is, by construction, the managed scaler's `metadata.name` (see `utils.VariantAutoscalingFromHPA` and `utils.VariantAutoscalingFromScaledObject`). So `llm_d_ai_variant` *is* the managed scaler's name.
2. **Same namespace.** Pod, scaler, and metric series all live in the same namespace. The metric carries `namespace` (or `exported_namespace`); the caller hands it through verbatim.
3. **Direct lookup, no walk.** `LocateByVariant(ctx, ns, name)` is a paired typed `Get` (`HPA ns/name`, `ScaledObject ns/name`), each gated on `llm-d.ai/managed: "true"`. Same correctness rules as the chain path: exactly one managed match → return it; both managed → ambiguity error; neither managed → `(nil, nil)`. No new index, no new RBAC.

#### Example

A managed HPA `ns=llm-d, name=llama-8b` scaling Deployment `llama-8b`. Metric series the saturation analyzer reads:

```
vllm:num_requests_running{namespace="llm-d", pod_name="llama-8b-7d4c-x9k2", llm_d_ai_variant="llama-8b"} 12
```

**Without the label** (Deployment / LWS layout — operator did not stamp `llm-d.ai/variant`; series carries `pod_name` only):

```go
ms, _ := loc.Locate(ctx, "llm-d", "llama-8b-7d4c-x9k2")
// pod → ReplicaSet llama-8b-7d4c → Deployment llama-8b
// HPAByScaleTargetKey hit on "llm-d/apps/v1/Deployment/llama-8b"
// ms.HPA.Name == "llama-8b"
```

**With the label** (shadow pod — vLLM pod has no ownerReferences path to Deployment `llama-8b`; series carries `llm_d_ai_variant`):

```go
ms, _ := loc.LocateByVariant(ctx, "llm-d", "llama-8b")
// Get HPA "llm-d/llama-8b"; verify llm-d.ai/managed=true
// ms.HPA.Name == "llama-8b"
```

Both paths return the same scaler; the synthetic VA built from `ms.HPA` has `name=llama-8b` either way.

## Index design

Two field indexes are added under `internal/controller/indexers/`, one file per indexed resource type. The existing `indexers.go` is split: shared helpers stay there, and the VA-specific index moves to its own file alongside the new HPA / ScaledObject files:

```
internal/controller/indexers/
  indexers.go              # SetupIndexes, shared chainKey/scaleTargetIndexKey helpers
  variantautoscaling.go    # existing — VAScaleTargetKey + VAScaleTargetIndexFunc
  hpa.go                   # new — HPAByScaleTargetKey + HPAByScaleTargetIndexFunc
  scaledobject.go          # new — ScaledObjectByScaleTargetKey + ScaledObjectByScaleTargetIndexFunc
```

Each new file owns exactly one indexed type and exposes the constant, the index function, and any typed lookup helpers (e.g. `FindHPAForScaleTarget`, `FindSOForScaleTarget`). The shared key shape — `Namespace/APIVersion/Kind/Name` — and `scaleTargetIndexKey` stay in `indexers.go` so all three files reuse them. `SetupIndexes` registers all three.

```go
const (
    HPAByScaleTargetKey          = ".spec.scaleTargetRef"  // hpa.go
    ScaledObjectByScaleTargetKey = ".spec.scaleTargetRef"  // scaledobject.go
)
```

For each managed scaler, the index function:

1. Verifies `llm-d.ai/managed: "true"` and a supported `scaleTargetRef.Kind` (Deployment or LWS). Otherwise returns no entries.
2. Returns a single index entry: the key for the scaler's `scaleTargetRef`.

```go
func scaleTargetKindSupported(kind string) bool {
    return kind == constants.DeploymentKind || kind == constants.LeaderWorkerSetKind
}
```

`client.List(HPAList, MatchingFields{HPAByScaleTargetKey: key(deploy)})` returns every managed HPA targeting that Deployment / LWS. This call is served from the controller-runtime cache — no API call on the hot path — because field indexes only work against the informer-backed cache. The HPA + ScaledObject informers are the only caches the locator relies on, and their fan-out is bounded by variant count.

Owner-walking is the *caller-side* job in `Locate` and uses `apiReader`, not the controller-runtime cache (see "Lookup algorithm"). Each ancestor produced by `walkOwnersUp` is probed against the indexed list, but the index itself stores only the top-level `scaleTargetRef` — never anything chain-derived.

## Lookup algorithm

Two reader paths, chosen per call:

- The starting Pod and every chain ancestor (ReplicaSet, Deployment, LWS) are fetched with `apiReader` — direct API server reads, no informer caching them.
- The scaler lookup (`HPAByScaleTargetKey` / `ScaledObjectByScaleTargetKey`) uses the cached client because field indexes only work against an informer-backed cache. The two indexed types are watched cluster-wide; their watch streams are tiny (one entry per variant).

The two steps are kept separate: cache the immutable `Pod → top-level resource` part; always run the field-indexed scaler lookup fresh.

```go
func (l *podLocator) Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error) {
    // Step 1: pod → top-level scale target. Immutable per Kubernetes' ownerReference
    // rules, so the result is cacheable indefinitely.
    target, hit := l.cache.Get(podKey(namespace, podName))
    if !hit {
        pod := &corev1.Pod{}
        if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
            if apierrors.IsNotFound(err) { return nil, nil }
            return nil, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
        }
        var err error
        target, err = l.resolveScaleTarget(ctx, pod, namespace)  // walks via apiReader
        if err != nil { return nil, err }
        l.cache.Add(podKey(namespace, podName), target)  // target may be the zero value (no scaler-eligible ancestor)
    }
    if target == (chainNode{}) {
        return nil, nil
    }

    // Step 2: top-level resource → managed scaler. NOT cached — always read through
    // the field index so a flipped llm-d.ai/managed annotation or an edited
    // scaleTargetRef takes effect on the next Locate.
    hpa, err := l.lookupHPAByScaleTarget(ctx, namespace, target)
    if err != nil { return nil, err }
    so, err := l.lookupSOByScaleTarget(ctx, namespace, target)
    if err != nil { return nil, err }

    switch {
    case hpa != nil && so != nil:
        return nil, fmt.Errorf("pod %s/%s ambiguous: matched HPA %q and ScaledObject %q at %s",
            namespace, podName, hpa.Name, so.Name, target)
    case hpa != nil:
        return &ManagedScaler{HPA: hpa}, nil
    case so != nil:
        return &ManagedScaler{ScaledObject: so}, nil
    }
    return nil, nil
}
```

`resolveScaleTarget` runs `walkOwnersUp` and returns the first ancestor whose kind is in `scaleTargetKindSupported` (Deployment or LWS). If the walk reaches no such ancestor, it returns the zero `chainNode`, which is what gets cached as a negative entry.

KEDA-generated HPAs are not returned: the index is built only for HPAs carrying `llm-d.ai/managed: "true"`, which KEDA's generated HPAs do not. A Deployment scaled by a managed ScaledObject resolves to the ScaledObject, not to the KEDA-owned HPA underneath.

## Owner-chain walk

```go
// walkOwnersUp returns the chain [self, owner, owner-of-owner, ...] starting
// from start and stopping at:
//   - the first node with no controller ownerReference,
//   - maxDepth reached,
//   - a previously-seen node (cycle), or
//   - an unknown kind (walk stops; chain so far is returned without error).
//
// Known kinds are fetched via apiReader (uncached). The walk does NOT touch
// the controller-runtime cache for these kinds, so no Pod / ReplicaSet /
// Deployment / LWS informer is started by the locator. RBAC is static.
func (l *podLocator) walkOwnersUp(ctx context.Context, start client.Object, namespace string) ([]chainNode, error)
```

Known kinds: `Pod`, `apps/v1.ReplicaSet`, `apps/v1.Deployment`, `leaderworkerset.x-k8s.io/v1.LeaderWorkerSet`.

Bounds:

- `maxDepth = 8` (Pod → ReplicaSet → Deployment → CR → wrapper-CR is 5 today; 8 leaves slack).
- Cycle guard via a seen-set keyed by `(apiVersion, kind, name)`.
- Cross-namespace ownerReferences are illegal in core/v1; the walk stays in the pod's namespace.

## Resolution cache

Pod-side reads go through `apiReader`, so without memoization a saturation tick that resolves N pods costs O(N × chain depth) GETs. The locator keeps a small in-process **LRU** of pod → top-level scale target results to amortize the chain walk.

```go
type chainNode struct {
    Namespace, APIVersion, Kind, Name string
}

type podLocator struct {
    cached    client.Reader
    apiReader client.Reader
    maxDepth  int
    cache     *lru.Cache[podKey, chainNode]  // value is the resolved scale-target node
}

const defaultCacheSize = 4096
```

**Cached:** Pod → top-level scale target (`chainNode` for a Deployment or LWS). Repeated `Locate` calls for the same pod skip the apiReader walk after the first one. Two pods that share a Deployment / LWS still each pay their own pod → ReplicaSet → Deployment walk on a miss; sharing happens at the *scale-target* level once the LRU is warm.

**Not cached:** the scale-target → managed scaler step. Every `Locate` runs `lookupHPAByScaleTarget` / `lookupSOByScaleTarget` against the field index. Those lookups are served by the controller-runtime cache (no API call) and reflect the current state of `llm-d.ai/managed` annotations and `scaleTargetRef` edits without a staleness window. Cost: two indexed list lookups per `Locate`. Benefit: no invalidation logic anywhere — the field index is the single source of truth for the mutable side of the resolution.

**Eviction is size-only.** `defaultCacheSize = 4096`, a package constant. No TTL. Each entry is one `podKey` (namespace + name strings) plus a `chainNode` value (four short strings); 4096 entries fit in a few hundred KB.

**Staleness of the cached step.** Kubernetes forbids changing a controller `ownerReference` after creation, so a pod is never re-parented to a different ReplicaSet, and a ReplicaSet is never re-parented to a different Deployment. The pod → scale-target answer is therefore correct for the lifetime of the cached pod. When the pod is deleted and recreated, the cache key includes its name (Kubernetes guarantees a fresh UID, but at this layer name uniqueness within namespace is sufficient) — a stale entry survives only until LRU eviction, and a re-resolve produces the same answer for an unchanged controller chain.

A negative result (no scaler-eligible ancestor) is also cached, so an unmanaged pod does not retrace its chain on every tick.

## Index freshness

Both indexes key managed scalers by their own `spec.scaleTargetRef` — a field of the indexed object itself, not anything derived from `ownerReferences` or any other resource. controller-runtime recomputes the index entry whenever the HPA / ScaledObject changes (which is the only event that can change `scaleTargetRef`), so the index is always fresh by construction. No periodic rebuild, no mid-chain watches, no staleness window to engineer around.

The chain portion (Pod → ReplicaSet → Deployment, etc.) is immutable per Kubernetes' `ownerReferences` rules — controllers cannot be changed after creation — so neither the cached client nor the locator's LRU has a freshness concern there. Only the scaler side can mutate, and that flows through the field index automatically.

## RBAC

New cluster-wide read-only grants required for the locator:

```yaml
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["apps"]
  resources: ["replicasets", "deployments"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["leaderworkerset.x-k8s.io"]
  resources: ["leaderworkersets"]
  verbs: ["get", "list", "watch"]
```

Existing HPA and ScaledObject grants from `HPAReconciler` / `ScaledObjectReconciler` already cover the index sources. No `get *.*` is required because the walk handles only the typed kinds above.

The shadow-pod case is covered by `LocateByVariant`: the caller pulls `llm_d_ai_variant` and `namespace` off the metric series (both present courtesy of the relabeling rule mandated by `docs/design/controller-behavior.md`) and resolves directly to the managed scaler. `Locate(podName)` returns `(nil, nil)` for shadow pods because the owner chain doesn't reach the scaler — that is the expected behavior, and callers use `LocateByVariant` instead.

## Error semantics

| Condition | Return | Log |
|---|---|---|
| Pod not found | `(nil, nil)` | DEBUG |
| No managed scaler in chain | `(nil, nil)` | DEBUG |
| Both HPA and ScaledObject match same chain node | `(nil, error)` | WARN — config error |
| Chain depth exceeded | `(nil, error)` | WARN with chain so far |
| Cycle detected | `(nil, error)` | WARN with cycle path |
| Unknown kind ends the walk before any hit | `(nil, nil)` | DEBUG |
| API / cache failure | `(nil, error)` | ERROR |

Callers treat `(nil, nil)` as "skip this metric this tick"; `error` is logged and the cycle continues with other pods.

## Impact on current code

The current code does pod → VA mapping by reading the `llm_d_ai_variant` Prometheus label off every metric series. The Locator replaces that label dependency for ordinary Deployment / LWS layouts while keeping the label as the supported fast path for shadow pods. The change is **additive**: with the label present everything keeps working unchanged; without the label, the Locator's owner-walk fills the gap for non-shadow-pod variants.

### Current call site

`internal/collector/replica_metrics.go:312-347` — `buildInstanceKey(labels)` returns `(instanceKey, podName, vaName)` where `vaName := labels[constants.VariantLabelPrometheusKey]`. Every Prometheus result row in `refreshReplicaMetrics` flows through this helper. Downstream uses at `:724` (`vaName := data.vaName`) and `:761` (`variantKey := utils.GetNamespacedKey(namespace, vaName)`) feed the engine's `variantAutoscalings[variantKey]` map.

The earlier owner-walking helper (`PodVAMapper.FindVAForPod`) was removed during recent refactors; today's code has only the label-based path.

### Locator integration — minimal edit

Replace the single label read in `buildInstanceKey` with: try the label first; if absent and `podName != ""`, ask the Locator. The synthetic VA built from the returned `ManagedScaler` shares the scaler's name, so the rest of `replica_metrics.go` is unchanged.

```go
// internal/collector/replica_metrics.go (around L319, inside buildInstanceKey)
vaName := labels[constants.VariantLabelPrometheusKey]
if vaName == "" && podName != "" {
    if ms, _ := l.locator.Locate(ctx, namespace, podName); ms != nil {
        switch {
        case ms.HPA != nil:          vaName = ms.HPA.Name
        case ms.ScaledObject != nil: vaName = ms.ScaledObject.Name
        }
    }
}
```

Threading the locator into `refreshReplicaMetrics` (existing signatures at `replica_metrics.go:94, 141, 185`) and the saturation engine (`engines/saturation/engine.go:1195`, `engine_v2.go:29`) is a constructor-parameter edit, no logic change.

### Compatibility matrix

| Layout | Pod has `llm-d.ai/variant` label | ServiceMonitor relabel rule | Locator path used | Result |
|---|---|---|---|---|
| Deployment | yes | yes | label fast path (current behaviour) | works — no change |
| Deployment | no | n/a | `Locate(podName)` owner-walk | works — owner-walk resolves to managed HPA / ScaledObject |
| LWS | yes | yes | label fast path | works — no change |
| LWS | no | n/a | `Locate(podName)` owner-walk | works — chain reaches the LWS |
| Shadow pod | yes | yes | label fast path (`vaName` from label) | works — no change |
| Shadow pod | no | n/a | `Locate(podName)` owner-walk → `(nil, nil)` | metric skipped — operator must add the label for shadow pods |

The "label present" rows are byte-identical to today's behaviour: the existing label read returns `vaName`, the locator branch is never taken. The "label absent + non-shadow" rows are new and previously returned `vaName=""` (the metric was silently skipped); they now resolve correctly via the locator.

### PromQL templates — no changes required

The templates in `internal/collector/registration/{saturation,throughput_analyzer,queueing_model}.go` already include `llm_d_ai_variant` in their `by (...)` clauses. PromQL treats an absent label as the empty string, so:

- Series from pods that carry the label group under `llm_d_ai_variant="<name>"` (today's behaviour).
- Series from pods that do not carry the label group under `llm_d_ai_variant=""` and are otherwise unchanged.
- Within a single pod, the relabel rule sets the label per *target*, not per-series, so all series for that pod carry the same value (or are all empty). No risk of split rows for the same pod.

Keeping the label in the `by (...)` clauses ("conservative" template policy) means shadow-pod operators continue to use the label fast path, and Deployment / LWS operators who omit the label produce empty-string rows that the Locator resolves Go-side. Cardinality is unchanged because the label is fully determined by `pod`.

### Documentation impact

- `docs/design/controller-behavior.md`, "Prerequisites" section: the `llm-d.ai/variant` pod-template label and the ServiceMonitor / PodMonitor relabeling rule become **required only for shadow-pod layouts**. For Deployment and LWS scale targets, they are an optional fast path; omitting them no longer breaks variant association.

### Net delta

One conditional in `replica_metrics.go` plus a constructor parameter threaded through three call sites. No engine logic changes, no datastore changes, no PromQL template changes, no operator action required for existing deployments.

## Components / Files

| File | Purpose |
|---|---|
| `internal/collector/locator/locator.go` | new — `PodLocator` interface, `ManagedScaler`, `Locate` impl |
| `internal/collector/locator/walk.go` | new — `walkOwnersUp`, `chainNode`, kind helpers |
| `internal/collector/locator/cache.go` | new — `chainNode` LRU (size 4096) wrapping `hashicorp/golang-lru/v2` |
| `internal/collector/locator/locator_test.go` | new — table-driven tests using envtest fake client |
| `internal/controller/indexers/indexers.go` | refactor — keep only `SetupIndexes` and shared helpers (`scaleTargetIndexKey`); register all three indexes here |
| `internal/controller/indexers/variantautoscaling.go` | new — move existing `VAScaleTargetKey` + `VAScaleTargetIndexFunc` + `FindVAFor*` helpers here |
| `internal/controller/indexers/hpa.go` | new — `HPAByScaleTargetKey`, `HPAByScaleTargetIndexFunc`, `FindHPAForScaleTarget` |
| `internal/controller/indexers/scaledobject.go` | new — `ScaledObjectByScaleTargetKey`, `ScaledObjectByScaleTargetIndexFunc`, `FindSOForScaleTarget` |
| `cmd/main.go` | construct `PodLocator`, inject into saturation analyzer / Coordinator |
| `config/rbac/role.yaml` + chart templates | add Pod / ReplicaSet / Deployment / LWS read grants |

## Tests

Table-driven tests under `internal/collector/locator/locator_test.go` using the existing controller-runtime fake-client patterns:

**`Locate` (owner-walk path):**

- Pod → ReplicaSet → Deployment → managed HPA → `ManagedScaler{HPA: ...}`.
- Pod → LWS → managed HPA → `ManagedScaler{HPA: ...}`.
- Pod → ReplicaSet → Deployment → managed ScaledObject → `ManagedScaler{ScaledObject: ...}`. Verify the KEDA-generated HPA on the same Deployment is **not** returned.
- Shadow pod via `Locate` only: vLLM pod whose `ownerReferences` chain does not reach the HPA's scale target → `(nil, nil)`. (Caller is expected to use `LocateByVariant` instead.)
- Unmanaged Deployment in chain → `(nil, nil)`.
- HPA and ScaledObject both annotated managed and pointing at the same Deployment → error.
- Unknown CR kind in chain → walk stops cleanly, returns `nil` if no managed scaler hit before that point.
- Cycle in ownerReferences → error.
- Depth exceeded → error.
- Pod not found → `(nil, nil)`.

**`LocateByVariant` (label fast path):**

- Variant name resolves to a managed HPA in the namespace → `ManagedScaler{HPA: ...}`.
- Variant name resolves to a managed ScaledObject in the namespace → `ManagedScaler{ScaledObject: ...}`.
- Shadow pod: vLLM pod's `llm-d.ai/variant` label propagated to `llm_d_ai_variant`; caller passes the label value → returns the managed scaler regardless of `ownerReferences` shape.
- Variant name resolves to a non-existent or unmanaged scaler → `(nil, nil)`.
- Variant name resolves to both a managed HPA and a managed ScaledObject of the same name → error.

**Resolution cache:**

- Repeated `Locate` for the same pod issues 0 apiReader Gets on the second call (the pod → scale-target step is cached) but still runs the field-indexed scaler lookups.
- Toggling `llm-d.ai/managed: "false"` on the HPA between two `Locate` calls flips the second result to `(nil, nil)` without any invalidation work — the field index reflects the toggle immediately.
- Editing the HPA's `scaleTargetRef` to point elsewhere makes the next `Locate` for the same pod return `(nil, nil)` (the cached scale-target is now unscaled by this HPA, and no other managed scaler points at it).
- Negative entry: a pod with no scaler-eligible ancestor caches the zero `chainNode` and subsequent `Locate` calls short-circuit without re-walking.
- LRU eviction: filling the cache past `defaultCacheSize` evicts the least-recently-used entry; a re-resolve after eviction issues a fresh apiReader walk.

## Potential future enhancements

- **Drop the variant label entirely.** When shadow-pod layouts are no longer needed (or when an alternative linkage — e.g. an annotation on the parent CR, or a configured resolver — is settled), the `llm-d.ai/variant` pod label and the ServiceMonitor / PodMonitor relabeling rule can be removed from the operator contract. `LocateByVariant` would be removed alongside.
- **More scale-target kinds.** Extend `scaleTargetKindSupported` and the typed-fetch switch in `walkOwnersUp` to handle StatefulSet, ReplicaSet-as-root, and DaemonSet when WVA grows to scale them.
- **Dynamic walk for unknown CR kinds.** A `locator.additionalChainKinds: [{group, kind}]` config knob, with operator-supplied RBAC, would let the walk traverse arbitrary CR kinds in the chain. Out of scope here because the typed kinds cover every layout WVA currently scales.
- **kube-state-metrics-driven pod → Deployment resolution.** kube-state-metrics already publishes `kube_pod_owner{pod, namespace, owner_kind, owner_name}` and `kube_replicaset_owner{...}` series; joining them in PromQL collapses the Pod → ReplicaSet → Deployment chain into a single label tuple on every metric series. Where the saturation analyzer already runs Prometheus queries, this would let the locator skip apiReader walks entirely and read the chain off the metric series. Adds a hard dependency on KSM and on the operator deploying it; therefore proposed as an opt-in resolver behind a config flag rather than a default.
