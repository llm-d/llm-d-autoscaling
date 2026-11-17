---
name: simulator-verification
description: Run step 2 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — prove on a real cluster, with a vLLM simulator instead of GPUs, that each failure the step 1 verdict predicted for a KEDA scenario actually shows up (or show that step 1 was wrong), and write the verdict with SVG plots drawn from captured data. Use when asked to verify, reproduce, confirm or test a step 1 / static-signal-analysis verdict on a cluster, to run a staging scenario against vllm-model-sim or llm-d-inference-sim, or to check a proposed fix to a scaling trigger. Invoke with /simulator-verification <step 1 verdict.md> [backend overlay] [--context <kube-context>].
---

# Simulator verification

**Arguments:** `$ARGUMENTS`: a step 1 verdict under
`benchmark/protocol/verdicts/<guide>/<strategy>.md`, optionally a backend
overlay under `benchmark/config/cluster-configs/` (if none is given, use the
one the verdict names), and optionally `--context <kube-context>`, the
Kubernetes context to run against (default: the current one).

The rules live in the protocol, not here. Read it first and apply it; this
skill adds only the order, the gates and the commands:

- [`benchmark/protocol/2-simulator-verification.md`](../../../benchmark/protocol/2-simulator-verification.md):
  which properties to test and how (§2.3), the simulator (§2.1, §2.2), the
  verdict and its proof (§2.4), plots (§2.5);
- [`benchmark/protocol/1-static-signal-analysis.md`](../../../benchmark/protocol/1-static-signal-analysis.md)
  for P1–P9, and [`benchmark/protocol/traps.md`](../../../benchmark/protocol/traps.md);
- the `run-benchmark` skill for the standup, smoketest, run and teardown
  commands.

## Step 0: Confirm the cluster with the user

Everything after the dry-run writes to a cluster. A write that succeeds does
not mean the cluster is meant for experiments, so tell the user which context
will be used, whether it came from `--context` or is the current one, and ask
whether it accepts experiments before the first write. Pass `--context` to
every `kubectl` and `oc` command and make sure `llmdbenchmark` extracts the
same one. If the user is not there to answer, stop after step 1 and say what
is needed.

## Step 1: Plan and render, locally

List the properties to test (§2.3 item 1) and pick the simulator (§2.1). If
the scenario's model has no tuned profile, write the Simulator gaps entry now.

Order the experiments by cost: static checks first (a PromQL query against live
series settles P7 and P9 in seconds), then P4 from the HPA's arithmetic, then
the load experiments (P1, P2, P3, P5, P8), which need the knee (§2.3 item 5).

Run `llmdbenchmark standup --dry-run` with the scenario and overlay and read
the rendered `plan/<stack>/27_keda-scaledobjects.yaml`: the queries must be the
ones step 1 analyzed, placeholders resolved. Check the EPP plugin config
against the pinned router image too; the router drops plugins between releases
(`disagg-headers-handler` went in v0.11.0) and a stale config crash-loops the
EPP.

## Step 2: Stand up

Name the namespace `step2-<scenario>-<YYYYMMDD>`. Pass the same `--spec`,
`--cluster-config`, `--workspace benchmark/results` and `-p` to every command.
The CLI expects `../llm-d-benchmark` next to the repository. Apply the gate in
§2.3 item 4 with the `run-benchmark` skill's ScaledObject check.

## Step 3: Run each property as its own experiment

Calibrate the knee first when it applies (§2.3 item 5), then one property at a
time:

- Run a poller in the cluster (a short-lived pod) that queries the trigger's
  exact PromQL and a ground-truth query every 15 s, and write the samples to a
  file **before** deleting the pod. The logs go with the pod.
- Take ground truth from `max(...)` or a per-job selector, not a bare
  `sum(... {pod=…})`: a pod scraped by two PodMonitor jobs reads about twice
  its value under `sum`. Check `count by (job)(...)` before trusting a sum.
- Record replica counts with `kubectl get deploy` on the same clock as the
  samples, and read the HPA (`kubectl get hpa`, `describe`) as well.

## Step 4: Plot and write the verdict

Write the verdict as §2.4 prescribes, structured by property, with the proof
and plots it requires (§2.5). Build each SVG with a script kept beside the CSV,
in `verdicts/<guide>/<scenario>-step2/`. Render every SVG to look at it; on
macOS `qlmanage -t -s 900 -o . file.svg` makes a PNG. Do not commit the PNGs.
When a run shows something outside the properties (an unexplained scale event,
a KV value above 1.0), record it as unexplained under *needs measurement*; do
not invent an explanation.

When the claim is that a fix works, add the fix as a sibling scenario (for
example `<strategy>-<fix>.yaml` and its `.yaml.j2`), leave the original
unchanged so its verdicts stay true, and re-run after a fresh teardown and
standup.

## Step 5: Tear down

Run `llmdbenchmark teardown` with the same flags, then
`kubectl delete namespace <namespace>`, then delete the leftovers named for it
(`kubectl get clusterrolebinding | grep <namespace>`). If the user wants the
namespace kept for a follow-up, say so in the verdict and give the commands.

Before finishing, check the verdict against §2.4 and §2.5, and that no context
or cluster name appears in it.
