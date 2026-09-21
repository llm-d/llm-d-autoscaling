# Documentation

Autoscaling for llm-d is driven by [KEDA](https://keda.sh). This repository ships
the KEDA manifest blueprints and the test bed that evaluates them; see the
[repository README](../README.md) for the overview.

## KEDA blueprints and evaluation

- **[Autoscaling development workflow](developer-guide/autoscaling-workflow.md)** —
  why benchmark-first, the strategy menu, and how a strategy moves from a local
  experiment to a shipped upstream guide.
- **[Benchmark test bed](../benchmark/README.md)** — how specifications,
  backend-agnostic scenarios, and cluster-config overlays compose, and the
  `standup → smoketest → run → teardown` lifecycle.
- **[Blueprints](../benchmark/config/scenarios/)** — recommended scaling
  strategies (`guides/`) and experiments awaiting evaluation (`staging/`).
- **[Benchmark report](../benchmark/docs/benchmark-report.md)** — reading a run:
  replicas, HPA/KEDA trigger values, latency, and throughput panels.
- **[Interactive dashboard](../benchmark/docs/interactive-dashboard.md)** —
  browsing historical sessions per specification.

## Upstream references

- **[Autoscaling architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)**
- **[Workload autoscaling guide](https://llm-d.ai/docs/guides/workload-autoscaling)**

## Contributing

- **[Contributing guide](../CONTRIBUTING.md)** — repository layout, how to
  propose a scaling-strategy change, and local checks.

## Deprecated

The Workload-Variant-Autoscaler controller and its documentation are deprecated.
The last supported version is on the `release-0.9` branch; the frozen copy under
[`legacy/`](../legacy/README.md) is staged for removal.
