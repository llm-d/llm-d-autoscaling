---
name: load-vs-supply
description: Use when drawing or checking a load-vs-supply figure from an llm-d-benchmark run of any scenario (standalone, modelservice, P/D, KEDA/HPA-scaled or static): the offered load clients sent, what the autoscaler read, and Ready and requested replicas, each in its own raw unit on a shared time axis. Triggers on asking for a stair, replica-vs-load or "did the pool keep up" figure, or on judging whether a run's data can support one. Not for running the benchmark or rendering its Grafana/HTML session report (that is run-benchmark).
---

<!-- markdownlint-disable MD013 -- wide tables and 100-column prose -->

# Load-vs-supply figure

One figure, stacked panels on one time axis, answers "what did clients send, what did the
autoscaler see, what did the pool do, and did requests meet their SLO?":

| Panel | Series | Unit |
| --- | --- | --- |
| Offered load (orange) | what clients sent, per bin | input tokens/s, output tokens/s, or requests/s |
| What the autoscaler read (green) | one panel per trigger, with its threshold levels | the trigger's own unit |
| Replicas (blue) | Ready (solid), requested (dashed), HPA desired (green, dotted) | replicas |
| SLO attainment (yellow) | share of the requests sent in each bin that met every SLO | 0-100 % |

**The figure shows raw data only.** Nothing is divided by a capacity or a threshold, and no series
is rescaled to fit another. The reader compares the panels by eye along the time axis. Anything
that needs a model (per-replica capacity μ_SLO, a target utilization, a token weighting, the
"right" replica count) is a separate claim and does not belong on this figure.

Everything the skill needs is in this directory. It works on any llm-d-benchmark scenario, because
it reads only what the harness leaves behind (client records, replica snapshots) plus Prometheus
range-query results you give it.

```sh
python3 -I scripts/load_vs_supply.py <results dir> --loop open|closed \
    [--role decode | --deployment NAME] [--load in,out,req] [--bin 30] \
    [--trigger FILE[=THRESHOLD][:NAME] ...] [--scaledobject YAML [--scaledobject-name N]] \
    [--hpa-desired FILE] [--recompute-metric M] \
    [--slo ttft=S --slo tpot=S --slo e2e=S] [--attainment-target 0.95] \
    --title ... --subtitle ... --out fig.svg
```

It writes `fig.svg` and `fig.csv` (every plotted series with its source: the table view) and
prints where each series came from. Standard library only.

## In this repo

- **Where the run comes from.** [`run-benchmark`](../run-benchmark/SKILL.md) produces the
  results directory: one session per `benchmark/results/<user>-<timestamp>/`, one experiment per
  `results/<experiment-id>_<N>/` inside it. Pass the experiment directory. The session's rendered
  ScaledObject is `plan/<stack>/30_keda-scaledobject.yaml`; point `--scaledobject` at it.
- **Paths.** Run the CLI from the repo root as
  `python3 -I .claude/skills/load-vs-supply/scripts/load_vs_supply.py ...`. Keep figures out of
  git: `benchmark/results/` is ignored, so write them next to the run.
- **Other figures of the same run.** `benchmark/hack/npd-fcoff/analyze.py` reduces one npd-fcoff
  run to that scenario's metrics (work tokens/s weighted for the npd-fcoff trigger, the trigger's
  components, the sync check). `benchmark/hack/benchmark_report.py` snapshots Grafana and renders
  the per-session HTML report. This skill is the scenario-neutral figure: raw series only, any
  trigger, no Grafana.

## 1. Inputs

| Input | What it is |
| --- | --- |
| llm-d-benchmark results dir | the `results/<exp_id>/` tree of `llmdbenchmark run --monitoring`. Needs per-request records from inference-perf (`per_request_lifecycle_metrics.json`, with `report.request_lifecycle.per_request: true`) or aiperf (`profile_export.jsonl`). guidellm and `vllm bench serve` write no per-request file the adapter reads |
| Prometheus range-query results | JSON of the API body (or its `result` list), one per trigger expression, and optionally Ready replicas. Placed on the run's clock by epoch time |
| rendered ScaledObject YAML | optional; supplies each trigger's `threshold` and `metricType` (e.g. llm-d-benchmark's `plan/<stack>/30_keda-scaledobject.yaml`). A file with several ScaledObjects is narrowed by `--scaledobject-name`, or by `--deployment` matching a `scaleTargetRef` |

If the run used `--data-collect results`, `metrics/` is gone (and, before llm-d-benchmark
`0fb187fb`, the per-request records too); re-collect with `default` or `fast`. What a run holds,
by llm-d-benchmark version, is in `references/stock-run-gaps.md`. In short: a run before
`0fb187fb` (#2192, 2026-11-18) never records what the autoscaler read, and its harness role may
not be allowed to list Deployments, so expect to supply a trigger series and possibly a replica
series from Prometheus. From `0fb187fb` on, the role can list them and the harness records HPA
state per tick: the skill reads the HPA's desired replicas from the run, but still needs the
trigger values from Prometheus.

**One pool per figure.** A P/D or multi-model stack has several serving controllers. The script
refuses to sum them: pick one with `--role` (`decode`, `prefill`) or `--deployment`.

## 2. Offered load

Load, as llm-d-autoscaling defines it, is λ = (λ_in, λ_out) = (q × ISL, q × OSL): the input and
output tokens per second clients send. Input tokens count whether or not they hit the prefix
cache. A prefill pool's load is λ_in, a decode pool's λ_out.

**Offered** is what clients send; **carried** is what the pool completed. Under overload the two
split: carried load stops at capacity while offered load keeps rising. The panel is offered load,
from the clients' own records: every request counted at the time it was **scheduled**, completed
or not, summed per bin and divided by the bin length. Never take it from completed-request
throughput or from a trigger; both are carried-side and flatten exactly when the figure needs to
show the pool falling behind.

The two components are shown in separate panels, not added: adding them needs a weighting between
prompt and output tokens, which is a model. `--role prefill` shows λ_in, `--role decode` λ_out,
otherwise both; `--load req` adds the request rate.

| `--loop` | Generator | What the panel is |
| --- | --- | --- |
| `open` | inference-perf `constant`, `poisson`; aiperf with a request rate | the load: requests arrive at the set rate whatever the server does |
| `closed` | inference-perf `concurrent`: N users, each sends the next request Z s after the previous answer ends | the rate clients **actually sent**, labelled "not the load". It falls when answers slow and rises with more replicas, so it moves with the replica count at fixed load. The load is a calculation, q = N / (W_SLO + Z) with W_SLO = TTFT_SLO + OSL × TPOT_SLO; state it in the subtitle if you want it on the page, but it is not plotted |

Bins: 30 s by default (`--bin`); shorter bins are not supported on llm-d-benchmark data, whose
clock bridge (`perf_counter` to wall clock via stage 0's `run.time.start`, or `scheduled_time` when
present) has an error of one inter-arrival gap. If requests fall outside every stage window, the
anchor is wrong. Check that the generator kept up (achieved rate near the requested one): a
generator that falls behind understates the offered load.

## 3. What the autoscaler read

One panel per trigger, in the trigger's own unit, from a Prometheus range query of the trigger
expression, or `keda_scaler_metrics_value`. A stock llm-d-benchmark run does not record it;
`--recompute-metric` sums a gauge over the pods' raw `/metrics` scrapes as a last resort, labelled
"recomputed, not what KEDA read".

**Several triggers:** repeat `--trigger FILE:NAME`, one per trigger. With `--scaledobject`, NAME is
matched to the trigger's `name` in the ScaledObject, which supplies that trigger's threshold and
`metricType`; a lone trigger matches without a name. `=THRESHOLD` overrides. An unmatched name is
drawn without threshold lines, with a note. With no ScaledObject, an explicit threshold is taken
as `AverageValue` (KEDA's default) and the CLI says it assumed so.

The reference lines draw the HPA's own rule for the trigger's `metricType`, from the
configuration, not from any capacity estimate:

| metricType | HPA asks for | Lines |
| --- | --- | --- |
| `AverageValue` (default) | ceil(value ÷ threshold) | at 1×, 2×, 3×… threshold, labelled with the replicas asked for above each |
| `Value` | ceil(current replicas × value ÷ threshold) | one line at the threshold (the ask depends on the current replica count) |
| `Utilization` | (CPU/memory triggers) | one line at the target |

The HPA takes the largest ask over the triggers, but the panels stay separate, each in its own
unit: combining them needs each value divided by its threshold. What the HPA decided is drawn as
raw data instead: its desired replicas in the replicas panel (section 4).

**`scalingModifiers`:** if the ScaledObject combines its triggers with a formula
(`advanced.scalingModifiers`), the HPA reads one composite metric, not the individual triggers. The
CLI says so. Pass the composite's series as `--trigger FILE:composite` to draw what the HPA used,
with the modifier's `target` and `metricType`.

## 4. Replicas

Ready (solid) and requested (`spec.replicas`, dashed) step lines, from
`metrics/processed/replica_status_timeseries.json`; fallback a Prometheus range query of
`kube_deployment_status_replicas_ready` (`--replicas-json`), labelled with its source. The gap
between the two lines is pods requested but not yet serving (cold start).

Optionally the HPA's desired replicas (green, dotted): what the autoscaler decided from all its
triggers, after its behavior policies. From a Prometheus range query of
`kube_horizontalpodautoscaler_status_desired_replicas` (`--hpa-desired FILE`), or from
`metrics/processed/autoscaler_timeseries.json` when the harness records HPA state per tick (the
HPA is the ScaledObject's, else the only one in the file). Requested replicas usually follow it;
where they differ, something other than the HPA set `spec.replicas`.

## 5. SLO attainment

With `--slo` (one or more of `ttft`, `tpot`, `e2e`, in seconds), the last panel shows, per bin, the
share of requests **sent** in that bin that completed without error and met every SLO given:

```text
attainment(bin) = requests scheduled in the bin, completed, meeting every SLO
                  ÷ every request scheduled in the bin (errors and timeouts included)
```

- **Errors are misses**, not left out: a timed-out request is load the pool failed to serve.
  Dropping it would make an overloaded bin look good.
- **By scheduled time**, like offered load, so a request that waited in a backlog is charged to the
  bin whose load caused the wait, and the dip lines up with the replica lag above it.
- **Latencies:** inference-perf `computed_metrics` (`time_to_first_token`, `time_per_output_token`,
  `request_latency`) when the profile sets `per_request_fields.computed_metrics: true`; otherwise
  derived from `start_time`, `end_time` and the first token time (raw, not tokenizer-corrected; the
  CLI says how many). aiperf: `time_to_first_token`, `inter_token_latency`, `request_latency`. A
  completed request with no latency the SLO needs cannot be judged; it is left out of both counts
  and the CLI says how many.
- An empty bin is a gap in the line, not a zero.
- `--attainment-target` draws the objective (for example 95 %) as a reference line.

The SLOs are configuration, like a threshold: state them, and the panel label does. They are not a
capacity estimate.

## 6. Visual grammar

The renderer (`scripts/lvs_render.py`) fixes the style; do not restyle.

| Mark | Class | Meaning |
| --- | --- | --- |
| Orange line, 1.75 | `s2` | offered load |
| Green line, 1.25 | `s3` | trigger value |
| Blue solid step, 2.5 | `s1` | Ready replicas |
| Blue dashed step, 1.5 | `s1` | requested replicas |
| Green dotted step, 1.25 | `s3` | HPA desired replicas |
| Yellow line, 1.75 | `s4` | SLO attainment |
| Gray dashed horizontal | `ref` | threshold levels |
| Gray dashed vertical | `ref` | stage boundary, across every panel |

The palette (categorical slots 1-4 in fixed order; validated with the `dataviz` validator: all
checks pass in both modes, and green and yellow fall below 3:1 against the light background, which
is why every panel is labelled and `fig.csv` exists), light mode: background `#fcfcfb`,
blue `#2a78d6`, orange `#eb6834`, green `#1baf7a`, yellow `#eda100`,
grid `#e1e0d9`, ink `#0b0b0b` / `#52514e` / `#898781`; the dark-mode steps are in
`lvs_render.STYLE`. Rules from general data-visualization practice (the `dataviz` skill, if you
have it): one y-scale per panel, never a second scale on the same panel; stacked panels share the
time axis instead of a dual axis; a legend on a panel with two or more series, while a single-series
panel's label names its series; thin marks; colour follows the series;
text in ink tokens, not the series colour; a table view exists (`fig.csv`). The SVG carries
`<title>` and `aria-label`. Render it and look at it for label collisions before you ship it.

## 7. Honesty rules

1. **Raw data only.** No normalisation by capacity, utilization target or token weighting, and no
   derived "needed replicas" line. A claim that needs μ_SLO goes in the text, with the mix and
   cache setup it was measured on, not on this figure.
2. **Offered load comes from client records**, never from the trigger or from completed throughput.
3. **Label every series by its source.** Recomputed (from scrapes) and fallback series say so in the
   panel label. The script does it; do not edit the labels to hide it.
4. **Closed loop: the send rate is not the load**, and its label says so.
5. **Attainment counts every request sent.** Errors and timeouts are misses; unjudged requests
   are reported, not hidden; the SLOs are in the label.
6. **State what is missing.** No trigger series → no green panel and a note, not a stand-in. No
   `spec` series → no dashed line.
7. **Say when the run is not real.** A synthetic fixture's title starts with `MOCK DATA`.
8. **Do not read causes off the figure.** It shows that supply lagged or led load. Why belongs to
   cold-start and signal analysis, which is a separate question.

## 8. Reuse and self-check

`scripts/lvs_render.py` has the binning (`bins`, `attainment`) and the drawing (`panels_svg`, a list of panels
with their series); `scripts/lvs_adapters.py` reads an llm-d-benchmark results directory into a
`Run` of raw series with their source labels. Import them from your own report generator rather
than copying the drawing.

`python3 -I scripts/selftest.py` builds a synthetic llm-d-benchmark directory
(`scripts/make_mock_llmd_benchmark_run.py`, invented numbers) and draws it through every input
path: one and two triggers with different metric types, a file with several ScaledObjects,
`scalingModifiers`, an unmatched trigger name, the HPA-desired line from both sources, SLO
attainment (checked to add back up to the good requests in the records), no trigger,
the request-rate panel, closed loop, and a two-pool stack with the prefill and decode roles. Titles of figures made from it start with `MOCK DATA`.
