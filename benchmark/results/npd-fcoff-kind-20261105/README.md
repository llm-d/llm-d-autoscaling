# npd-fcoff kind staging validation, 2026-11-05

`analyze.py run` output for each run reported in
[`benchmark/config/workloads/npd-fcoff/README.md`](../../config/workloads/npd-fcoff/README.md)
§ Staging validation: per-segment client and control-plane metrics, the
trigger-vs-offered sync bins, HPA decisions, and the Prometheus time series
(`series`, seconds relative to the trace origin).

| file | workload | session (`benchmark/results/`) |
|---|---|---|
| `stair.json` | `stair.yaml.in` + `kind-stair.csv` | `villardl-20261105-004554-626` |
| `mix.json` | `mixshift.yaml.in` + `kind-mixshift.csv` | `villardl-20261105-010956-713` |
| `over.json` | `overload.yaml.in` + `kind-overload.csv` | `villardl-20261105-013608-665` |
| `boot.json` | `bootstrap.yaml.in` + `kind-bootstrap.csv`, min 0 | `villardl-20261105-020737-258` (`-015649-000` is the aborted first attempt: `/v1/models` check against an empty pool) |
| `agentic.json` | `agentic.yaml.in` (`conversation_replay`) | `villardl-20261105-022333-243` |
| `static.json` | `mixshift.yaml.in` + `kind-mixshift.csv`, SO pinned at 3 | `villardl-20261105-025619-744` |

The standup session is `villardl-20261105-003941-181`.
