# Two-Variant WVA Benchmark

End-to-end guide for the **two-variant efficiency-aware scaling** benchmark: a
single model deployed as two variants of differing `variantCost` under one
shared `InferencePool` / EPP, used to exercise the WVA saturation V2 cost-aware
optimizer. The optimizer scales the **most efficient** variant — the one with
the best serving-capacity per unit cost — up first, not simply the cheapest.
With the shipped pricing (primary cost 10 / TP=2, secondary cost 5 / TP=1, so
the cost ratio equals the GPU/TP ratio) the **TP=2 primary is the more efficient
variant** and scales up first, while the cheaper TP=1 secondary absorbs
spillover. See the efficiency note in `variants/v2-tp1-cheaper.yaml` for why.

For cluster login, namespace setup, and HuggingFace token configuration, follow
[`benchmark-guide.md`](benchmark-guide.md) Steps 1–4 first; this document picks
up after those.

---

## Topology

One `InferencePool` and one EPP front two `vLLM` `Deployment`s of the same
model. Each `Deployment` has its own `VariantAutoscaling` (VA) and `HPA`, but
both VAs share the same `spec.modelID` so the WVA saturation engine groups
them and applies cost-weighted scaling.

```
            +------------- Gateway --------------+
            |  HTTPRoute -> InferencePool (1 EPP)|
            +-------------------------------------+
                              |
               +--------------+--------------+
               |                             |
     +---------+--------+        +-----------+-----+
     | vLLM decode      |        | vLLM decode     |
     | primary (cost 10)|        | secondary (5)   |
     | VA + HPA         |        | VA + HPA        |
     +------------------+        +-----------------+
                   ^                       ^
                   +-------- WVA ----------+
                              controller
```

### Label strategy (how both Deployments share one pool)

The `InferencePool` EPP selects pods by two camelCase labels:

```
llm-d.ai/inferenceServing: "true"
llm-d.ai/model:            <model-hash>
```

The primary `Deployment` (managed by the `llm-d-modelservice` chart) adds a
third selector label, kebab-case: `llm-d.ai/inference-serving: "true"`.

The secondary `Deployment` created by `add_variant.py`:

- **Keeps** `llm-d.ai/inferenceServing` + `llm-d.ai/model` so the pool picks
  up its pods.
- **Omits** `llm-d.ai/inference-serving` so the primary `Deployment` does
  not claim secondary pods.
- **Adds** `wva.llmd.ai/variant: <suffix>` (default `v2`) as the secondary
  `Deployment`'s own selector discriminator.

Both VAs additionally carry a `llm-d.ai/variant: <va-name>` pod label so
Prometheus can map each scrape target back to its VA (see "Required
relabeling" below).

---

## Required pieces

The benchmark only works end-to-end when **all** of these are in place. The
scenario yaml at [`hack/benchmark/scenarios/guides/two-variant-wva.yaml`](../../hack/benchmark/scenarios/guides/two-variant-wva.yaml)
pins versions that satisfy these requirements out of the box.

### 1. WVA chart and controller image at v0.8.0-rc5 or newer

The published chart and controller image must include:

- The `vllm-servicemonitor.yaml` template (propagates the per-pod
  `llm-d.ai/variant` label into scraped metrics as `llm_d_ai_variant`, gated
  by `wva.vllmService.enabled: true`). Without this, every reconcile prints
  `No saturation metrics available for model, skipping analysis` and the
  controller cannot scale.
- The `cache_config_info` collector fixes (PR #1198) so the V2 analyzer
  groups per-replica KV capacity correctly when more than one model lives
  in the cluster.

Both landed in `v0.8.0-rc1`. The scenario yaml pins both `chartVersions.wva`
and `wva.image.tag` to `v0.8.0-rc5`. `v0.7.0` and earlier are missing one or
both fixes — do not downgrade.

### 2. Saturation V2 enabled via configmap

Saturation V2 is not the default in upstream (the default was reverted in
PR #1286). The benchmark flow enables it via [`make benchmark-enable-v2-saturation`](#step-3--enable-saturation-v2)
which applies the ConfigMap at
[`hack/benchmark/scenarios/wva_threshold/wva_saturation_v2_config.yaml`](../../hack/benchmark/scenarios/wva_threshold/wva_saturation_v2_config.yaml)
and restarts the controller. Each reconcile should then print
`Processing model (V2)` in the controller log.

### 3. Newer vLLM image

The scenario yaml pins `docker.io/vllm/vllm-openai:v0.14.0`. This is required
because the default llm-d ships `v0.9.2` which does **not** emit
`vllm:cache_config_info` at all.

---

## How to run

Set `NS` to your namespace, then walk the steps below from the repo root.

### Step 0 — Install the llm-d-benchmark CLI (one-time)

The make targets shell out to `llmdbenchmark` from a checkout of
[`llm-d-benchmark`](https://github.com/llm-d/llm-d-benchmark). On a fresh
clone of this repo:

```bash
make benchmark-install
```

Idempotent — re-running checks out the pinned `BENCHMARK_REPO_REF` and
re-installs the CLI.

### Cluster prerequisites — check before Step 1

The standup installs `prometheus-adapter` via Helm into
`openshift-user-workload-monitoring`. On clusters where `prometheus-adapter`
is **already** running but **not** as a Helm release (the new
Kustomize-based WVA install does this, as do plain `kubectl apply`
deployments), the install fails because the cluster-scoped APIService
`v1beta1.external.metrics.k8s.io` is owned by the non-Helm install.

Run all three of these checks. If the answers match what's shown, you must
pass `BENCHMARK_SKIP_PROMETHEUS_ADAPTER=true` to **every** `benchmark-standup`
invocation in Step 1:

```bash
helm list -A | grep prometheus-adapter                    # → empty
kubectl get apiservice v1beta1.external.metrics.k8s.io    # → exists
kubectl get clusterrole prometheus-adapter-resource-reader # → NotFound
```

The flag creates a stub `prometheus-adapter-resource-reader` ClusterRole
annotated with Helm release metadata, which makes `llmdbenchmark`'s
existing-PA probe pass and the conflicting Helm install is skipped. The
cluster's existing PA continues to serve `wva_desired_replicas` to your
HPAs. Override the release-namespace annotation with
`WVA_MONITORING_NAMESPACE=<ns>` if your PA lives somewhere other than
`workload-variant-autoscaler-monitoring`.

If all three checks return the *opposite* (no APIService, ClusterRole
exists with Helm annotations, or a Helm release shows up), skip the flag —
the standup will install or upgrade PA cleanly.

### Step 1 — Stand up the primary variant

```bash
make benchmark-standup BENCHMARK_NAMESPACE=$NS \
                       BENCHMARK_SPEC=guides/two-variant-wva \
                       BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct
```

> **Model requirement — must be chat-template-bearing.** Use an
> instruct/chat-tuned model (the scenario default is
> `unsloth/Meta-Llama-3.1-8B-Instruct`). On llm-d-benchmark ≥ v0.7.0 the
> decode image ships transformers ≥ 4.44, which **rejects a model whose
> tokenizer defines no chat template** (i.e. base models such as
> `unsloth/Meta-Llama-3.1-8B`) with `ChatTemplateResolutionError`, and every
> request errors. To use a base model anyway, supply `--chat-template` or
> pin a transformers-< 4.44 decode image.

`benchmark-standup` copies two files into the `llm-d-benchmark` checkout:

- [`hack/benchmark/scenarios/guides/two-variant-wva.yaml`](../../hack/benchmark/scenarios/guides/two-variant-wva.yaml)
  — the scenario values (variant costs, replicas, image pins, HPA behavior).
- [`hack/benchmark/scenarios/guides/two-variant-wva.yaml.j2`](../../hack/benchmark/scenarios/guides/two-variant-wva.yaml.j2)
  — the specification wrapper that the `--spec` flag actually loads.

Standup then installs the `llm-d-infra`, `inferencepool-gaie`,
`modelservice`, and `workload-variant-autoscaler` Helm releases for the
chosen model with `variantCost: "10.0"`, `min/maxReplicas: 1/10`, and primary
`tensor: 2`. `BENCHMARK_MODEL_ID` is required — without it the standup
defaults to a placeholder dummy model.

### Step 2 — Add the secondary variant

```bash
make benchmark-add-variant BENCHMARK_NAMESPACE=$NS
```

This invokes `hack/benchmark/add_variant.py` against the variant config at
`hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml` (default —
override with `VARIANT_CONFIG=<path>`), creating a secondary `Deployment`,
`VariantAutoscaling`, and `HPA` named with the `v2` suffix and
`variantCost: "5.0"`.

Verify both VAs and HPAs are present:

```bash
kubectl get va,hpa -n $NS
kubectl get pods -n $NS -l 'llm-d.ai/inferenceServing=true,llm-d.ai/model=unsloth--6b24a594-instruct'
```

### Step 3 — Enable saturation V2

Saturation V2 is not the default upstream. Apply the ConfigMap and restart
the controller:

```bash
make benchmark-enable-v2-saturation BENCHMARK_NAMESPACE=$NS
```

Confirm the analyzer switched:

```bash
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler \
  --tail=200 | grep "Processing model"
```

You want `Processing model (V2)`, not `(V1)`.

### Step 4 — (Optional) Tune HPA scale-up

The shipped HPA matches the canonical WVA install samples
(`config/samples/hpa/...`): `scaleUp.stabilizationWindowSeconds: 0`
(immediate — the HPA follows WVA's optimizer decisions without damping)
and `scaleDown.stabilizationWindowSeconds: 120` (windowed, to avoid
flapping when a brief lull arrives). No patch is needed for responsive
scaling.

If you instead want to *damp* scale-up (e.g. to study the optimizer under
a slower actuator), patch the window up:

```bash
for hpa in unsloth--6b24a594-instruct-decode unsloth--6b24a594-instruct-decode-v2; do
  kubectl patch hpa -n $NS "$hpa" --type=json \
    -p '[{"op":"replace","path":"/spec/behavior/scaleUp/stabilizationWindowSeconds","value":120}]'
done
```

### Step 5 — Run the benchmark

The default workload is `test/benchmark/scenarios/prefill_heavy.yaml.in`.
Edit the file (`rate`, `max_seconds`, `prompt_tokens`, `output_tokens`)
before invoking — `make benchmark-run` copies the file at run-time, so the
value on disk at invocation is what gets used.

For multi-run comparisons, restart the controller between runs to flush k2
history (otherwise stale per-replica capacity estimates from the previous
run can poison the next):

`BENCHMARK_MODEL_ID` must be passed to `benchmark-run` as well — without it
the CLI defaults to `e2ewva/dummy-model` and fails model verification.

Pass `BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2` to enable two-variant
post-processing (per-variant replica rows, weighted cost column, and a
full-pipeline PNG plot). The GPU counts for the cost formula are read automatically
from the scenario and variant config YAMLs (`tensor:` field).

```bash
make benchmark-restart-controller BENCHMARK_NAMESPACE=$NS
make benchmark-run BENCHMARK_NAMESPACE=$NS BENCHMARK_SPEC=guides/two-variant-wva \
                   BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct \
                   BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2
# Override workload:
make benchmark-run BENCHMARK_NAMESPACE=$NS BENCHMARK_SPEC=guides/two-variant-wva \
                   BENCHMARK_MODEL_ID=unsloth/Meta-Llama-3.1-8B-Instruct \
                   BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2 \
                   BENCHMARK_WORKLOAD=symmetrical.yaml
```

Each run produces a workspace under `$REPO/$USER-<timestamp>/...` with raw
metrics, logs, and processed timeseries.

### Step 5a — Post-run output

When `BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX=v2` is set, `benchmark-run`
automatically produces two outputs after the run completes:

**Run `post_run_analyze.sh` promptly** (within a few minutes of run completion —
the WVA controller pod's log buffer rotates and the window for extracting
controller decisions closes):

```bash
bash hack/benchmark/post_run_analyze.sh <results-dir> $NS
# e.g. bash hack/benchmark/post_run_analyze.sh \
#   biran-20260704-135514-081/results/guidellm-1783162554-04wm0f_1 biran
```

This runs five steps: dumps WVA controller decisions + V2 saturation analysis
numbers from pod logs, computes capacity/demand estimates from raw vLLM/EPP
scrapes, extracts EPP throughput and WVA Prometheus timeseries, and renders the
full-pipeline PNG plot into `<results-dir>/metrics/graphs/`.

**Markdown table** (printed to stdout, copy-paste into `docs/benchmark.md`):

```
| Metric                                | Run 1 |
|---------------------------------------|-------|
| P99 TTFT (ms)                         | 601   |
| P99 ITL (ms/token)                    | 6.2   |
| Avg primary replicas                  | 5.84  |
| Max primary replicas                  | 10    |
| Avg secondary replicas                | 2.31  |
| Max secondary replicas                | 8     |
| Avg KV cache utilization              | 71.3% |
| Avg queue depth (EPP)                 | 12.4  |
| Error count                           | 0     |
| Avg pod startup (s)                   | 118   |
| Cost (weighted avg replicas × GPU/hr) | 14.31 |
```

The cost is `(primary_avg × 2 + secondary_avg × 1)` — weighted by TP (GPU)
count, read from the scenario and variant config YAMLs so no manual input
is needed.

**Full-pipeline PNG plot** saved to `<results-dir>/metrics/graphs/two_variant_v2_full_pipeline.png`.
Panels (up to 7, optional panels appear when data is present):
1. Replica count — ready (solid) + WVA desired (dashed) per variant
2. Estimated demand (stacked: in-use / vLLM waiting / EPP queue) vs capacity
3. KV cache utilisation (avg per variant)
4. Requests running (sum per variant)
5. vLLM requests waiting (sum per variant)
6. EPP queue metrics (flow-control queue, pool average, per-variant per-pod sum)
7. Gateway throughput (requests/s and errors/s from EPP counters)

To regenerate either output against an older results directory:

```bash
# Markdown table:
python3 hack/benchmark/postprocess.py \
    --secondary-suffix v2 \
    --scenario-yaml hack/benchmark/scenarios/guides/two-variant-wva.yaml \
    --variant-config hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml \
    <results-dir>

# Plot:
python3 hack/benchmark/plot_two_variant_pipeline.py <results-dir>
```

### Step 6 — Teardown

```bash
make benchmark-teardown BENCHMARK_NAMESPACE=$NS \
                        BENCHMARK_SPEC=guides/two-variant-wva
```

`BENCHMARK_SPEC` must be passed (the make target's default is the
`guides/workload-autoscaling` scenario, which the CLI can't find for
two-variant teardown).

This removes the four Helm releases and the secondary variant. The
secondary `Deployment` is created by `benchmark-add-variant` outside any
Helm release; the llm-d-benchmark teardown explicitly deletes orphaned
Deployments in the namespace, and `add_variant.py` sets `ownerReferences`
on the secondary `VariantAutoscaling` and `HPA` pointing at the secondary
`Deployment` so they cascade-delete with it.

---

## Verifying efficiency-aware behavior

In the controller log during sustained load you should see, ordered by
priority:

1. The **more efficient** variant scaling up first — the TP=2 primary at the
   shipped 10/5 pricing.
2. The less efficient variant joining only when the efficient one's
   `maxReplicas` cannot absorb demand alone.
3. On scale-down, the less efficient variant shrinking first.

Sample line (taken from a real run):

```
saturation/engine_v2.go:65   V2 saturation analysis completed
  modelID=unsloth/Meta-Llama-3.1-8B-Instruct totalSupply=2503400 totalDemand=2229945
  utilization=0.89 requiredCapacity=120065 spareCapacity=0
```

`totalSupply` should track the realized capacity (sum of `cache_config_info`
across ready pods) — typical values for Llama-3.1-8B / H100 are ~315k tokens
per pod, so ~3M tokens at 10+0 replicas. If you see numbers near 13k, the
collector fell back to the per-step batch budget — verify the chart and
controller image are both at `v0.8.0-rc5` or newer
([Required pieces #1](#1-wva-chart-and-controller-image-at-v080-rc5-or-newer)).

---

## Files involved

| Path | Role |
|---|---|
| `hack/benchmark/scenarios/guides/two-variant-wva.yaml` | Scenario / values for primary stack (cost 10, min/max 1/10, TP=2, HPA 100% per 15 s, vllmService enabled). Copied into the `llm-d-benchmark` checkout automatically by `make benchmark-standup`. |
| `hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml` | Default secondary-variant config (suffix `v2`, cost 5.0, TP=1) consumed by `make benchmark-add-variant`. Override path with `VARIANT_CONFIG=<path>`. |
| `hack/benchmark/add_variant.py` | Creates secondary `Deployment`/`VA`/`HPA` from primary, with the kebab-label trick. |
| `hack/benchmark/post_run_analyze.sh` | Wraps the five post-run dump+plot steps. Must run promptly after `benchmark-run` — the WVA controller log buffer rotates. Usage: `bash hack/benchmark/post_run_analyze.sh <results-dir> [namespace]`. |
| `hack/benchmark/dump_wva_target_timeseries.py` | Extracts WVA controller decisions and V2 saturation analysis numbers (supply, demand, utilization, required/spare capacity) from pod logs into `metrics/processed/wva_target_timeseries.json`. |
| `hack/benchmark/dump_capacity_demand_estimate.py` | Computes per-variant capacity/demand estimate from raw vLLM/EPP scrapes into `metrics/processed/capacity_demand_estimate.json`. |
| `hack/benchmark/dump_epp_throughput.py` | Derives request rate from EPP counters into `metrics/processed/epp_throughput.json`. |
| `hack/benchmark/dump_wva_full_timeseries.py` | Extracts WVA Prometheus metrics timeseries into `metrics/processed/wva_metrics_timeseries.json`. |
| `hack/benchmark/postprocess.py` | Generates a markdown results table (matching `docs/benchmark.md`) from a results directory. Called automatically by `make benchmark-report`. Pass `--secondary-suffix v2` for per-variant replica rows and weighted cost. |
| `hack/benchmark/plot_two_variant_pipeline.py` | Generates the full-pipeline PNG (up to 7 panels: replicas, capacity/demand, KV cache, requests running/waiting, EPP queue, gateway throughput, WVA saturation utilization). Called automatically by `make benchmark-plot-two-variant`. |
| `hack/benchmark/scenarios/wva_threshold/wva_saturation_v2_config.yaml` | ConfigMap setting `analyzerName: saturation` to select V2. Applied by `make benchmark-enable-v2-saturation`. |
| `test/benchmark/scenarios/prefill_heavy.yaml.in` | Default workload for `make benchmark-run`. Edit `rate`/`max_seconds` here — `make benchmark-run` copies this file at run-time, overriding any stale defaults in the benchmark repo. |

---

## Tuning knobs

| Knob | Where | Effect |
|---|---|---|
| `scenario[0].wva.variantAutoscaling.variantCost` | `two-variant-wva.yaml` | Primary cost (default 10) |
| `variantCost` field | `variants/v2-tp1-cheaper.yaml` (or other `VARIANT_CONFIG`) | Secondary cost (default 5) |
| `suffix` field | variant config yaml | Secondary `Deployment`/VA/HPA name suffix (default `v2`) |
| `minReplicas` / `maxReplicas` | scenario yaml & variant config | Per-variant scaling bounds |
| `HPA spec.behavior.scaleUp.stabilizationWindowSeconds` | `two-variant-wva.yaml` (default `0`) or live patch | 0 = follow WVA immediately (shipped default, matches install samples); raise to damp |
| `rate`, `max_seconds`, `prompt_tokens`, `output_tokens` | `prefill_heavy.yaml.in` | Workload shape |

---

## Common failure modes

- **`No saturation metrics available for model, skipping analysis` on every reconcile**
  → Variant label not propagated. Verify the chart pinned in the scenario
  yaml is `v0.8.0-rc5` or newer
  ([Required pieces #1](#1-wva-chart-and-controller-image-at-v080-rc5-or-newer)).
- **`Processing model (V1)` instead of `(V2)`**
  → Saturation configmap missing `analyzerName: saturation`. Run
  `make benchmark-enable-v2-saturation BENCHMARK_NAMESPACE=$NS`
  ([Step 3](#step-3--enable-saturation-v2)).
- **Both variants scale to `maxReplicas` immediately under modest load**
  → V2 read fallback capacity, not real KV. Verify the controller image
  pinned in the scenario yaml is `v0.8.0-rc5` or newer (carries the
  `cache_config_info` collector fixes from PR #1198), and that the model
  server image emits `vllm:cache_config_info`
  ([Required pieces #3](#3-newer-vllm-image)).
- **Primary scales up while the secondary stays at one replica**
  → Expected at the shipped 10/5 pricing: the TP=2 primary is the more
  efficient variant (cost ratio == GPU/TP ratio), so the optimizer scales it
  first and the cheaper TP=1 secondary only joins once the primary hits its
  `maxReplicas`. To make the secondary the preferred-first variant instead,
  price it *below* its proportional GPU share (cost ratio > TP ratio) — see
  the efficiency note in `variants/v2-tp1-cheaper.yaml`.
- **Stale capacity estimates after a previous run**
  → k2 history persists for the controller's lifetime. Run
  `make benchmark-restart-controller BENCHMARK_NAMESPACE=$NS` between runs
  to flush it ([Step 5](#step-5--run-the-benchmark)).
- **Standup fails at `[03] workload_monitoring` with**
  `APIService "v1beta1.external.metrics.k8s.io" exists and cannot be imported into the current release`
  → You skipped the [Cluster prerequisites](#cluster-prerequisites--check-before-step-1)
  check. Re-run standup with `BENCHMARK_SKIP_PROMETHEUS_ADAPTER=true`. The
  proper long-term fix is migrating the scenario yaml to the Kustomize-based
  WVA install path (tracked as follow-up to the Helm → Kustomize migration).
