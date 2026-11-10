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

To place a gauge in this table, find when a request stops being counted (P9).

## 1.4 Procedure per trigger

For each trigger in the scenario file:

1. Read its query, `metricType`, threshold, `activationThreshold`, and the
   ScaledObject's `minReplicaCount`, `maxReplicaCount`, `pollingInterval`,
   `fallback` and `behavior`. Note the line numbers.
2. Find what the metric counts and when, from its source, and record the
   deployed version of each component it reads (P9).
3. Classify its kind with the table in §1.3, and from the kind, the role it can
   play.
4. Check every property that role must pass (§1.3), using the definitions and
   notes in §1.2. For each property, record pass or fail, the scenario line it
   rests on, the load range where it fails, and an evidence tag: *from the
   config*, *from the metric's source*, or *needs measurement*.
5. If the trigger's role in the configuration differs from the role its kind
   allows (for example a backlog used as the driver), record that as a failure.

## 1.5 Checks on the combination

- **Who decides where.** For each trigger, decide under which conditions it
  sets the replica count: when it wins the maximum, or when the formula's
  branch or weight gives it control. Sound: one proportional driver, with
  guards and alarms that stay out of the result in normal operation. Unsound:
  a guard or alarm that decides in steady state, or a configuration with no
  proportional trigger at all. The maximum of non-proportional signals is
  still non-proportional.
- **Scale-in.** A backlog that collapses after a scale-up can drive the pool
  below what the driver asks. A trigger that stays high after load drops (a
  frozen gauge, a long window) blocks scale-in.
- **`scalingModifiers.formula`.** The formula replaces the maximum, so the
  HPA sees one metric and the per-trigger thresholds and `metricType` no
  longer apply. Apply P1–P4 to the formula's result, check its own `target` and
  `metricType`, and check each operator across the load range:
  - a `min` lets the smallest trigger win, so a backlog that collapses after a
    scale-up drags the pool down;
  - a weighted sum is proportional only if every term is;
  - a conditional switches between laws, and each branch must pass on the
    range where it is taken.
- **Disaggregated pools.** Each pool's trigger must measure that pool's work. On
  prefill/decode, `inflight_tokens` on the prefill pool counts until the first
  token, which comes from the decode pool, so it carries the KV transfer and
  decode delays.
- **`behavior`.** Slow scale-up policies bound the swing of a non-proportional
  signal but do not stop it oscillating. An exploding signal pins at
  `maxReplicaCount`; that is harmless only if the true need is already at the
  cap.

## 1.6 Verdict

Record, in the variant's header comment or the run report:

1. per trigger: kind, role, each required property with its result, scenario
   line and evidence tag, and the estimated lag;
2. the combination: which trigger drives in normal operation, which win when,
   and whether that is sound;
3. predicted behavior in one or two sentences;
4. what needs measurement (per-replica capacity, mix range, cold start) — the
   questions for the next step;
5. fixes, each tied to a failed check.

A configuration with no trigger that passes P1–P4 in its intended load range is
**rejected** at this step; running it measures a known failure.

## 1.7 Worked example: `staging/pd-disaggregation/baseline.yaml`

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
