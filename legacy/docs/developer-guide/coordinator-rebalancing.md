# POC: Multi-Model GPU Coordinator

This POC demonstrates GPU starvation and Coordinator-driven rebalancing across two
models sharing a GPU `ResourceQuota` on a kind cluster. Each model has its own EPP,
InferencePool, HPA, and llm-d-inference-sim workload.

## Architecture

```
multi-model-gateway (nginx, port 9002)
  /model-a/...  →  model-a EPP  →  InferencePool model-a  →  llm-d-sim A  ← HPA A
  /model-b/...  →  model-b EPP  →  InferencePool model-b  →  llm-d-sim B  ← HPA B

Prometheus  ←  ServiceMonitor per EPP
              ←  inference_extension_flow_control_queue_size{inference_pool="model-a|b"}
HPA  ←  external metric via Prometheus Adapter
WVA Coordinator  →  patches HPA spec.maxReplicas proportionally to queue depth
```

Each model has its own dedicated GAIE EPP instance and InferencePool. A shared EPP
is incorrect — the EPP uses load-based scoring, not model-name filtering, so it would
route ~50% of model-a requests to model-b pods and vice versa.

## Prerequisites

- `kind`, `kubectl`, `helm`, `docker` installed locally
- `kubectl` configured against the target cluster

## Demo

```bash
# 1. Create a kind cluster with 10 emulated nvidia GPUs
make create-kind-cluster CLUSTER_NODES=1 CLUSTER_GPUS=10 CLUSTER_GPU_TYPE=nvidia

# 2. Build and load the WVA image
make docker-build IMG=wva-local:poc
kind load docker-image wva-local:poc --name kind-wva-gpu-cluster

# 3. Deploy WVA controller + kube-prometheus-stack + Prometheus Adapter
make deploy-e2e-infra IMG=wva-local:poc SKIP_BUILD=true \
  SCALER_BACKEND=prometheus-adapter LLMD_NS=llm-d-sim

# 4. Enable the Coordinator (experimental feature, off by default)
make poc-enable-coordinator

# 5. Install per-model EPPs, sim workloads, gateway, and adapter rules
make poc-install

# 6. Verify everything is healthy
make poc-status

# 7. Reproduce model starvation (applies ResourceQuota, strips HPA annotations)
make poc-starvation

# 8. Activate the Coordinator (re-add HPA annotations), watch rebalance
make poc-rebalance

# 9. Tear down
make destroy-kind-cluster
```

## Experiment setup

### Cluster and quota

| Resource | Value |
|---|---|
| Kind cluster | 1 node, 10 emulated `nvidia.com/gpu` |
| ResourceQuota (`gpu-quota`) | `requests.nvidia.com/gpu: 10` |
| Namespace | `llm-d-sim` |

### HPAs (both models identical)

| Field | Value |
|---|---|
| `minReplicas` | 1 |
| `maxReplicas` | 10 (ceiling overridden by Coordinator once annotation is present) |
| Metric | External: `model_a/b_epp_queue_size`, target `Value: 1` |
| `scaleUp.stabilizationWindowSeconds` | 0 (immediate) |
| `scaleUp` policy | +4 pods per 15s |
| `scaleDown.stabilizationWindowSeconds` | 120s |
| `scaleDown` policy | 100% per 30s |

The low target value (`1`) means even a single queued request triggers scale-up,
making the starvation effect visible quickly in a sim environment.

### Phase 1 — model-a starvation (`starvation-load-a` job)

`make poc-starvation` applies the ResourceQuota, recreates both HPAs **without** the
`llm-d.ai/epp-inference-pool` annotation (so the Coordinator skips them), then
launches `starvation-load-a`:

- **Burst**: 80 concurrent requests to model-a fired immediately
- **Drip**: 10 req/s to model-a for 240 seconds
- Target: `multi-model-gateway:9002/model-a/...`
- Prompt: ~50-word ML description, `max_tokens: 300` (long enough to hold pods busy)

model-a's HPA fires within ~30s and scales to 10 pods, consuming all 10 GPU slots.

### Phase 2 — equal load, starvation visible (`starvation-load-b` job)

After phase 1, `poc-starvation` launches `starvation-load-b`:

- **Burst**: 80 concurrent requests to **both** model-a and model-b simultaneously
- **Drip**: 10 req/s to each model for 300 seconds (same prompt, `max_tokens: 300`)

model-b's HPA computes `desiredReplicas > 1` from the rising queue, but every new
pod is rejected by the ResourceQuota admission controller
(`exceeded quota: requests.nvidia.com/gpu`). model-b stays at 1 pod regardless of
demand — starvation.

### Rebalance (`poc-rebalance`)

`make poc-rebalance` re-adds the `llm-d.ai/epp-inference-pool` annotation to both
HPAs. On the next Coordinator tick (~15s) it reads both EPP queue depths, computes
proportional `maxReplicas`, and patches both HPAs. Kubernetes immediately enforces the
new ceilings: model-a pods above its new limit are terminated, freeing GPU slots for
model-b pods to schedule.

### Scale-down after load ends

When a model's queue hits 0 the Coordinator sets its `maxReplicas` to 1 (0% share →
clamped to `minReplicas`). Because `currentReplicas > spec.maxReplicas`, Kubernetes
enforces the ceiling immediately, bypassing `scaleDown.stabilizationWindowSeconds`.
Scale-down completes within ~15s of the queue clearing.

## How the Coordinator works

The Coordinator runs a 15-second loop. On each tick it:

1. Reads the namespace `ResourceQuota` GPU budget
2. Queries each EPP's `inference_extension_flow_control_queue_size` metric
3. Patches each HPA's `spec.maxReplicas` proportionally:

```
maxReplicas(pool) = round( queue(pool) / sum(all queues) × gpu_quota )
                    clamped to [minReplicas, absolute_max]
```

The `llm-d.ai/epp-inference-pool` annotation on each HPA is the on/off gate:
- annotation absent → Coordinator skips the HPA
- annotation present → Coordinator manages `spec.maxReplicas`

`poc-starvation` strips the annotation; `poc-rebalance` adds it back.

Ceiling patches use optimistic concurrency (`MergeFromWithOptimisticLock`): the
patch pins the object's `resourceVersion`, so a concurrent `maxReplicas` change
is rejected with a `409 Conflict` instead of being silently overwritten. On
conflict the Coordinator re-reads the object and re-applies its computed target
(bounded retry); the Coordinator remains authoritative for the ceiling.

## Known limitations and TODOs

| Area | Status | Detail |
|---|---|---|
| Scale-to-zero | TODO | When a pool's queue hits 0 the Coordinator sets `maxReplicas=1`, not 0. Full scale-to-zero requires `spec.minReplicas=0` on the HPA, cold-start handling at the EPP, and a drain window before zeroing. |
| Multiple ResourceQuotas | TODO | Only the first quota with a GPU field is used. The effective limit should be the minimum across all quotas in the namespace. |
| `minReplicas` floor | TODO | The allocation floor is hardcoded to 1. If an HPA has `spec.minReplicas > 1`, patching `maxReplicas` below it produces an invalid object. Floor should be `max(1, hpa.Spec.MinReplicas)`. |
| n HPAs > quota | TODO | When more HPAs exist than GPU slots, every pool is clamped to 1 and total allocation exceeds quota. Top-N ranking by queue depth is needed. |
| Wobble | TODO | Noisy queue readings cause 1-replica flips every tick. A minimum-delta guard and per-HPA cooldown (or EWMA smoothing) are needed. |
| Stale patch | Done | Ceiling patches use `MergeFromWithOptimisticLock` with a bounded conflict retry, so a concurrent `maxReplicas` change is no longer silently overwritten. |
| Pool name collision | TODO | Queue query has no namespace label; pools with the same name in different namespaces inflate each other's readings. |

## Files

### Kustomize base (`base/`)

| File | Purpose |
|---|---|
| `base/kustomization.yaml` | Lists all base resources |
| `base/model-a-hpa.yaml` | HPA for model-a — no `epp-inference-pool` annotation (starvation state) |
| `base/model-b-hpa.yaml` | HPA for model-b — no `epp-inference-pool` annotation (starvation state) |
| `base/model-a-decode.yaml` | llm-d-sim Deployment + Service for model-a |
| `base/model-b-decode.yaml` | llm-d-sim Deployment + Service for model-b |
| `base/multi-model-gateway.yaml` | nginx path-based gateway (port 9002) |
| `base/llm-d-sim-gpu-quota.yaml` | ResourceQuota: requests.nvidia.com/gpu=10 |

### Kustomize overlay (`overlays/rebalance/`)

| File | Purpose |
|---|---|
| `overlays/rebalance/kustomization.yaml` | Applies base + patches `epp-inference-pool` annotation onto both HPAs |

### Top-level

| File | Purpose |
|---|---|
| `model-a-epp-values.yaml` | GAIE standalone Helm values for model-a EPP |
| `model-b-epp-values.yaml` | GAIE standalone Helm values for model-b EPP |
| `prometheus-adapter-epp-rules.yaml` | Adapter rules: EPP queue → external metrics |
| `starvation-load-a.yaml` | Load job for model-a |
| `starvation-load-b.yaml` | Load job for model-b |
| `poc.mk` | All make targets for this POC |
