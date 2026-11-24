---
name: degraded-replica-test
description: Inject a capacity-shaped degradation into a running benchmark scenario by locking GPU clocks on all model-server pods, then restore. Tests how a KEDA scaling strategy responds to a supply-side failure (degraded pods) rather than a demand-side change.
---

# Degraded-Replica Test

Lock GPU clocks on all model-server pods to simulate a supply-side failure.
The arrival rate does not change. The service rate falls.

## Prerequisites

Pods must be privileged (nvidia-smi clock control requires it):

    oc adm policy add-scc-to-user privileged -z default -n <namespace>
    oc -n <namespace> patch deploy <deploy> --type strategic \
      -p '{"spec":{"template":{"spec":{"serviceAccountName":"default",
      "containers":[{"name":"vllm","securityContext":{"privileged":true}}]}}}}'

If privileged pods wrote to the shared compile-cache PVC, clean it before
standing up non-privileged pods (root-owned cache files crash fresh pods).

## Usage

    bash benchmark/hack/degrade/inject_degradation.sh \
      -n <namespace> -l <label> -c <clock_mhz> -d <duration_s>

Defaults: clock 495 MHz (quarter of H100 1980 MHz max), duration 900 s.

## What to measure

| Signal | Where |
| --- | --- |
| Replica count | oc get hpa |
| Queue depth by priority | llm_d_epp_flow_control_queue_size |
| TTFT / latency attainment | per-request records |
| Class separation | latency and queue split by priority |

## What it tests

Queue-based triggers detect this failure (requests pile up). Rate-based
triggers do not (arrival rate unchanged). This is the separation the
benchmark exists to measure.

## Validated on

OpenShift (pokprod001), H100 GPUs, Qwen3-8B, llm-d-router EPP:

- Open-loop (three classes, 4.0 req/s): rate-only de-scaled mid-crisis
  (4 to 2); rate+queue scaled to cap (4 to 6) and recovered.
- AgentX sessions (growing histories, three classes): rate-only held a
  stale count (2); rate+queue scaled 2 to 3 to 4. Best-effort worst-case
  latency improved from 109 s to 10 s.

## Operational lessons

- Lock all pods, not one. The 0.70 margin absorbs a single degraded pod.
- Quarter clock (495 MHz), not half (990 MHz). Half clock produces a
  service-time failure with no pile-up; quarter clock produces a capacity
  deficit.
- Reset all running pods at window end, not just the originally locked set.
- Clean the compile-cache PVC when switching between privileged and
  non-privileged pods.
