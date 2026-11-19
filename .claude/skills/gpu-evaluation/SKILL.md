---
name: gpu-evaluation
description: Run step 5 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — evaluate a KEDA scenario on a real GPU plant with the fixed run set (knee staircase, SO-active staircase, mix-shift, overload under and over the cap, agentic, static-replica baseline, scale from zero, bursty, short spikes, scale-down) and write one report per run (llm-d-benchmark's report plus a load-vs-supply figure) and a summary, from data captured on the live cluster. Use when asked to run a scenario on GPUs, to measure a scaling configuration on real hardware, to calibrate a knee or μ_SLO on a GPU, or to compare an autoscaled run with a static fleet. Invoke with /gpu-evaluation <scenario.yaml> [backend overlay] [--context <kube-context>].
---

# GPU evaluation

**Arguments:** `$ARGUMENTS`: a scenario under `benchmark/config/scenarios/`,
optionally a GPU backend overlay under `benchmark/config/cluster-configs/ocp/`
(default: the one the scenario's header names), and
optionally `--context <kube-context>` (default: the current one).

The rules live in the protocol; this skill adds the order, the gates and the
cluster commands. Read first:

- [`benchmark/protocol/run-set.md`](../../../benchmark/protocol/run-set.md):
  load, SLO and capacity (§1), the runs (§2), the reports and their writing
  rules (§3), the shared gate (§4) and the commands shared with step 3 (§5);
- [`benchmark/protocol/5-gpu-evaluation.md`](../../../benchmark/protocol/5-gpu-evaluation.md):
  prerequisites (§5.1), the plant and the arms (§5.2), what the reports add
  (§5.4), the cluster gate (§5.5);
- the scenario's step 1 to 3 verdicts under `benchmark/protocol/verdicts/<guide>/`,
  if any (step 5 does not need them);
- the `run-benchmark` skill for the `llmdbenchmark` commands, and the
  `load-vs-supply` skill for the figure.

Scripts in this directory (standard library Python, bash):

- `scripts/stage.sh` copies the traces onto the namespace's workload PVC;
- `scripts/arm.sh` sets a run's starting state;
- `scripts/trigger_series.py` exports the trigger and replica series from
  Prometheus.

The loads (`gen_load.py`) and the per-stage table (`stage_table.py`) are
shared with step 3, in `benchmark/protocol/scripts/`. The shell scripts take
`--context` through `KUBECTL="kubectl --context <ctx>"`.

## Step 0: Confirm with the user

Stop and ask before any cluster write. Name the context and where it came
from, the GPUs the runs hold (`maxReplicaCount` × GPUs per replica, for about
8.8 h plus standup), the run set's inputs (SLO and X, the mix set, the
replica unit) and the run list. If nobody answers, do Step 1 only and say
what is needed. Never switch the user's context.

## Step 1: Prepare, locally

1. Check §5.1: the llm-d-benchmark commit, the overlay, Prometheus access.
2. Fix the run set's inputs (run-set.md §1), from the scenario and its guide:
   the SLO, the mix set M (the three `random_*` mixes, plus the guide's own
   workload if it names one), the replica unit, and an estimate of μ_SLO for
   each mix (a simulator profile, an earlier run, or a short probe). Write
   one knee load per mix (run-set.md §5) with the default target. The
   blend's knee is written after the per-mix knees, from their measured
   μ_SLO; the other runs' loads wait for all the knees.
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
arguments. Create the summary
`benchmark/protocol/verdicts/<guide>/<scenario>-step5.md` now: the shared
header, the run set's inputs, and the run table with every run *pending*:

```markdown
| # | Run | Status | Outcome | A | Errors | GPU-s | Report |
|---|---|---|---|---|---|---|---|
| 1 | knee 8192:256 | pending | | | | | |
...
```

Commit it. Send one minute of smoke load and check that every trigger leaves
0 (`trigger_series.py` over that minute). A trigger that stays at 0 stops the
evaluation: fix the metric path first.

## Step 3: Run each run

`S=.claude/skills/gpu-evaluation/scripts`. Stage the loads once written
(`$S/stage.sh $NS $LOAD`), then for each run, in the run set's order:

```bash
$S/arm.sh $NS <so> pin 1                # knees; run 6: pin <peak Ready of run 3>;
                                        # runs 2-5, 8-10: active; run 7: zero
date +%s > $RUN/start
llmdbenchmark ... run -l inference-perf --monitoring \
  --workload-file-path $LOAD/<run>.yaml --dataset /requests/datasets/<run>.csv \
  --wait-timeout 4800                   # agentic: no --dataset
date +%s > $RUN/end
kubectl -n $NS get scaledobject <so> -o json > $RUN/scaledobject.json
python3 -I $S/trigger_series.py --scaledobject $RUN/scaledobject.json \
  --prom <url> [--insecure] --start $(cat $RUN/start) --end $(cat $RUN/end) --out $RUN/triggers
kubectl -n $NS get events --sort-by=.lastTimestamp > $RUN/events.txt
```

With several pools, pin the other pools large enough for the knees
(run-set.md §1). With bearer-auth Prometheus (OpenShift Thanos Querier),
export the token as `PROM_TOKEN`.

Run the per-mix knees first, reading each one's μ_SLO with
`stage_table.py --knee X --trace $LOAD/knee-<mix>.csv` (run-set.md §5) and
reporting it before the next. Then write the blend's knee from them
(run-set.md §5), stage it with `$S/stage.sh $NS $LOAD` and run it last.
After the knees, a capacity-derived threshold is
applied with `kubectl patch scaledobject` and recorded. Read the cold start
at the first scale-up you see, or take it from the overlay's earlier runs,
write the other runs' loads (run-set.md §5) and stage them.

For run 7, llm-d-benchmark wakes a scaled-to-zero stack before its model
check (#2188); if the check still fails on the empty pool, pass
`--endpoint-url` (the router's service) and `--model`.

Capture the ScaledObject during or right after the run, before any patch for
the next one. Then apply the gate (run-set.md §4, §5.5). A run that fails it
for an infrastructure fault is discarded and rerun; its summary row says so,
with the session and the reason.

## Step 4: Report each run as it finishes

Follow run-set.md §5, "Reporting each run", with the results directory of
the run's session, `stage_table.py --trace $LOAD/<run>.csv`, and, in item 1,
these files: the results directory's `benchmark_report_v0.2,_*.yaml`
unchanged (gzip it if over 1,000 KB), the profile, and `scaledobject.json`.

The figure's `--scaledobject` is the rendered manifest in `plan/<stack>/`,
not the captured JSON; read a patched threshold from `scaledobject.json`. The
session's replica series is read from the run; pass `--replicas-json
triggers/replicas-ready.json` only when it is empty, and say so. A measured
cold start that differs from the one R used changes no report: regenerate
runs 8 to 10's loads before run 8.

The reread against the writing rules also checks that no cluster, context or
node name is in the text.

## Resuming

As run-set.md §5, "Resuming". The plant check: the namespace still exists,
and its ScaledObject is the one in the last report's `scaledobject.json`.

## Step 5: Tear down

`llmdbenchmark teardown` with the same flags, `kubectl delete namespace $NS`,
then the cluster-scoped leftovers named for it
(`kubectl get clusterrole,clusterrolebinding | grep $NS`). Never delete a
namespace this run did not create. If the user wants the plant kept, scale it
to `minReplicaCount` and say so in the summary.
