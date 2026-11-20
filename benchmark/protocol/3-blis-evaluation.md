# Step 3: Evaluation in blis-signal-lab

Part of the [evaluation protocol](README.md).

Step 3 runs the [evaluation run set](run-set.md), the same runs, loads,
reports and gate as [step 5](5-gpu-evaluation.md), on a laptop instead of a
GPU cluster. The plant is
[blis-signal-lab](https://github.com/vishakha-ramani/blis-signal-lab), a fork of
the BLIS inference simulator with a KEDA + HPA autoscaler in its event loop.
A run takes seconds to minutes of CPU time.

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
| blis-signal-lab at `727e998` (branch `signals-hpa`), built | the KEDA + HPA loop and the signal recorder; §3.2 describes this commit | `git -C <lab> rev-parse HEAD`; `GOTOOLCHAIN=auto go build -o blis main.go` in the checkout (go.mod pins Go 1.27.2) |
| llm-d-benchmark, as for step 5 | `standup --dry-run` renders the ScaledObject the policy is translated from; no cluster is used | the `run-benchmark` skill's setup |
| a single-pool scenario | blis-signal-lab runs no HPA on a P/D or E/P/D deployment | the scenario's `keda` block scales one Deployment |
| a BLIS scenario with the scenario's model | the model is never changed (§3.3); without one, step 3 reports missing model and stops | `grep -l '^model: <name>$' <lab>/testdata/kernel/aisimulate/*.yaml`, with `<name>` the Hugging Face id without its organization, lowercased (`Qwen/Qwen3-8B` is `qwen3-8b`) |
| a translation of every trigger (§3.4) | blis reads signals, not PromQL | one entry per trigger in `terms.json`, each classed exact or approximated |

## 3.2 What the simulator models

From `llm-d-autoscaling-research`'s description of the fork
(`findings/raw/blis-signal-lab/blis-signal-lab.md`) and its sanity check
(`findings/derived/blis-signal-lab/blis-signal-lab-sanity.md`), at `727e998`,
and the code where noted.

**What it models as Kubernetes does.** Each HPA sync (15 s) reads every
trigger, proposes per trigger by `AverageValue` or `Value` with the 10 %
tolerance, takes the largest proposal, applies the stabilization windows and
the Pods/Percent policies, and clamps to `[min, max]`. Replaying all 789 syncs
of the sanity check through a reference model of the Kubernetes controller
gave the simulator's count at every sync. New pods go through Pending,
Loading (`instance_lifecycle.loading_delay`, the cold start) and WarmingUp to
Ready; removal takes Pending, then not-ready, then the newest Ready pods, and a
removed Ready pod drains. Runs are deterministic: the same inputs and seed
give byte-identical outputs.

**What it does not model, and what that changes in the run set:**

| Gap | Effect on step 3 |
|---|---|
| KEDA is only the ScaledObject-to-HPA translation: no `pollingInterval`, activation, `idleReplicaCount`, `fallback` or `useCachedMetrics` | run 7 (scale from zero) is *not run*; `minReplicaCount: 0` runs as 1 |
| no `scalingModifiers` formula; triggers combine only as the HPA's max | a configuration with `scalingModifiers` is not evaluated |
| signals are linear sums of terms (`coef × function(aggregation(metric))`) over the recorder's metrics, not PromQL | each trigger is translated by hand (§3.4); a product or ratio of series is approximated |
| always the HPA `behavior` path | a ScaledObject without `advanced.horizontalPodAutoscalerConfig.behavior` gets the legacy path on a cluster and the behavior path here: say so in the summary; the sanity check found 7 pods against 6 at one steady load |
| no metrics-pipeline delay: the HPA reads the scrape taken at its own sync | the dead time after a load change is a lower bound of the cluster's |
| `rate()` is the slope between the first and last scrape in the window, with no extrapolation; quantiles are exact over the window's samples, and TTFT counts from gateway arrival | a trigger near its threshold can propose differently from Prometheus's value |
| GPUs are unlimited (no `node_pools`) | pods are never Pending for lack of a GPU; `maxReplicaCount` is the only cap |
| a draining pod finishes all its work, with no deadline | scale-down never fails a request: `requests_errored_at_end` is 0 by construction in runs 8 to 10 |
| the client timeout is not applied to a single-shot client with lifecycle windows (`sim/workload/generator.go`, the windowed single-shot path sets `Deadline: 0`) | requests past the timeout keep running and hold engine capacity that a cluster would free; `blis_run.py` counts each as the timeout error the client would have raised, so A and the error counts mean what they mean in step 5, but recovery after overload is slower than on a cluster |
| the cost model prices each step; there is no GPU | μ_SLO, latencies and replica-seconds are BLIS's, for the scenario it priced |

## 3.3 The plant

**The scenario.** A BLIS scenario
(`<lab>/testdata/kernel/aisimulate/<model>-<gpu>-<dtype>-<engine>-tp<N>.yaml`)
states the model, GPU, parallelism and engine. Use the one with the
scenario's model that matches its backend overlay. The model is never
changed: when no BLIS scenario has the scenario's model (blis has no
Qwen3-8B, for example), step 3 stops. The summary reports **missing model**,
names the model, the lab commit and the catalog searched, and marks every run
*not run*. If an entry has the model but not the overlay's GPU type or
parallelism, every header names the difference.

**The cold start** is the policy's `loading_delay`. Use the plant's measured
one when there is one (step 5's run 2, or an earlier GPU run of the overlay),
otherwise 120 s, and record its source. The §2 lengths of the run set use it.

**The scrape and sync** are 15 s each, Kubernetes' default sync period.

**The arms.**

- **pinned** (runs 1 and 6): `min_replicas = max_replicas = N`, starting from
  N Ready pods. The HPA still reads and logs every trigger but cannot move the
  pool, as KEDA's `paused-replicas` holds it on a cluster.
- **active** (runs 2 to 5 and 8 to 10): the policy as translated, starting
  from `minReplicaCount` Ready pods, with every series empty.

A run starts from a new simulation, so there is no drain or wait between
runs.

**Agentic sessions** are a blis multi-turn client: a shared 4,096-token system
prompt (one prefix group), 10 rounds per session whose context accumulates,
and a 2 s pause between rounds. They have no per-conversation system prompt,
no subagent fan-out, and per-turn lengths truncated (input ≤ 2,400, output
≤ 640 tokens) so the context stays under 32k tokens. The rate is sessions per
second: the run set's request rate divided by 10. The report states this.

## 3.4 Translating the triggers

`so_to_policy.py` carries over what translates mechanically: `minReplicaCount`,
`maxReplicaCount`, each trigger's threshold and `metricType`, and the HPA
behavior. Each trigger's query is written by hand as blis terms in
`terms.json`, using the signal recorder's metrics (autoscaling-kit README,
"Metrics"):

| PromQL series | blis metric | Fidelity |
|---|---|---|
| `vllm:num_requests_waiting`, `vllm:num_requests_running` | `waiting`, `running` (per-pod gauges; `sum`, `avg`, `max`, `min` across Ready pods) | exact |
| `vllm:kv_cache_usage_perc` | `kv_usage` | exact |
| EPP in-flight requests, in-flight prompt tokens | `inflight_requests`, `inflight_prompt_tokens` | exact; prompt tokens include cached ones |
| `rate(llm_d_epp_request_total)` plus the shed requests of `llm_d_epp_request_error_total` | `arrivals`, `function: rate` | exact |
| `rate(llm_d_epp_request_error_total)` | `rejections`, `function: rate` | exact |
| `rate(vllm:request_success_total)` | `completions`, `function: rate` | exact |
| `rate(vllm:generation_tokens_total)` | `generated_tokens`, `function: rate` | approximated: blis counts a request's tokens when it completes, vLLM at each step |
| `rate(vllm:prompt_tokens_total)`, `rate(vllm:prefix_cache_queries_total)` | `completed_prompt_tokens` (carried), or `arrived_prompt_tokens` (offered) | approximated: vLLM counts at admission to the engine |
| `rate(vllm:num_preemptions_total)` | `preemptions`, `function: rate` | exact |
| `histogram_quantile` of TTFT, ITL, e2e | `ttft`, `itl`, `e2e`, `function: quantile` | approximated: exact quantile over the window, not over buckets |
| `kube_deployment_status_replicas_ready` | `ready_pods` | exact |

A window `[1m]` is `window_s: 60`. Guards that only keep a series alive with
no pods or no traffic (`or vector(<default>)`, `clamp_min`) are dropped: blis
has at least one pod, and its rates are 0, not absent, without traffic.

A product or ratio of series has no blis form. Write the nearest linear sum,
class the trigger **approximated**, and state where it diverges and which runs
that affects. `npd-fcoff`'s offered work rate,
`(D1 + D15) × (L_p + c × L_out)` with `L_p` and `L_out` the mean prompt and
output lengths of completed requests, becomes

```json
{"epp-offered-work-rate": [
  {"metric": "arrived_prompt_tokens", "function": "rate", "window_s": 60},
  {"metric": "generated_tokens", "function": "rate", "window_s": 60, "coef": 3.3}]}
```

The first term is the offered prompt-token rate, the trigger's first product
when the mix is steady. The second is the carried output-token rate: it
equals the trigger's second product while the pool keeps up, and falls short
of it when completions fall behind arrivals (runs 1, 4b and the high knee
stages), where the cluster's trigger keeps rising.

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
   ScaledObject: placeholders resolved, the thresholds intended.
3. **Choose the BLIS scenario** with the scenario's model (§3.3). If there
   is none, write the summary with **missing model** and every run *not
   run*, commit it, and stop.
4. **Translate** the triggers (§3.4), choose the cold start (§3.3), write the policy, and create the summary with every run
   *pending* and run 7 *not run* (run-set.md §3).
5. **Gate the translation on the first knee.** Its HPA log shows every
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
workload.yaml               # the blis workload as run
signals-hpa.csv             # the HPA log: every sync's trigger values, proposals and decision
signals-summary.json        # blis's pod-seconds and goodput summary
blis-run.json               # the clock, the fates of every request, the inputs
command.txt                 # the blis command line
```

`triggers/<trigger>.json` are the trigger values the HPA read at each sync,
from `signals-hpa.csv`, in the format of a Prometheus range query. The raw
results (blis's Result with every request, and the llm-d-benchmark-shaped
`results/`) stay under `benchmark/results/step3-<scenario>-<YYYYMMDD>/<run>/`.
`terms.json` and the summary's translation table are written once, at the
evaluation's root directory.

The header table adds the blis-signal-lab commit, the BLIS scenario (and any
GPU type or parallelism it does not share with the overlay), the cold start and its source, the seed, and the
gaps of §3.2 the run is exposed to. The figure's caption says that the
trigger panel is the HPA's own reading, not a Prometheus query.

## 3.7 Gate after each run, in the simulator

On top of run-set.md §4:

| Check | Source | Fails when |
|---|---|---|
| blis ran clean | `stderr.txt` | any `level=error` or `fatal` line (`engine assumption` warnings are expected) |
| every request has a fate | `blis-run.json` `fates` | the fates do not sum to `generated` |
| the HPA ran | `signals-hpa.csv` | no row after t = 0, or a trigger column missing |
| repeatable | the HPA log's sha256 in `blis-run.json` | the first run, rerun with the same command, gives another sha256 |

## 3.8 What step 3 does not settle

- The GPU plant: capacities, cold start, latencies and costs are BLIS's.
  Comparing step 3 with step 5 for the same scenario measures the simulator,
  not the configuration.
- A cluster's metrics pipeline, KEDA's own loop, and scale from zero (§3.2).
- Approximated triggers: a run that depends on where a translation diverges
  shows the approximation, and its report says so.
- Variance: each run is one seed. blis makes repeats cheap; a claim that needs
  them reruns with `--seed` and reports each seed.
