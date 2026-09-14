---
name: run-benchmark
description: Run an llm-d-benchmark scenario from this repo's benchmark/ directory — the autoscaling test bed under benchmark/config/ (specifications, backend-agnostic scenarios, and cluster-config backend overlays). Drives the llmdbenchmark CLI through the standup → smoketest → run → teardown lifecycle against a Kubernetes cluster, then optionally renders the per-session Grafana/HTML report. Use when the user wants to run, stand up, smoke-test, or tear down a benchmark scenario (e.g. pd-disaggregation, lws-pd-disaggregation, a staging KEDA experiment), verify a spec + backend compose with --dry-run, or produce a session report. Invoke with /run-benchmark [spec] [backend] [-p namespace] or in response to natural-language requests.
---

# Run llm-d-benchmark

**Arguments:** `$ARGUMENTS` — free-form. May contain a spec name, a backend, a
namespace (`-p <ns>` / `--namespace <ns>`), a lifecycle phase (`dry-run`,
`standup`, `smoketest`, `run`, `teardown`, `report`, or `all`), and a
harness/workload for `run`. Anything missing is resolved in Step 1.

## What this skill does

The [`benchmark/`](../../../benchmark) directory is an **autoscaling test bed**
layered on [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark). It is
**not** wired into the Makefile — it is driven directly by the upstream
`llmdbenchmark` CLI (installed from a sibling `../llm-d-benchmark` clone) with
two repo-local inputs:

- `--spec benchmark/config/specification/{guides,staging}/<name>.yaml.j2` — a
  thin Jinja entrypoint that points at a scenario + upstream defaults/templates.
- `--cluster-config benchmark/config/cluster-configs/<backend>.yaml` — a
  swappable backend overlay (real vLLM, vLLM-CPU-sim, or inference-sim).
- `--workspace benchmark/results` — every session lands under this repo-local
  directory instead of a scattered temp dir.

`benchmark/README.md` is the source of truth for the directory's design; read it
if anything here is ambiguous. This skill encodes the **operational workflow and
guardrails** for running one.

## Reference

### Specifications (`--spec`)

| `--spec` path | Maturity |
|---|---|
| `benchmark/config/specification/guides/pd-disaggregation.yaml.j2` | recommended |
| `benchmark/config/specification/staging/pd-disaggregation/baseline.yaml.j2` | experiment (control) |
| `benchmark/config/specification/staging/pd-disaggregation/queue-aggressive.yaml.j2` | experiment |
| `benchmark/config/specification/staging/pd-disaggregation/kv-early.yaml.j2` | experiment |
| `benchmark/config/specification/staging/lws-pd-disaggregation.yaml.j2` | WIP |

Run `ls benchmark/config/specification/**/*.yaml.j2` to pick up specs added since
this was written. A bare name like `pd-disaggregation` or
`staging/pd-disaggregation/kv-early` maps to the matching `.j2`.

### Backends (`--cluster-config`)

| `--cluster-config` path | Runtime | GPU | Notes |
|---|---|---|---|
| `benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml` | real vLLM (CPU) + simulation plugin | no | **default for local/Kind**; native `vllm:` metrics; per-model |
| `benchmark/config/cluster-configs/k8s/inference-sim.yaml` | llm-d-inference-sim | no | fake server; lowest fidelity |
| `benchmark/config/cluster-configs/ocp/vllm.yaml` | real vLLM | yes | GPU cluster only |

All three expose native `vllm:` metrics, so a scenario's KEDA strategy
(triggers/thresholds/scaleTargetRef) is unchanged across backends. All three
overlays now set `keda.prometheus` (baseUrl/port/authMode/secretName only) to
route through the Thanos Querier route with bearer-token auth — this cluster
has no plain-HTTP `monitoring` namespace, so the scenario's default
`prometheus-operated.monitoring...` address never resolves. See the
`KEDA PROMETHEUS AUTH` comment block in any cluster-config file for the
one-time per-namespace secret-creation command this still requires.

### Lifecycle commands

`standup` → (`smoketest`) → verify ScaledObjects → `run` → `teardown`. **The
same `--spec`, `--cluster-config`, `--workspace benchmark/results`, and `-p
<namespace>` must be passed to every command in the lifecycle.** Add
`--dry-run` / `-n` to any of them to render locally without touching the
cluster. ScaledObject verification is a plain `kubectl` check, not a CLI
subcommand — see Step 7.

### Common `run` workloads (`-l inference-perf -w <workload>`)

`sanity_random.yaml` (quick sanity), `guide_pd-disaggregation_1.yaml`,
`shared_prefix_synthetic.yaml`, `chatbot_synthetic.yaml`. Profiles live in
`../llm-d-benchmark/workload/profiles/<harness>/`; list them if the user needs
options. Other harnesses: `guidellm`, `vllm-benchmark`, `aiperf`.

## Workflow

### Step 1 — Resolve inputs

From `$ARGUMENTS` and the conversation, settle:

1. **Spec** — default `guides/pd-disaggregation` if the user gave none and only
   one guide exists; otherwise ask which.
2. **Backend** — default `ocp/model-sim-qwen3-32b` for a local/Kind or non-GPU
   cluster; `ocp/vllm` only if the user says they have GPUs. Confirm if unsure.
3. **Namespace** (`-p`) — **required**. Never guess; ask if not supplied.
4. **Phase(s)** — `dry-run`, `standup`, `smoketest`, `run`, `teardown`,
   `report`, or `all` (dry-run → standup → smoketest → verify ScaledObjects →
   run → report, stopping on failure; teardown only on explicit request).
5. **Harness + workload** (for `run`) — default `-l inference-perf -w
   sanity_random.yaml`.

Restate the resolved plan in one line before proceeding.

### Step 2 — Put `llmdbenchmark` on PATH

Run everything **from the repo root**
(`/Users/villardl/Projects/github.com/llm-d/llm-d-autoscaling`).
The CLI comes from the sibling clone's venv:

```bash
source ../llm-d-benchmark/.venv/bin/activate
llmdbenchmark --version
```

If that fails: the sibling clone or its venv is missing. Per `benchmark/README.md`:

```bash
git clone https://github.com/llm-d/llm-d-benchmark.git ../llm-d-benchmark
cd ../llm-d-benchmark && ./install.sh && cd -
```

(If the clone lives elsewhere, activate that venv and be ready to pass
`--base-dir <path-to-clone>` so the spec's sibling paths resolve.)

### Step 3 — Preflight the cluster (skip for `dry-run` only)

Anything past `--dry-run` writes to a cluster. **Confirm the target with the
user before the first write:**

```bash
kubectl config current-context
kubectl cluster-info
```

For KEDA-based scenarios (all current ones), verify the dependencies the
scenario expects (see `benchmark/README.md` "Prepare a Kubernetes cluster"):

```bash
kubectl get deploy -n monitoring        # kube-prometheus-stack (prometheus-operated)
kubectl get deploy -n keda              # KEDA >= 2.20
kubectl get deploy -n openshift-keda    # OpenShift custom-metrics-autoscaler, if the above is empty
```

If missing, surface the install commands from `benchmark/README.md`; don't
install them silently. A local Kind cluster is `make create-kind-cluster`.

### Step 4 — Dry-run composition check (always, before any standup)

```bash
llmdbenchmark standup \
  --spec <spec.j2> \
  --cluster-config <backend.yaml> \
  --workspace benchmark/results \
  -p <namespace> --dry-run
```

Then inspect the rendered merge under the printed session directory
(`plan/<scenario>/config.yaml`) and confirm:

- backend fields came from the chosen overlay — `image`, `accelerator.count`
  (0 for CPU sims), per-role `resources`, `initContainers`, the vLLM command,
  any `extraObjects`;
- lists appear **in full** (the harness replaces lists wholesale, deep-merges
  dicts);
- the scenario's `keda` block is unchanged.

Report what you checked. Stop here if the phase was `dry-run`.

### Step 5 — Standup  *(cluster write — confirm first)*

```bash
llmdbenchmark standup \
  --spec <spec.j2> \
  --cluster-config <backend.yaml> \
  --workspace benchmark/results \
  -p <namespace>
```

The **last line of output is the workspace path** — record it. Runs live under
`benchmark/results/<user>-<timestamp>/`.

### Step 6 — Smoketest (recommended)

```bash
llmdbenchmark smoketest \
  --spec <spec.j2> --cluster-config <backend.yaml> \
  --workspace benchmark/results -p <namespace>
```

Per-scenario checks that the deployed pods match the scenario (resources,
parallelism, env, probes, routing, vLLM flags).

### Step 7 — Verify ScaledObjects are healthy (always, before `run`)

The CLI's `smoketest` checks pods, not autoscaling. A ScaledObject can be
silently broken (bad PromQL, label mismatch, auth failure) while pods are
Ready — the benchmark would then run against a scenario that never scales.
Gate `run` on all of the following:

```bash
kubectl get scaledobject -n <namespace> -o wide
```

Every ScaledObject in the namespace (prefill/decode queue+KV triggers, plus
any `epp-` saturation ones the scenario renders) must show
`READY=True`. `ACTIVE=False` is normal when idle (traffic is below
threshold) — that is not a failure. `READY=False`/blank is.

```bash
kubectl describe scaledobject <name> -n <namespace>   # for any not READY
```

Read the `Conditions` block for the actual error (bad trigger config, can't
reach the metrics backend, etc).

```bash
kubectl get hpa -n <namespace>
```

Check the `TARGETS` column on the KEDA-managed HPA(s) — `<unknown>/<target>`
means KEDA/the metrics adapter cannot fetch the metric at all, even if the
ScaledObject itself reports Ready. This is the most common "looks fine but
isn't" failure mode.

If HPA targets are `<unknown>`, check the operator logs for the underlying
error (Prometheus query failure, DNS, auth). KEDA's namespace varies by
distribution — try both:

```bash
kubectl logs -n keda deploy/keda-operator --tail=100 | grep -iE "error|fail"
kubectl logs -n openshift-keda deploy/keda-operator --tail=100 | grep -iE "error|fail"   # OpenShift custom-metrics-autoscaler
```

If logs point to the query itself, confirm the trigger's PromQL actually
returns data by running the exact query (label values included, e.g.
`model_name="<model.name>"`) against Prometheus directly — a label mismatch
between the scenario's `model.name` and what vLLM actually exports is the
usual culprit (see the `model.name` gotcha below).

**Do not proceed to Step 8 until every ScaledObject is `READY=True` and its
HPA reports real (non-`<unknown>`) current metric values.** Report what you
checked either way.

### Step 8 — Run a workload

```bash
llmdbenchmark run \
  --spec <spec.j2> --cluster-config <backend.yaml> \
  --workspace benchmark/results -p <namespace> \
  -l inference-perf -w sanity_random.yaml
```

Record the session directory from the output
(`benchmark/results/<user>-<timestamp>/`); results land in its `results/`
subdir. To benchmark again with a different workload, re-run `run` with
another `-w` — no need to re-standup.

### Step 9 — Report (optional but encouraged)

For a quick look at what's happening across every session (including this one
while it's still in progress), no Grafana needed:

```bash
benchmark/hack/benchmark_report.sh serve --workspace benchmark/results
```

then open `http://127.0.0.1:8787/`. It re-scans the workspace on every
refresh, so it works mid-standup or mid-run, not just after `teardown`. Each
experiment's **Grafana** section has a **Live dashboard** link (time-boxed to
that run's window and namespace, so it shows only that run's data) and, until a
snapshot exists, a **Create Grafana snapshot** button — both need a local
Grafana at `http://localhost:3000` (see below), no CLI step required.

`benchmark/hack/benchmark_report.sh` also renders a standalone `report.html`
for one finished session with lifecycle metrics + Grafana links, and captures a
**Grafana snapshot** that survives Prometheus retention. It manages its own
venv.

One-time: a local Grafana at `http://localhost:3000` and a port-forward to the
cluster's Prometheus (`kubectl port-forward -n monitoring svc/prometheus-operated
9090:9090`), then `benchmark/hack/benchmark_report.sh configure`. See
`benchmark/docs/benchmark-report.md`.

After a benchmark run, with the port-forward still up:

```bash
benchmark/hack/benchmark_report.sh all <session_dir>
```

`<session_dir>` is the top-level `benchmark/results/<user>-<timestamp>/`. If Grafana isn't
running the snapshot step is skipped and `report.html` still renders (noting the
gap).

### Step 10 — Teardown  *(cluster write — only on explicit request)*

```bash
llmdbenchmark teardown \
  --spec <spec.j2> --cluster-config <backend.yaml> \
  --workspace benchmark/results -p <namespace>
```

To redeploy after editing a scenario/overlay: **teardown, then standup** (no
in-place update).

## Gotchas

- **Same flags every phase.** A `-p` / `--spec` / `--cluster-config` /
  `--workspace` mismatch between standup and teardown orphans resources.
- **Run from the repo root**, with the sibling clone's venv active. Nothing
  under `benchmark/config/` is vendored — it references `../llm-d-benchmark`.
- **`model.name`** in a scenario must equal the vLLM `--served-model-name` and
  the HuggingFace id (it is the harness's request/tokenizer id); `shortName` is
  the k8s label. Don't "fix" one without the others.
- **KEDA triggers live in the scenario; connection/auth lives in the backend
  overlay.** `keda.scaledObjects` (triggers, thresholds, scaleTargetRef) must
  only come from the scenario. `keda.prometheus` (baseUrl/port/authMode/
  secretName) is cluster-dependent and belongs in the `--cluster-config`
  overlay — all three overlays set it for this cluster's Thanos Querier.
  If a backend overlay grows a `keda.scaledObjects` block, that's a bug.
- **KEDA's own namespace varies by distribution.** Upstream KEDA installs into
  `keda`; OpenShift's custom-metrics-autoscaler operator installs into
  `openshift-keda` instead. The Step 3 dependency check and the Step 7
  ScaledObject check both need to look in whichever one is actually present.
- **`ocp/vllm.yaml` backend needs real GPUs.** Default to `ocp/model-sim-qwen3-32b`
  for CPU/Kind.
- Confirm `kubectl config current-context` before every cluster write; treat
  namespace as required input, never a default.

## Staging KEDA experiments

`staging/<guide>/` holds scaling-strategy variants: `baseline.yaml` is the
control (verbatim copy of the guide's `keda:`), each `<strategy>.yaml` changes
**only** the `keda:` block. Run one exactly like a guide, pointing `--spec` at
its `.j2`. Compare a variant against `baseline` on the same backend; promote a
winner by copying its `keda:` block into `scenarios/guides/<guide>.yaml`.

## DoE sweeps

For parameter sweeps (e.g. over `keda` min/max or workload rate), the CLI has
`llmdbenchmark experiment` (auto standup/run/teardown per treatment) and
`llmdbenchmark results` (store/query/diff/pull). Reach for these when the user
asks for a sweep rather than a single run.
