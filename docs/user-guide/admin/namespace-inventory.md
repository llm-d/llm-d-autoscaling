# Namespace-scoped GPU inventory

By default the GPU limiter tracks one cluster-wide pool of GPUs per accelerator
type. In a multi-tenant cluster where namespaces are pinned to specific node
pools, that lets a workload in one namespace consume GPU capacity that
physically belongs to another tenant's nodes — the limiter approves an
allocation the scheduler then cannot place, and one namespace can exhaust the
shared total and stall scaling for everyone else.

The **namespace-inventory** limiter partitions GPU capacity per namespace by
intersecting discovered cluster nodes with a per-namespace node label selector.
Each namespace draws only from the GPUs on the nodes its selector matches.

## When to use it

Use namespace inventory when tenants are mapped to node pools and you want
scaling decisions to respect those boundaries. It is **opt-in**: without a
`namespace-inventory` entry in the saturation ConfigMap's `limiters:` list the
limiter behaves exactly as before (cluster-wide).

## Why node label selectors

Kubernetes nodes are cluster-scoped; there is no native "this node belongs to
namespace X". Operators that pin namespaces to node pools already rely on node
labels as the ground truth:

- node taints plus per-namespace toleration injection (e.g. Kyverno,
  `PodNodeSelector` admission),
- node labels plus per-namespace `nodeSelector` injection,
- cloud-managed node pools with team labels.

The limiter reuses those labels rather than duplicating the mapping. Selectors
are re-evaluated on every refresh, so node additions and removals (autoscaling,
repair) and node relabeling are picked up automatically.

### Prerequisite: pods must actually be pinned

**The limiter accounts for capacity as if each namespace's pods run on its
selector's nodes, but it does not place them there.** It bounds scaling
decisions; the scheduler still decides where pods land. If nothing pins a
namespace's pods to its nodes, they can be scheduled onto another bucket's
nodes and overcommit that bucket, while the pool they were charged against sits
idle.

Pair the selectors with a real placement mechanism covering the same nodes:
node taints plus per-namespace toleration injection, `PodNodeSelector`
admission, or per-namespace `nodeSelector`/`nodeAffinity` injection. The node
labels used here should be the same ones that placement keys on.

## Configuration

Declare a `namespace-inventory` entry in the `limiters:` list on the **global
`default`** entry of the saturation ConfigMap (`wva-saturation-scaling-config`).
That list is the single source that selects the GPU limiter.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    limiters:
      - name: namespace-inventory
        type: namespace-inventory
        exclude:
          - kube-system
          - workload-variant-autoscaler-system
        selectors:
          llm-d-prod:
            matchLabels:
              team: prod
          llm-d-dev:
            matchExpressions:
              - key: team
                operator: In
                values: [dev, dev-canary]
          default:
            matchLabels:
              pool: shared
```

`selectors` values are standard Kubernetes `LabelSelector`s (`matchLabels` and
`matchExpressions`). The limiter is rebuilt from the live configuration, so
**editing the ConfigMap takes effect without restarting the controller.**

Limiter modes are mutually exclusive. A `quota` entry takes precedence over
`namespace-inventory`, which in turn takes precedence over a plain
`gpu-inventory` entry; with no `limiters:` declared, the cluster-wide inventory
limiter is used. Composing a quota with physical capacity as
`min(physical, quota)` is the limiter chain's job, tracked in issue #1003.

### Namespace lookup rules

For each scaling decision, the workload namespace resolves to a pool as follows:

1. Namespace in `exclude` → bypass the limiter (no inventory constraint).
2. Exact match in `selectors` → use that selector's pool.
3. Otherwise, if a `default` selector is present → use the `default` pool.
4. Otherwise → zero inventory (scale-up is denied for that namespace).

The reserved key `default` cannot name a real namespace; add the literal
`default` namespace to `exclude` if it needs to bypass the limiter.

## Scope: applies to the V1 saturation path

Namespace isolation is enforced on the **V1 saturation analyzer** path only.
The V2 (token-based saturation) and queueing-model optimizer path computes
GPU constraints in a type-keyed, cluster-wide form that cannot express
per-namespace partitioning, so it continues to use the cluster-wide limiter.
When namespace inventory is configured while an analyzer that routes to the V2
optimizer is active, the controller logs a one-time warning and applies
cluster-wide constraints on that path. Extending namespace-aware constraints to
the V2 path is tracked separately.

## Relationship to the cluster-wide limiter

Namespace inventory **replaces** the cluster-wide GPU limiter when configured,
rather than intersecting with it: selecting `namespace-inventory` selects it as
the limiter for the pipeline. This is deliberate: each node is assigned to
exactly one bucket, so per-bucket capacity never exceeds physical capacity and a
separate cluster-total cap on top would be redundant. One consequence is
intentional — **nodes matching no selector and no `default` contribute to no
pool**, so their GPUs are unusable until those nodes are labeled or a `default`
selector is added.

Excluded namespaces are unconstrained by this limiter (they are never capped),
but their existing usage is still charged against the pool they draw from so a
shared pool is not overcommitted. A namespace whose selector matches no GPU
nodes has zero inventory and cannot scale up until matching nodes appear —
surface this with the decision's `DecisionStep`, which records
`limited by namespace-inventory[ns=<ns>, type=<type>]`.

## Limitations

Like the cluster-wide limiter, inventory accounting is **decision-based**: it
subtracts the GPUs used by the WVA-managed variants it sees this cycle, not
actual pod placement. One consequence follows:

- **Replicas with an unresolved accelerator type in a multi-type pool are not
  counted.** When a namespace's pool spans more than one accelerator type and a
  variant's accelerator cannot be resolved, its existing replicas are not
  attributed to a type and so do not reduce that type's available capacity.
  Pin GPU type via `nodeSelector`/`nodeAffinity` or the VA accelerator label so
  resolution succeeds (see [Accelerator Name Resolution](configuration.md)).

Disjoint, single-accelerator-type node pools per namespace avoid this case.

If node discovery fails mid-cycle, this limiter **fails closed**: scale-up is
held at the current replica counts and retried on the next cycle, rather than
applying unconstrained decisions. Isolation is a safety control, so a transient
discovery error must not silently lift it. The cluster-wide and quota limiters
keep their existing fail-open behavior.
