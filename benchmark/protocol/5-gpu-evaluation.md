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

## 5.2 Load, SLO and capacity

Load is λ = (λ_in, λ_out): the input and output tokens per second the clients
send, as the `load-vs-supply` skill defines it. Every run is open loop: requests
arrive on a schedule (a replayed trace, or Poisson stages for the agentic run),
so the offered load does not depend on what the pool does. Prompts are random
tokens except in the agentic run, so the prefix cache never hits and a run can
be repeated with the same result for the cache.

Three inputs are fixed before run 1 and do not change during the evaluation:

- **The SLO**: TTFT and TPOT limits, from the guide the scenario belongs to
  (for example llm-d-benchmark's `ttft: 2`, `tpot: 0.05`), and the objective
  X (for example 0.95). **Attainment A** is the share of requests **sent** that
  completed and met every limit; failed and timed-out requests count as
  misses. The limits go in every profile's `report.goodput.constraints`.
  The client timeout is 1.5 × the slowest request that still meets them
  (`TTFT + longest OSL × TPOT`), and at least 60 s; a shorter one would fail
  requests the SLO counts as met (256:4096 at 50 ms TPOT takes about 205 s).
- **The mix set M** (ISL:OSL): llm-d-benchmark's three `random_*` profiles,
  prefill-heavy 8192:256, symmetrical 2048:2048 and decode-heavy 256:4096.
  No mix in M is privileged; a scenario whose guide names a workload of its own
  adds it to M as one more peer. **The blend B** draws each request's mix from
  M with equal weight.
- **The replica**: the unit the ScaledObject scales (a pod, or a
  LeaderWorkerSet group).

The knee runs (run 1) give the numbers every other rate follows from:

- **μ_SLO(m)**: the highest request rate one replica serves at A ≥ X for
  mix m. It is measured for every mix in M and for B.
- **C = maxReplicaCount × μ_SLO(B)**: what the pool serves at its cap, in
  requests per second of the blend.

With several pools (P/D), each scaled pool has its own μ_SLO, measured with the
other pools pinned large enough that they cannot be the bottleneck, and C is
the smallest of the pools' `maxReplicaCount × μ_SLO(B)`. The report names the
pool that sets C.

The knees have two uses. If the configuration derives its threshold from
capacity (as `npd-fcoff`'s `thresholdWork = 0.70 × ν_max`), the knee its rule
names sets that threshold once, before run 2, and it stays fixed; a rule that
names no mix takes the blend's. In every configuration, μ_SLO is also the
ruler the reports use to say how many replicas a load needed.

## 5.3 The runs

One standup serves every run. Each run starts from the same plant state, its
**arm**, set with the ScaledObject before the load starts (§5.4). The
[`gen_load.py`](../../.claude/skills/gpu-evaluation/scripts/gen_load.py)
script of the `gpu-evaluation` skill writes every run's trace and profile from
the inputs above, so no rate is chosen by hand.

**Stage length T.** A staircase stage must outlast the pool's reaction to it
and leave time to measure:
`T ≥ ΔR × scale-up period + cold start + pollingInterval + 120 s`, and
`T ≥ 300 s`, where ΔR is the most replicas a stage adds, the scale-up period
comes from the HPA behavior (`npd-fcoff`: 1 pod per 180 s), and cold start is
the time from a new replica being requested to Ready, read off the first
scale-up. Phases (runs 3 to 5) last `max(600 s, 2 T)`.

**Burst lengths** (run 8) come from the same HPA behavior. The spike lasts
the pool's reaction time `R = pollingInterval + scale-up stabilization + cold
start` (at least 60 s): a replica requested at the start of a spike is Ready
when the spike ends. The quiet phase lasts
`Q = scale-down stabilization + scale-down period + 120 s` (at least 300 s),
so the pool can shrink between spikes.

**Short spikes** (run 9) last `R/2` (at least 30 s, one bin of the figure),
so a replica requested for a spike is Ready only after it has ended. The gap
between spikes is `G = min(2 R, scale-down stabilization / 2)` (at least
60 s), short enough that the pool cannot shrink between them.

**Scale-down lengths** (run 10). The opening hold lasts
`H = (ceil(0.9 × maxReplicaCount) − minReplicaCount) × scale-up period + R +
120 s`, long enough to reach the replicas 0.9 × C needs. Each step down
lasts `D = ΔR↓ × scale-down period + scale-down stabilization + 120 s`, where
ΔR↓ is the most replicas a step sheds (`ceil` of the need before minus the
need after).

| # | Run | Arm | Load | Read from the run | Shows a problem when |
|---|---|---|---|---|---|
| 1 | Knees | 1 replica, autoscaling paused | one run per mix in M and one for B; six 300 s stages at 0.4, 0.7, 1.0, 1.4, 1.9 and 2.5 × an estimate of μ_SLO (from a simulator profile, a previous run, or a short probe) | A, TTFT and TPOT per stage; μ_SLO is the rate where A crosses X, interpolated linearly between the two stages around it | the crossing is not between stages 2 and 5: move the estimate and rerun |
| 2 | SO-active staircase | ScaledObject active, `minReplicaCount` Ready | B, five stages of length T at 0.15, 0.3, 0.5, 0.7 and 0.9 × C | Ready, requested and HPA-desired replicas per stage, against the stage's need `ceil(q / μ_SLO(B))`; A per stage | replicas do not rise with the stages, or A < X once the pool settled |
| 3 | Mix-shift at constant rate | as 2 | one request rate `q = 0.5 × maxReplicaCount × min over M of μ_SLO(m)` throughout, one phase per mix in M: prefill-heavy, symmetrical, decode-heavy | settled Ready replicas in each phase against that phase's need `ceil(q / μ_SLO(m))`; A per phase | A < X once settled, or settled replicas differ from the need by more than the configuration's stated headroom (for a target utilization U, `ceil(q / (U × μ_SLO(m)))`) |
| 4a | Overload under the cap | as 2 | B, a phase at 1.2 × μ_SLO(B), then a phase at 0.8 × C | replicas, A and errors by type through the step | A < X after Ready replicas stop rising |
| 4b | Overload over the cap | as 2 | B, a phase at 1.2 × μ_SLO(B), then a phase at 1.5 × C | the trigger at the cap; errors by type (timeouts, 429, 503) | the trigger reads 0 or falls while the load holds, or replicas stop below `maxReplicaCount` |
| 5 | Agentic | as 2 | multi-turn sessions (shared system prompt, growing context, tool pauses), a phase at 0.1 then a phase of twice the length at 0.3 of the pool's prompt-token capacity `C × mean ISL of B`, converted to requests per second with the sessions' estimated mean prompt length; contexts capped by `--max-model-len` | replicas; A and errors by type; TTFT by prompt-length bucket | A < X once settled; any context-overflow error (the sessions do not fit the plant: fix the profile and rerun) |
| 6 | Static-replica baseline | autoscaling paused at the peak Ready count of run 3 | run 3's trace, byte-identical | A per phase; replica-seconds against run 3 | not a pass/fail run: it is the cost reference for run 3 |
| 7 | Scale from zero | 0 Ready, ScaledObject active (only when `minReplicaCount` is 0) | B, one phase at 0.4 × μ_SLO(B) | time to the first Ready replica; requests failed before it | requests still fail after the first replica is Ready |
| 8 | Bursty | as 2 | B, a quiet phase Q at 0.2 × C, then three cycles of a spike R at 0.7 × C and a quiet phase Q | per cycle: scale-ups and scale-downs, replicas requested in a spike that were Ready only after it ended, A in the spike and in the quiet phase; requests failed when a replica was removed (`requests_errored_at_end`); replica-seconds | A < X in a quiet phase, or requests fail at a scale-down |
| 9 | Short spikes | as 2 | B, 300 s at 0.2 × C, then six spikes of R/2 at 0.7 × C separated by gaps G at 0.2 × C, then Q at 0.2 × C | per spike: whether it caused a scale-up, and whether that replica was Ready before the spike ended; A in each spike and each gap; Ready replicas through the run and after the last spike; replica-seconds | A < X in a gap, or requests fail at a scale-down |
| 10 | Scale-down | as 2 | B, H at 0.9 × C, then steps of length D at 0.6, 0.3 and 0.1 × C | per step down: time until Ready replicas reach the new need `ceil(q / μ_SLO(B))`, replica-seconds above that need, A, and requests failed when a replica was removed (`requests_errored_at_end`) | A < X after a step down, requests fail at a scale-down, or replicas have not reached the new need at the end of a step |

Notes on the run set:

- **Order.** The knees, then 2 to 5, then 8 to 10 (they need the cold start
  read in run 2), then 6 (it needs run 3's peak), then 7. For a single-pool
  scenario with the three mixes of M and T = 300 s, and `npd-fcoff`'s HPA
  behavior (R ≈ 105 s, Q = 720 s, G = 150 s, H ≈ 1,125 s, D = 1,020 s), the
  load lasts about 7.3 h (four knees of 30 min, run 8 about 53 min, run 9
  about 35 min, run 10 about 70 min); with the drain and the arm between runs
  (about 10 min each), about 9.5 h, plus standup.
- **The blend** gives every load-shape run all the mixes at once, so no run
  favors a configuration tuned to one of them. Its arrivals are one Poisson
  process; only the mix of each request is drawn.
- **Mix-shift holds the request rate constant**, so a trigger that counts
  requests reads no change while the need changes with the mix. `q` keeps
  every phase under the cap.
- **Bursty is not overload.** Overload holds a load above capacity for longer
  than any scale-up and tests the cap; a burst is shorter than the pool's
  reaction and its peak is within the cap.
- **Runs 8 to 10 split the time-varying cases.** Bursty (8): a spike as long
  as the reaction, then time to shrink, so each cycle scales up and down.
  Short spikes (9): too short to catch and too close together to shrink
  between, so it shows whether the pool chases spikes and what it holds
  afterwards. Scale-down (10): load leaving a full pool in steps, so it shows
  how fast and how cleanly capacity is released.
- **Overload** is measured against the cap, not one replica: 0.8 × C needs a
  ramp to most of the cap and stays servable; 1.5 × C cannot be served. Each
  starts from a phase at 1.2 × μ_SLO, so the step comes from a pool already
  past one replica.
- **Agentic** load needs more GPU per prompt token than the blend
  (long contexts), so its rates are a fraction of C and the report does not
  judge its sizing, only attainment, errors and replicas. inference-perf's
  `conversation_replay` is the closest shape llm-d-benchmark expresses: it has
  no subagent fan-out, and it drains in-flight requests at the phase boundary.
  State both in the report.
- **The static baseline** may instead be the guide's `baseline.yaml` static
  fleet, if its replica count equals run 3's peak.
- **Why overload is relative to C.** The research campaign behind
  `npd-fcoff` (Qwen3-8B, one H100 per replica, one 8192:128 mix,
  `maxReplicaCount` 6) measured μ_SLO = 2.42 req/s, so C = 14.5 req/s for that
  mix. Its 2.5× and 6.6× of one replica (6.0 and 16.0 req/s) were 0.41 × C and
  1.1 × C: the first was absorbed without overloading the pool.

## 5.4 Procedure

The [`gpu-evaluation` skill](../../.claude/skills/gpu-evaluation/SKILL.md)
runs these steps in this order, with the commands.

1. **Confirm the cluster** with the user before the first write: which
   context, and whether its GPUs may be used for the duration (about 9.5 h for
   the runs, plus standup). Never switch the user's context.
2. **Render and check locally.** `standup --dry-run`; read the rendered
   ScaledObject (placeholders resolved, the thresholds intended). Check §5.1.
3. **Stand up** in a new namespace `step5-<scenario>-<YYYYMMDD>`. Resolve and
   record every image digest, the KEDA release and the vLLM arguments, and
   create the summary with every run *pending* (§5.5).
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
      **discarded**: rerun it and list the discarded attempt with its reason;
   5. **report it** (§5.5): write the run's report, update the summary, amend
      any earlier report the run changes, and commit, before arming the next
      run.
6. **Close the summary** when the last run is reported, or when the
   evaluation stops early, with the runs not done and why.
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
   profile and trace with their entry in `gen_load.py`'s `index.json`, the
   arm, the session, start and end times (UTC);
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

### Reporting as results come in

The run set takes most of a working day, and a session can stop at any run
(a lost cluster, the end of a GPU slot, a failed gate). The reports are
therefore built one run at a time, and the tree is correct after every
commit:

- **The summary exists from the start.** It is created at standup with the
  shared header and one row per run, each with a status: *pending*,
  *running*, *done*, *discarded* (with the reason and a link to the rerun) or
  *not run* (with the reason).
- **One run, one update.** When a run passes the gate, its `report.md` is
  written and its summary row filled in, and both are committed before the
  next run is armed. A run that fails the gate gets its *discarded* row in
  the same way.
- **Partial conclusions say they are partial.** The summary's conclusions
  cover the *done* runs only, start with the UTC time of the update and the
  runs it includes, and name the runs still pending. The run 3 against run 6
  comparison appears once both are done.
- **Amend, do not rewrite.** A later finding can change an earlier report:
  a fault found during a later run that was already present in earlier ones
  (a node evicting pods, a scrape gap), or a corrected tool or query that
  changes a reported number. Update the changed numbers in the earlier
  report and add a dated entry to its **Amendments** section: what changed,
  from which value to which, and what found it. The summary keeps the same
  log. A run the finding invalidates is marked *superseded*, links its
  rerun, and stays in the tree. A rerun that replaces a *discarded* run
  changes only the summary.
- **The knees are final once run 2 starts.** A knee is rerun only before
  anything uses it (its crossing missed the stages, or it failed the gate).
  A change to the plant after that (GPU type, node pool, image, vLLM
  arguments) makes it a different plant: close the summary and start a new
  evaluation.
- **Git history holds the earlier versions.** Commit at each update; never
  rewrite a committed report's history.

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
