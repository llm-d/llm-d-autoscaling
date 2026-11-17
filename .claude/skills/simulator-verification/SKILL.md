---
name: simulator-verification
description: Run step 2 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — prove on a real cluster, with a vLLM simulator instead of GPUs, that each failure the step 1 verdict predicted for a KEDA scenario actually shows up (or show that step 1 was wrong), and write the verdict with SVG plots drawn from captured data. Use when asked to verify, reproduce, confirm or test a step 1 / static-signal-analysis verdict on a cluster, to run a staging scenario against vllm-model-sim or llm-d-inference-sim, or to check a proposed fix to a scaling trigger. Invoke with /simulator-verification <step 1 verdict.md> [backend overlay] [--context <kube-context>].
---

# Simulator verification

**Arguments:** `$ARGUMENTS`: a step 1 verdict under
`benchmark/protocol/verdicts/<guide>/<strategy>.md`, optionally a backend
overlay under `benchmark/config/cluster-configs/` (if none is given, use the
one the verdict names), and optionally `--context <kube-context>`, the
Kubernetes context to run against. If no context is given, use the current
one (`kubectl config current-context`).

The procedure lives in the protocol, not here. Read it first and cite its
sections instead of restating them:

- [`benchmark/protocol/2-simulator-verification.md`](../../../benchmark/protocol/2-simulator-verification.md):
  which simulator for which property (§2.1), simulator gaps (§2.2), the
  procedure (§2.3), the verdict (§2.4), plots (§2.5);
- [`benchmark/protocol/1-static-signal-analysis.md`](../../../benchmark/protocol/1-static-signal-analysis.md)
  for the properties P1–P9 being tested, and
  [`benchmark/protocol/traps.md`](../../../benchmark/protocol/traps.md);
- the `run-benchmark` skill for the standup, smoketest, run and teardown
  commands.

This skill adds what the protocol cannot say in the abstract: the order to do
things in and the gates.

## Step 0: Get the cluster decision from the user

Everything after this writes to a cluster: namespaces, ClusterRoleBindings,
Secrets, Deployments, load. A write succeeding does not mean the cluster is
meant for it, so do not infer permission from `kubectl config
current-context` and a working `kubectl create`. Whether the context came from `--context` or is the current one, tell the
user which it is and ask whether that cluster accepts experiments, before the
first write. When `--context` is given, pass it to every `kubectl`/`oc`
command (`--context`) and make sure `llmdbenchmark` extracts the same one;
never switch the user's current context. If the
user is not there to answer, stop after the read-only work (steps 1 and 2) and
say what is needed.

Never put the context or cluster name in a committed verdict. Record the
cluster version, the KEDA release and the namespace.

## Step 1: List the properties to test

Test every property P1–P9 for every trigger of every ScaledObject in the
scenario, including the ones step 1 marked `pass`, `n/a` or `measure`. A
passing prediction can be wrong, and a verdict that only revisits the failures
cannot say it holds. Build the list from §2.3 item 1: per property, the
experiment, what a pass looks like and what a fail looks like. If a property
cannot be shown with the chosen simulator, keep it on the list and mark it
*not reproducible here* with the reason in the verdict.

Order the experiments by cost. Static checks first (a PromQL query against
live series settles label and series claims in seconds: P7 and P9), then P4
from the HPA's arithmetic, then the load experiments (P1, P2, P3, P5, P8),
which need the knee from step 4.

Pick the simulator per §2.1. If the scenario's model has no tuned profile,
write the Simulator gaps entry now, not after the run.

## Step 2: Render and check locally

Run `llmdbenchmark standup --dry-run` with the scenario and overlay and read
the rendered `plan/<stack>/27_keda-scaledobjects.yaml`. Confirm the queries
are the ones step 1 analyzed, placeholders resolved. Dry-run is local and
safe. Check the EPP plugin config against the pinned router image too: the
router drops plugins between releases (`disagg-headers-handler` was removed
in v0.11.0), and a stale config crash-loops the EPP.

## Step 3: Stand up in a new namespace

Name it `step2-<scenario>-<YYYYMMDD>`; never reuse one. Pass the same
`--spec`, `--cluster-config`, `--workspace benchmark/results` and `-p` to
every command. The CLI expects `../llm-d-benchmark` next to the repository.

Gate on the run-benchmark skill's ScaledObject check before any load: every
ScaledObject `READY=True` and the HPA showing real values. A run against
ScaledObjects that cannot read Prometheus measures nothing.

## Step 4: Run each property as its own experiment

Calibrate the knee first when the simulator is `vllm-model-sim` and the claim
needs a load axis (§2.3 item 6). Then, one property at a time:

- Run a poller inside the cluster (a short-lived pod) that queries the
  trigger's exact PromQL and a ground-truth query every 15 s, and write the
  samples to a file **before** deleting the pod. Logs are gone with the pod.
- Take ground truth from `max(...)` or a per-job selector, not a bare
  `sum(... {pod=…})`: each pod can be scraped by two PodMonitor jobs, and
  `sum` then reads about twice the real value. Check
  `count by (job)(...)` before trusting a sum.
- Record replica counts with `kubectl get deploy` on the same clock as the
  samples, so scale events line up with the signal.
- Read the HPA (`kubectl get hpa`, `describe`) as well as Prometheus. The
  replica count alone cannot say which trigger decided.

## Step 5: Plot and write the verdict

Follow §2.4 and §2.5. Structure the verdict by property, P1 to P9, each
section holding a table with one row per trigger: step 1's result, the
measured observation, and the outcome. Every property appears, passing or not.

A **confirmed fail** needs concrete empirical proof, all three of:

- the measured values, as read: the query result, the HPA's
  current/target/desired, the replica counts, with load levels or timestamps;
- the arithmetic from those values to the property's number, with the
  scenario's own thresholds (for example `ε = ln(1878/102) / ln(…)`);
- an SVG plot drawn from the exported CSV, with a caption.

Reasoning from the source or the configuration is step 1's evidence. If the
run did not show the failure, the outcome is *not confirmed* (or *refuted*
when it shows the opposite), however convincing the argument. A pass carries
its measured number too, without a plot.

Build each SVG from the exported CSV with a script kept beside it, in
`verdicts/<guide>/<scenario>-step2/`. Then **render every SVG and look at it**
before committing: on macOS `qlmanage -t -s 900 -o . file.svg` makes a PNG to
read. Check specifically that the title, legend, annotations and event labels
do not overlap each other or the data. Do not commit the PNGs. Give each plot a
caption that states what varied, what was measured, what the plot shows and
whether it confirms the prediction, with the deciding number.

Outcomes are `confirmed pass`, `confirmed fail`, `refuted` or `not
reproducible here`. When a run exposes something outside the properties (an
unexplained scale event, a KV value above 1.0), record it as unexplained under
*needs measurement*; do not invent an explanation.

When the claim is that a fix works, add the fix as a sibling scenario (for
example `<strategy>-<fix>.yaml` and its `.yaml.j2`), leave the original
scenario unchanged so its verdicts stay true, and re-run in a fresh teardown
and standup.

## Step 6: Tear down

Run `llmdbenchmark teardown` with the same flags, delete the namespace, and
delete cluster-scoped leftovers named for it
(`kubectl get clusterrolebinding | grep <namespace>`). Do this after a failed
or abandoned run too. If the user wants the namespace kept for a follow-up,
say so in the verdict and give the teardown commands. Never delete a
namespace this run did not create.

Before finishing, check the verdict against §2.4: all nine properties appear
for every trigger, each with an outcome, every confirmed fail has values,
arithmetic and a rendered, captioned plot, the Simulator gaps section exists,
and no cluster name appears.
