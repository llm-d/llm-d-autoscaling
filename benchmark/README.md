# Benchmark Specs

This directory is an **autoscaling test bed** built on top of
[llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark). It holds
`llm-d-benchmark` specifications and scenarios for exercising this repository's
autoscaler, organized by guide and scaling strategy, and runnable across several
inference backends.

## Relationship to `llm-d-benchmark`

Upstream `llm-d-benchmark/config/` is the source of **curated, recommended**
general benchmark configs and of the shared machinery (Jinja templates and the
base `defaults.yaml`). This directory:

- **Reuses upstream by reference.** Each specification points `template_dir` and
  `values_file` at a sibling `../llm-d-benchmark` clone — nothing is vendored, so
  there is no drift.
- **Extends it with autoscaling.** Scenarios layer recommended KEDA scaling
  strategies on top of the upstream deployment topologies.
- **Stages work in progress.** Unlike upstream (recommended only), this repo
  keeps both recommended configs (`guides/`) **and** experimental ones
  (`staging/`).

## Directory layout

```
benchmark/
└── config/
    ├── specification/        # thin .j2 entrypoints — point llmdbenchmark at these
    │   ├── guides/           #   recommended
    │   └── staging/          #   WIP / test bed
    ├── scenarios/            # BACKEND-AGNOSTIC deployment + scaling strategy
    │   ├── guides/           #   recommended (one file per guide)
    │   └── staging/          #   WIP + KEDA experiments (one dir per guide)
    │       └── pd-disaggregation/
    │           ├── baseline.yaml         # control = recommended strategy
    │           ├── queue-aggressive.yaml # variant (<strategy>.yaml)
    │           ├── kv-early.yaml          # variant
    │           └── token-aware.yaml       # variant
    └── cluster-configs/      # swappable backend overlays (--cluster-config), grouped by platform
        ├── k8s/
        │   └── inference-sim.yaml       #   llm-d-inference-sim
        └── ocp/
            ├── vllm.yaml                #   real vLLM, GPU
            └── model-sim-qwen3-32b.yaml # real vLLM (CPU) + latency-simulation plugin (per-model)
```

- **Scenarios are backend-agnostic.** A scenario describes the deployment
  topology and the scaling strategy (KEDA min/max, behavior, metric triggers),
  but not the inference backend.
- **Backend is a swappable overlay** applied with `--cluster-config`. Because the
  harness deep-merges dicts but **replaces lists wholesale**, each overlay carries
  the *complete* backend-specific lists (image, resources, init containers, env,
  volumes, vLLM command). Overlays never set `keda` (the scaling strategy stays
  with the scenario, whose `scaleTargetRef.kind` is topology-specific).

### Backend matrix

| Backend (`--cluster-config`)                | Runtime                              | GPU | Metrics source        | Priority |
|----------------------------------------------|--------------------------------------|-----|-----------------------|----------|
| `cluster-configs/ocp/vllm.yaml`               | real vLLM                             | yes | native `vllm:` metrics | high     |
| `cluster-configs/ocp/model-sim-qwen3-32b.yaml`| real vLLM (CPU) + simulation plugin  | no  | native `vllm:` metrics | high     |
| `cluster-configs/k8s/inference-sim.yaml`      | llm-d-inference-sim (fake server)   | no  | native `vllm:` metrics | low      |

`vllm` and `model-sim-*` are the priority pair (the `model-sim` overlay is
per-model, e.g. `model-sim-qwen3-32b.yaml`). All three expose native `vllm:`
metrics, so the scenario's scaling strategy applies unchanged across backends.

> Note: `--cluster-config` is a single-use flag that upstream documents for
> user-local cluster constants (storageClassName/serviceAccount/runAsUser). Here
> it doubles as the backend selector. If you also need per-cluster constants,
> fold them into the chosen backend overlay or pass them with repeatable `--set`.

## KEDA experiments (naming convention)

To try alternative KEDA scaling strategies for a guide, add scenarios under a
per-guide directory in `staging/`. The topology stays fixed; only the `keda:`
block varies between siblings, so results isolate the scaling strategy.

```
scenarios/staging/<guide>/<strategy>.yaml
specification/staging/<guide>/<strategy>.yaml.j2   # thin mirror, points at the scenario
```

Rules:

- **`baseline.yaml`** is the control — a verbatim copy of the recommended
  strategy in `scenarios/guides/<guide>.yaml`. Every variant is compared against
  it.
- **Each variant is `<strategy>.yaml`**, a short kebab-case name describing what
  the strategy does — e.g. `queue-aggressive.yaml`, `kv-early.yaml`. The file's
  header comment records the exact delta from `baseline.yaml`.
- **Change only the `keda:` block** in a variant; keep everything else identical
  to `baseline.yaml`. The one admissible exception is a trigger whose *metric
  does not exist* under baseline — then add the minimum needed to publish it and
  say so in the header. `token-aware.yaml` does this: its prefill trigger reads
  `llm_d_epp_inflight_tokens`, so it adds the EPP `inflight-load-producer`
  plugin (a producer, referenced from no scheduling profile, so routing is
  unchanged).
- **Promote a winner** by copying its `keda:` block back into
  `scenarios/guides/<guide>.yaml`; retire the losing variants.

Run an experiment exactly like a guide, pointing `--spec` at its `.j2`:

```bash
llmdbenchmark standup \
  --spec benchmark/config/specification/staging/pd-disaggregation/queue-aggressive.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results \
  -p <namespace>
```

## Prerequisites

### Install the `llm-d-benchmark` CLI

Clone `llm-d-benchmark` as a sibling of this repo and install the CLI:

```bash
git clone https://github.com/llm-d/llm-d-benchmark.git ../llm-d-benchmark
cd ../llm-d-benchmark && ./install.sh
```

Then activate the virtual environment so `llmdbenchmark` is on your PATH:

```bash
source ../llm-d-benchmark/.venv/bin/activate
```

If you cloned `llm-d-benchmark` somewhere other than the default sibling
location, activate its venv accordingly and pass `--base-dir` so the specs can
resolve the sibling paths.

### Prepare a Kubernetes cluster (local Kind, CPU backends)

1. Create the cluster:

   ```bash
   make create-kind-cluster
   ```

2. Install Prometheus (required for autoscaler metrics):

   ```bash
   helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
   helm upgrade --install prometheus prometheus-community/kube-prometheus-stack \
     --namespace monitoring --create-namespace \
     --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
     --set-json 'prometheus.prometheusSpec.serviceMonitorSelector={}' \
     --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
     --set-json 'prometheus.prometheusSpec.podMonitorSelector={}'
   ```

   This exposes Prometheus at
   `http://prometheus-operated.monitoring.svc.cluster.local:9090`, which the
   scenarios are pre-configured to use. If you install into a different namespace
   or release name, update `keda.prometheus.baseUrl` in the scenario file.

3. Install KEDA 2.20 or later (required for autoscaler triggers):

   ```bash
   helm repo add kedacore https://kedacore.github.io/charts
   helm install keda kedacore/keda -n keda --create-namespace
   ```

## Running a benchmark

Pick a specification (`guides/` or `staging/`) and a backend overlay. Run
`llmdbenchmark` from the repo root after activating the venv:

```bash
# Standup — vLLM-CPU-sim backend (no GPU)
llmdbenchmark standup \
  --spec benchmark/config/specification/guides/pd-disaggregation.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results \
  -p <namespace>

# Run
llmdbenchmark run \
  --spec benchmark/config/specification/guides/pd-disaggregation.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results \
  -p <namespace> -l inference-perf -w guide_pd-disaggregation_1.yaml

# Teardown
llmdbenchmark teardown \
  --spec benchmark/config/specification/guides/pd-disaggregation.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results \
  -p <namespace>
```

Swap the `--cluster-config` value to target a different backend (e.g.
`.../cluster-configs/k8s/inference-sim.yaml` or, on a GPU cluster,
`.../cluster-configs/ocp/vllm.yaml`). Use `--dry-run` / `-n` to preview what would be
applied without touching the cluster.

`--workspace benchmark/results` keeps every session under a single repo-local
directory (`benchmark/results/<user>-<timestamp>/`) instead of scattering
them across the filesystem in temp dirs. Pass the same value to every command
in a lifecycle (standup/smoketest/run/teardown).

### Verifying composition without a cluster

`--dry-run` renders the merged config and manifests locally, so you can confirm a
spec + backend overlay compose as intended before touching a cluster:

```bash
llmdbenchmark standup \
  --spec benchmark/config/specification/guides/pd-disaggregation.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/ocp/model-sim-qwen3-32b.yaml \
  --workspace benchmark/results \
  -p bench --dry-run
```

Inspect the rendered `plan/pd-disaggregation/config.yaml` under the session directory
and check the backend-specific values match the chosen overlay — image,
`accelerator.count`, resources, `initContainers`, the vLLM command, and any
`extraObjects`. Because the harness deep-merges dicts but replaces lists wholesale,
each overlay's lists should appear in full and the scenario's `keda` block should
be unchanged.

## Status

| Spec | Maturity | Verified |
|------|----------|----------|
| `guides/pd-disaggregation` | recommended | dry-run render across all three backends |
| `staging/pd-disaggregation/baseline` | experiment (control) | dry-run render |
| `staging/pd-disaggregation/queue-aggressive` | experiment | not yet run |
| `staging/pd-disaggregation/kv-early` | experiment | not yet run |
| `staging/lws-pd-disaggregation` | WIP / test bed | dry-run render only |

Dry-run composition is confirmed for the three backend overlays. A live
end-to-end run on a cluster (standup → workload → teardown) has not yet been
exercised for these specs.

## Observability

`benchmark/hack/benchmark_report.py serve` runs an interactive dashboard,
organized spec-first: a landing page listing every spec under
`benchmark/config/specification/`, drilling into each spec's historical and
in-progress sessions (with per-session stage/log status and per-experiment
latency/goodput charts) plus a form to standup/run/teardown that spec with a
chosen cluster-config + harness/workload, without leaving the browser. See
[`docs/interactive-dashboard.md`](docs/interactive-dashboard.md).

Each session can also get a standalone HTML summary (`report.html`) with
links to Grafana panels for that session's benchmark-run time window --
vLLM engine metrics, llm-d EPP (router) metrics, and the replica counts the
scaling strategy is judged on -- including a permanent snapshot that survives
Prometheus data retention. See [`docs/benchmark-report.md`](docs/benchmark-report.md).
