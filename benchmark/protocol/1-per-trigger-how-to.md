# How to analyze one trigger

Part of [step 1, static signal analysis](1-static-signal-analysis.md). That
document defines the properties (§1.2) and the kinds and roles (§1.3); this one
says how to establish each of them for one trigger, where to look, and what
counts as evidence. The
[`static-signal-analysis` skill](../../.claude/skills/static-signal-analysis/SKILL.md)
runs the same procedure and adds the checks on the combination and the verdict.

Every finding gets an evidence tag:

- *from the config*: it follows from the rendered ScaledObject alone;
- *from the metric's source*: it rests on code read at the deployed version,
  cited as `repo@commit:path`;
- *needs measurement*: it depends on a number only a cluster can give
  (capacity, mix range, cold start). These become questions for step 2.

## 1. Get the ScaledObject as KEDA will see it

The scenario's `keda:` block is not what KEDA receives: llm-d-benchmark renders
it through a template that fills in defaults and drops fields it does not know.
Analyze the rendered object.

1. Render without touching a cluster:

   ```bash
   llmdbenchmark standup \
     --spec benchmark/config/specification/staging/<guide>/<strategy>.yaml.j2 \
     --cluster-config benchmark/config/cluster-configs/<platform>/<backend>.yaml \
     --workspace benchmark/results -p <namespace> --dry-run
   ```

   The rendered ScaledObjects are in `27_keda-scaledobjects.yaml`, next to the
   merged `config.yaml`, under the printed session's `plan/<scenario>/`
   directory. `${...}` placeholders are resolved there.

2. If rendering is not possible, read the scenario and apply the template's
   defaults by hand. In `config/templates/jinja/27_keda-scaledobjects.yaml.j2`
   (llm-d-benchmark at `bd7d768`, 2026-10-09):

   | Field | When the scenario omits it |
   |---|---|
   | `minReplicaCount` (scenario key `minReplicas`) | 1 |
   | `maxReplicaCount` (scenario key `maxReplicas`) | 10 |
   | `metricType` | `AverageValue` |
   | `threshold` | `"1"` |
   | `activationThreshold` | `"0"` |
   | `pollingInterval` | not rendered: KEDA's default, 30 s |
   | `behavior` | not rendered: the HPA's defaults |

   The template has no field for `ignoreNullValues`, `fallback` or
   `scalingModifiers`. Whatever a scenario writes for them is dropped, so every
   Prometheus trigger runs with `ignoreNullValues: true` (KEDA's default, trap
   T7) and no fallback. Re-read the template at the deployed llm-d-benchmark
   version before relying on this table.

3. Record, with line numbers in the rendered file: the query, `metricType`,
   `threshold`, `activationThreshold`, and the ScaledObject's replica bounds,
   `pollingInterval` and `behavior`.

## 2. Record the deployed versions

List every component the query reads: the exporter of each metric (vLLM, the
EPP, the gateway, kube-state-metrics) and KEDA itself. For each, find its
version:

- images: the overlay's `images:` block wins; otherwise llm-d-benchmark's
  `config/templates/values/defaults.yaml` (the `*_version` anchors at the top,
  for example `llm-d-router-endpoint-picker_version`);
- KEDA: the cluster's installed release (`helm list -n keda`), or the minimum
  in `benchmark/README.md` if no cluster is at hand.

A floating tag (`latest`, `main`, `auto`) is not a version. Resolve it to an
image digest from a running pod, or tag every finding that depends on it
*needs measurement*.

## 3. Find what each metric counts, and when

The metric name is a hint, not evidence. Read the code that writes the series,
at the deployed version.

1. Clone the exporter's repository at the deployed tag
   (`git clone --depth 1 --branch <tag> <repo>`).
2. Search for the metric's base name. Exporters add prefixes: vLLM writes
   `vllm:<name>`, the EPP builds `llm_d_epp_<name>` from a namespace and a
   subsystem, so search for `<name>` alone.
3. Find the definition. It says whether the metric is a gauge, a counter or a
   histogram.
4. Find every write:
   - gauge: `Inc`/`Add` and `Dec`/`Sub`/`Set`, and any series deletion;
   - counter: `Inc`/`Add`;
   - histogram: `Observe`.
5. For each write, name the request event it happens at: arrival at the
   gateway, dispatch by the router, admission by vLLM, first output token, last
   token, completion. Check whether a configuration option changes the event.

**Worked example: `llm_d_epp_inflight_tokens`**, at `llm-d/llm-d-router@6ab5c74`
(`main`, 2026-11-03), in
`pkg/epp/framework/plugins/requesthandling/dataproducer/inflightload/`:

- `metrics.go` defines `inflight_tokens` as a gauge.
- `producer.go`, `PreRequest`: `tokenGauge.Add(tokens)` when the router
  dispatches the request. The tokens are the prompt minus its cached prefix.
- `producer.go`, `ResponseBody`: on the first response chunk
  (`resp.StartOfStream`), the tokens are subtracted. That is the first output
  token.
- The config option `addEstimatedOutputTokens` (default `false`) changes this.
  When it is `true`, estimated output tokens are added too, and only the prefill
  endpoint's entry is released at the first chunk; the others stay until
  `EndOfStream`.

So with the default config the gauge counts from dispatch to first token: a
prefill backlog (§1.3, "Placing a gauge"). With `addEstimatedOutputTokens: true`
it counts, on decode endpoints, until the last token. The plugin's config
belongs in the record, not just its version. *From the metric's source.*

The package moved since the version `traps.md` cites (`requestcontrol/` then,
`requesthandling/` now): a path is only valid at its commit.

## 4. Classify the kind and the role it can play

With step 3's answer, place the trigger in §1.3's table:

- a gauge: by the event that removes a request ("Placing a gauge");
- a counter under `rate()`: by the event that increments it ("Placing a
  counter");
- a histogram under `histogram_quantile()`: a latency outcome;
- a bounded ratio (KV-cache usage, slot use): capped saturation.

The kind gives the role the trigger can play. Note the role it plays in this
configuration too: the driver is the trigger meant to win in normal operation.
If the two differ, for example a backlog used as the only trigger, that is a
failure in itself.

## 5. Check the properties the role requires

§1.3 lists which properties each role must pass. For each one:

| Property | How to establish it | Usual tag |
|---|---|---|
| P1 proportional | A gauge: by Little's law it reads arrival rate × time counted. If the time counted includes queue wait, `ε > 1` near saturation and `≈ 0` below it. A counter at arrival: `ε = 1` at a fixed mix. | from the metric's source |
| P2 coupled to actuation | Ask whether the query's total changes when pods are added at fixed load. A rate counted before any queue does not; a backlog does, because adding pods drains it. | from the metric's source |
| P3 fixed busyness | Write what the threshold means as a fraction of one replica's capacity. If the query divides by a capacity constant, find where the constant came from. Its value is never established statically. | needs measurement |
| P4 metric type | Read the outermost aggregation of the query: `sum` is extensive, `max`/`avg`/ratio/quantile intensive. Compare with the rendered `metricType`. | from the config |
| P5 bounds | Name the ceiling, if any: KV-cache usage ≤ 1, `running` ≤ `max_num_seqs` (from the vLLM command in the overlay). Check `threshold < ceiling / 1.1`. For a global queue, estimate growth × cold start ÷ `threshold` against `maxReplicaCount`. | from the config; the growth rate needs measurement |
| P6 zero replicas | Only if `minReplicaCount` is 0. Find where the series is exported: a vLLM pod's series disappear with it; a gateway's or router's do not. Check `activationThreshold`. | from the metric's source |
| P7 fails visibly | Look for `or vector(0)` in the query. With this repo's template, `ignoreNullValues` is always true and there is no fallback (step 1), so a driver always reads 0 on an outage. | from the config |
| P8 lag and noise | Lag: scrape interval of the exporter (`monitoring.scrapeInterval`, default 30 s for vLLM pods; the router's `monitoring.interval`, default 10 s, in llm-d-benchmark's defaults) + half the `rate`/`avg_over_time` window + `pollingInterval`. Noise: for a gauge, one request's step divided by `threshold`. | from the config; the step size may need measurement |
| P9 measures what it claims | Step 3's answer, plus every trap in [traps.md](traps.md) on a component the query reads, judged against step 2's versions. | from the metric's source |

For each property record: pass or fail, the rendered line it rests on, the load
range where it fails, and the tag.

## 6. Hand over

The result for one trigger is a row per property, plus the kind, the role it
can play and the role it plays. The skill combines the rows of every trigger in
the ScaledObject into the verdict.
