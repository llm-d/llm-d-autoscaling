# Step 5: Evaluation on GPU

Part of the [evaluation protocol](README.md).

Steps 1 and 2 decide whether a configuration's triggers can compute the right
replica count. Step 5 runs the configuration on the GPU plant it targets and
records what it does: how the pool follows a fixed set of load shapes, what
latency clients get, and what it costs against a static fleet. Every number in
its reports comes from a run on a live cluster. Step 5 measures; it does not
rank policies against an oracle (efficiency η is a later step, with the
`autoscaling-efficiency` skill).

Step 5 does not depend on steps 1 and 2: any scenario can run it. When
verdicts from those steps exist, the summary links them, and a run that shows
a failure they predicted says so.

## 5.1 Prerequisites

| What | Why | How to check |
|---|---|---|
| llm-d-benchmark `main` at or after `0fb187fb` (#2192), which includes #2187 and #2191 | the harness records replica and HPA state per tick (`metrics/processed/replica_status_timeseries.json`, `autoscaler_timeseries.json`) and embeds offered load and SLO attainment in the report | `git -C ../llm-d-benchmark merge-base --is-ancestor 0fb187fb HEAD` |
| harness Role can read Deployments, HPAs and ScaledObjects | without it the replica series is empty and looks like a stack with no replicas | `kubectl auth can-i list deployments --as=system:serviceaccount:<ns>:<harness sa> -n <ns>`, after standup |
| inference-perf profiles with per-request records | the load-vs-supply figure reads offered load and attainment from them | `report.request_lifecycle.per_request: true`, `per_request_fields.computed_metrics: true`, and the SLO in `report.goodput.constraints` |
| `run --monitoring`, `--data-collect default` | `--data-collect results` drops the per-request records and `metrics/` | the command line kept with the run |
| a GPU backend overlay for the scenario's topology | the plant | `cluster-configs/ocp/vllm.yaml` (P/D) or `vllm-npd.yaml` (no P/D) |
| the `load-vs-supply` skill | the per-run figure (§5.5) | the `autoscaling` plugin of `llm-d-autoscaling-research` (`plugins/autoscaling/skills/load-vs-supply/`) |
| Prometheus reachable from the workstation, with the run's retention | the run directory holds the HPA's reading per tick (per pod, for `AverageValue`), not the trigger's value; the figure takes the trigger from a Prometheus range query after each run | a port-forward or route, and a token where the overlay uses bearer auth |

## 5.2 Load and SLO

Load is λ = (λ_in, λ_out): the input and output tokens per second the clients
send, as the `load-vs-supply` skill defines it. Every run is open loop
(`trace_replay`, `poisson` or `constant`), so the offered load does not depend
on what the pool does. The agentic run is Poisson too: requests arrive at the
stage rate, and each turn's context comes from its session.

The SLO is configuration. Fix it before the first run, write it in every
profile's `report.goodput.constraints`, and use it unchanged in every report.
**Attainment A** is the share of requests **sent** that completed and met
every constraint; failed and timed-out requests count as misses. The objective
X (for example 0.95) is fixed with the SLO.

All rates below are multiples of **μ_SLO**, the highest offered rate one
replica serves at A ≥ X for the reference mix, measured by run 1. The
reference mix is the scenario's own (8192:128 for `npd-fcoff`).

## 5.3 The runs

One standup serves every run. Each run starts from the same plant state, its
**arm**, set with the ScaledObject before the load starts (§5.4). The
`npd-fcoff` profiles in [`workloads/npd-fcoff/`](../config/workloads/npd-fcoff/)
implement every shape below; another scenario copies them and changes the
rates and the mix.

| # | Run | Arm | Load | Read from the run | Shows a problem when |
|---|---|---|---|---|---|
| 1 | Knee staircase | 1 replica, autoscaling paused | reference mix, six 300 s stages of constant rate spanning the knee (`npd-fcoff`: 1 to 6 req/s) | A, TTFT and ITL per stage; μ_SLO is the rate where A crosses X, interpolated linearly between the two stages around it | no stage, or the first stage, is below X: the range missed the knee; move it and rerun |
| 2 | SO-active staircase | ScaledObject active, `minReplicaCount` Ready | reference mix, five 300 s stages from 0.33 to 1.65 × μ_SLO | Ready, requested and HPA-desired replicas against each stage; A per stage | replicas do not rise with the stages, or A < X at a stage after the pool settled |
| 3 | Mix-shift at constant rate | ScaledObject active, `minReplicaCount` Ready | two 600 s phases, reference mix then a decode-heavy mix (`npd-fcoff`: 128:512), each at the same multiple (about 2×) of one replica's capacity for its mix | replicas across the shift; A per phase; the request rate swing | replicas move by more than one across the shift, or A < X in phase 2 |
| 4a | Overload 2.5× | ScaledObject active, `minReplicaCount` Ready | 600 s at 1.2 × μ_SLO, then 600 s at 2.5 × μ_SLO | replicas, A and errors by type through the step | A < X after Ready replicas stop rising |
| 4b | Overload 6.6× | as 4a | 600 s at 1.2 × μ_SLO, then 600 s at 6.6 × μ_SLO | the trigger at the cap; errors by type (timeouts, 429, 503) | the trigger reads 0 or falls while the load holds, or replicas stop below `maxReplicaCount` |
| 5 | Agentic | ScaledObject active, `minReplicaCount` Ready | AgentX-shaped multi-turn sessions (shared system prompt, growing context, tool pauses), 600 s at a low request rate then 1,200 s at 3× that rate (`npd-fcoff`: 0.5 then 1.5 req/s), context scaled to fit `--max-model-len` | replicas; A and errors by type; TTFT by prompt-length bucket | A < X after the pool settled; any context-overflow error (the trace does not fit the plant, so fix the trace) |
| 6 | Static-replica baseline | autoscaling paused at the peak Ready count of run 3 | run 3's trace, byte-identical | A per phase; replica-seconds against run 3 | not a pass/fail run: it is the cost reference for run 3 |

Notes on the run set:

- **Order.** Run 1 first: it fixes μ_SLO and so the rates of every other run.
  Run 6 after run 3, whose peak sets its fleet size.
- **The second mix's capacity** (run 3) comes from a knee run on that mix
  (repeat run 1 with it), or from the scenario's own work model when it has one
  (`npd-fcoff`'s `c_W`). The report says which.
- **Overload multipliers** are relative to one replica's μ_SLO. 2.5× must fit
  under `maxReplicaCount` and 6.6× must exceed it; with `npd-fcoff`'s cap of 6
  they do. For another cap, keep 2.5× and set the hard step to
  `1.1 × maxReplicaCount × μ_SLO`, and say so.
- **Agentic** is the closest shape llm-d-benchmark expresses:
  inference-perf `conversation_replay` has no subagent fan-out and drains at
  the stage boundary (see the `npd-fcoff` workload README). State this in the
  report; do not call it AgentX.
- **Scale from zero** is a seventh run when `minReplicaCount` is 0: arm at 0
  Ready, constant load at about 0.4 × μ_SLO (`bootstrap.yaml.in`).
- **Thresholds.** A threshold derived from μ_SLO (as `npd-fcoff`'s
  `thresholdWork = 0.70 × ν_max`) is set from run 1 before run 2, with
  `--set` at standup or a `kubectl patch` of the live ScaledObject. Record the
  value used.

## 5.4 Procedure

The [`gpu-evaluation` skill](../../.claude/skills/gpu-evaluation/SKILL.md)
runs these steps in this order, with the commands.

1. **Confirm the cluster** with the user before the first write: which
   context, and whether its GPUs may be used for the duration (about 3 h for
   the six runs, plus standup). Never switch the user's context.
2. **Render and check locally.** `standup --dry-run`; read the rendered
   ScaledObject (placeholders resolved, the thresholds intended). Check §5.1.
3. **Stand up** in a new namespace `step5-<scenario>-<YYYYMMDD>`. Resolve and
   record every image digest, the KEDA release and the vLLM arguments.
4. **Gate before load.** The ScaledObject is `READY=True`, the HPA shows real
   values, and a one-minute smoke load moves every trigger off 0. An
   `or vector(0)` guard turns a broken metric into a zero signal, and a run
   against it measures nothing.
5. **For each run:**
   1. arm the plant (`autoscaling.keda.sh/paused-replicas` to pin, then wait
      for the pinned count Ready; to start an active run, pin at
      `minReplicaCount`, wait out the triggers' longest rate window, then
      remove the annotation);
   2. run the profile with `llmdbenchmark run ... --monitoring`;
   3. **collect before anything is deleted**: the results directory, then the
      Prometheus range query of each trigger and of
      `kube_deployment_status_replicas_ready` over the run's window;
   4. **gate after the run** (§5.6). A run that fails the gate for a reason
      outside the configuration (node eviction, image pull, a dead client) is
      **discarded**: rerun it and list the discarded attempt with its reason.
6. **Write the reports** (§5.5).
7. **Tear down**, also after a failed run: the releases, the namespace, and the
   cluster-scoped objects named for it.

## 5.5 Reports

Files live under `verdicts/`, next to any other verdicts for the scenario:

```
verdicts/<guide>/<scenario>-step5.md          # summary
verdicts/<guide>/<scenario>-step5/<run>/
    report.md                                 # the run's report
    benchmark_report_v0.2,_<...>.yaml         # llm-d-benchmark's report, unchanged
    load-vs-supply.svg, load-vs-supply.csv    # the figure and its table view
    triggers/<trigger>.json                   # Prometheus range queries
    scaledobject.json                         # the live ScaledObject during the run
    profile.yaml                              # the inference-perf profile as run
```

The raw results directory stays under `benchmark/results/` (not committed);
`report.md` names its session. A benchmark report over the repository's
1,000 KB file limit is committed gzip-compressed.

**Each run's `report.md`** holds, in this order:

1. a header table: namespace, cluster version, scenario, overlay, model, GPU
   type and GPUs per replica, image digests, KEDA release, the SLO and X, the
   profile and trace, the arm, the session, start and end times (UTC);
2. the load-vs-supply figure, drawn with the `load-vs-supply` skill
   (`--slo` and `--attainment-target` from §5.2, `--scaledobject` from the
   captured object, one `--trigger` per trigger), and a caption of two to four
   sentences: what was sent, what the pool did, what the figure shows;
3. a table, one row per stage or phase, from the benchmark report and the
   figure's CSV: offered req/s, λ_in, λ_out, requests sent, errors by type, A,
   TTFT and ITL p50/p90/p99, Ready replicas (min–max), requested and Ready
   replica-seconds;
4. what the run shows against the run's "shows a problem when" column (§5.3),
   and an outcome: **as expected**, **problem shown** (with the numbers), or
   **inconclusive** (with the missing data).

**The summary `<scenario>-step5.md`** holds the shared header, μ_SLO with the
mix and SLO it applies to, one row per run (outcome, A, errors, GPU-seconds,
link), the discarded attempts, and the run 3 against run 6 comparison:
GPU-seconds (requested replica-seconds × GPUs per replica, since a GPU is held
from the moment a pod is scheduled) and A per phase, side by side.

### Writing rules

- **Every number comes from a file** in the run's directory or an exported
  query, and the report can name which. Nothing is estimated, modeled or
  carried over from another run without saying so.
- **Each run is one sample.** Say n = 1; do not attach a confidence to a
  single run, and do not call a difference significant.
- **Comparisons are arithmetic** between measured numbers ("31 % fewer
  GPU-seconds": 3,295 against 4,760). A cause is stated only when the run
  measured it; otherwise the difference is reported as unexplained.
- **Errors are counted by type and kept in A.** "0 errors" is checked against
  the per-request records, not read off a summary.
- **Anything outside the expectation** (an unexplained scale event, a trigger
  spike, a gap in a series) is listed as unexplained, with its time.
- **No cluster, context or node names.** The namespace and cluster version are
  enough to identify the run.
- Short sentences, units on every number, no adjectives where a number fits.

## 5.6 Gate after each run

A run's report is written only if its data holds:

| Check | Source | Fails when |
|---|---|---|
| per-request records present | `per_request_lifecycle_metrics.json` | missing or empty |
| the generator kept up | `results.observability.load_generator.schedule_delay_seconds` | the delay grows through a stage: the run was not open loop |
| replica series populated | `metrics/processed/replica_status_timeseries.json` | empty, or holds `collection_error` |
| HPA series populated (active arms) | `metrics/processed/autoscaler_timeseries.json` | no entry for the ScaledObject's HPA |
| trigger series exported | `triggers/<trigger>.json` | missing, or reads 0 while load was sent |
| no infrastructure fault | pod events in the namespace during the run | an eviction, OOM kill or image pull failure of a serving pod |
| placed in time | `results.observability.load_generator` | `unavailable`, or requests outside every stage window |

## 5.7 What step 5 does not settle

- Efficiency against a perfect-foresight oracle (η) and its waste breakdown:
  a later step, from the same runs.
- Run-to-run variance: one run per shape. A claim that two configurations
  differ by less than the variance of repeated runs needs repeated runs.
- Other plants: μ_SLO, cold start and every latency are for the model, GPU,
  vLLM arguments and mix in the header, and do not transfer.
