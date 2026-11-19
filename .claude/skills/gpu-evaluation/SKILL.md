---
name: gpu-evaluation
description: Run step 5 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — evaluate a KEDA scenario on a real GPU plant with the fixed run set (knee staircase, SO-active staircase, mix-shift, overload 2.5× and 6.6×, agentic, static-replica baseline) and write one report per run (llm-d-benchmark's report plus a load-vs-supply figure) and a summary, from data captured on the live cluster. Use when asked to run a scenario on GPUs, to measure a scaling configuration on real hardware, to calibrate a knee or μ_SLO on a GPU, or to compare an autoscaled run with a static fleet. Invoke with /gpu-evaluation <scenario.yaml> [backend overlay] [--context <kube-context>].
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

Scripts in this directory: `scripts/arm.sh` sets a run's starting state,
`scripts/trigger_series.py` exports the trigger and replica series from
Prometheus. Both take the namespace and ScaledObject name; pass `--context`
through `KUBECTL="kubectl --context <ctx>"`.

## Step 0: Confirm with the user

Stop and ask before any cluster write. Name the context and where it came
from, the GPUs the runs hold (`maxReplicaCount` × GPUs per replica, for about
3 h plus standup), the SLO and X (§5.2) and the run list. If nobody answers,
do Step 1 only and say what is needed. Never switch the user's context.

## Step 1: Prepare, locally

1. Check §5.1: the llm-d-benchmark commit, the overlay, Prometheus access.
2. Pick the profiles. For `optimized-baseline/npd-fcoff` they exist in
   `benchmark/config/workloads/npd-fcoff/` with traces from
   `benchmark/hack/npd-fcoff/gen_traces.py`. For another scenario copy them
   to `benchmark/config/workloads/<scenario>/`, set the mix, put the SLO in
   `report.goodput.constraints`, and keep `per_request: true` and
   `computed_metrics: true`. The rates of runs 2 to 6 wait for run 1.
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

## Step 3: Run 1, the knee

```bash
scripts/arm.sh $NS <so> pin 1
llmdbenchmark ... run -l inference-perf --monitoring \
  --workload-file-path <knee profile> [--dataset <trace>] --wait-timeout 3600
```

Read A per stage from the benchmark report's per-stage results (or the
figure's CSV) and interpolate μ_SLO at A = X. If no stage, or the first stage,
is below X, move the range and rerun (§5.3). Derive the scenario's threshold
from μ_SLO if it has such a rule, apply it with `kubectl patch scaledobject`,
and record the value. Then set the rates of the other runs.

## Step 4: Runs 2 to 6

Order: 2, 3, 4a, 4b, 5, 6 (run 6 needs run 3's peak Ready count). Each run:

```bash
date +%s > $RUN/start
scripts/arm.sh $NS <so> active          # run 6: scripts/arm.sh $NS <so> pin <peak of run 3>
llmdbenchmark ... run -l inference-perf --monitoring \
  --workload-file-path <profile> [--dataset <trace>] --wait-timeout 4800
date +%s > $RUN/end
kubectl -n $NS get scaledobject <so> -o json > $RUN/scaledobject.json
python3 -I scripts/trigger_series.py --scaledobject $RUN/scaledobject.json \
  --prom <url> [--insecure] --start $(cat $RUN/start) --end $(cat $RUN/end) --out $RUN/triggers
kubectl -n $NS get events --sort-by=.lastTimestamp > $RUN/events.txt
```

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
