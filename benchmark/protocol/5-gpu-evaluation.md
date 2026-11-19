# Step 5: Evaluation on GPU

Part of the [evaluation protocol](README.md).

Steps 1 and 2 decide whether a configuration's triggers can compute the right
replica count. Step 5 runs the configuration on the GPU plant it targets and
records what it does: how the pool follows a fixed set of load shapes, what
latency clients get, and what it costs against a static fleet. Every number in
its reports comes from a run on a live cluster. Step 5 measures; it does not
rank policies against an oracle (efficiency η is a later step, with the
`autoscaling-efficiency` skill).

The runs, their loads, the reports and the shared gate are the
[evaluation run set](run-set.md), which step 3 also runs in blis-signal-lab.
This document adds the GPU plant: what it needs (§5.1), how a run is armed,
run and captured on it (§5.2, §5.3), what its reports add (§5.4) and the
checks only a cluster needs (§5.5).

Step 5 does not depend on steps 1 to 3: any scenario can run it. When
verdicts from those steps exist, the summary links them, and a run that shows
a failure they predicted says so.

## 5.1 Prerequisites

| What | Why | How to check |
|---|---|---|
| llm-d-benchmark `main` at or after `0fb187fb` (#2192), which includes #2187 and #2191 | the harness records replica and HPA state per tick (`metrics/processed/replica_status_timeseries.json`, `autoscaler_timeseries.json`) and embeds offered load and SLO attainment in the report | `git -C ../llm-d-benchmark merge-base --is-ancestor 0fb187fb HEAD` |
| harness Role can read Deployments, HPAs and ScaledObjects | without it the replica series is empty and looks like a stack with no replicas | `kubectl auth can-i list deployments --as=system:serviceaccount:<ns>:<harness sa> -n <ns>`, after standup |
| inference-perf profiles with per-request records | the load-vs-supply figure and the per-stage table read offered load and attainment from them | `report.request_lifecycle.per_request: true`, `per_request_fields.computed_metrics: true`, and the SLO in `report.goodput.constraints` (`gen_load.py` writes all three) |
| `run --monitoring`, `--data-collect default` | `--data-collect results` drops the per-request records and `metrics/` | the command line kept with the run |
| a GPU backend overlay for the scenario's topology | the plant | `cluster-configs/ocp/vllm.yaml` (P/D) or `vllm-npd.yaml` (no P/D) |
| Prometheus reachable from the workstation, with the run's retention | the run directory holds the HPA's reading per tick (per pod, for `AverageValue`), not the trigger's value; the figure takes the trigger from a Prometheus range query after each run | a port-forward or route, and a token where the overlay uses bearer auth |

## 5.2 The plant and the arms

One standup serves every run, in a new namespace
`step5-<scenario>-<YYYYMMDD>`. Every image digest, the KEDA release and the
vLLM arguments are recorded at standup; a change to any of them, to the GPU
type or to the node pool makes a different plant (run-set.md §3).

A run's arm is set with KEDA's `autoscaling.keda.sh/paused-replicas`
annotation on the ScaledObject:

- **pinned** (runs 1 and 6): set the annotation to the count and wait until
  that many replicas are Ready. KEDA holds the target there; the triggers are
  still in Prometheus.
- **active** (runs 2 to 5 and 8 to 10): pin at `minReplicaCount`, wait out the
  triggers' longest rate window, then remove the annotation.
- **zero** (run 7): pin at 0, wait, remove the annotation.

The cold start in the §2 lengths of the run set is read off the first
scale-up (requested to Ready), in run 2.

For a single-pool scenario with the three mixes of M and T = 300 s, and
`npd-fcoff`'s HPA behavior (R ≈ 105 s, Q = 720 s, G = 150 s, H ≈ 1,125 s,
D = 1,020 s), the load lasts about 6.6 h (four knees of 20 min, run 8 about
53 min, run 9 about 35 min, run 10 about 70 min); with the drain and the arm
between runs (about 10 min each), about 8.8 h, plus standup.

**Agentic sessions** are inference-perf's `conversation_replay`, the closest
shape llm-d-benchmark expresses: it has no subagent fan-out, and it drains
in-flight requests at the phase boundary. The report states both.

## 5.3 Procedure

The [`gpu-evaluation` skill](../../.claude/skills/gpu-evaluation/SKILL.md)
runs these steps in this order, with the commands.

1. **Confirm the cluster** with the user before the first write: which
   context, and whether its GPUs may be used for the duration (about 8.8 h for
   the runs, plus standup). Never switch the user's context.
2. **Render and check locally.** `standup --dry-run`; read the rendered
   ScaledObject (placeholders resolved, the thresholds intended). Check §5.1.
3. **Stand up** (§5.2). Resolve and record every image digest, the KEDA
   release and the vLLM arguments, and create the summary with every run
   *pending* (run-set.md §3).
4. **Gate before load.** The ScaledObject is `READY=True`, the HPA shows real
   values, and a one-minute smoke load moves every trigger off 0. An
   `or vector(0)` guard turns a broken metric into a zero signal, and a run
   against it measures nothing.
5. **For each run**, in the run set's order:
   1. arm the plant (§5.2);
   2. run the profile with `llmdbenchmark run ... --monitoring`;
   3. **collect before anything is deleted**: the results directory, then the
      Prometheus range query of each trigger and of
      `kube_deployment_status_replicas_ready` over the run's window;
   4. **gate after the run** (run-set.md §4 and §5.5). A run that fails the
      gate for a reason outside the configuration (node eviction, image pull,
      a dead client) is **discarded**: rerun it and list the discarded
      attempt with its reason;
   5. **report it** (run-set.md §3 and §5.4) and commit, before arming the
      next run.
6. **Close the summary** when the last run is reported, or when the
   evaluation stops early, with the runs not done and why.
7. **Tear down**, also after a failed run: the releases, the namespace, and the
   cluster-scoped objects named for it.

## 5.4 What the reports add

Each run's directory (run-set.md §3) also holds:

```text
benchmark_report_v0.2,_<...>.yaml         # llm-d-benchmark's report, unchanged
scaledobject.json                         # the live ScaledObject during the run
profile.yaml                              # the inference-perf profile as run
```

`triggers/<trigger>.json` are Prometheus range queries of each trigger as the
live object states it. The header table adds the namespace, the cluster
version, the overlay, the image digests, the KEDA release, the profile and
trace, the session, and the start and end times (UTC). The per-stage table is
aligned on the run's trace (`stage_table.py --trace`).

## 5.5 Gate after each run, on a cluster

On top of run-set.md §4:

| Check | Source | Fails when |
|---|---|---|
| the generator kept up | `results.observability.load_generator.schedule_delay_seconds` | the delay grows through a stage: the run was not open loop |
| no infrastructure fault | pod events in the namespace during the run | an eviction, OOM kill or image pull failure of a serving pod |
| placed in time | `results.observability.load_generator` | `unavailable` |

## 5.6 What step 5 does not settle

- Efficiency against a perfect-foresight oracle (η) and its waste breakdown:
  a later step, from the same runs.
- Run-to-run variance: one run per shape. A claim that two configurations
  differ by less than the variance of repeated runs needs repeated runs.
- Other plants: μ_SLO, cold start and every latency are for the model, GPU,
  vLLM arguments and mix in the header, and do not transfer.
