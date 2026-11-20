# Step 3: Evaluation in blis-signal-lab

Part of the [evaluation protocol](README.md).

Step 3 runs the [evaluation run set](run-set.md), the same runs, loads,
reports and gate as [step 5](5-gpu-evaluation.md), on a laptop instead of a
GPU cluster. The plant is
[blis-signal-lab](https://github.com/vishakha-ramani/blis-signal-lab), a fork of
the BLIS inference simulator that runs KEDA's scale loop and a Kubernetes HPA
in its event loop, for a single pool or for each role of a prefill/decode
(P/D) deployment. A run takes seconds to minutes of CPU time.

Step 3 tells what the configuration does with the run set's loads before any
GPU is held: whether the pool follows the staircase and the mix shift, what it
does at the cap, after a burst and on the way down. It also rehearses step 5:
the knees, the stage lengths, a capacity-derived threshold and every report
are produced the same way, so step 5 starts with a tested procedure and a
list of what to look for.

Every number step 3 reports is a simulator number, for the scenario BLIS
priced (§3.3). It is not a measurement of the GPU plant, and step 3 never
takes μ_SLO or any rate from step 5, or gives them to it.

Step 3 does not depend on steps 1 and 2. When their verdicts exist, the
summary links them, and a run that shows a failure they predicted says so.

## 3.1 Prerequisites

| What | Why | How to check |
|---|---|---|
| blis-signal-lab at `25f2dfb` (branch `signals-hpa`), built | KEDA's scale loop, the HPA per pool or per role, the signal recorder; §3.2 describes this commit | `git -C <lab> rev-parse HEAD`; `GOTOOLCHAIN=auto go build -o blis main.go` in the checkout (go.mod pins Go 1.27.2) |
| llm-d-benchmark, as for step 5 | `standup --dry-run` renders the ScaledObjects the policy is translated from, and the PodMonitor's scrape interval; no cluster is used | the `run-benchmark` skill's setup |
| a single-pool or P/D scenario | blis-signal-lab runs no HPA on E/P/D, on shared prefill-decode pods, or on a data-parallel P/D pool | the scenario's `keda` block scales one Deployment, or one prefill and one decode Deployment |
| blis has the scenario's model | the model is never changed (§3.3); without it, step 3 reports missing model and stops | `<lab>/blis catalog resolve --model <Hugging Face id> --json` gives an `identity` |
| a translation of every trigger (§3.4) | blis reads signals, not PromQL | one entry per trigger in `terms.json`, each classed exact or approximated |

## 3.2 What the simulator models

From the fork's `autoscaling-kit/README.md` and code at `25f2dfb`, and
`llm-d-autoscaling-research`'s description of the fork
(`findings/raw/blis-signal-lab/blis-signal-lab.md`) and sanity check
(`findings/derived/blis-signal-lab/blis-signal-lab-sanity.md`), which
cover the HPA at `727e998`.

**What it models as KEDA and Kubernetes do.**

- **The HPA.** Each sync (15 s) reads every trigger, proposes per trigger by
  `AverageValue` (over the pods that exist, Pending included) or `Value`
  with the tolerance, takes the largest proposal, and applies either the
  autoscaling/v2 `behavior` (stabilization windows, Pods/Percent policies,
  per-direction tolerance) or, when the ScaledObject sets no `behavior`, the
  legacy path KEDA's default HPA takes. Replaying the sanity check's 789 syncs
  through a reference model of the Kubernetes controller gave the simulator's
  count at every sync.
- **KEDA's scale loop.** Every `pollingInterval`, activation against each
  trigger's `activationThreshold` (or the formula's `activationTarget`),
  scale to and from zero, `cooldownPeriod`, `initialCooldownPeriod`,
  `idleReplicaCount`, `useCachedMetrics`, `fallback` (every `behavior`), and
  `scalingModifiers` (an expr-lang subset: arithmetic, comparisons, `?:`,
  `??`, `min`, `max`, `abs`, `ceil`, `floor`, `round`).
- **The metrics pipeline.** The recorder scrapes every pod at the scrape
  interval; `metrics_delay_s` is the extra time from a scrape to the HPA
  reading it.
- **P/D.** One KEDA + HPA per role, each with its own triggers, behavior and
  decision log. The scenario's pools are the GPU budget: a pod beyond a
  pool's size stays Pending. A draining prefill pod waits until decode has
  pulled its KV, a draining decode pod finishes its handoffs.
- **Pods.** New pods go through Pending, Loading
  (`instance_lifecycle.loading_delay`, the cold start) and WarmingUp to
  Ready; removal takes Pending, then not-ready, then the newest Ready pods,
  and a removed Ready pod drains.
- Runs are deterministic: the same inputs and seed give byte-identical
  outputs.

**What it does not model, and what that changes in the run set:**

| Gap | Effect on step 3 |
|---|---|
| a signal is a linear sum of terms (`coef × function(aggregation(metric))`) over the recorder's metrics, not PromQL; counters and latencies are pool-wide (except `prefill_prompt_tokens`), only per-pod gauges and pod counts take a `role` | each trigger is translated by hand (§3.4); a product or ratio of series is exact only through a formula, and a role-filtered counter is approximated |
| `rate()` is the slope between the first and last scrape in the window, with no extrapolation; quantiles are exact over the window's samples, and TTFT counts from gateway arrival | a trigger near its threshold can propose differently from Prometheus's value |
| scrapes and syncs fall on the same instants unless `sync_offset_s` is set | the dead time after a load change is the shortest the scrape and sync phases allow; research E2 puts the cluster's median at `S/2 + H/2 + 0.5 s` |
| at 0 pods the router rejects every request until a pod is Ready; llm-d with flow control holds them for `noEndpointRequestTTL` (60 s) | in a scale from zero (run 7, and any active run with `minReplicaCount: 0`) every request of the cold start fails as `routing_rejected` |
| no pool starts at 0 pods | `blis_run.py` shifts the load by a quiet lead-in so KEDA scales the pool to zero first (§3.3) |
| a single pool has unlimited GPUs (no `node_pools`) | a single pool is never Pending for lack of a GPU; `maxReplicaCount` is its only cap |
| a draining pod finishes all its work, with no deadline | scale-down never fails a request: `requests_errored_at_end` is 0 by construction in runs 8 to 10 |
| a metrics outage fails every query at once; no partial failure | `fallback` is exercised only by a whole outage |
| the client timeout is not applied to a single-shot client with lifecycle windows ([inference-sim#2291](https://github.com/inference-sim/inference-sim/issues/2291)) | requests past the timeout keep running and hold engine capacity that a cluster would free; `blis_run.py` counts each as the timeout error the client would have raised, so A and the error counts mean what they mean in step 5, but recovery after overload is slower than on a cluster |
| the cost model prices each step; there is no GPU | μ_SLO, latencies and replica-seconds are BLIS's, for the scenario it priced |

## 3.3 The plant

**The model** is the scenario's, never another. `blis catalog resolve --model
<Hugging Face id>` names blis's entry for it; when it names none, step 3
stops. The summary reports **missing model**, names the model, the lab commit
and the catalog searched, and marks every run *not run*.

**The scenario.** A BLIS scenario states the model, GPU, pools, parallelism
and engine.

- **A single pool:** the vendored scenario
  (`<lab>/testdata/kernel/aisimulate/<model>-<gpu>-<dtype>-<engine>-tp<N>.yaml`)
  with the scenario's model that matches its backend overlay.
- **P/D:** the vendored scenarios are all colocated, so the P/D scenario is
  written for the evaluation, as the kit's
  `autoscaling-kit/scenarios/glm-5-h200-pd-autoscale.yaml` is: the vendored
  scenario's model, hardware and engine settings, split into a `prefill` and
  a `decode` pool with the overlay's parallelism per role, and a router
  config with the scenario's P/D decider (the kit's `router/pd-always.yaml`
  or `pd-prefix.yaml`). Each pool holds at least the role's
  `maxReplicaCount` and any pin, or blis refuses the run. The scenario and
  its router config are committed with the evaluation.

If the scenario's GPU type or parallelism differs from the overlay's, every
header names the difference.

**The cold start** is the policy's `loading_delay`. Use the plant's measured
one when there is one (step 5's run 2, or an earlier GPU run of the overlay),
otherwise 120 s, and record its source. The §2 lengths of the run set use it.

**The metrics pipeline.** The scrape interval is the rendered PodMonitor's
(`plan/<stack>/18_podmonitor.yaml`), and the HPA syncs every 15 s.
`metrics_delay_s` is what the plant adds beyond those two loops: a recording
rule's evaluation interval when a trigger reads one, or the plant's measured
delay. It is 0 when the triggers query raw series, as research E2 found (the
query itself takes about 0.5 s). Record the value and its source.

**The arms**, per pool:

- **pinned** (runs 1 and 6): `min_replicas = max_replicas = N`, starting from
  N Ready pods. The HPA still reads and logs every trigger but cannot move the
  pool, as KEDA's `paused-replicas` holds it on a cluster. For P/D, each
  role's knee pins it at 1 and the other role large enough not to be the
  bottleneck (run-set.md §1).
- **active** (runs 2 to 5 and 7 to 10): the policy as translated, starting
  from `minReplicaCount` Ready pods. With `minReplicaCount: 0`, the pool
  starts at 1 pod and the load is shifted by a quiet lead-in of
  `pollingInterval + initialCooldownPeriod + 30 s`, so KEDA scales it to zero
  before the first request; `blis-run.json` records the Ready pods at the
  load's start, and every series is placed on the load's clock.
- **static** (P/D): a role no ScaledObject scales is held at its replica count.

A run starts from a new simulation, so there is no drain or wait between
runs.

**Agentic sessions** are a blis multi-turn client: a shared 4,096-token system
prompt (one prefix group), 10 rounds per session whose context accumulates,
and a 2 s pause between rounds. They have no per-conversation system prompt,
no subagent fan-out, and per-turn lengths truncated (input ≤ 2,400, output
≤ 640 tokens) so the context stays under 32k tokens. The rate is sessions per
second: the run set's request rate divided by 10. The report states this.

## 3.4 Translating the triggers

`so_to_policy.py` carries over every field that translates mechanically: the
replica bounds, `pollingInterval`, the cooldowns, `idleReplicaCount`,
`fallback`, `scalingModifiers`, the HPA `behavior` or its absence, and each
trigger's threshold, `metricType`, `activationThreshold` and
`useCachedMetrics`. With several ScaledObjects it gives each its role
(`hpa_by_role`). Each trigger's query is written by hand as blis terms in
`terms.json`, using the signal recorder's metrics (autoscaling-kit README,
"Metrics"):

| PromQL series | blis metric | Fidelity |
|---|---|---|
| `vllm:num_requests_waiting`, `vllm:num_requests_running` | `waiting`, `running` (per-pod gauges; `sum`, `avg`, `max`, `min` across Ready pods) | exact |
| `vllm:kv_cache_usage_perc` | `kv_usage` | exact |
| a per-pod gauge or pod count filtered by role (`{role="prefill"}`) | the same metric with `role: prefill` or `role: decode` | exact |
| EPP in-flight requests, in-flight prompt tokens | `inflight_requests`, `inflight_prompt_tokens` | exact; prompt tokens include cached ones |
| `rate(llm_d_epp_request_total)` plus the shed requests of `llm_d_epp_request_error_total` | `arrivals`, `function: rate` | exact |
| `rate(llm_d_epp_request_error_total)` | `rejections`, `function: rate` | exact |
| `rate(vllm:request_success_total)` | `completions`, `function: rate` | exact for a single pool; pool-wide for P/D |
| `rate(vllm:generation_tokens_total)` | `generated_tokens`, `function: rate` | approximated: blis counts a request's tokens when it completes, vLLM at each step |
| `rate(vllm:prompt_tokens_total)`, `rate(vllm:prefix_cache_queries_total)` | `completed_prompt_tokens` (carried), or `arrived_prompt_tokens` (offered) | approximated: vLLM counts at admission to the engine |
| the same series filtered to the prefill role (`{role="prefill"}`) | `prefill_prompt_tokens`, `function: rate`: the prompt tokens of requests dispatched to a prefill pod; a request the decider keeps on decode adds nothing | approximated: counted at dispatch, not at admission, and cached tokens are not subtracted |
| `rate(vllm:num_preemptions_total)` | `preemptions`, `function: rate` | exact for a single pool; pool-wide for P/D |
| `histogram_quantile` of TTFT, ITL, e2e | `ttft`, `itl`, `e2e`, `function: quantile` | approximated: exact quantile over the window, not over buckets |
| `kube_deployment_status_replicas_ready` | `ready_pods` (with `role` for P/D) | exact |

A window `[1m]` is `window_s: 60`. Guards that only keep a series alive with
no pods or no traffic (`or vector(<default>)`, `clamp_min`) are dropped: blis
rates are 0, not absent, without traffic.

**A product or ratio of series** is written as helper signals and an
expression over them. `so_to_policy.py` turns them into helper triggers and a
`scalingModifiers` formula whose target, metric type and activation are the
trigger's own, so the HPA reads the same composite value KEDA would. This
needs a ScaledObject with one trigger and no `scalingModifiers` of its own;
otherwise write the nearest linear sum and class the trigger approximated.
`npd-fcoff`'s offered work rate,
`(D1 + D15) × (L_p + c × L_out)` with `L_p` and `L_out` the mean prompt and
output lengths of completed requests, becomes

```json
{"epp-offered-work-rate": {
  "helpers": {
    "d":    [{"metric": "arrivals", "function": "rate", "window_s": 60}],
    "lp":   [{"metric": "completed_prompt_tokens", "function": "rate", "window_s": 60}],
    "lout": [{"metric": "generated_tokens", "function": "rate", "window_s": 60}],
    "s":    [{"metric": "completions", "function": "rate", "window_s": 60}]},
  "expr": "d * ((lp + 3.3 * lout) / max(s, 0.001))"}}
```

Its structure is exact; it is classed approximated for the counter timing of
`lp` and `lout` (table above).

A trigger with no translation at all (a non-Prometheus scaler, a series blis
does not record) stops step 3 for the configuration; the summary says which
and why.

The translation table goes in the summary: each trigger's query, its terms,
the fidelity class, and for an approximated one, where it diverges.

## 3.5 Procedure

The [`blis-evaluation` skill](../../.claude/skills/blis-evaluation/SKILL.md)
runs these steps in this order, with the commands.

1. **Build and check** the lab (§3.1).
2. **Render** the scenario with `standup --dry-run` and read the
   ScaledObjects: placeholders resolved, the thresholds intended.
3. **Check the model** (§3.3). If blis has none, write the summary with
   **missing model** and every run *not run*, commit it, and stop.
4. **Write the plant**: the BLIS scenario (for P/D, written with its router
   config), the cold start and the metrics pipeline (§3.3); translate the
   triggers (§3.4); write the policy; and create the summary with every run
   *pending* (run-set.md §3).
5. **Gate the translation on the first knee.** Its HPA logs show every
   trigger off 0 while load was sent, and each exact trigger at the value its
   query gives for that load (a rate trigger near the offered rate, a gauge
   within its range). Otherwise fix `terms.json` and rerun the knee.
6. **For each run**, in the run set's order: run it with the arm (§3.3),
   gate it (run-set.md §4 and §3.7), report it (run-set.md §3 and §3.6) and
   commit before the next. A run that fails the gate has a tool or input
   error, not an infrastructure fault: fix it, rerun, and list the failed
   attempt with its reason.
7. **Close the summary** when the last run is reported, or when the
   evaluation stops early.

## 3.6 What the reports add

Each run's directory (run-set.md §3) also holds:

```text
policy.json                 # the blis policy as run (arm applied)
workload.yaml               # the blis workload as run (shifted by any lead-in)
signals-hpa[-<role>].csv    # the decision log: each HPA sync and KEDA poll, trigger values, proposals
signals-summary.json        # blis's pod-seconds and goodput summary
blis-run.json               # the clock, the arm, the fates of every request, the inputs
command.txt                 # the blis command line
```

`triggers/<trigger>.json` (for P/D, `triggers/<role>/<trigger>.json`) are the
values the HPA read at each sync, from the decision log, in the format of a
Prometheus range query; with `scalingModifiers`, `composite.json` is the
metric the HPA used. While a pool is at 0 pods the HPA does not run, so its
series has no points. For P/D, each pool gets its own per-stage table and
figure (`--deployment <the role's target>`). The raw results (blis's Result
with every request, and the llm-d-benchmark-shaped `results/`) stay under
`benchmark/results/step3-<scenario>-<YYYYMMDD>/<run>/`. `terms.json`, the
policy, `scaledobjects.json`, a written P/D scenario and the summary's
translation table are committed once, at the evaluation's root directory.

The header table adds the blis-signal-lab commit, the BLIS scenario (and any
GPU type or parallelism it does not share with the overlay), the cold start,
the scrape interval and metrics delay with their sources, the seed, and the
gaps of §3.2 the run is exposed to. The figure's caption says that the
trigger panel is the HPA's own reading, not a Prometheus query.

## 3.7 Gate after each run, in the simulator

On top of run-set.md §4:

| Check | Source | Fails when |
|---|---|---|
| blis ran clean | `stderr.txt` | any `level=error` or `fatal` line (`engine assumption` and deprecation warnings are expected) |
| every request has a fate | `blis-run.json` `fates` | the fates do not sum to `generated` |
| the HPAs ran | `signals-hpa[-<role>].csv` | no row after t = 0 for a scaled pool, or a trigger column missing |
| the arm held | `blis-run.json` `ready_at_load_start` | a pool with `minReplicaCount: 0` was not at 0 when the load started |
| repeatable | the decision logs' sha256 in `blis-run.json` | the first run, rerun with the same command, gives another sha256 |

## 3.8 What step 3 does not settle

- The GPU plant: capacities, cold start, latencies and costs are BLIS's.
  Comparing step 3 with step 5 for the same scenario measures the simulator,
  not the configuration.
- A cluster's metrics-pipeline jitter, flow control's holding of requests at
  0 pods, partial metric failures, and the gaps of §3.2.
- Approximated triggers: a run that depends on where a translation diverges
  shows the approximation, and its report says so.
- Variance: each run is one seed. blis makes repeats cheap; a claim that needs
  them reruns with `--seed` and reports each seed.
