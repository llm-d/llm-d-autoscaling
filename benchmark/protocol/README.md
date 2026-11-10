# Evaluation protocol for autoscaling configurations

This protocol lists the steps for evaluating a KEDA autoscaling configuration (a
`keda:` block in `benchmark/config/scenarios/`) before and after it runs on a
cluster. Each step either rejects the configuration or hands the next step a
list of questions to settle.

| Step | What it decides | Needs a cluster? |
|---|---|---|
| [1. Mathematical assessment](#step-1-mathematical-assessment) | whether each trigger, and their combination, can compute the right replica count at all | no |

Later steps (calibration, benchmark runs, efficiency against an oracle) will be
added here.

## Step 1: Mathematical assessment

Judge the configuration from its text alone: the PromQL queries, `metricType`,
thresholds, replica bounds and `behavior`. This step predicts behavior; it does
not measure it. Mark every conclusion as *from the config*, *from the metric's
source*, or *needs measurement*.

### 1.1 What the HPA computes

KEDA serves each trigger to the HPA as an external metric. For a trigger whose
query returns the total `U` and whose threshold is `T`:

```
AverageValue:  desired = ceil(U / T)                 (pod count cancels)
Value:         desired = ceil(N_ready × U / T)       (N_ready = Running and Ready pods)
```

The HPA does nothing while the ratio of current to target lies within 10 %
(tolerance `τ = 0.1`). With several triggers it takes the **maximum** of their
desired counts. External metrics get none of the per-pod corrections resource
metrics get: pods that are still starting are not set aside. The signal itself
has to behave.

**Load** below means replicas' worth of work at the SLO: arrival rate divided
by the capacity of one replica for the current traffic mix (prompt length,
output length, prefix sharing). It is not a token rate or a request rate.

### 1.2 Properties of a driver signal

The trigger that sets the replica count in normal operation (the **driver**)
must have all of the following. Symbols: `λ` load, `R` replicas, `s` the signal,
`M` the traffic mix, `μ_SLO` the capacity of one replica at the SLO.

| # | Property | Definition | Passes when |
|---|---|---|---|
| P1 | **Proportional to load** | elasticity `ε = d ln s / d ln λ` over `[0.2, 1.0]·μ_SLO` | `ε ≈ 1`: double the load, double the signal. No dead band (`ε ≈ 0`) and no explosion (`ε ≫ 1`) |
| P2 | **Coupled to actuation** | `η = d ln s_per_pod / d ln R` at fixed `λ` | `η = −1`: adding a pod spreads the same total over more pods. The total `U` does not change with `R` |
| P3 | **Target means a fixed busyness** | mix sensitivity `V = max_M s*(M) / min_M s*(M)`, where `s*(M)` is the signal at `λ = μ_SLO(M)` | `V ≈ 1`: one threshold is the same fraction of capacity for every mix, and it leaves spare room for bursts and cold starts |
| P4 | **Metric type matches aggregation** | extensive (`sum`, grows with `R` at fixed per-pod load) or intensive (`max`, `avg`, ratio, quantile) | extensive → `AverageValue`; intensive → `Value`; an absolute replica count from `scalingModifiers` → `AverageValue`, target `1` |
| P5 | **Unbounded across the needed range** | saturation point `λ_sat = inf{λ : ε < 0.1}` | `λ_sat > μ_SLO`, or else the cap is the binding resource and the trigger is a guard, not a driver |
| P6 | **Defined at zero replicas** | value when `R = 0`, `λ > 0` | informative (`s > 0`), if `minReplicaCount` is 0 |
| P7 | **Fails visibly** | value when the source is down (scrape failure, EPP restart, wrong label) | the scaler errors and `fallback` applies; it does not read 0 |
| P8 | **Bounded lag and noise** | lag ≈ scrape interval + ½ `rate` window + `pollingInterval`; noise = sample-to-sample swing at steady load against `T` | lag well below cold start; noise well below the 10 % tolerance |

P1–P3 are the core of the assessment. A queue or prefill backlog fails all
three: by Little's law, per pod it equals utilization × time to first token, and
time to first token rises without limit near saturation.

### 1.3 Classify each trigger

The kind of signal predicts which properties it can have, and therefore which
role it can play.

| Kind | Looks like | P1–P3 | Role |
|---|---|---|---|
| Offered work rate, counted before any queue | `rate()` of a gateway or router counter at arrival, optionally token-weighted, ÷ per-replica capacity | pass at a fixed mix; token weighting narrows `V` (2.6 against 7.6 for request rate) but does not reach 1 | **driver** |
| Completed work rate | `rate()` of a completion or served-tokens counter | flat at capacity: equals supply under overload | not a driver |
| Queue or prefill backlog | waiting requests, router queue, `inflight_tokens` (send to first token), even when divided by a throughput | fail: dead band, then explosion, then a target that means congestion | **alarm** (fast scale-up, alert) |
| Requests in progress (send to last token) | running or in-flight request gauge | close to P1 below saturation (`ε` 1.0–1.24); `V` up to 31 across output lengths | driver only for a stable output-length mix |
| Capped saturation | KV-cache usage, slot use, pool saturation | only below the cap | **guard** for the resource it measures |
| Latency outcome | `histogram_quantile(...)` of TTFT, ITL | fail: lags, flat until steep | **scale-in veto** |

For a gauge, find when a request stops being counted (admission, first token,
last token): that decides whether it is a backlog or a count in progress. The
metric name often does not say; read its source.

### 1.4 Checks per trigger

Work through these for each trigger, citing the line of the scenario file.

1. **Kind and role** (§1.3). A driver must be a proportional kind.
2. **P1–P3**, and over which load range each fails.
3. **P4, metric type.** A mismatch is off by a factor of the pod count.
   `Value` on a sum is positive feedback (more pods, larger sum, more pods).
   `AverageValue` on a `max` caps the ask at `ceil(s_max / T)` whatever the pool
   size, and can scale in while the pool sits at target.
4. **Capacity constant.** If the query or threshold divides by a per-replica
   capacity, where did it come from? An idle-replica peak or a rate the pool
   once served overstated real capacity by 1.2–6× in measurements. *Needs
   measurement.*
5. **Capped signals (P5).** A per-pod signal bounded by `s_max` can scale up
   only if `T < s_max / (1 + τ) = s_max / 1.1`. Then it ramps geometrically, by
   at most `s_max / T` per cold start. A KV-cache threshold of 0.95 never scales
   up.
6. **Global integrating signals** (router queue, total waiting) keep growing
   while new pods start: overshoot ≈ growth rate × cold start ÷ `T`, bounded only
   by `maxReplicaCount` and the scale-up policy.
7. **Zero replicas (P6).** Pod-exported `vllm:*` series vanish with their pods,
   and KEDA's `ignoreNullValues: true` (default) turns that into 0: the pool
   never wakes. Check `activationThreshold` too.
8. **Missing data (P7).** `or vector(0)` and `ignoreNullValues: true` turn an
   outage into a reading of 0. The HPA then scales toward `minReplicaCount` and
   `fallback` never fires. Acceptable on guards and alarms, not on the driver.
9. **Lag and noise (P8).** An instant gauge that moves by a whole request's
   work per request is noisy: estimate one request's step against `T` (for
   example 8,192 tokens ÷ 35,594 tokens/s ≈ 0.23 s against `T = 1.5 s`, 15 %).
   Noise restarts the scale-down stabilization window.
10. **Known traps.**
    - vLLM exports no arrival counter: a vLLM-side "demand" query reads
      admitted or completed work.
    - `llm_d_epp_average_*` gauges freeze at their last value when no endpoint
      has fresh metrics, so a trigger on them never scales in.
    - `llm_d_epp_inflight_tokens` can keep a negative residue after a scale-in;
      clamp each series at 0 before summing.
    - A catch-all gateway route counts every proxied path, `/metrics` scrapes
      included, as demand.
    - `vllm:num_requests_waiting` with EPP flow control on is a bounded
      sawtooth, not the true backlog.

### 1.5 Checks on the combination

- **Who wins where.** For each trigger, decide under which conditions it
  produces the maximum. Sound: one proportional driver, with guards and alarms
  that stay below it in normal operation. Unsound: a guard or alarm that wins in
  steady state, or a configuration with no proportional trigger at all. The
  maximum of non-proportional signals is still non-proportional.
- **Scale-in.** A backlog that collapses after a scale-up can drive the pool
  below what the driver asks. A trigger that stays high after load drops (a
  frozen gauge, a long window) blocks scale-in.
- **`scalingModifiers.formula`.** Apply P1–P4 to the formula's result and check
  each operator across the load range.
- **Disaggregated pools.** Each pool's trigger must measure that pool's work. On
  prefill/decode, `inflight_tokens` on the prefill pool counts until the first
  token, which comes from the decode pool, so it carries the KV transfer and
  decode delays.
- **`behavior`.** Slow scale-up policies bound the swing of a non-proportional
  signal but do not stop it oscillating. An exploding signal pins at
  `maxReplicaCount`; that is harmless only if the true need is already at the
  cap.

### 1.6 Verdict

Record, in the variant's header comment or the run report:

1. per trigger: kind, role, P1–P8 with their evidence tag, estimated lag;
2. the combination: which trigger drives in normal operation, which win when,
   and whether that is sound;
3. predicted behavior in one or two sentences;
4. what needs measurement (per-replica capacity, mix range, cold start) — the
   questions for the next step;
5. fixes, each tied to a failed check.

A configuration with no trigger that passes P1–P4 in its intended load range is
**rejected** at this step; running it measures a known failure.

### 1.7 Worked example: `staging/pd-disaggregation/baseline.yaml`

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
