# Autoscaling Development Workflow

How we develop and evaluate autoscaling strategies in this repository, and how a
strategy travels from a local experiment to a shipped upstream guide.

This is a process and orientation document. It deliberately does **not** copy the
per-strategy metric names, PromQL, or thresholds - those are owned upstream in
[`llm-d/llm-d/guides/workload-autoscaling`](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling)
and would drift if duplicated here. Follow the links for the authoritative
details. For how to actually run the test bed, see the
[benchmark test bed README](../../benchmark/README.md).

## Why benchmark-first

Autoscaling for llm-d is driven by [KEDA](https://keda.sh); this repository ships
the KEDA blueprints and the test bed that evaluates them. The starting point for
any autoscaling problem is a **benchmark configuration**, not engine code.

The reason is that CPU/GPU utilization is a lagging, unreliable signal for LLM
inference - GPU utilization sits near 100% during active batching regardless of
real load, so it only reacts after latency has already degraded. Autoscaling
needs proactive, demand-based signals from the EPP router and vLLM. The only way
to know whether a scaling strategy behaves is to exercise it against a
reproducible workload and watch the closed loop:

> demand signal -> autoscaler decision -> replica readiness -> demand recovery

The test bed in [`benchmark/`](../../benchmark/) exists to reproduce that loop
cheaply (GPU-free on simulator backends) before committing to hardware.

## The strategy menu

The upstream workload-autoscaling guide documents a menu of strategies. Each is
the source of truth for its own metrics, thresholds, and manifests; the table
below is orientation only.

| Strategy | What it solves | Status | Upstream guide |
|----------|----------------|--------|----------------|
| Queue | Homogeneous pools, absolute thresholds; reacts to bursts + holds target concurrency | stable | [keda-epp-queue](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/keda-epp-queue) |
| Saturation | Normalized, portable version of queue (pool saturation 0.0-1.0+) | experimental | [keda-epp-saturation](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/keda-epp-saturation) |
| Token-aware | Heterogeneous prompt sizes; measures prefill (tokens) and decode (KV) in their own units | experimental | [keda-epp-token-aware](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/keda-epp-token-aware) |
| SLO-aware | Per-request latency SLOs directly (predicted/measured TTFT/TPOT), not proxies | experimental / scope-limited | [slo-aware](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/slo-aware) |
| KEDA + WVA | Multi-variant cost optimization across heterogeneous GPUs | deprecated here (see below) | [wva](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/wva) |

Two orthogonal infrastructure layers compose under any strategy:

- [multi-inference-pool](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/multi-inference-pool) - serving multiple pools per cluster, one ScaledObject per Deployment.
- [kueue-rebalancing](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling/kueue-rebalancing) - GPU quota sharing across models whose HPAs scale independently; Kueue gates pods below the HPA while KEDA keeps replica ownership.

The legacy HPA + Prometheus Adapter path
([promadapter.md](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/promadapter.md))
is deprecated: it cannot coexist with KEDA, and KEDA is the sole backend going
forward.

> **WVA is deprecated in this repository.** The Workload-Variant-Autoscaler
> decision engine is frozen under [`legacy/`](../../legacy/README.md) and staged
> for removal. New work targets the KEDA + EPP strategies above. Do not build on
> the `legacy/` engine.

## The pipeline: incubate -> promote -> ship

A new scaling strategy moves through three stages. The scaling logic (the `keda:`
block) is the same artifact at every stage; what changes around it is the
packaging.

### 1. Incubate (here, in `benchmark/`)

Add the strategy as a staging variant under
[`benchmark/config/scenarios/staging/<guide>/<strategy>.yaml`](../../benchmark/config/scenarios/staging/).
Only the `keda:` block varies from `baseline.yaml` (a verbatim copy of the
recommended `guides/<guide>.yaml` strategy), so results isolate the strategy.
Validate the loop cheaply, GPU-free, on the simulator backends
(`inference-sim` / `model-sim`) with a dry-run compose first, then a live run.

See the [KEDA experiments section](../../benchmark/README.md#keda-experiments-naming-convention)
of the benchmark README for the naming rules and the run commands.

### 2. Promote (here, in `benchmark/`)

Once a variant wins, copy its `keda:` block into the recommended
[`benchmark/config/scenarios/guides/<guide>.yaml`](../../benchmark/config/scenarios/guides/)
and retire the losing variants.

### 3. Ship (upstream, in `llm-d/llm-d`)

Package the confirmed strategy as a guide under
`llm-d/llm-d/guides/workload-autoscaling/<strategy>/`, usually as the
`base` + `k8s` + `ocp` overlay form, marked experimental where appropriate.

The gap between a confirmed staging variant and a shippable upstream guide is
**not** the scaling logic - that already matches. It is:

- the `k8s` / `ocp` overlay split (bundled Prometheus vs Thanos, auth, RBAC),
- real-hardware calibration (where a strategy needs it, e.g. token-aware's
  `peakPrefillThroughput`), and
- the `TriggerAuthentication` / RBAC wiring.

## Where things live

| Path | Contents |
|------|----------|
| [`benchmark/config/scenarios/guides/`](../../benchmark/config/scenarios/guides/) | Recommended, backend-agnostic scenarios (one per guide) |
| [`benchmark/config/scenarios/staging/`](../../benchmark/config/scenarios/staging/) | Experiments awaiting evaluation (per-guide variant dirs) |
| [`benchmark/config/specification/`](../../benchmark/config/specification/) | Thin `.j2` entrypoints (`--spec`) that wire paths to a scenario |
| [`benchmark/config/cluster-configs/`](../../benchmark/config/cluster-configs/) | Swappable backend overlays (`--cluster-config`) |
| [`legacy/`](../../legacy/README.md) | Frozen WVA engine, deprecated, staged for removal |

## Adding or evaluating a strategy: checklist

1. Pick the guide/topology (today: `pd-disaggregation`) and read its recommended
   scenario in `scenarios/guides/`.
2. Add `staging/<guide>/<strategy>.yaml` changing only the `keda:` block from
   `baseline.yaml`; record the exact delta in the file header.
3. Dry-run compose against a simulator backend to confirm the spec + overlay
   render (no cluster needed). See the benchmark README.
4. Run it live on a simulator backend and read the result (replicas, KEDA/HPA
   trigger values) via the [benchmark report](../../benchmark/docs/benchmark-report.md).
5. Promote the winner into `scenarios/guides/<guide>.yaml`.
6. When ready, ship it as an upstream guide (stage 3 above).

## See also

- [Benchmark test bed README](../../benchmark/README.md) - run mechanics and the
  spec/scenario/overlay composition model.
- [Documentation index](../README.md)
- [Upstream workload-autoscaling guide](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling)
