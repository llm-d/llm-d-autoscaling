# Contributing to llm-d-autoscaling

Welcome! This repository holds the KEDA autoscaling blueprints for llm-d and the
evaluation test bed that validates them.

Start with the general
[llm-d contributing guide](https://github.com/llm-d/llm-d/blob/main/CONTRIBUTING.md)
for the community process (DCO/signed commits, code of conduct, review flow).
This document covers what is specific to this repository.

## Where things live

| Path | What it is |
|------|------------|
| `benchmark/config/scenarios/guides/` | recommended KEDA blueprints — change these deliberately, they are what users copy |
| `benchmark/config/scenarios/staging/` | experiments: new triggers, thresholds, and behaviors awaiting evaluation |
| `benchmark/config/cluster-configs/` | backend overlays (inference-sim, simulated vLLM, real GPU vLLM) |
| `benchmark/` | the llm-d-benchmark based test bed, its docs, and captured results |
| `hack/kind-emulator/` | Kind cluster with emulated GPUs (`make create-kind-cluster`) |
| `legacy/` | the deprecated Workload-Variant-Autoscaler, staged for removal — do not extend it |

Read [`benchmark/README.md`](benchmark/README.md) before editing anything under
`benchmark/config/`; it is the source of truth for how specifications,
scenarios, and cluster-configs compose.

## Proposing a scaling-strategy change

Autoscaling changes are evaluated, not asserted:

1. Add the variant under `benchmark/config/scenarios/staging/<guide>/` alongside
   the `baseline.yaml` control.
2. Run it against a backend (`--cluster-config`) and keep the session results.
3. Include the comparison — replicas over time, trigger values, latency, and
   throughput versus the baseline — in the pull request.
4. Promote to `scenarios/guides/` only once it beats the current recommendation.

See [`benchmark/docs/benchmark-report.md`](benchmark/docs/benchmark-report.md)
for how to read and share a run.

## Checks

```bash
make lint-scripts   # shell syntax check
make help           # available targets
```

Markdown, YAML, and shell hygiene run through
[pre-commit](https://pre-commit.com): `pip install pre-commit && pre-commit install`,
then `pre-commit run --all-files`.

## Getting help

- [GitHub Issues](https://github.com/llm-d/llm-d-autoscaling/issues)
- The llm-d autoscaling community meetings — join via [Slack](https://llm-d.ai/slack)

Thank you for contributing!
