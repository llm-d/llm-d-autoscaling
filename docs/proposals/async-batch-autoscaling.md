# Batch and asynchronous inference autoscaling driven by llm-d-async

Status: Draft — Milestone 1 validated end-to-end on the ap-135-vllm prototype
cluster (2026-09-21); M2/M3 blueprints authored, validation pending
Author: @jacobmurry

## Summary

llm-d gains a documented, supported path for autoscaling model servers for
**batch and asynchronous inference**: workloads that care about finishing by a
deadline, not about millisecond latency. The design drives KEDA — llm-d's
established autoscaling engine — from metrics that
[llm-d-async](https://github.com/llm-d/llm-d-async) already exports: the broker
backlog gauge (`llm_d_async_async_broker_backlog`) and the deadline-proximity
histogram of queued requests (`llm_d_async_async_deadline_proximity_millis`).
Model servers scale to zero between bursts; requests submitted while the pool
is empty are held durably in the broker by llm-d-async's fail-closed dispatch
gates, and the growing backlog is itself the scale-up signal.

The work is staged as three milestones, each a self-contained KEDA blueprint:

1. **M1 — Scheduled windows (Cron, 0↔1)**: scale the model server up during a
   configured time window and back to zero outside it, with llm-d-async
   configured to hold requests while the pool has no ready backends.
2. **M2 — Backlog-driven (0↔N)**: scale replicas proportionally to broker
   backlog; return to zero when the queue drains and in-flight work completes.
3. **M3 — Deadline-proximity-driven**: compute the number of replicas required
   to meet outstanding deadlines — given a measured cold-start latency and
   per-replica throughput — and wake the pool at the latest safe moment.

No new controllers and no llm-d-async code changes are required through M3;
everything is expressed as KEDA `ScaledObject`s, Prometheus recording rules,
and llm-d-async gate configuration.

## Motivation

llm-d autoscaling today is built for real-time interactive inference. The
existing blueprints in this repository and the strategy menu in
`llm-d/guides/workload-autoscaling/` scale on instantaneous load signals
(vLLM queue depth, KV-cache utilization, EPP flow-control queue size, TTFT/TPOT
SLOs). There are no mechanisms or guides for batch and asynchronous inference.

Batch and asynchronous use cases — recomputing recommendation profiles every
hour, labeling a dataset overnight — care about finishing by a **deadline**,
not about millisecond latency. For these workloads the priority is to minimize
infrastructure footprint while maximizing throughput. Yet today, teams running
them either keep GPUs powered on 24/7 between bursts, or write and maintain
their own scaling scripts to fill the gap. llm-d does not serve this class of
workload well, and that is the opportunity.

**Who this is for**: teams with scheduled or event-driven inference against
completion windows measured in minutes-to-hours, who want to stop paying for
idle accelerators and stop building bespoke batch pipelines.

The economics are significant: accelerator nodes frequently bill at whole-node
granularity (an 8-GPU A3 node costs ~$88/hour on GKE), capacity for premium
SKUs requires reservations or queued provisioning (DWS), and an "always-on"
posture for a workload that runs a few minutes per hour wastes >90% of spend.

### Goals

- Scale llm-d model servers to zero between bursts of asynchronous work, and
  back up when work arrives — with zero request loss.
- Meet request deadlines: the scaling signal ultimately incorporates how close
  queued requests are to their deadlines (M3), not just how many there are.
- Reuse what exists: KEDA as the engine, llm-d-async's exported metrics as the
  signal, llm-d-async's dispatch gates as the hold-back mechanism. No new
  controllers, no forked components.
- Produce reusable blueprints and a validation runbook, following this
  repository's staging-scenario conventions, that graduate into a
  `llm-d/guides/workload-autoscaling/` strategy after evaluation.

### Non-Goals

- A deadline-aware planner **controller** (exact earliest-deadline-first
  feasibility, per-item scheduling, `redis-leased-rate` lease writing). M3
  deliberately approximates this in pure PromQL; the controller is future work.
- Multi-pool or multi-model bin-packing and cost-aware variant selection.
- Changes to llm-d-async, llm-d-router, or vLLM code.
- Autoscaling the llm-d-async processor itself (it is CPU-cheap and can stay
  at fixed replicas; see `docs/operations/async-processor.md` in llm-d).
- Scaling on token-weighted work estimates (future work; requires calibration).

## Proposal

### Architecture: the closed loop

```
producers ──► Redis sorted set (score = request deadline, Unix seconds)
                  │
                  ▼
            llm-d-async ──── dispatch gate (fail-closed when the pool
                  │           has no ready backends: requests stay in
                  │           the broker; backlog + deadline metrics
                  │           keep accruing)
                  │ /metrics: broker_backlog, deadline_proximity_millis, …
                  ▼
             Prometheus ──► KEDA ScaledObject ──► HPA ──► model server
                                                          Deployment (0..N)
                  ▲                                            │
                  └────────── pool has ready backends ─────────┘
                              gate opens, backlog drains
```

Two properties make this loop sound:

1. **Hold-back is durable and unbounded.** llm-d-async's dispatch gates fail
   closed when the model-server pool is scaled to zero (see "Gate
   configuration" below), so undispatched requests remain in the Redis sorted
   set — not in pod memory. This contrasts with the EPP in-memory flow-control
   queue used by the `fast-model-actuation-keda` guide, which is bounded by
   `priorityBands[].maxRequests`/`maxBytes`, is lost on EPP restart, and holds
   request bodies in memory for the whole `noEndpointRequestTTL` budget during
   a cold start. For asynchronous workloads with minutes-to-hours of slack,
   broker-side holding is strictly better; it is also what makes the backlog
   metric a truthful scaling signal.
2. **The signal survives scale-to-zero.** `broker_backlog` and
   `deadline_proximity_millis` are computed broker-side (Redis `ZCARD` /
   `ZCOUNT`) by the always-on llm-d-async processor. They keep reporting — and
   growing — while the model server is at zero replicas, which is precisely
   when interactive-path metrics (vLLM, EPP) go silent.

### User stories

#### Story 1: Hourly recommendation refresh

A team recomputes recommendation profiles at the top of every hour: a few
minutes of GPU work, then nothing until the next cycle. With M1 they configure
a KEDA Cron trigger matching their schedule; the model server is up for the
window and at zero replicas the other ~50 minutes of each hour. Requests
enqueued early are held in the broker and drain when the window opens. With
M2 they drop the schedule entirely: the burst of enqueued work itself scales
the pool up, and the drained queue scales it back down.

#### Story 2: Overnight dataset labeling

A team enqueues hundreds of thousands of captioning requests at 6 PM with a
9 AM deadline. With M3, the deadline-proximity histogram shows all work is
many hours out; the required-replica computation stays below the activation
threshold and the pool sleeps. As the deadline approaches, the computed
requirement crosses the threshold at the latest moment at which the remaining
work still fits (given cold-start latency and per-replica throughput), the
pool wakes, and the backlog drains just-in-time. GPU-hours consumed approach
the theoretical minimum for the job instead of `wall-clock hours × replicas`.

To make the win concrete: a 500k-item job at a per-replica throughput of
`R = 15` req/s on `N = 2` replicas needs `500000 / (R·N) ≈ 4.6` GPU-hours of
actual work (plus ~0.3 GPU-hours of cold start at the measured `C`), whatever
the deadline. Holding those 2 replicas warm across a 15-hour overnight window
instead costs `N × 15 = 30` GPU-hours — an ~85% reduction, and the gap widens
as the ratio of slack to work grows.

### Metrics contract

The blueprints depend on the following llm-d-async series (subsystem
`llm_d_async`, metric names prefixed `async_`; standard labels `queue_id`,
`queue_name`, `pool_name`). This section is the compatibility promise the
blueprints need from llm-d-async maintainers.

| Series | Type | Meaning |
| --- | --- | --- |
| `llm_d_async_async_broker_backlog` | gauge | Undelivered/pending messages held by the broker queue (Redis `ZCARD`), polled every `--metrics-backlog-poll-interval` (default 15s). |
| `llm_d_async_async_broker_backlog_source_available` | gauge | 1 when the last backlog read succeeded. A zero backlog is trustworthy **only** when this is 1; every query below joins on it. |
| `llm_d_async_async_deadline_proximity_millis` | snapshot histogram | Per-poll distribution of milliseconds remaining until deadline for items still queued. Bucket boundaries (ms): 0, 1s, 5s, 15s, 30s, 1m, 2m, 5m, 10m, 30m, 1h, 2h, 6h, 24h. `le="0"` counts items already past deadline; cumulative buckets therefore include expired items in every horizon. Counts are exact (`ZCOUNT`), not sampled. **Not monotonic — never apply `rate()`/`increase()`.** Redis sorted-set transport only. |
| `llm_d_async_async_queue_depth` | gauge | Requests pulled from the broker and buffered in-process awaiting a free worker. Summed alongside backlog and inflight so the metric stays above zero while work is buffered. |
| `llm_d_async_async_inflight_requests` | gauge | Requests dispatched and awaiting completion. Used to hold off scale-to-zero while work is in flight. |
| `llm_d_async_async_gate_decisions_total{reason}` | counter | Observability for hold-back (`reason="gate_closed"` climbing while the pool is at zero is the expected signature). |
| `llm_d_async_async_gate_wait_requeues_total` | counter | Increments each time a gate-waiting request is requeued after `--gate-wait-timeout`. The only signal for the otherwise-invisible gate-wait layer (see the blind spot below). |
| `llm_d_async_async_exceeded_deadline_requests_total` | counter | The failure metric every milestone's acceptance criteria reference. |

Operational guardrail shipped with the blueprints: alert when
`llm_d_async_async_broker_backlog_source_available == 0` for more than 5
minutes — while the source is untrusted, backlog-driven scale-*down* decisions
are blind.

> **Prototype finding — the gate-wait blind spot**: a held request passes
> through four in-system layers, and exactly one of them is invisible:
>
> | Layer | Metric |
> | --- | --- |
> | In the broker sorted set (unclaimed) | `broker_backlog` (and the deadline histogram) |
> | Claimed, buffered awaiting a free worker | `queue_depth` |
> | Held by a worker blocked in gate-wait | **none** |
> | Dispatched to the gateway | `inflight_requests` |
>
> The worker decrements `queue_depth` the moment it takes a message off the
> merged channel (`pkg/asyncworker/worker.go:95`) — *before* entering the
> pool-gate wait — and `inflight_requests` starts only at dispatch. So
> requests blocked in gate-wait (up to `--gate-wait-timeout`, default 5m,
> per cycle before requeue and near-instant re-claim) are counted nowhere.
> That layer holds up to the pool's `workers` count, so a burst smaller than
> that can produce **zero** KEDA signal while being held: in the M1 prototype
> test, 3 gate-waiting requests read as `broker_backlog = 0`,
> `queue_depth = 0`, and no inflight series. Under M2/M3 alone, that job
> never wakes the model server and dies at its deadline. Mitigations, in
> order of preference:
>
> 1. **Upstream metrics fix** (tracked in
>    [llm-d-async#464](https://github.com/llm-d/llm-d-async/issues/464);
>    proposed as an M2 deliverable): llm-d-async keeps counting gate-waiting
>    requests — either leave them in `queue_depth` until the gate opens, or
>    expose a `gate_waiting_requests` gauge.
> 2. **Query workaround**: sum all three visible layers
>    (`broker_backlog + queue_depth + inflight_requests`, as the M2 blueprint
>    does) and estimate the hidden layer as
>    `rate(llm_d_async_async_gate_wait_requeues_total[10m]) × gate-wait-timeout-seconds`.
> 3. Accept the deadband and size thresholds/job floors above `workers`.

### Gate configuration: holding back when the pool is empty

llm-d-async has no single "hold when no backends" flag; hold-back is a
property of the configured **dispatch gate** and its `fallback` behavior. The
blueprints standardize on the `prometheus-budget` gate as the inner
saturation gate (composable under `tier-priority-admission` where tiering is
in use):

```json
{
  "id": "model-a",
  "workers": 8,
  "gate_type": "tier-priority-admission",
  "gate_params": {
    "tier_label": "tier",
    "saturation_gate": "prometheus-budget",
    "saturation_gate_params": {
      "pool": "llm-d-router",
      "namespace": "llm-d-async",
      "max_concurrency": "12",
      "baseline": "0.05",
      "fallback": "0.0"
    }
  }
}
```

Why `prometheus-budget`:

- Its metric cascade is designed for the scaled-to-zero case: capacity is
  computed as `ready_pods × max_concurrency` from EPP's ready-pods metric, and
  when the pool drains the per-pod sources stop reporting, so the gate falls
  back. With `fallback: "0.0"` (the default) the fallback is a **budget** of
  zero — fail **closed** — and requests stay in the broker.
- It also paces dispatch when the pool *is* up, closing as observed load
  reaches `max_concurrency × (1 − baseline)` per ready pod. `max_concurrency`
  must be sized to what one replica actually serves (not the default 100), or
  the gate never closes under load.

> **Caution — audit existing configs**: a `prometheus-saturation` inner gate
> whose metric source is unavailable uses its `fallback` as a **saturation**
> value, and the default `0.0` means *unsaturated* — fail **open**. A pool
> using that gate will dispatch into a scaled-to-zero backend and burn
> requests' retry budgets. Either switch to `prometheus-budget` or set the
> saturation gate's `fallback: "1.0"`. (The prototype cluster had exactly this
> misconfiguration.)

> **Prototype finding — EPP metric-name skew**: llm-d-async's `prometheus-saturation`
> gate was updated to the new `llm_d_epp_flow_control_pool_saturation` metric
> in [llm-d-async#445](https://github.com/llm-d/llm-d-async/pull/445)
> (in v0.10.0), but the `prometheus-budget` cascade still builds its queries
> from the *deprecated* EPP names — see the three source builders in
> `pkg/async/inference/flowcontrol/promql_metric_source_factory.go`:
> `inference_extension_flow_control_queue_size` + `inference_pool_ready_pods`
> (flow-control queue-size source), `inference_pool_per_pod_queue_size`
> (per-pod queue source), and `vllm:num_requests_running` ×
> `inference_pool_ready_pods` (vLLM running source). On the prototype
> cluster's EPP only the `llm_d_epp_*` names (and raw `vllm:*`) are present,
> so the cascade never resolves and the gate stays closed **even with
> backends ready** — a total dispatch stall, not just a missing hold-back.
> (The fixed saturation gate is also not a hold-back option here: its metric
> exists only when EPP runs the flowControl plugin, and its fallback default
> reads as *zero saturation* — fail open.) Migrating the budget cascade the
> same way as #445 is tracked in
> [llm-d-async#460](https://github.com/llm-d/llm-d-async/issues/460); until
> then, use a `prometheus-query` inner gate with an explicit readiness
> budget, which is what the prototype validated
> (`worker-pools-readiness-gate.json`):
>
> ```
> clamp_max(sum(llm_d_epp_ready_endpoints{name="llm-d-router",namespace="llm-d-async"}) or vector(0), 1)
> ```
>
> Budget 0 while no endpoint is ready (hold), 1 once any endpoint is ready
> (dispatch); `or vector(0)` fails closed when EPP itself is unreachable.

Alternatives for the inner gate, documented for operators: `prometheus-query`
with an explicit readiness expression (above, or on
`kube_deployment_status_replicas_ready`) is maximally legible and binary;
`endpoint-scrape` avoids the Prometheus dependency by scraping EPP `/metrics`
directly. Both fail closed by default. `redis-leased-rate` is the hook for an
external planner and is reserved for the future-work controller.

### What scales, and what stays up

Only the model-server Deployment scales (`Deployment/vllm` in the prototype;
any Deployment or LeaderWorkerSet selected by an `InferencePool` in general —
new replicas join the pool automatically via label selection). The router/EPP,
Redis, Prometheus, and the llm-d-async processor remain always-on; they are
CPU-only and cheap relative to a single accelerator.

## Design Details

### Milestone 1 — KEDA Cron trigger, 0↔1 scaling

Simplest case, and the foundation the later milestones build on: prove that a
cluster can run with the model server at zero replicas most of the time,
accept and hold inference requests throughout, and serve them during a
scheduled window.

**Deliverables**
- Gate configuration change (above) so the pool holds requests at zero
  backends — this is the real M1 config work, not the ScaledObject.
- `ScaledObject` with a `cron` trigger (see
  [`async-batch-autoscaling/m1-cron-scaledobject.yaml`](async-batch-autoscaling/m1-cron-scaledobject.yaml)):

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: vllm-batch-window
  namespace: llm-d-async
spec:
  scaleTargetRef:
    name: vllm
  minReplicaCount: 0
  maxReplicaCount: 1
  cooldownPeriod: 60        # window end is authoritative; don't idle GPUs 300s
  triggers:
  - type: cron
    metadata:
      timezone: Etc/UTC
      start: "0 2 * * *"
      end: "0 4 * * *"
      desiredReplicas: "1"
```

**Design notes and pitfalls**
- *Window end vs. cooldown*: outside the window the trigger goes inactive;
  scale-to-zero happens after `cooldownPeriod` (plus any HPA scale-down
  stabilization). Budget GPU-minutes as `window + cooldown`.
- *Node provisioning is inside the window*: on clusters where accelerator
  nodes are provisioned on demand (GKE Autopilot NAP, DWS flex-start, spot),
  scale-up latency can be many minutes and provisioning can fail outright
  (`whenUnsatisfiable: DoNotScaleUp`). Start the window one measured
  cold-start early relative to when work must start, and alert on pods
  Pending longer than the expected provisioning time. The measured cold-start
  distribution collected here becomes M3's `C` constant.
- *Deadlines vs. windows*: requests enqueued with deadlines that expire before
  the window opens will (correctly) complete as `DEADLINE_EXCEEDED` — producer
  deadlines must account for the schedule. This is also the negative test.

**Acceptance criteria**
1. With the model server at 0 replicas, enqueued requests are held —
   `gate_decisions_total{reason="gate_closed"}` and (each gate-wait cycle)
   `gate_wait_requeues_total` climb, nothing reaches the router; no request
   is failed or lost. (`broker_backlog` itself only moves once the burst
   exceeds the pool's worker count — see the gate-wait blind spot above.)
2. At window start, replicas 0→1, the backlog drains to zero, and results are
   delivered to the result queue.
3. After window end + cooldown, replicas return to 0.
4. `exceeded_deadline_requests_total` is unchanged for requests whose
   deadlines fit the window.

### Milestone 2 — Backlog-driven autoscaling, 0↔N

Replace the schedule with the work itself: scale up when broker backlog
crosses a threshold, scale to zero when the queue drains.

**ScaledObject** (see
[`async-batch-autoscaling/m2-backlog-scaledobject.yaml`](async-batch-autoscaling/m2-backlog-scaledobject.yaml)):

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: vllm-backlog
  namespace: llm-d-async
spec:
  scaleTargetRef:
    name: vllm
  minReplicaCount: 0
  maxReplicaCount: 2
  cooldownPeriod: 300
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 0
        scaleDown:
          stabilizationWindowSeconds: 300
          policies:
          - type: Pods
            value: 1
            periodSeconds: 120
  triggers:
  - type: prometheus
    metricType: AverageValue
    metadata:
      serverAddress: http://llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc.cluster.local:9090
      threshold: "50"           # X: backlog items one replica should own
      activationThreshold: "0"
      query: >-
        (
          sum(llm_d_async_async_broker_backlog{namespace="llm-d-async"}
              and (llm_d_async_async_broker_backlog_source_available == 1))
          or vector(0)
        )
        +
        (
          sum(llm_d_async_async_queue_depth{namespace="llm-d-async"})
          or vector(0)
        )
        +
        (
          sum(llm_d_async_async_inflight_requests{namespace="llm-d-async"})
          or vector(0)
        )
```

The query sums the three *visible* holding layers — broker backlog, in-process
buffer (`queue_depth`), and dispatched (`inflight_requests`) — leaving only the
gate-wait layer uncounted (see the gate-wait blind spot above).

**Design notes**
- *`AverageValue` semantics*: the threshold is a per-replica work quota, so
  desired replicas = `ceil((backlog + queue_depth + inflight) / X)` — natural
  0→N scaling.
  `Value` would compare the raw total and pin the pool at one step. This
  matches the blueprint convention in this repository.
- *Trust guard*: the `and (… == 1)` join drops backlog series whose last
  broker read failed (identical label sets make plain vector matching work).
  Dropping is conservative for scale-up; for scale-down protection, the
  shipped alert on `source_available == 0` covers the blind spot. The
  `or vector(0)` wrappers guarantee KEDA always receives a sample.
- *Scale-to-zero vs. draining*: adding `queue_depth` and `inflight_requests`
  to the query keeps the metric above `activationThreshold: 0` while any work
  is buffered or dispatched, so KEDA cannot deactivate the pool mid-drain —
  the clean fix for the 1→0 edge. For N→N−1 scale-down, the HPA picks victims
  arbitrarily:
  set `terminationGracePeriodSeconds` on the model server ≥ llm-d-async's
  `--request-timeout` (default 5m) so vLLM finishes in-flight work on
  SIGTERM; dispatch failures that do occur surface to llm-d-async's retry
  path, so they degrade to retries, not loss.
- *Flapping*: the backlog signal is stepwise (15s broker poll × 30s KEDA
  poll). Scale-down stabilization of 300s plus a 1-pod-per-120s policy
  prevents sawtoothing while a burst is mid-drain.
- *Single-model-server assumption*: the query `sum()`s over every queue and
  worker pool in the namespace and drives one `Deployment/vllm`. That is
  correct only when all queues feed a single model server (as in the
  prototype). Clusters routing pools to different model servers need
  per-`pool_name` ScaledObjects — the metrics already carry the label; see
  Future work.

**Acceptance criteria**
1. Enqueue B items with the pool at 0 → replicas reach
   `min(ceil(B/X), maxReplicaCount)`.
2. Backlog drains; the pool scales to 0 only after backlog = 0 **and**
   inflight = 0, plus cooldown.
3. No `DEADLINE_EXCEEDED` results, no requests lost to pod termination.

### Milestone 3 — Deadline-proximity-driven autoscaling

Scale on *urgency*, not just *volume*: wake the pool at the latest time at
which the outstanding work still meets its deadlines, given two calibrated
constants. Note M3 depends on `deadline_proximity_millis`, which llm-d-async
emits only for the **Redis sorted-set transport**; on Pub/Sub transport a
deployment stops at M2 (backlog-driven), which carries no deadline signal.

- `C` — cold-start latency in seconds (scale-up decision → first token served;
  includes node provisioning, image pull, model load). Use the p90 of the
  cold starts measured in M1/M2.
- `R` — per-replica throughput in requests/second for the workload's request
  mix, measured from an M2 drain
  (`completed requests ÷ (wall time × replicas)`).

**Control law.** For each histogram boundary `t` seconds out (with `t > C`),
all work due within `t` must complete inside the `t − C` seconds a replica
started *now* would actually have. With `B(t)` = cumulative bucket count at
boundary `t`:

```
r(t)      = B(t) / (R × (t − C))        replicas required for horizon t
required  = max over feasible horizons t of r(t)
desired   = ceil(required)
```

Because buckets are cumulative and `le="0"` counts already-expired items,
past-deadline work automatically inflates every horizon — the pool wakes at
max pressure to minimize further lateness. Any count in a bucket with
`t ≤ C` is infeasible by definition and pins the requirement to
`maxReplicaCount`.

**The deferral property.** KEDA's `activationThreshold` is set just below one
replica's worth (`0.85`). While all queued work is far from its deadline,
`required` stays ≪ 1 and the pool stays asleep; `required` crosses the
activation threshold at (approximately) the latest safe scale-up time. This
single knob is the "minimize footprint" behavior — the margin below 1.0
absorbs the 15s broker poll, the KEDA poll interval, and bucket quantization.

**Where the math lives.** PromQL cannot lift the `le` label into arithmetic,
so the computation is a Prometheus recording-rule chain (the same pattern as
the `slo-aware` guide): one rule per feasible horizon emitting
`asyncq:deadline_replicas_required{horizon="…"}`, one rule for the infeasible
horizon (`≤ C`), and a `max()` rollup that the ScaledObject queries with
`threshold: "1"` / `metricType: AverageValue` (the metric *is* desired
replicas). See
[`async-batch-autoscaling/m3-deadline-prometheusrule.yaml`](async-batch-autoscaling/m3-deadline-prometheusrule.yaml)
and
[`async-batch-autoscaling/m3-deadline-scaledobject.yaml`](async-batch-autoscaling/m3-deadline-scaledobject.yaml).
Constants `R` and `C` appear as literals with a header table in the rule file,
per repository convention. The rule file's labels must match the target
Prometheus's `ruleSelector` — this is the classic silent-failure mode of the
recording-rule pattern.

**Degradation design.** The M3 ScaledObject keeps the M2 backlog trigger as a
second trigger. KEDA ORs trigger activity and the HPA takes the max of their
replica proposals, so a broken rule chain (rules not picked up, Prometheus
restart) degrades to backlog-driven scaling — never to "asleep past a
deadline". The M2 alert on `source_available` carries over unchanged.

**Known limitations (accepted for M3, listed for the record)**
- *Bucket quantization*: an item due in 61 minutes is only constrained at the
  2-hour boundary and can be up to one bucket-width late. Mitigations: a
  safety divisor on `(t − C)`, or the conservative variant (ship commented
  out) that evaluates `B(t)` against the next-lower boundary.
- *Homogeneous-work assumption*: `R` in requests/second ignores per-request
  token variance. Future work calibrates live from
  `rate(llm_d_async_async_tokens_total{direction="output"}[10m])`.
- *`C` variance*: on-demand accelerator provisioning (DWS/spot) has high
  variance; p90 is recommended, and operators trading cost for certainty can
  use max.
- *Intentional re-sleep*: if near-term work drains and remaining work is far
  out, the pool scales to zero and re-wakes later — two cold starts instead
  of one idle stretch. Operators who prefer fewer cold starts lengthen
  `cooldownPeriod`.

**Acceptance criteria**
1. Two cohorts enqueued at pool = 0 (deadlines +30 min and +6 h):
   requirement < activation → pool stays at 0.
2. Pool wakes at ≈ `deadline − C − B/R` for the near cohort (within one
   bucket boundary + one polling interval).
3. Both cohorts complete before their deadlines;
   `exceeded_deadline_requests_total` unchanged.
4. GPU-minutes consumed ≪ always-on baseline for the same job (report the
   ratio in the evaluation).

## Prototype environment and validation

Primary validation runs on a live dev cluster (GKE Autopilot,
`ap-135-vllm`): llm-d-async v0.10.0 (Redis sorted-set transport, six queues
across two worker pools), llm-d-router EPP + Envoy, single `Deployment/vllm`
(Qwen3-8B, 1×L4) selected by InferencePool `llm-d-router`, kube-prometheus-
stack already scraping all components, GPU capacity via a DWS
flex-start/spot ComputeClass. KEDA is installed from the official release
manifests. The step-by-step runbook, including how to enqueue test requests
with the `producer` module and how to measure `C` and `R`, lives in
[`async-batch-autoscaling/README.md`](async-batch-autoscaling/README.md).

**M1 validation results (2026-09-21).** All four acceptance criteria passed:
3 requests enqueued at 0 replicas were held (3 `gate_closed` decisions, 3
gate-wait requeues, zero dispatched, zero lost); the 20:45 UTC window opened
and KEDA scaled 0→1 at 20:45:26; the flex-start L4 node was provisioned by
20:47:19 (~2 min); vLLM (Qwen3-8B) was Ready at 20:53:54 and all 3 requests
completed with HTTP 200 results by 20:54:12; the pool scaled 1→0 at window
end + 60s cooldown and the GPU node was reclaimed. **Measured cold start:
C ≈ 526s** from the scale-up decision (20:45:26) to first completion
(20:54:12), breaking down as ≈113s node provisioning (→20:47:19) + ≈395s vLLM
image pull, model load, and torch.compile (→20:53:54 Ready) + ≈18s gate
reopen and first inference — one sample; collect ≥3 across windows for a p90
before calibrating M3. The negative test (deadline shorter than the sleep)
produced an explicit `DEADLINE_EXCEEDED` result, not a silent drop.

Graduation path: once validated, each milestone's blueprint is added as a
scenario under `benchmark/config/scenarios/staging/async-batch/` for
evaluation with this repository's test bed, then promoted to a
`llm-d/guides/workload-autoscaling/` strategy with a row in the strategy
menu. Note that the benchmark harness currently drives synchronous HTTP load;
evaluating async scenarios requires a Redis-enqueue load path in the harness,
which is tracked as part of the M2 engineering work.

## Engineering milestones

| # | Milestone | Deliverables | Exit criteria |
| --- | --- | --- | --- |
| M1 | Cron 0↔1 | Gate hold-back config + audit note; `m1-cron-scaledobject.yaml`; validation runbook; measured cold-start distribution (`C`) | M1 acceptance criteria pass on the prototype cluster |
| M2 | Backlog 0↔N | `m2-backlog-scaledobject.yaml`; `source_available` alert rule; drain-safety guidance (`terminationGracePeriodSeconds`, inflight guard); gate-wait visibility fix in llm-d-async ([#464](https://github.com/llm-d/llm-d-async/issues/464)) or the requeues-rate query workaround; measured per-replica throughput (`R`); Redis-enqueue load path for the benchmark harness | M2 acceptance criteria pass; staging scenario `async-batch/backlog.yaml` runs in the test bed |
| M3 | Deadline-proximity | `m3-deadline-prometheusrule.yaml` + `m3-deadline-scaledobject.yaml` (with M2 fallback trigger); limitations doc; two-cohort evaluation with GPU-minutes ratio | M3 acceptance criteria pass; comparison vs. M2 baseline reported |
| — | Graduation | `staging/async-batch/{baseline,cron-window,backlog,deadline-proximity}.yaml`; guide under `llm-d/guides/workload-autoscaling/`; strategy-menu row; cross-link from the batch-serving guides | Guide merged in llm-d |

## Alternatives

- **EPP in-memory queue + `llm_d_epp_flow_control_queue_size`** (the
  `fast-model-actuation-keda` pattern): works for scale-from-zero of
  interactive traffic, but the queue is bounded, volatile, holds bodies in
  EPP memory for the full cold start, and carries no deadline information.
  Wrong substrate for hours-of-slack batch work; complementary, not competing.
- **HPA + external metrics adapter** (prior art: a hand-rolled
  `llm_d_replica_scaling_demand` external-metric HPA found broken in the
  prototype environment): requires operating a metrics adapter, has no cron
  or activation semantics, no scale-to-zero without gymnastics. KEDA subsumes
  it; this proposal replaces that pattern.
- **A deadline-aware planner controller now**: exact EDF feasibility,
  per-item deadlines (no bucket quantization), live throughput calibration,
  writing `redis-leased-rate` leases (`max_admission_rps`) to pace dispatch
  and desired replicas together. Strictly more capable than M3 — and
  strictly more to build, operate, and get accepted. The PromQL
  approximation ships value with zero new components and generates exactly
  the operational data (measured `C`, `R`, quantization error) that a future
  controller design needs. It is the headline item of future work, and the
  `redis-leased-rate` gate (which fails closed on lease expiry and is
  observable via the `async_drain_limit_*` gauges) is the ready-made
  integration point llm-d-async already ships for it.

## Future work

- The deadline-aware planner controller described above.
- Token-weighted work estimates (`tokens_total`-calibrated `R`), replacing
  the homogeneous-request assumption.
- KEDA `scalingModifiers` formula variant to fold the M2 and M3 triggers into
  one composite metric.
- Per-queue/per-pool ScaledObjects for multi-model clusters (the metrics
  already carry `pool_name`; the blueprints sum over it today).
- Benchmark-harness support for Redis-enqueue load generation, enabling
  apples-to-apples evaluation of async strategies in the test bed.
