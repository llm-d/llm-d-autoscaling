# The evaluation run set

Part of the [evaluation protocol](README.md).

Steps 3 and 5 run the same set of runs on two plants: blis-signal-lab
([step 3](3-blis-evaluation.md)) and the GPU cluster
([step 5](5-gpu-evaluation.md)). This document holds what the two share: the
load, the SLO and the capacity they are expressed in (§1), the runs (§2), the
reports (§3), the gate after each run (§4) and the commands (§5). Each step
adds its plant: prerequisites, how a run is armed and started, what is
captured, and the gate checks only that plant needs.

A **plant** is what serves the load: the model, the hardware and the serving
stack, on a cluster or in the simulator. A **run** is one load sent to the
plant from one starting state, its **arm**.

## 1. Load, SLO and capacity

Load is λ = (λ_in, λ_out): the input and output tokens per second the clients
send, as the `load-vs-supply` skill defines it. Every run is open loop: requests
arrive on a schedule (a replayed trace, Poisson stages for the agentic run, or
in step 3 blis's own Poisson draws at the same rates), so the offered load does
not depend on what the pool does. Prompts are random tokens except in the
agentic run, so the prefix cache never hits and a run can be repeated with the
same result for the cache.

Three inputs are fixed before run 1 and do not change during the evaluation:

- **The SLO**: TTFT and TPOT limits, from the guide the scenario belongs to
  (for example llm-d-benchmark's `ttft: 2`, `tpot: 0.05`), and the objective
  X (for example 0.95). **Attainment A** is the share of requests **sent** that
  completed and met every limit; failed and timed-out requests count as
  misses. The client timeout is 1.5 × the slowest request that still meets them
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
  requests per second of the blend, if capacity grows linearly with the
  replica count. Neither step measures that: the router's scorers and,
  with flow control on, its saturation gate can make a pool of N replicas
  serve less than N × μ_SLO. When a run misses X near the cap with every
  replica Ready, the report lists the miss as unexplained, not as a fault
  of the configuration.

With several pools (P/D), each scaled pool has its own μ_SLO, measured with the
other pools pinned large enough that they cannot be the bottleneck, and C is
the smallest of the pools' `maxReplicaCount × μ_SLO(B)`. The report names the
pool that sets C.

A pool that serves only part of the load, as prefill behind a decider that
keeps short prompts on decode, is measured on the requests it serves. Its A
counts those requests only, at the request rate of the whole load, so its
μ_SLO stays in requests per second of the load; its knee is not run for a mix
it never serves, and a phase of such mixes needs `minReplicaCount`. Counted
over every request, the knee is overstated by the requests the pool never
sees: in `coordinator-kv`'s step 3 the prefill pool's blend knee was 4.37 req/s
over all requests and 3.96 req/s over the 8192-token requests it served (a
third of the blend), within 2 % of 3 × its 8192:256 knee (1.34 req/s). With
random prompts the requests a prefix-length decider sends to the pool are those
at or over its threshold, which `stage_table.py --min-input-tokens` selects.

The knees have two uses. If the configuration derives its threshold from
capacity (as `npd-fcoff`'s `thresholdWork = 0.70 × ν_max`), the knee its rule
names sets that threshold once, before run 2, and it stays fixed; a rule that
names no mix takes the blend's. In every configuration, μ_SLO is also the
ruler the reports use to say how many replicas a load needed.

μ_SLO, C and every rate derived from them belong to one plant. Step 3 measures
its own knees in the simulator and never takes them from step 5, or the other
way round.

## 2. The runs

Each run starts from the same plant state, its **arm**, set before the load
starts. The [`gen_load.py`](scripts/gen_load.py) script writes every run's load
from the inputs above, for either plant, so no rate is chosen by hand.

**Stage length T.** A staircase stage must outlast the pool's reaction to it
and leave time to measure:
`T ≥ ΔR × scale-up period + cold start + pollingInterval + 120 s`, and
`T ≥ 300 s`, where ΔR is the most replicas a stage adds, the scale-up period
comes from the HPA behavior (`npd-fcoff`: 1 pod per 180 s), and cold start is
the time from a new replica being requested to Ready. Phases (runs 3 to 5) last
`max(600 s, 2 T)`.

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
| 1 | Knees | 1 replica, autoscaling paused | one run per mix in M, then one for B; four 300 s stages at 0.6, 0.8, 1.0 and 1.25 × an estimate of μ_SLO (per mix: from a simulator profile, a previous run, or a short probe; for B: `1 / mean over M of 1 / μ_SLO(m)`, from the measured per-mix knees) | A, TTFT and TPOT per stage; μ_SLO is the rate where A crosses X, interpolated linearly between the two stages around it | stage 1 fails or stage 4 passes: the crossing is outside the stages; move the estimate and rerun |
| 2 | SO-active staircase | ScaledObject active, `minReplicaCount` Ready | B, five stages of length T at 0.15, 0.3, 0.5, 0.7 and 0.9 × C | Ready, requested and HPA-desired replicas per stage, against the stage's need `ceil(q / μ_SLO(B))`; A per stage | replicas do not rise with the stages, or A < X once the pool settled |
| 3 | Mix-shift at constant rate | as 2 | one request rate `q = 0.5 × maxReplicaCount × min over M of μ_SLO(m)` throughout, one phase per mix in M: prefill-heavy, symmetrical, decode-heavy | settled Ready replicas in each phase against that phase's need `ceil(q / μ_SLO(m))`; A per phase | A < X once settled, or settled replicas differ from the need by more than the configuration's stated headroom (for a target utilization U, `ceil(q / (U × μ_SLO(m)))`) |
| 4a | Overload under the cap | as 2 | B, a phase at 1.2 × μ_SLO(B), then a phase at 0.8 × C | replicas, A and errors by type through the step | A < X after Ready replicas stop rising |
| 4b | Overload over the cap | as 2 | B, a phase at 1.2 × μ_SLO(B), then a phase at 1.5 × C | the trigger at the cap; errors by type (timeouts, 429, 503) | the trigger reads 0 or falls while the load holds, or replicas stop below `maxReplicaCount` |
| 5 | Agentic | as 2 | multi-turn sessions (shared system prompt, growing context, tool pauses), a phase at 0.1 then a phase of twice the length at 0.3 of the pool's prompt-token capacity `C × mean ISL of B`, converted to requests per second with the sessions' estimated mean prompt length; contexts capped by `--max-model-len` | replicas; A and errors by type; TTFT by prompt-length bucket | A < X once settled; any context-overflow error (the sessions do not fit the plant: fix the profile and rerun) |
| 6 | Static-replica baseline | autoscaling paused at the peak Ready count of run 3 | run 3's load | A per phase; replica-seconds against run 3 | not a pass/fail run: it is the cost reference for run 3 |
| 7 | Scale from zero | 0 Ready, ScaledObject active (only when `minReplicaCount` is 0) | B, one phase at 0.4 × μ_SLO(B) | time to the first Ready replica; requests failed before it | requests still fail after the first replica is Ready |
| 8 | Bursty | as 2 | B, a quiet phase Q at 0.2 × C, then three cycles of a spike R at 0.7 × C and a quiet phase Q | per cycle: scale-ups and scale-downs, replicas requested in a spike that were Ready only after it ended, A in the spike and in the quiet phase; requests failed when a replica was removed (`requests_errored_at_end`); replica-seconds | A < X in a quiet phase, or requests fail at a scale-down |
| 9 | Short spikes | as 2 | B, 300 s at 0.2 × C, then six spikes of R/2 at 0.7 × C separated by gaps G at 0.2 × C, then Q at 0.2 × C | per spike: whether it caused a scale-up, and whether that replica was Ready before the spike ended; A in each spike and each gap; Ready replicas through the run and after the last spike; replica-seconds | A < X in a gap, or requests fail at a scale-down |
| 10 | Scale-down | as 2 | B, H at 0.9 × C, then steps of length D at 0.6, 0.3 and 0.1 × C | per step down: time until Ready replicas reach the new need `ceil(q / μ_SLO(B))`, replica-seconds above that need, A, and requests failed when a replica was removed (`requests_errored_at_end`) | A < X after a step down, requests fail at a scale-down, or replicas have not reached the new need at the end of a step |

Notes on the run set:

- **Order.** The knees, then 2 to 5, then 8 to 10 (they need the cold start
  read in run 2), then 6 (it needs run 3's peak), then 7.
- **Knee stages.** The knee is a cliff: on one H100, A fell from 0.90 to
  0.03 within one +33 % stage (`npd-fcoff`'s knee, 8192:128), and between
  the last passing and the first failing rate, 4 to 8 % apart, P90 TTFT or
  ITL jumped past its limit for three mixes (research C1). One failing
  stage therefore brackets μ_SLO, and a stage past it only adds timeouts
  and drain time: the `npd-fcoff` knee's six stages, 1 to 6 req/s, spent
  their last three (15 of 30 min) at A ≤ 0.025. The four stages cover an
  estimate from 20 % below to 67 % above μ_SLO; an estimate from short stages tends to
  be high (next note). The stages are not shorter than 300 s, because
  short stages overestimate the capacity of KV-bound mixes: a replica
  that passed 4 minutes at a rate tipped after about 7.5 minutes (research
  E6), and long-out passed 120 s at 10 req/s but failed 180 s at 10.5
  (C1). For a decode-heavy mix even 300 s is under two request lifetimes
  at the TPOT limit, so its μ_SLO is an upper bound; the report says so.
- **The blend's estimate** comes from the per-mix knees, so the blend's
  knee runs last. It is the rate the blend would reach if the mixes did
  not interfere; prefill chunks raise the decode mixes' ITL, so the
  measured μ_SLO(B) is usually lower, within the stages' 40 %.
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
  judge its sizing, only attainment, errors and replicas. Neither plant
  expresses every part of an agentic session (each step lists what its
  sessions lack); the report states it.
- **The static baseline** may instead be the guide's `baseline.yaml` static
  fleet, if its replica count equals run 3's peak.
- **Why overload is relative to C.** The research campaign behind
  `npd-fcoff` (Qwen3-8B, one H100 per replica, one 8192:128 mix,
  `maxReplicaCount` 6) measured μ_SLO = 2.42 req/s, so C = 14.5 req/s for that
  mix. Its 2.5× and 6.6× of one replica (6.0 and 16.0 req/s) were 0.41 × C and
  1.1 × C: the first was absorbed without overloading the pool.

## 3. Reports

Files live under `verdicts/`, next to any other verdicts for the scenario, with
`<N>` the step (3 or 5):

```text
verdicts/<guide>/<scenario>-step<N>.md          # summary
verdicts/<guide>/<scenario>-step<N>/<run>/
    report.md                                   # the run's report
    load-vs-supply.svg, load-vs-supply.csv      # the figure and its table view
    stages.json                                 # stage_table.py's output
    triggers/<trigger>.json                     # what each trigger read
    ...                                         # the plant's own files (each step lists them)
```

The raw results stay under `benchmark/results/` (not committed); `report.md`
names where. A file over the repository's 1,000 KB limit is committed
gzip-compressed.

**Each run's `report.md`** holds, in this order:

1. a header table: the plant (each step lists its fields), scenario, model,
   GPU type and GPUs per replica, the SLO and X, the load with its entry in
   `gen_load.py`'s index, the arm, and where the raw results are;
2. the load-vs-supply figure, drawn with the `load-vs-supply` skill
   (`--slo` and `--attainment-target` from §1, `--scaledobject` from the
   rendered object, one `--trigger` per trigger), and a caption of two to four
   sentences: what was sent, what the pool did, what the figure shows;
3. the per-stage table from `stage_table.py`, one row per stage or phase:
   offered req/s, λ_in, λ_out, requests sent, errors by type, A, TTFT and
   TPOT p50/p90/p99, Ready replicas (min–max), the stage's need, requested and
   Ready replica-seconds;
4. what the run shows against the run's "shows a problem when" column (§2),
   and an outcome: **as expected**, **problem shown** (with the numbers), or
   **inconclusive** (with the missing data).

**The summary `<scenario>-step<N>.md`** holds the shared header, μ_SLO with the
mix and SLO it applies to, one row per run (outcome, A, errors, GPU-seconds,
link), the discarded attempts, and the run 3 against run 6 comparison:
GPU-seconds (requested replica-seconds × GPUs per replica, since a GPU is held
from the moment a pod is scheduled) and A per phase, side by side. When
verdicts from earlier steps exist, the summary links them, and a run that shows
a failure they predicted says so.

### Reporting as results come in

A session can stop at any run (a lost cluster, the end of a GPU slot, a failed
gate). The reports are therefore built one run at a time, and the tree is
correct after every commit:

- **The summary exists from the start.** It is created before run 1 with the
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
  A change to the plant after that makes it a different plant: close the
  summary and start a new evaluation.
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
  enough to identify a cluster run.
- Short sentences, units on every number, no adjectives where a number fits.

## 4. Gate after each run

A run's report is written only if its data holds. These checks apply to both
plants; each step adds its own.

| Check | Source | Fails when |
|---|---|---|
| per-request records present | `per_request_lifecycle_metrics.json` | missing or empty |
| replica series populated | `metrics/processed/replica_status_timeseries.json` | empty, or holds `collection_error` |
| HPA series populated (active arms) | `metrics/processed/autoscaler_timeseries.json` | no entry for the ScaledObject's HPA |
| trigger series exported | `triggers/<trigger>.json` | missing, or reads 0 while load was sent |
| placed in time | `stage_table.py`'s placement check | requests before the first stage or after the last (except the agentic run's later session rounds), or a stage's request count more than 3 standard deviations of a Poisson count from its set rate × length |
| no context overflow | the per-request records | an error for a prompt longer than `--max-model-len` (the profile does not fit the plant) |

## 5. Commands shared by steps 3 and 5

`S=benchmark/protocol/scripts`, `X` the objective, `SLO=ttft=<s>,tpot=<s>`,
`$LOAD` a working directory outside the repository, and `--target` `blis` in
step 3 (the default, `inference-perf`, in step 5).

**The knee loads.** One per mix; with a mix set other than the default, pass
`--mixes` to every command:

```bash
python3 -I $S/gen_load.py knee --mix 8192:256 --estimate <req/s> --slo $SLO --out $LOAD [--target blis]
python3 -I $S/gen_load.py knee --mix 2048:2048 --estimate <req/s> --slo $SLO --out $LOAD [--target blis]
python3 -I $S/gen_load.py knee --mix 256:4096 --estimate <req/s> --slo $SLO --out $LOAD [--target blis]
```

Each writes `knee-<ISL>x<OSL>.yaml` and `knee-<ISL>x<OSL>.json` (its
schedule), plus `knee-<ISL>x<OSL>.csv` for inference-perf. The blend's knee
runs last, with its estimate from the measured per-mix knees
(`1 / mean over M of 1 / μ_SLO(m)`, §2):

```bash
python3 -I $S/gen_load.py knee --mix blend --estimate <req/s> --slo $SLO --out $LOAD [--target blis]
```

**Reading a knee.** After the run (each step says how to run it):

```bash
python3 -I $S/stage_table.py <results dir> --index $LOAD/knee-<mix>.json --run knee-<mix> \
  --slo $SLO --knee X --json $RUN/stages.json [--trace $LOAD/knee-<mix>.csv]
```

For a pool that serves part of the load (§1), add `--min-input-tokens N`.
It prints the per-stage table and μ_SLO, and says when the crossing is
outside the stages (stage 1 fails or the last stage passes). Then regenerate the knee with a new estimate and
rerun. If the configuration derives its threshold from capacity, compute it
from the knee its rule names (the blend's if it names none) and record the
value; each step says how to apply it.

**The other runs' loads.** Once every knee is measured, set T, R, Q, G, H and
D (§2) from the cold start and the ScaledObject's behavior, then:

```bash
python3 -I $S/gen_load.py runs --mu 8192:256=<μ>,2048:2048=<μ>,256:4096=<μ> --mu-blend <μ> \
  --max-replicas <max> --min-replicas <min> --stage <T> --spike <R> --quiet <Q> \
  --gap <G> --hold <H> --down-stage <D> --slo $SLO --out $LOAD [--target blis]
```

`$LOAD/index.json` holds every schedule and the numbers it came from; each
report's header cites it. Runs: 2 `stair`, 3 `mixshift`, 4a
`overload-under`, 4b `overload-over`, 5 `agentic`, 6 `mixshift` again
(pinned), 7 `bootstrap` (only with `minReplicaCount: 0`), 8 `bursty`, 9
`short-spikes`, 10 `scale-down`. If run 2 shows a cold start other than the
one R and H were computed from, regenerate with the measured one before run 8.

**Reporting each run.** Mark the run *running* in the summary when it starts.
When it ends, in `verdicts/<guide>/<scenario>-step<N>/<run>/`:

1. Copy what the step lists for the plant, and `triggers/`.
2. Write the per-stage table and check the placement (§4):

   ```bash
   python3 -I $S/stage_table.py <results dir> --index $LOAD/index.json --run <run> \
     --slo $SLO --gpus-per-replica <G> --json stages.json [--trace $LOAD/<run>.csv]
   ```

   For a pool that serves part of the load (§1), add `--min-input-tokens N`
   and its own knees, `--mu <mix>=<μ_SLO>` per mix it serves and
   `--mu-blend <μ_SLO(B)>`, so its `need` column uses its capacity.

3. Draw the figure with the `load-vs-supply` skill:

   ```bash
   python3 -I .claude/skills/load-vs-supply/scripts/load_vs_supply.py <results dir> --loop open \
     --deployment <target> --load in,out,req \
     --trigger triggers/<name>.json:<name> ... --scaledobject <plan>/<keda yaml> \
     --slo ttft=<s> --slo tpot=<s> --attainment-target <X> \
     --title "<scenario>: <run> (step <N>)" --subtitle "<model>, <GPU>, <date>" --out load-vs-supply.svg
   ```

   `--scaledobject` takes the rendered manifest (block YAML). If run 1 set a
   capacity-derived threshold, the manifest holds the old value: give the one
   the run used per trigger as `--trigger FILE=<threshold>:<name>`. Render the
   SVG (`qlmanage -t -s 900 -o . load-vs-supply.svg` on macOS) and look at it.
   Do not commit the PNG.
4. Write `report.md` per §3: header, figure and caption, the per-stage
   table, what the run shows and its outcome, and an empty **Amendments**
   section.
5. Update the summary: the run's row, the conclusions so far (with the UTC
   time and the runs they cover), and, once runs 3 and 6 are both done, their
   comparison.
6. Amend what this run changes (§3): a fault or a tool error it reveals
   that also affected earlier runs. Add a dated entry to each amended report
   and to the summary's log.
7. Reread the new and amended text against the writing rules (§3), run
   `pre-commit` on the files, and commit.

When the last run is reported, or the evaluation stops early, close the
summary: mark the runs not done as *not run* with the reason.

**Resuming.** A new session continues from the summary, not from memory.
Read its run table; check that `$LOAD/index.json` is the one the reports cite
(regenerate it with the same arguments if the directory is gone; the seeds
make the loads identical) and that the plant is the one the last report
names (each step says how). A run left *running* has no report: if its
results are complete, gate and report it; otherwise mark it *discarded* and
rerun it. Then go on with the first *pending* run.
