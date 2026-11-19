---
name: gpu-evaluation
description: Run step 5 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — evaluate a KEDA scenario on a real GPU plant with the fixed run set (knee staircase, SO-active staircase, mix-shift, overload under and over the cap, agentic, static-replica baseline, scale from zero) and write one report per run (llm-d-benchmark's report plus a load-vs-supply figure) and a summary, from data captured on the live cluster. Use when asked to run a scenario on GPUs, to measure a scaling configuration on real hardware, to calibrate a knee or μ_SLO on a GPU, or to compare an autoscaled run with a static fleet. Invoke with /gpu-evaluation <scenario.yaml> [backend overlay] [--context <kube-context>].
---

# GPU evaluation

**Arguments:** `$ARGUMENTS`: a scenario under `benchmark/config/scenarios/`,
optionally a GPU backend overlay under `benchmark/config/cluster-configs/ocp/`
(default: the one the scenario's header names), and
optionally `--context <kube-context>` (default: the current one).

The rules live in the protocol; this skill adds the order, the gates and the
commands. Read first:

- [`benchmark/protocol/5-gpu-evaluation.md`](../../../benchmark/protocol/5-gpu-evaluation.md):
  prerequisites (§5.1), load and SLO (§5.2), the runs (§5.3), the reports and
  their writing rules (§5.5), the gate after each run (§5.6);
- the scenario's step 1 and step 2 verdicts under `benchmark/protocol/verdicts/<guide>/`,
  if any (step 5 does not need them);
- the `run-benchmark` skill for the `llmdbenchmark` commands, and the
  `load-vs-supply` skill for the figure.

Scripts in this directory (standard library Python, bash):

- `scripts/gen_load.py` writes every run's trace and inference-perf profile
  from the §5.2 inputs, by the §5.3 rules;
- `scripts/stage.sh` copies the traces onto the namespace's workload PVC;
- `scripts/arm.sh` sets a run's starting state;
- `scripts/trigger_series.py` exports the trigger and replica series from
  Prometheus.

The shell scripts take `--context` through `KUBECTL="kubectl --context <ctx>"`.

## Step 0: Confirm with the user

Stop and ask before any cluster write. Name the context and where it came
from, the GPUs the runs hold (`maxReplicaCount` × GPUs per replica, for about
5 h plus standup), the §5.2 inputs (SLO and X, the two mixes, the replica
unit) and the run list. If nobody answers,
do Step 1 only and say what is needed. Never switch the user's context.

## Step 1: Prepare, locally

1. Check §5.1: the llm-d-benchmark commit, the overlay, Prometheus access.
2. Fix the §5.2 inputs, from the scenario and its guide: the SLO, the
   reference mix (the guide's default workload), the second mix, the replica
   unit, and an estimate of μ_SLO for each mix (a simulator profile, an
   earlier run, or a short probe). Write the knee loads:

   ```bash
   S=.claude/skills/gpu-evaluation/scripts
   python3 -I $S/gen_load.py knee --mix <ref> --estimate <req/s> --slo ttft=<s>,tpot=<s> \
     --name knee-ref --out $LOAD
   python3 -I $S/gen_load.py knee --mix <second> --estimate <req/s> --slo ttft=<s>,tpot=<s> \
     --name knee-second --out $LOAD
   ```

   `$LOAD` is a working directory outside the repository; the profile of each
   run is committed later with its report. The other runs' loads wait for the
   knees.
3. `llmdbenchmark standup --dry-run` with the spec and overlay. Read the
   rendered ScaledObject in `plan/<stack>/`: placeholders resolved, the
   thresholds intended, and, if verdicts exist, the object they analyzed.

## Step 2: Stand up and gate

Namespace `step5-<scenario>-<YYYYMMDD>`; same `--spec`, `--cluster-config`,
`--workspace benchmark/results`, `-p` on every command. Then:

```bash
kubectl -n $NS get scaledobject,hpa                  # READY=True, no <unknown>
kubectl auth can-i list deployments -n $NS \
  --as=system:serviceaccount:$NS:<harness sa>        # yes
kubectl -n $NS get pods -o jsonpath='{range .items[*]}{.spec.containers[*].image}{"\t"}{.status.containerStatuses[*].imageID}{"\n"}{end}'
```

Record the digests, the KEDA release, the cluster version and the vLLM
arguments. Send one minute of smoke load and check that every trigger leaves
0 (`trigger_series.py` over that minute). A trigger that stays at 0 stops the
evaluation: fix the metric path first.

## Step 3: Runs 1a and 1b, the knees

```bash
$S/stage.sh $NS $LOAD
$S/arm.sh $NS <so> pin 1                # other pools: pin them large enough (§5.2)
llmdbenchmark ... run -l inference-perf --monitoring \
  --workload-file-path $LOAD/knee-ref.yaml --dataset /requests/datasets/knee-ref.csv \
  --wait-timeout 3600
```

Then the same with `knee-second`. Read A per stage from the benchmark
report's per-stage results (or the figure's CSV) and interpolate μ_SLO at
A = X. If the crossing is not between stages 2 and 5, regenerate the knee with
a new estimate and rerun (§5.3). If the configuration derives its threshold
from capacity, compute it from the reference knee, apply it with
`kubectl patch scaledobject`, and record the value. With several pools, repeat
for each scaled pool.

Read the cold start (requested to Ready) at the first scale-up you see, or
take it from the overlay's earlier runs, and set the stage length T (§5.3).
Then write the other runs' loads and stage them:

```bash
python3 -I $S/gen_load.py runs --mix <ref> --mu <μ ref> --mix2 <second> --mu2 <μ second> \
  --max-replicas <max> --min-replicas <min> --stage <T> --slo ttft=<s>,tpot=<s> --out $LOAD
$S/stage.sh $NS $LOAD
```

`$LOAD/index.json` holds every schedule and the numbers it came from; each
report's header cites it.

## Step 4: Runs 2 to 7

Order: 2 (`stair`), 3 (`mixshift`), 4a (`overload-under`), 4b
(`overload-over`), 5 (`agentic`, no `--dataset`), 6 (`mixshift` again,
pinned), 7 (`bootstrap`, only with `minReplicaCount: 0`). Each run:

```bash
$S/arm.sh $NS <so> active               # run 6: pin <peak Ready of run 3>; run 7: zero
date +%s > $RUN/start
llmdbenchmark ... run -l inference-perf --monitoring \
  --workload-file-path $LOAD/<run>.yaml --dataset /requests/datasets/<run>.csv \
  --wait-timeout 4800
date +%s > $RUN/end
kubectl -n $NS get scaledobject <so> -o json > $RUN/scaledobject.json
python3 -I $S/trigger_series.py --scaledobject $RUN/scaledobject.json \
  --prom <url> [--insecure] --start $(cat $RUN/start) --end $(cat $RUN/end) --out $RUN/triggers
kubectl -n $NS get events --sort-by=.lastTimestamp > $RUN/events.txt
```

For run 7, llm-d-benchmark wakes a scaled-to-zero stack before its model
check (#2188); if the check still fails on the empty pool, pass
`--endpoint-url` (the router's service) and `--model`.

Capture the ScaledObject during or right after the run, before any patch for
the next one. Then apply the gate (§5.6). A run that fails it for an
infrastructure fault is discarded and rerun; keep its session and reason for
the summary. With bearer-auth Prometheus (OpenShift Thanos Querier), export
the token as `PROM_TOKEN`.

## Step 5: Reports

For each run, in `benchmark/protocol/verdicts/<guide>/<scenario>-step5/<run>/`:

1. Copy the results directory's `benchmark_report_v0.2,_*.yaml` unchanged
   (gzip it if over 1,000 KB), the profile, `scaledobject.json` and
   `triggers/`.
2. Draw the figure with the `load-vs-supply` skill:

   ```bash
   python3 -I <load-vs-supply>/scripts/load_vs_supply.py <results dir> --loop open \
     --deployment <target> --load in,out,req \
     --trigger $RUN/triggers/<name>.json:<name> ... --scaledobject <plan>/<keda yaml> \
     --slo ttft=<s> [--slo tpot=<s>] --attainment-target <X> \
     --title "<scenario>: <run>" --subtitle "<model>, <GPU>, <date>" --out load-vs-supply.svg
   ```

   `--scaledobject` takes the rendered manifest (block YAML), not the
   captured JSON. If run 1 patched the threshold, the manifest holds the old
   value: give the live one per trigger as `--trigger FILE=<threshold>:<name>`,
   read from `scaledobject.json`. The session's replica series is read from the run; pass
   `--replicas-json triggers/replicas-ready.json` only when it is empty, and
   say so. Render the SVG (`qlmanage -t -s 900 -o . load-vs-supply.svg` on
   macOS) and look at it. Do not commit the PNG.
3. Write `report.md` per §5.5: header, figure and caption, the per-stage
   table, what the run shows and its outcome.

Then the summary `<scenario>-step5.md`, with run 3 against run 6.

Before finishing, reread every report against the writing rules in §5.5: each
number traceable to a file, n = 1 stated, errors by type, no cause the run did
not measure, no cluster, context or node name. Run `pre-commit` on the new
files.

## Step 6: Tear down

`llmdbenchmark teardown` with the same flags, `kubectl delete namespace $NS`,
then the cluster-scoped leftovers named for it
(`kubectl get clusterrole,clusterrolebinding | grep $NS`). Never delete a
namespace this run did not create. If the user wants the plant kept, scale it
to `minReplicaCount` and say so in the summary.
