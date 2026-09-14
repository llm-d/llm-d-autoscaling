# llm-d-autoscaling

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

Autoscaling for [llm-d](https://github.com/llm-d/llm-d) inference deployments.
This repository holds **KEDA manifest blueprints** and the **evaluation test bed**
used to validate and tune them.

## 1. The Workload-Variant-Autoscaler (WVA) is deprecated

WVA — the custom autoscaling controller this repository used to host — is
deprecated on `main`. It has **not** disappeared:

- The last supported code, manifests, and docs are on the
  [`release-0.9`](https://github.com/llm-d/llm-d-autoscaling/tree/release-0.9)
  branch, released as [`v0.9.0`](https://github.com/llm-d/llm-d-autoscaling/releases/tag/v0.9.0).
  Use that branch for anything WVA-related.
- On `main`, everything WVA has moved untouched into [`legacy/`](legacy/). That
  directory is **staging for removal** — it is frozen, its CI is not wired up,
  and it will be deleted in a future release. Do not build on it.

## 2. KEDA is the autoscaling engine

Autoscaling for llm-d is driven by [KEDA](https://keda.sh) reading inference
metrics (queue depth, KV-cache utilization, and other vLLM/EPP signals) straight
from Prometheus and scaling model-server Deployments through the HPA it manages.
No custom controller sits in that path.

This repository's role is therefore twofold:

### KEDA manifest blueprints

Recommended, reviewed KEDA scaling strategies — `ScaledObject` min/max, scaling
behavior, and metric triggers per serving role (prefill/decode) — live with the
deployment topologies they belong to, under
[`benchmark/config/scenarios/`](benchmark/config/scenarios/):

- `scenarios/guides/` — **recommended** blueprints, one per llm-d guide (e.g.
  `pd-disaggregation.yaml`). These are the configurations to copy from.
- `scenarios/staging/` — experiments and work in progress: trigger and threshold
  variants (`baseline`, `queue-aggressive`, `kv-early`, `token-aware`, …) staged
  for evaluation before being promoted.

Because scenarios are backend-agnostic, the same blueprint runs against
`llm-d-inference-sim`, a latency-simulating vLLM, or real GPU vLLM by swapping a
[cluster-config overlay](benchmark/config/cluster-configs/).

### Evaluation

[`benchmark/`](benchmark/README.md) is an autoscaling test bed built on
[llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark). It stands up a
scenario, drives load through the harness, and captures autoscaling behavior
(replicas, HPA/KEDA trigger values, latency, throughput) so blueprints are
compared on evidence rather than intuition.

```bash
# Optional: a local Kind cluster with emulated GPUs
make create-kind-cluster

# Stand up + run a scenario (see benchmark/README.md for the full lifecycle)
llmdbenchmark standup \
  --spec benchmark/config/specification/guides/pd-disaggregation.yaml.j2 \
  --cluster-config benchmark/config/cluster-configs/k8s/inference-sim.yaml \
  --workspace benchmark/results -p <namespace>
```

Results and reports: [`benchmark/docs/benchmark-report.md`](benchmark/docs/benchmark-report.md)
and [`benchmark/docs/interactive-dashboard.md`](benchmark/docs/interactive-dashboard.md).

## Documentation

- [Repository docs index](docs/README.md)
- [Benchmark test bed](benchmark/README.md)
- [llm-d autoscaling architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)
- [llm-d KEDA autoscaling guide](https://llm-d.ai/docs/guides/workload-autoscaling)

## Contributing

We welcome contributions! See [CONTRIBUTING.md](CONTRIBUTING.md).

Join the [llm-d autoscaling community meetings](https://llm-d.ai/slack) to get involved.

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Related projects

- [llm-d main repository](https://github.com/llm-d/llm-d)
- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark)
- [KEDA](https://keda.sh)
