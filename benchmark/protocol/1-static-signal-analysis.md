# Step 1: Static signal analysis

Part of the [evaluation protocol](README.md).

Judge the configuration from its text alone: the PromQL queries, `metricType`,
thresholds, replica bounds and `behavior`. This step predicts behavior; it does
not measure it. Mark every conclusion as *from the config*, *from the metric's
source*, or *needs measurement*.

## 1.1 What the HPA computes

KEDA serves each trigger to the HPA as an external metric. For a trigger whose
query returns the total `U` and whose threshold is `T`:

```
AverageValue:  desired = ceil(U / T)                 (pod count cancels)
Value:         desired = ceil(N_ready × U / T)       (N_ready = Running and Ready pods)
```

The HPA does nothing while the ratio of current to target lies within 10 %
(tolerance `τ = 0.1`).

Several triggers are combined in one of two ways:

- **By default, the maximum.** KEDA exposes one external metric per trigger,
  and the HPA takes the maximum of their desired counts.
- **With `advanced.scalingModifiers`, a formula.** KEDA evaluates
  `scalingModifiers.formula` over the triggers' raw values and exposes only the
  result, as one composite metric with its own `target` and `metricType`. The
  formula can take a weighted sum, a minimum, or a conditional such as
  `queue > 0 ? max(rate, queue) : rate`.

External metrics get none of the per-pod corrections resource
metrics get: pods that are still starting are not set aside. The signal itself
has to behave.

**Load** below means replicas' worth of work at the SLO: arrival rate divided
by the capacity of one replica for the current traffic mix (prompt length,
output length, prefix sharing). It is not a token rate or a request rate.

## 1.2 Properties of a signal

A trigger is judged against the properties below. Which ones it must pass
depends on its role (§1.3): the driver, which sets the replica count in normal
operation, must pass all of them; guards, alarms and vetoes must pass a subset.
Symbols: `λ` load, `R` replicas, `s` the signal,
`M` the traffic mix, `μ_SLO` the capacity of one replica at the SLO.

| # | Property | Definition | Passes when |
|---|---|---|---|
| P1 | **Proportional to load** | elasticity `ε` of `s` with respect to `λ`, over `[0.2, 1.0]·μ_SLO` | `ε ≈ 1`: double the load, double the signal. No dead band (`ε ≈ 0`) and no explosion (`ε ≫ 1`) |
| P2 | **Coupled to actuation** | elasticity `η` of the per-pod signal with respect to `R`, at fixed `λ` | `η = −1`: adding a pod spreads the same total over more pods. The total `U` does not change with `R` |
| P3 | **Target means a fixed busyness** | mix sensitivity `V`: for each traffic mix `M`, read the signal when one replica is exactly at capacity for that mix; `V` is the largest reading divided by the smallest | `V ≈ 1`: one threshold is the same fraction of capacity for every mix, and it leaves spare room for bursts and cold starts |
| P4 | **Metric type matches aggregation** | extensive (`sum`, grows with `R` at fixed per-pod load) or intensive (`max`, `avg`, ratio, quantile) | extensive → `AverageValue`; intensive → `Value`; an absolute replica count from `scalingModifiers` → `AverageValue`, target `1` |
| P5 | **Bounded the right way** | *capped*: saturation point `λ_sat`, the lowest load at which `ε` drops below 0.1 (the signal stops responding). *Integrating*: the signal keeps growing while load exceeds supply | capped: `λ_sat > μ_SLO`, or the cap is the binding resource and `T < s_max / 1.1`. Integrating: the overshoot during a cold start is bounded |
| P6 | **Defined at zero replicas** | value when `R = 0`, `λ > 0` | informative (`s > 0`) and above `activationThreshold`, if `minReplicaCount` is 0 |
| P7 | **Fails visibly** | value when the source is down (scrape failure, EPP restart, wrong label) | the scaler errors and `fallback` applies; it does not read 0 |
| P8 | **Bounded lag and noise** | lag ≈ scrape interval + ½ `rate` window + `pollingInterval`; noise = sample-to-sample swing at steady load against `T` | lag well below cold start; noise well below the 10 % tolerance |
| P9 | **Measures what it claims** | what the metric's source counts, and when: arrival, admission, first token, last token, completion | the query reads the quantity its role needs, and no known trap applies |

P1–P3 are the core of the assessment. A queue or prefill backlog fails all
three: by Little's law, per pod it equals utilization × time to first token, and
time to first token rises without limit near saturation.

The notes below explain each property with an example. The examples use a
prefill pool whose replica reads 15,928 prompt tokens per second, serving
8,192-token prompts.

**P1, proportional to load.** `desired = ceil(U / T)` is a pod count only if
`U` counts pods' worth of work. The test is the **elasticity** `ε` of the
signal with respect to load: the percentage change in the signal per 1 % change
in load. Between two measurements `(λ₁, s₁)` and `(λ₂, s₂)`:

```
ε = ln(s₂ / s₁) / ln(λ₂ / λ₁)
```

`ε = 1` means proportional, `ε ≈ 0` flat (a dead band), `ε ≫ 1` an explosion.
P2 and P5 use the same measure.

Compare two prefill signals as one pod's utilization rises from 50 % to 95 %, a
factor of 1.9. For the backlog,
`ε = ln(5.130 / 0.386) / ln(1.9) = 2.59 / 0.64 = 4.0`:

| Signal | At 50 % | At 95 % | Factor | `ε` |
|---|---:|---:|---:|---:|
| Prompt tokens dispatched per second ÷ 15,928 | 0.50 | 0.95 | 1.9 | 1.0 |
| In-flight prompt tokens ÷ 15,928 (a backlog) | 0.386 | 5.130 | 13.3 | 4.0 |

The backlog equals utilization × time to first token, and time to first token
grows as the queue builds. With a 1.5 s target it reads 26 % of target at half
load (dead band) and 342 % at 95 % (explosion).

**P2, coupled to actuation.** The HPA predicts the effect of its own action by
assuming the total stays put when it adds pods. Take 40,000 prompt tokens per
second, 2.511 pods' worth, and go from 3 pods to 4:

| Signal | Per pod at 3 | Per pod at 4 | Total at 3 | Total at 4 | Desired at 4 |
|---|---:|---:|---:|---:|---:|
| Dispatched rate ÷ 15,928, target 0.7 | 0.837 | 0.628 | 2.511 | 2.511 | 4 |
| Backlog, target 1.5 s | 1.537 | 0.595 | 4.610 | 2.381 | 2 |

The rate's per-pod value falls by 3/4 (`η = −1`), so the ask stays at 4. The
backlog's falls to 39 % (`η = ln(0.595 / 1.537) / ln(4 / 3) = −3.3`) because the queue drains too, so the ask
drops to 2 pods, which cannot carry 2.511 pods' worth. The pool shrinks,
congests, and scales up again: oscillation at constant load.

**P3, a target that means a fixed busyness.** A threshold is only right if it
names the same fraction of capacity for every traffic mix the pool sees. Take a
request-rate trigger on a replica that serves 20 req/s with short answers and
4 req/s with long ones, so `V = 20 / 4 = 5`. A threshold of 14 req/s per pod is
70 % of capacity for short answers and 350 % for long ones: when the mix turns
long, the pool runs at 3.5× overload while the trigger reads on target.
Weighting by tokens narrows `V`; dividing by capacity measured for the current
mix removes it. The target must also leave room: 70 % leaves
`1 / 0.7 − 1 = 43 %` spare to absorb a burst during a cold start.

When the query or threshold divides by a per-replica capacity, the target is
only as right as that constant. Check where it came from. An idle-replica peak,
or a rate the pool once served, overstates the capacity a replica sustains at
the SLO, by 1.2–6× in measurements; the pool then runs that much too small at
"70 %". The constant's value *needs measurement*.

**P4, metric type matches aggregation.** The two types do different arithmetic,
and each is right for one kind of query. Ask: if the pods double and each
carries the same load, does the query's value double?

- Yes, it is a total (extensive): use `AverageValue`. `sum(rate(requests[1m]))`
  = 20 req/s with target 5 gives `ceil(20 / 5) = 4`, whatever the pod count.
- No, it is a per-pod level (intensive): use `Value`. 4 pods at
  `avg(kv_cache_usage)` = 0.9 with target 0.7 give `ceil(4 × 0.9 / 0.7) = 6`.

The mismatches:

- A level under `AverageValue` ignores the pod count. `max(kv_cache_usage)` =
  0.9, target 0.7, 40 pods: `ceil(0.9 / 0.7) = 2`. It can never ask for more
  than `ceil(1 / 0.7) = 2`, and it scales a busy pool *in* toward 2.
- A total under `Value` multiplies by the pod count twice.
  `sum(rate(requests[1m]))` = 30 req/s, target 10, 2 pods:
  `ceil(2 × 30 / 10) = 6`, then 18, then 54, until `maxReplicaCount`. The answer
  was 3.
- A `scalingModifiers` formula that computes the replica count itself uses
  `AverageValue` with target `1`, so the HPA passes `ceil(formula)` through.

A correct type does not make a signal proportional:
`sum(vllm:num_requests_waiting)` under `AverageValue` passes P4 and still fails
P1–P3.

**P5, bounded the right way.** A signal can fail in two opposite ways once
load exceeds supply.

A *capped* signal has a ceiling, and cannot say how much capacity is missing
once it hits it.

- KV-cache usage stops at 1.0. With threshold 0.95, one full pod gives
  `1.0 / 0.95 = 1.05`, inside the 10 % tolerance: it never scales up. The
  threshold must be below `1 / 1.1 = 0.909`.
- vLLM's `running` stops at `max_num_seqs`. With 256 slots and
  `sum(vllm:num_requests_running)` at target 200 per pod, a saturated pool asks
  for at most 1.28× its size per step: 2 → 3 → 4 → 6 → 8 → 11 pods takes five
  cold starts, about 15 minutes at 3 minutes each.

Capped signals are guards for the resource they measure, not drivers. A guard
still needs `T < s_max / 1.1`, or it never fires.

An *integrating* signal has no ceiling: a global queue (router queue, total
waiting) grows by the excess arrivals every second, including while the pods
it asked for are still starting. Pending pods do not lower it, so the ask
overshoots by about growth rate × cold start ÷ `T`. A router queue growing by
100 requests per second, a 180 s cold start and `T = 20` overshoot by
`100 × 180 / 20 = 900` replicas; only `maxReplicaCount` and the scale-up policy
stop it. A per-pod signal summed over Ready pods cannot run away this way,
because pending pods export nothing.

**P6, defined at zero replicas.** A pool that scales to zero needs a signal
that reads above 0 when requests arrive and no pod runs.
`sum(rate(vllm:request_success_total[1m]))` comes from the vLLM pods, so it
disappears with the last pod; `ignoreNullValues: true` (the default) turns the
missing series into 0, and the pool never wakes while requests wait at the
gateway. A counter at the gateway or router, such as
`envoy_cluster_upstream_rq_total`, exists with no pods. It must also clear
`activationThreshold`: KEDA wakes the pool only when the value is above it, so
a threshold of 5 on a rate of 2 req/s leaves those requests waiting.

**P7, fails visibly.** An outage must not look like zero load. A driver written
`sum(rate(...)) or vector(0)` reads 0 while the EPP restarts or a label changes;
the HPA holds the pool for the 300 s scale-down window, then removes pods while
traffic continues. `fallback` never fires, because the scaler did not fail.
Without `or vector(0)` and with `ignoreNullValues: false`, an empty result is a
scaler error, and after `fallback.failureThreshold` errors KEDA sets
`fallback.replicas`.

**P8, bounded lag and noise.**

- Lag: with a 30 s scrape, a `rate(...[2m])` window and `pollingInterval: 30`,
  the HPA sees a load step about `30 + 60 + 30 = 120 s` late. Add a 3-minute
  cold start, and the pool runs short for 5 minutes after every step.
- Noise: `inflight_tokens` jumps by a whole prompt each time a request enters
  or leaves. One 8,192-token prompt moves the signal by
  `8,192 / 15,928 = 0.51 s`, 34 % of a 1.5 s target and more than three times
  the 10 % tolerance. One high sample restarts the scale-down window, so
  scale-in stalls. A `rate()` over a window averages many requests and is
  smoother, at the cost of lag.

**P9, measures what it claims.** A metric's name does not say what it counts;
its source does. For a gauge, find when a request stops being counted:
`inflight_tokens` stops at the first token, so it is a backlog, while a running
count stops at the last token, so it is a count in progress (§1.3).

Then check the trigger against the [metric traps](traps.md): known cases where
a query does not measure what its name suggests, such as EPP average gauges
that freeze when the pool empties. Each trap holds only for the component
versions it was verified on, so first record the deployed version of every
component the trigger reads; `traps.md` says how to treat a version outside a
trap's range.

## 1.3 Classify each trigger

The kind of signal predicts which properties it can have, and therefore which
role it can play. A role says when a trigger should decide the replica count.
With the default maximum (§1.1), that means when it should win the maximum;
with a `scalingModifiers` formula, the formula must give it that say.

- **Driver.** Sets the replica count in normal operation, so it alone decides
  how many pods run at steady load. It must pass P1–P4. Example: dispatched
  prompt tokens per second ÷ per-replica capacity, target 0.7.
- **Guard.** Protects one resource the driver does not see. It stays below the
  driver until that resource runs out, then takes over and adds pods. Example:
  `avg(vllm:kv_cache_usage_perc)` under `Value`, target 0.85, which fires only
  when long answers fill the KV cache before the driver's rate says the pool is
  busy.
- **Alarm.** Reports overload after it has happened. It reads near 0 in normal
  operation and becomes large only when work piles up, so it is useful for a
  fast scale-up or an alert, never for sizing the pool. Example:
  `sum(llm_d_epp_flow_control_queue_size)`, which is 0 while the pool keeps up.
- **Scale-in veto.** Stops the pool from shrinking while users are already
  suffering. Under the default maximum it asks for the current replica count
  while a latency quantile is over the SLO, which holds the pool at its size,
  and for little otherwise. In a formula, the same hold needs a trigger that
  reads the current replica count (for example
  `kube_deployment_status_replicas`), since the formula sees only trigger
  values. It must not add pods: latency lags and is flat
  until it is steep. Example: P90 time to first token above its SLO.

Each role must pass these properties:

| Role | Must pass | Why the others do not apply |
|---|---|---|
| Driver | P1–P9 (P6 only if `minReplicaCount` is 0) | — |
| Guard | P4, P5 (capped, `T < s_max / 1.1`), P8, P9 | it is not proportional by design; reading 0 on an outage is safe while the driver runs |
| Alarm | P4, P5 (integrating, overshoot bounded by `maxReplicaCount` and `behavior`), P8, P9 | it reads 0 in normal operation by design |
| Scale-in veto | P4 (`Value`, since a quantile is intensive), P8, P9, and it never asks for more than the current count | it holds the pool and does not size it |

| Kind | Looks like | P1–P3 | Role |
|---|---|---|---|
| Offered work rate, counted before any queue | `rate()` of a gateway or router counter at arrival, optionally token-weighted, ÷ per-replica capacity | pass at a fixed mix; token weighting narrows `V` (2.6 against 7.6 for request rate) but does not reach 1 | **driver** |
| Completed work rate | `rate()` of a completion or served-tokens counter | flat at capacity: equals supply under overload | not a driver |
| Queue or prefill backlog | waiting requests, router queue, `inflight_tokens` (send to first token), even when divided by a throughput | fail: dead band, then explosion, then a target that means congestion | **alarm** (fast scale-up, alert) |
| Requests in progress (send to last token) | running or in-flight request gauge | close to P1 below saturation (`ε` 1.0–1.24); `V` up to 31 across output lengths | driver only for a stable output-length mix |
| Capped saturation | KV-cache usage, slot use, pool saturation | only below the cap | **guard** for the resource it measures |
| Latency outcome | `histogram_quantile(...)` of TTFT, ITL | fail: lags, flat until steep | **scale-in veto** |

**Placing a gauge.** A gauge of requests or tokens adds each request at one
event and removes it at a later one, so by Little's law it reads arrival rate ×
time counted. The removal event decides the row. Removed at admission or at the
first token, the time is mostly queue wait: a backlog. Removed at the last
token, the time is mostly decode: requests in progress. The metric's name
rarely says which; read its source (P9). For example,
`llm_d_epp_inflight_tokens` stops at the first token, so it is a backlog.

**Placing a counter.** A counter is incremented at one event, so its `rate()`
reads how often that event happens. Incremented at arrival, before any queue,
it is offered work; at admission or completion, it equals supply once the pool
is overloaded. For example, vLLM's `http_requests_total` is incremented at
completion, so it is completed work, not demand (trap T1 in
[traps.md](traps.md)).

## 1.4 Running the analysis

The procedure that applies §1.2 and §1.3 is in two places:

- [How to analyze one trigger](1-per-trigger-how-to.md): where to find the
  rendered ScaledObject, the deployed versions and the metric's source, and how
  to establish each property with an evidence tag.
- The [`static-signal-analysis` skill](../../.claude/skills/static-signal-analysis/SKILL.md):
  runs the per-trigger analysis on every trigger of a scenario, checks the
  combination, and writes the verdict. Invoke it with
  `/static-signal-analysis <scenario.yaml> [backend overlay]`.

A configuration with no trigger that passes P1–P4 in its intended load range is
**rejected** at this step; running it measures a known failure.

## 1.5 Worked example: `staging/pd-disaggregation/baseline.yaml`

Both pools use the same two triggers, `minReplicas: 1`, `maxReplicas: 4`, at
most one pod added per 180 s and removed per 300 s.

**`vllm-queue-size`**: `max(vllm:num_requests_waiting{role=...})`,
`AverageValue`, threshold 20.

- Kind: backlog (requests not yet admitted by vLLM). Role it can play: alarm.
- P1 fails: `waiting` reads about 0 until a pod saturates (`ε ≈ 0`), then
  integrates the excess. P2 fails: a `max` over pods does not spread over more
  pods. P3 fails: 20 waiting requests is a different congestion level for every
  prompt and output length.
- P4 fails: an intensive `max` under `AverageValue` gives `ceil(max / 20)`, the
  queue of the single worst pod, regardless of how many pods run.
- P7: no `or vector(0)`, but `ignoreNullValues` defaults to true, so a missing
  series reads 0.

**`vllm-kv-utilization`**: `max(vllm:kv_cache_usage_perc{role=...})`,
`AverageValue`, threshold 0.95.

- Kind: capped saturation, `s_max = 1`. Role it can play: guard.
- P5 fails: `0.95 > 1 / 1.1 = 0.909`. At one pod with KV full, the ratio is
  `1.0 / (0.95 × 1) = 1.05`, inside the 10 % tolerance, so it **never scales
  up**. With more pods, `AverageValue` on a `max` caps the ask at
  `ceil(1 / 0.95) = 2`.

**Combination.** No trigger is proportional, so neither pool has a driver. The
pool stays at one replica until one pod's vLLM queue passes 22 (20 plus the
tolerance). After that it grows by one pod per 180 s for as long as the worst
pod's queue stays above about 20 × the current replica count. Predicted: under-provisioning through the whole
pre-saturation range, then a slow climb to the cap of 4 driven by the queue.

**Verdict: rejected as a driver configuration; useful as the control.** Fixes:
add an offered-work-rate driver per pool (§1.3), demote the queue to an alarm,
and set the KV guard to `metricType: Value` with a threshold below 0.909 for
long-output traffic. **Needs measurement:** per-replica capacity of each role for
the benchmark's mix.

The same steps on `token-aware.yaml`: its prefill trigger (`inflight_tokens` ÷
peak prefill throughput, `AverageValue` 1.5 s) is a backlog, so it fails P1–P3,
and its `or vector(0)` fails P7. Predicted: oscillation even at constant load.

## 1.6 Verdict: `staging/pd-disaggregation/kv-early.yaml`

Produced with the `static-signal-analysis` skill on 2026-11-11. Cited files
`27_keda-scaledobjects.yaml` and `ms-values.yaml` are from the rendered plan.

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/pd-disaggregation/kv-early.yaml` |
| Backend overlay | `benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml` (the scenario names none; this is the README's example backend) |
| Rendered with | `llmdbenchmark standup --dry-run`, llm-d-benchmark@`bd7d768`. The Jinja plan rendered; the Helm step failed offline, so the modelservice values (`helm/ms-values.yaml`) were read before chart rendering |
| Date | 2026-11-11 |
| Verdict | **Rejected** |

Rejected: no trigger is proportional to load, and every query filters on a
`role` label that, per the source, no series carries, so on this stack all four
triggers most likely read 0. Predicted: both pools stay at `minReplicaCount: 1`
whatever the load. If the label does exist, the KV guard can take a pool to 2
replicas and no further, and only the queue backlog can go beyond, late and
with oscillation.

kv-early differs from `baseline.yaml` only in the KV threshold, 0.95 → 0.80
(scenario lines 205 and 238). That change fixes one check, P5 on the KV
trigger, and leaves the rest of the baseline's failures in place.

### Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| vLLM (both roles) | `vllm/vllm-openai-cpu:v0.27.0-x86_64` | overlay `model-sim-qwen3-32b.yaml:73-74` |
| llm-d-modelservice chart (PodMonitors) | `v0.4.16` (`6965740`) | llm-d-benchmark `defaults.yaml:116` |
| llm-d-benchmark (renderer) | `bd7d768` | the clone used to render |
| Prometheus endpoint | OpenShift Thanos querier, bearer auth | `27_keda-scaledobjects.yaml:24-25` |
| KEDA | 2.20 or later, release not known | `benchmark/README.md`; trap T7 is *unverified* for it |

The EPP and the gateway are not read by any trigger.

### ScaledObject `pd-disaggregation-prefill`

- `minReplicaCount: 1`, `maxReplicaCount: 4` (`27_keda-scaledobjects.yaml:15-16`).
- `pollingInterval` not rendered: KEDA's default, 30 s.
- Scale-up: one pod per 180 s, no stabilization (`:51-56`). Scale-down: one pod
  per 300 s after a 300 s window (`:45-50`).
- No `fallback`, no `ignoreNullValues`: the template cannot render them.
- vLLM scrape interval 15 s (`ms-values.yaml:564`), no relabelings
  (`ms-values.yaml:569-571`).

#### Trigger `vllm-queue-size`

```promql
max(vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B",role="prefill"})
```

`metricType: AverageValue`, `threshold: "20"`, `activationThreshold: "0"`
(`27_keda-scaledobjects.yaml:19-29`).

- **What it counts:** a gauge, set every scheduler step to the length of the
  engine's waiting queue: requests that have reached vLLM and are not yet
  admitted to the running batch. Labels `model_name` and `engine` only.
  `source`: `vllm-project/vllm@4bdc8a7` (`v0.27.0`)
  `vllm/v1/core/sched/scheduler.py#L2521` (`num_waiting_reqs=len(self.waiting)`),
  `vllm/v1/metrics/loggers.py#L468` (labels), `#L503-L508` (definition),
  `#L1116` (`set`).
- **Kind:** queue backlog. **Role it can play:** alarm. **Role it plays here:**
  driver: it is the only trigger that can ask for more than 2 pods.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | fail | A request is counted only while vLLM cannot admit it, so the gauge stays near 0 until a pod's batch is full, then grows with the excess: a dead band, then integration | source |
| P2 coupled to actuation | fail | `max` over pods: adding a pod drains queues and lowers the reading at fixed load | source |
| P3 fixed busyness | fail | 20 waiting requests is a different overload for every prompt and output length; no capacity in the query | config |
| P4 metric type | fail | `max(...)` is intensive under `AverageValue` (`:20`): the HPA computes `ceil(max / 20)` whatever the pod count | config |
| P5 bounds | pass | integrating, but `maxReplicaCount: 4` and one pod per 180 s bound the overshoot | config |
| P7 fails visibly | fail | an empty result reads 0 (`ignoreNullValues` defaults to true, no fallback); see P9 for why it is empty | config |
| P8 lag and noise | pass | lag `15 + 0 + 30 = 45 s` (scrape, no window, polling); one request moves it by `1 / 20 = 5 %` of the threshold | config |
| P9 measures what it claims | fail | `role="prefill"` matches no series: vLLM `v0.27.0` labels its gauges `model_name` and `engine` only (`loggers.py#L468`), and the rendered PodMonitor values add no relabeling (`ms-values.yaml:569-571`). The chart puts `llm-d.ai/role` on the pod (`llm-d-modelservice@6965740:charts/llm-d-modelservice/templates/_helpers.tpl#L69`), but a PodMonitor copies pod labels onto series only through `relabelings` or `podTargetLabels`, and neither is set. Whether anything else in the cluster adds it *needs measurement* | source |

**Failure case (P4):** 4 prefill pods, each with 30 requests waiting. The HPA
computes `ratio = 30 / (20 × 4) = 0.375`, outside the tolerance, and asks for
`ceil(30 / 20) = 2`: a scale-in while every pod is overloaded. To grow from
`R` to `R + 1` pods, the worst pod's queue must exceed `20 × 1.1 × R`: 22, 44,
then 66 waiting requests.

**Failure case (P9):** any load. If the series is empty, the trigger reads 0 and
asks for `ceil(0 / 20) = 0`, clamped to `minReplicaCount: 1`.

#### Trigger `vllm-kv-utilization`

```promql
max(vllm:kv_cache_usage_perc{model_name="Qwen/Qwen3-32B",role="prefill"})
```

`metricType: AverageValue`, `threshold: "0.80"`, `activationThreshold: "0"`
(`27_keda-scaledobjects.yaml:30-41`).

- **What it counts:** a gauge, the engine's KV-cache usage, 1 meaning full; set
  every scheduler step. Same labels. `source`: `vllm-project/vllm@4bdc8a7`
  `vllm/v1/metrics/loggers.py#L561-L566` (definition), `#L1123` (`set`),
  `vllm/v1/core/sched/scheduler.py#L2523`.
- **Kind:** capped saturation, ceiling 1. **Role it can play:** guard. **Role
  it plays here:** guard.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P4 metric type | fail | `max(...)` under `AverageValue` (`:32`): the ask is `ceil(max / 0.80)`, at most `ceil(1 / 0.80) = 2` at any pod count | config |
| P5 bounds | pass | `1 / 0.80 = 1.25 > 1.1`: one pod at KV ≥ `0.80 × 1.1 = 0.88` asks for `ceil(0.88 / 0.80) = 2`. This is the check kv-early fixes; at 0.95, `1 / 0.95 = 1.05` never left the tolerance | config |
| P8 lag | pass | `15 + 0 + 30 = 45 s` | config |
| P9 measures what it claims | fail | same `role` label as above | source |

Whether 0.80 marks the binding resource for the benchmark's mix is not decided
here: *needs measurement*.

**Failure case (P4):** 2 pods, both with full KV caches. The ratio is
`1.0 / (0.80 × 2) = 0.625`, and the ask is `ceil(1.0 / 0.80) = 2`: the guard
can never take the pool past 2, however many pods are full.

### ScaledObject `pd-disaggregation-decode`

Same bounds, `behavior`, defaults and scrape interval
(`27_keda-scaledobjects.yaml:71-112`, `ms-values.yaml:296-301`). Its two
triggers are the prefill ones with `role="decode"` (`:73-97`), so every row and
failure case above applies unchanged. One addition for the KV guard: the
`token-aware.yaml` comment at line 346 records decode KV usage of 0.72–0.98 in
an 8,192-token run while decode was only waiting on prefill's KV transfers. At
0.80 that guard would add decode pods that were not the bottleneck. *Needs
measurement* on this stack.

### Combination

- **Driver in normal operation:** none.
- **Who decides where:** if the `role` label is absent, nobody: both triggers
  read 0 and both pools hold 1 replica. If it is present, in each pool:
  - below saturation, the queue reads about 0 and the KV guard decides; it
    adds one pod once any pod's KV reaches 0.88, and stops at 2;
  - past saturation, the queue decides, one pod per 180 s, each step needing
    `20 × 1.1 × R` waiting requests on the worst pod.
- **Scale-in:** the queue collapses as soon as the new pod drains it, and the
  KV guard asks for at most 2. After the 300 s window the pool loses a pod
  every 300 s, down to 2 or fewer, while load is unchanged.
- **`behavior` and bounds:** one pod per 180 s bounds the swing;
  `maxReplicaCount: 4` caps it.
- **Disaggregated pools:** each pool reads its own engines' gauges, so neither
  carries the other pool's work, apart from decode KV filling while it waits on
  prefill (above).

**Predicted behavior:** without the `role` label, a fixed pool of 1 prefill and
1 decode replica, so a run of kv-early would show the same scaling as a run of
the baseline: none. With the label, each pool sits at 1–2 replicas until it is
already overloaded, then climbs one pod per 3 minutes, and at constant load
cycles between 2 and the overloaded count as the queue drains and refills.

### Needs measurement

1. Do the vLLM series carry a `role` label on the target cluster? Run
   `count by (role) (vllm:num_requests_waiting{model_name="Qwen/Qwen3-32B"})`
   against the Thanos querier. Needed by P9 and P7 of all four triggers.
2. Per-replica capacity of each role at the TTFT and ITL SLOs for the
   benchmark's mix. Needed to size a proportional driver (fix 3).
3. KV-cache usage of each role at that capacity. Needed to tell whether 0.80
   guards the binding resource (KV P5, decode note).
4. Cold start of a prefill and of a decode pod. Needed to judge the 45 s lag
   and the one-pod-per-180 s policy.
5. The KEDA release on the cluster. Needed to settle trap T7.

### Fixes

| Fails | Fix | Effect |
|---|---|---|
| All four triggers, P9 | Select by pod name, as `token-aware.yaml:365` does: `role="prefill"` → `pod=~".*-prefill-.*"`, and the same for decode. Or add to both PodMonitors: `relabelings: [{sourceLabels: [__meta_kubernetes_pod_label_llm_d_ai_role], targetLabel: role}]` | the queries match series; P9 passes on the label |
| KV guard, P4 | `avg(vllm:kv_cache_usage_perc{…})` with `metricType: Value`, or `sum(…)` with `AverageValue`, threshold 0.80 | the ask scales with the pod count; the guard can go past 2 |
| No driver; queue P1–P4 | Add a driver per pool from a counter of offered work at the router or gateway, divided by the measured per-replica capacity, with `AverageValue` and target 0.7. No such counter has been verified on this stack yet; find one before writing the query | a proportional trigger decides in normal operation |
| Queue as alarm | Keep it, as `sum(...)` under `AverageValue`, with a threshold that only fires past saturation | it stays out of the result in normal operation and adds pods fast under overload |
| All drivers, P7 | Change llm-d-benchmark's `27_keda-scaledobjects.yaml.j2` to render `ignoreNullValues` and `fallback`; then set `ignoreNullValues: "false"` and a `fallback` sized for peak | an outage becomes a scaler error, not a reading of 0 |
