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

Record the EPP's configuration with its version: P11 reads it.

- the `EndpointPickerConfig` the EPP loads: the scenario's
  `router.epp.pluginsCustomConfig` entry named by `pluginsConfigFile`, a
  guide's values file layered by the scenario, or, when neither names a file,
  the router chart's `default-plugins.yaml`
  (`config/charts/routerlib/templates/_config.yaml`). After a `--dry-run`, the
  rendered `12_router-values.yaml` holds it;
- its `featureGates` (and any `--feature-gates` flag in
  `router.epp.flags`), its `flowControl` section, and every plugin's
  `parameters`;
- the EPP flags that set data-layer timing: `--refresh-metrics-interval`
  (50 ms), `--refresh-prometheus-metrics-interval` (5 s),
  `--metrics-staleness-threshold` (2 s).

On a running EPP, the startup log has the result: an `Effective raw
configuration` line with the file and the loader's defaults, then one
`auto-created default producer` line per producer the loader added (trap T18).

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
6. Record the definition's label names. They decide which selectors can tell
   this pool's series from another model's (P10).

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

`6ab5c74` is a `main` commit. On the `v0.11.0` and `v1.0.0` releases the same
package is at `…/plugins/requestcontrol/dataproducer/inflightload/`; it moved to
`requesthandling/` on `main` (#3830). The release points above are the same on
`v1.0.0` (`b9393d1a`, `producer.go` lines 77–78 and 592–612). The two differ in
what happens after a scale-in (trap T3b). A path, and a behavior, is only valid
at its commit: read the source at the deployed version.

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

§1.3 lists which properties each role must pass. Check only those, plus any
other property that obviously fails. Each check below gives the question to
answer, how to answer it from what steps 1–3 produced, when it passes, and an
example.

All the examples use one trigger: the prefill trigger of
`staging/pd-disaggregation/token-aware.yaml`, which plays the driver.

```
( sum(inflight_tokens, restricted to live prefill endpoints) / 2696 ) or vector(0)
```

(abridged; the full query is at `token-aware.yaml:307-322`)

`metricType: AverageValue`, `threshold: 1.5`, `minReplicas: 1`,
`maxReplicas: 10`, `pollingInterval: 15`, scale-up one pod per 180 s
(`token-aware.yaml:299-331`). Step 3 found that the gauge counts each request's
uncached prompt tokens from dispatch to first token. The divisor 2696 is the
scenario's `tokenAware.peakPrefillThroughput`, in prompt tokens per second.

### Putting numbers on a trigger

Several checks need the trigger's value at a given load. When step 3 shows that
the time counted includes queue wait, estimate that wait with the simplest
queue that has one, M/D/1, using the scenario's own constants:

```
service time   S = prompt tokens ÷ capacity           (8,192 ÷ 2,696 = 3.04 s)
utilization    ρ = load per pod ÷ capacity
time counted   W = S + ρ × S ÷ (2 × (1 − ρ))          (prefill + queue wait)
per-pod value  s = ρ × W                               (Little's law, in seconds of work)
```

The model gets the shape right and the numbers only roughly: real servers batch,
and prompt lengths vary. Present the numbers as an illustration of the shape,
and put the real curve under *needs measurement*.

### P1, proportional to load

**Question.** Does the value double when the load doubles, at a fixed traffic
mix and cache hit rate?

**How.**

1. Write the value as Little's law gives it: rate × time counted, for a gauge;
   just the rate, for a counter under `rate()`.
2. Decide whether the time counted depends on load. Queue wait does: it is
   near 0 at low load and grows without limit near capacity. Prefill time and
   decode time depend on load only mildly.
3. If it depends on load, compute the value at 20 %, 50 %, 80 % and 95 % of
   capacity and the elasticity between each pair (§1.2, P1). Do not skip 95 %:
   queue wait grows as `ρ / (1 − ρ)`, so a signal can stay in range up to 80 %
   and explode above it, where the pool runs during a burst or a cold start.

**Passes when** the time counted does not include queue wait, or `ε` stays
within 0.8–1.25 over 20–95 % of capacity.

**Example.** The gauge counts until first token, so the time counted includes
queue wait. With `S = 3.04 s`:

| Utilization | 20 % | 50 % | 80 % | 95 % |
|---|---:|---:|---:|---:|
| `s` (seconds of work per pod) | 0.684 | 2.279 | 7.293 | 30.31 |

`ε = ln(2.279 / 0.684) / ln(2.5) = 1.31` from 20 % to 50 %, and
`ln(7.293 / 2.279) / ln(1.6) = 2.47` from 50 % to 80 %, and
`ln(30.31 / 7.293) / ln(1.1875) = 8.29` from 80 % to 95 %. **Fail**, from 50 % of
capacity upward. *From the metric's source*; the real curve *needs
measurement*.

### P2, coupled to actuation

**Question.** If the HPA adds a pod and the load does not change, does it still
ask for the same count?

**How.** Fix a load. Compute the query's total `U` and the desired count at
the pod count `R` and at `R + 1`, with the §1.1 rule for the trigger's
`metricType`.

**Passes when** `U`, and so the desired count, is the same at `R` and `R + 1`.
For a per-pod value this is `η = −1` (§1.2, P2).

**Example.** Load of 6,740 prompt tokens per second,
`6,740 / 2,696 = 2.5` replicas' capacity:

| Pods `R` | 3 | 4 | 5 | 6 |
|---|---:|---:|---:|---:|
| Total `U` | 26.6 | 13.9 | 11.4 | 10.3 |
| Desired `ceil(U / 1.5)` | 18 | 10 | 8 | 7 |

The ask falls as pods are added, from 18 to 7, while the right answer stays
`ceil(2.5 / 0.7) = 4`. **Fail**: adding pods drains the queue, which lowers `U`
(`η = ln(2.279 / 3.482) / ln(5 / 4) = −1.9` from 4 to 5 pods). *From the
metric's source.*

### P3, a target that means a fixed busyness

**Question.** What fraction of one replica's capacity does the threshold stand
for, and is it the same for every traffic mix the pool will see?

**How.**

1. Find the utilization at which one pod reads exactly the threshold. For a
   rate divided by a capacity, that is the threshold itself. Otherwise solve
   `s(ρ) = threshold` with the model above.
2. Repeat for the shortest and longest prompts or answers the benchmark sends.
   For a signal that weights tokens, repeat for the extremes of the mix.
3. If the query or the threshold divides by a capacity constant, find where the
   constant came from: the scenario's comments, a calibration script, a
   finding. A capacity measured on an idle replica, or a rate the pool once
   served, overstates what a replica sustains at the SLO. Note the cache setup
   it was measured with (prefix hit rate, KV-cache offloading on or off), and
   whether it counts prompt tokens sent or only those the cache misses.

**Passes when** the utilization is the same across the mix and leaves room for
bursts (70 % leaves 43 %), and the constant is a capacity measured at the SLO
on the deployed stack, with the deployed cache setup, in the same tokens the
query counts. The constant's value always *needs measurement*.

**Example.** `s(ρ) = 1.5` at 37.8 % utilization for 8,192-token prompts and at
76.1 % for 2,048-token prompts: the same threshold buys twice the busyness on
short prompts. The constant 2696 is, per the scenario's own comment, "the
reference P/D stack's calibrated figure"; upstream lists 15,928 for the same
model on the aggregated path. **Fail** on mix; the constant *needs
measurement*.

### P4, metric type matches aggregation

**Question.** Does the HPA's rule for this `metricType` match what the query
returns?

**How.** Find the outermost aggregation of the query. Ignore the
`or vector(0)` and any division by a constant. Then apply the test: if the pods
double at the same per-pod load, does the value double?

| Outermost operator | Kind | Needs |
|---|---|---|
| `sum`, `count`, a `rate` of a pool-wide counter | extensive | `AverageValue` |
| `max`, `min`, `avg`, a ratio of sums, `histogram_quantile` | intensive | `Value` |

**Passes when** the rendered `metricType` matches the table.

**Example.** The outermost operator is `sum(...)`, extensive, and the rendered
type is `AverageValue` (`token-aware.yaml:306`). **Pass.** *From the config.*

### P5, bounded the right way

**Question.** Does the signal stop responding before the load it must measure
(capped), or keep growing while new pods start (integrating)?

**How.**

- **Capped.** Name the ceiling: KV-cache usage ≤ 1; `running` ≤
  `--max-num-seqs`, from the vLLM command in the backend overlay (vLLM's
  default if absent); a saturation ratio ≤ 1. Compute `ceiling / threshold`.
- **Integrating.** A gauge that counts queued work grows by the excess
  arrivals every second. Estimate the ask after one cold start at a given
  overload, `(queued work after the cold start) ÷ threshold`, and compare it
  with `maxReplicaCount` and the scale-up policy.

**Passes when**, for a capped signal, `ceiling / threshold > 1.1` (the HPA's
tolerance), and the ceiling is the resource the trigger guards. For an
integrating signal, when `maxReplicaCount` and `behavior` keep the overshoot at
a cost the configuration accepts.

**Example.** No ceiling: the gauge integrates. One pod at 1.5× its capacity
gains 0.5 s of work per second, so after a 180 s cold start the ask is
`0.5 × 180 / 1.5 = 60` pods. `maxReplicas: 10` and one pod per 180 s cap it.
**Pass** as bounded, with the overshoot noted. *From the config*; the cold start
*needs measurement*.

### P6, defined at zero replicas

**Question.** If no replica is Ready and requests arrive, does the trigger
still read the load? Answer it whatever `minReplicaCount` is, so the record
says whether scaling from zero would work.

**How.** From step 3, find the process that exports the series, and check
what the query does with the pool's own series gone. A vLLM pod's series
disappear with the pod; the router's and the gateway's exist with no pods,
unless step 3 found that the series is deleted when no endpoint exists (trap
T5). A join on a per-pod series, or a filter on live endpoints, also empties
the result.

**Passes when** `minReplicaCount` is 1 or more, whatever the query reads at
zero. If `minReplicaCount` is 0: when the query reads above 0 at zero Ready
replicas while load arrives, and above `activationThreshold` for the smallest
load that should wake the pool.

**Example.** The query keeps only endpoints that also have a
`per_endpoint_queue_size` series (`and on (target_pod)`,
`token-aware.yaml:315`). With no prefill pod there is none, so the result is
empty and `or vector(0)` (`:322`) makes it 0; the gauge is also only added at
dispatch, which needs a prefill endpoint. Scaling from zero would
not work, but `minReplicas: 1` (`:299`) keeps the pool from reaching it.
**Pass**, with the reading at zero noted. *From the config* and *from the
metric's source*.

### P7, fails visibly

**Question.** When the metric's source is down, does the trigger produce an
error, or a reading that looks like low load?

**How.** Look for `or vector(0)`, `or on() vector(0)` or a default in the
query. Then check `ignoreNullValues` and `fallback` in the rendered
ScaledObject; with this repo's template they are always `true` and absent
(§1). Follow an outage through: the reading, the HPA's response, and what stops
it.

**Passes when** an outage makes the scaler fail and `fallback.replicas`
applies. Never with this repo's template, so a driver always fails P7 here;
record it, and the fix belongs to the template.

**Example.** `) or vector(0)` (`token-aware.yaml:322`). If the EPP restarts,
the trigger reads 0; after the 300 s scale-down window the pool loses one pod
per 300 s down to `minReplicas`, while traffic continues. **Fail.** *From the
config.*

### P8, bounded lag and noise

**Question.** How late does the HPA see a change in load, and does the value
jump between samples at steady load?

**How.**

- **Lag** = scrape interval of the exporter + half of any `[window]` in the
  query + `pollingInterval`. The scrape intervals come from llm-d-benchmark's
  `defaults.yaml`: `monitoring.scrapeInterval` (30 s) for the model servers,
  `router.monitoring.interval` (10 s) for the EPP. A scenario may override
  them. `pollingInterval` is KEDA's 30 s if not rendered.
- **Noise.** For a gauge, compute one request's step: what one request adds
  when it enters, divided by the threshold. For a `rate()` over a window `w`,
  the relative noise is about `1 / √(events per second × w)`.

**Passes when** lag is well below the cold start, and one sample's noise is
well below the 10 % tolerance.

**Example.** Lag `10 + 0 + 15 = 25 s`: instant gauge, EPP scrape 10 s,
`pollingInterval: 15`. One 8,192-token prompt adds `8,192 / 2,696 = 3.04 s`,
203 % of the 1.5 s threshold: a single request crosses the tolerance on its own.
**Pass** on lag, **fail** on noise. *From the config.*

### P9, measures what it claims

**Question.** Does the query read the quantity its role needs, on the deployed
versions?

**How.** Compare step 3's answer with what the role needs: a driver needs
offered work, a guard the resource it guards, an alarm a backlog. Then take
every trap in [traps.md](traps.md) on a component the query reads, and judge
it against step 2's versions as `traps.md` says.

**Passes when** the quantity matches the role, and every trap in range is
absent or worked around in the query.

**Example.** The driver needs offered work; the gauge is a prefill backlog.
**Fail.** EPP `v0.11.0` is in the range of trap T3b, which needs three things
on that release: the liveness intersection, `clamp_min(…, 0)` on each series,
and `namespace` pinned on both sides. The query has the intersection
(`and on (target_pod)` with `per_endpoint_queue_size`, `token-aware.yaml:315`)
and pins `namespace` on the `inflight_tokens` side only (`:311`); the
`per_endpoint_queue_size` side selects by `name` (`:317`), and nothing is
clamped. On a router built from `main` after #3439, plain `sum()` would be
correct and the intersection harmless. *From the metric's source.*

### P10, selects its own pool

**Question.** On a cluster that also serves other models, does each selector
match all of the scaled pool's series and nothing else?

**How.**

1. List every vector selector in the query: both sides of each arithmetic
   operator, of each `and on (…)`, and the numerator and denominator of each
   ratio.
2. For each, find the metric's family in the table below, and the labels the
   selector pins.
3. Check each selector against the four neighbours of step 1 §1.2, P10: N1
   another model in the namespace, N2 the scenario in another namespace, N3
   the other role, N4 a second scrape. A regex matches a neighbour if the
   neighbour's names fit it: `.*decode.*` matches every model's decode pods.
4. Check the other direction: does a pin drop series the pool serves? A
   `model_name` pin on EPP series drops adapters and other models routed to
   the same pool; a `model_name` pin on vLLM series matches nothing if the
   server loads a local path without `--served-model-name`; a `role` selector matches
   nothing without the PodMonitor relabeling (trap T14).

Labels that identify a pool, at llm-d-router `v0.11.0`, `v1.0.0` and `main`
(`333f331e`) and vLLM `v0.27.0` and `v0.30.0` (traps T15, T16). Names are as
llm-d-benchmark deploys a stack (trap T17): InferencePool
`<idLabel>-router`, EPP Service and scrape `job` `<idLabel>-router-epp`,
Deployments `<idLabel>-decode` and `<idLabel>-prefill`.

| Family | Labels the exporter writes that name a pool or model | Pin |
|---|---|---|
| vLLM `vllm:*` | `model_name` (first served name), `engine` (DP rank) | `namespace` and `pod=~"<idLabel>-decode-.*"`; or a `role` label added by relabeling, with `namespace` and `model_name`. Aggregate over `engine` |
| vLLM `http_*`, `vllm:lora_requests_info`, `vllm:cache_config_info` | none | `namespace` and `pod` |
| EPP `request_*` (`total`, `error_total`, `running`, `input_tokens`, `output_tokens`, `ttft_seconds`, `outcome_total`) | `model_name`, `target_model_name` | `namespace` and `job="<idLabel>-router-epp"` (or `service`) |
| EPP `inflight_tokens`, `inflight_requests` | `endpoint_name` (the pod), and a `namespace` of their own, scraped as `exported_namespace` | `namespace` and `endpoint_name=~"<idLabel>-prefill-.*"`, or `job` |
| EPP `average_*`, `std_dev_*`, `ready_endpoints`, `per_endpoint_queue_size` | `name` = pool name | `namespace` and `name="<idLabel>-router"` |
| EPP `flow_control_queue_size`, `_pool_saturation`, `_requests_total`, `fairness_id_resolved_total` | `inference_pool` = pool name | `namespace` and `inference_pool="<idLabel>-router"` |
| EPP `flow_control_stale_endpoints`, `_detector_saturation` | none | `namespace` and `job` |
| Envoy `envoy_cluster_upstream_rq_total` | `envoy_cluster_name` | the cluster of this pool's route only (trap T6); on a shared gateway, check that each pool has its own cluster |
| kube-state-metrics `kube_deployment_*` | `namespace`, `deployment` | both |

`namespace`, `pod`, `service` and `job` are not written by the exporters: the
Prometheus operator adds them from the PodMonitor or ServiceMonitor target. A
scrape job written by hand with `kubernetes_sd_configs` adds none of them
unless its relabeling does, and every selector on them then matches nothing.

**Passes when** every selector pins `namespace` and one pool identity from the
table, every regex fits this stack's names only, and no pin drops the pool's
own traffic. Whether the labels exist on the cluster, with one series per pod
and engine, *needs measurement*: step 2 runs
`count by (namespace, job, pod) (<selector>)` for each selector and expects
exactly the pool's pods.

**Example.** The prefill trigger reads two selectors. `inflight_tokens` pins
`namespace` and `endpoint_name=~".*prefill.*"` (`token-aware.yaml:312-314`):
the regex matches another model's prefill pods in the same namespace (N1), but
the `and on (target_pod)` keeps only pods that also appear in
`per_endpoint_queue_size{name="${model.idLabel}-router"}` (`:318`), which is
this pool's. That side has no `namespace` (N2): it holds only if no pod in
another namespace has the same name, which is true of Deployments' random
suffixes and false of LeaderWorkerSets and StatefulSets (`…-prefill-0`).
**Pass** on N1 and N3, conditional on N2. *From the config.*

The decode trigger of the same scenario,
`sum(vllm:kv_cache_usage_perc{namespace="${namespace.name}", pod=~".*decode.*"})`
(`token-aware.yaml:365-366`), sums every model's decode pods in the namespace.
With this pool's 2 pods at 0.5 and another model's 4 pods at 0.6, it reads
`2 × 0.5 + 4 × 0.6 = 3.4` against a threshold of 0.8: it asks for
`ceil(3.4 / 0.8) = 5` pods, where this pool's own reading, 1.0, asks for 2.
**Fail** on N1. Fix: `pod=~"${model.idLabel}-decode-.*"`. *From the config.*

### P11, matches the router's configuration

**Question.** Does the EPP's effective configuration write every EPP series
the query reads, once, and leave every series in the query meaning what the
trigger's role needs?

**How.**

1. Build the effective plugin set from §2's record: the plugins the file
   lists, plus what the loader adds (trap T18). The `utilization-detector`
   saturation detector and the vLLM metrics data source are always there
   unless replaced. For each plugin, list the data it requires; if nothing
   listed produces it, the loader adds the default producer with no
   parameters, named after its type.
2. For each EPP selector, find the producer in the table below and check that
   it is in the effective set. Count the instances that write the series: more
   than one needs a pin on `plugin_name` or `producer_name`.
3. For every series in the query, EPP or vLLM, check the parameters in the
   second table below against the deployed values.
4. If the EPP image floats (`main`), the plugin set and the defaults are those
   of the resolved commit: the result *needs measurement* until it is resolved.

Producers, at llm-d-router `v1.0.0` and `main` (`26871a0e`):

| Series (`llm_d_epp_…`) | Written only when | Instance label |
|---|---|---|
| `request_*`, `model_rewrite_decisions_total` | always | none |
| `inflight_requests`, `inflight_tokens` | `inflight-load-producer` is in the effective set; `inflight_tokens` also needs a `token-producer` (estimate backend by default) | `producer_name` |
| `prefix_predicted_cached_tokens`, `prefix_best_predicted_cached_tokens`, `prefix_best_available_cached_tokens`, `prefix_prompt_tokens`, `prefix_indexer_*` | an `approx-prefix-cache-producer` or `precise-prefix-cache-producer`; `prefix_*_cached_tokens` are absent on `v0.11.0` | `plugin_name` |
| `kv_cache_events_*`, `kv_cache_index_*` | `precise-prefix-cache-producer`; `kv_cache_index_*` also needs `indexerConfig.enableMetrics` | none |
| `disagg_decision_total` | `disagg-profile-handler` | `plugin_name` |
| `token_producer_*` | `token-producer` with the vLLM `/render` backend | `plugin_name` |
| `request_predicted_*`, `request_slo_violation_total` | `predicted-latency-producer` | `plugin_name` |
| `ready_endpoints`, `average_*`, `per_endpoint_queue_size` | the vLLM metrics extractor (injected unless `dataLayer.injectDefaults: false`); otherwise 0, or frozen (trap T2) | `name` |
| `flow_control_*` (queue, saturation, `requests_total`) | the `flowControl` feature gate (default off) | `inference_pool` |
| `flow_control_stale_endpoints` | `utilization-detector`; with the gate off, only when a request with priority < 0 arrives | `detector` |
| `flow_control_detector_saturation` | `max-saturation-detector` | `detector` |
| `fairness_id_resolved_total`, `request_outcome_total` | `main` only, always | none |

Parameters that change what a trigger reads:

| Parameter (default) | Plugin | Series it changes | Check |
|---|---|---|---|
| `addEstimatedOutputTokens` (`false`) | `inflight-load-producer` | `inflight_tokens`: prompt tokens until the first token, or plus an estimate of 100, 1000 or 4096 output tokens held to the last token on decode endpoints | the gauge's kind (§1.3) matches the role |
| `prefixMatchInfoProducerName` (the approximate producer) | `inflight-load-producer` | which cached prefix `inflight_tokens` subtracts | names a producer in the effective set |
| `blockSizeTokens` (64), `maxPrefixTokensToMatch` (131072), `lruCapacityPerServer`, `autoTune` (`true`) | `approx-prefix-cache-producer` | `prefix_*_cached_tokens`: 64-token steps, capped; an LRU smaller than the pods' KV cache predicts fewer hits | a capacity divisor is in the same tokens |
| `queueDepthThreshold` (5), `kvCacheUtilThreshold` (0.8), `headroom` (0), `metricsStalenessThreshold` (200 ms) | `utilization-detector` (default) | per-pod `vllm:num_requests_waiting` and `vllm:kv_cache_usage_perc` levelled at these limits (trap T19); `flow_control_pool_saturation` | a per-pod `max()` threshold above a limit fires only when every pod is past it |
| `stalenessPolicy` (`saturated`) | `utilization-detector` | `flow_control_pool_saturation` reads 1.0 for a stale pod (trap T9) | the query gates on `flow_control_stale_endpoints == 0` |
| `maxConcurrency` (100), `concurrencyMode` (`requests`), `maxTokenConcurrency`, `failOpen` (`true`) | `concurrency-detector` | with the gate on, per-pod `running + waiting ≤ maxConcurrency` and the excess in `flow_control_queue_size` (trap T20) | a `running` trigger's threshold `< maxConcurrency / 1.1` (P5); no `waiting` trigger is meant to see overload |
| the `flowControl` feature gate (off) | — | `vllm:num_requests_waiting` bounded (T4); `request_total` counts at dispatch (T8); vLLM's TTFT misses the router queue (T10) | the trigger reads the backlog where it now accumulates |
| `defaultRequestTTL` (60 s), band `maxRequests` (5000), `maxBytes` | `flowControl` | `flow_control_queue_size` stops growing; the rest leaves as 429 in `request_error_total{error_code="ResourceExhausted"}` | an integrating alarm on the queue is capped (P5) |
| `conditions[].maxValue`, `fallbackOnEmpty` (`false`) | `utilization-filter` | per-pod caps on running, waiting or KV usage; past them, 503 instead of queueing | a trigger on the capped series has `T < cap / 1.1` |
| `affinityThreshold` (0.80), `maxTTFTPenaltyMs` (18000) | `prefix-cache-affinity-filter` | load concentrates on cache-warm pods: per-pod `max()` reads above the pool's mean | a `max()` trigger is meant to follow the hottest pod |
| the profile weights of `prefix-cache-scorer` against the load scorers | scorers | the same concentration, softer | as above |

**Passes when** every EPP series is written by a plugin or gate in the
effective set, each selector matches one producer instance, and no parameter
changes the quantity the role needs. Whether the series exist on the running
EPP *needs measurement* when the image floats: step 2 runs
`count by (job, plugin_name, producer_name) (<selector>)`.

**Example.** The prefill trigger reads `inflight_tokens` and
`per_endpoint_queue_size`. The file lists `inflight-load-producer`
(`token-aware.yaml:171`); `active-request-scorer` in the decode profile would
have brought it in anyway. It runs with no parameters, so
`addEstimatedOutputTokens` is `false`: the gauge is a prefill backlog, as
step 3 found. No tokenizer is configured, so the loader adds `token-producer`
with its estimate backend, and the prompt tokens are an estimate, not the
engine's count. `per_endpoint_queue_size` needs the metrics extractor, which
the loader injects. The trigger pins neither `producer_name` nor `plugin_name`;
one instance writes the series, so that is safe here, and it breaks as soon as a
second producer is configured. **Pass**, with the estimated tokens noted under
P3. *From the config* and *from the metric's source*.

The decode trigger reads `vllm:kv_cache_usage_perc` against 0.8. The
default `utilization-detector` is injected into both profiles with
`kvCacheUtilThreshold: 0.8`, so while any decode pod is below 80 % no request
goes to a pod above it: the pool fills evenly, which suits a `sum()` trigger.
A per-pod `max()` guard at 0.95, as in `baseline.yaml`, rises above 0.8 only
once every decode pod is past 0.8 (trap T19). *From the metric's source.*

### Recording the result

For each property, record a row:

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P2 coupled to actuation | fail | at 6,740 prompt tokens/s (2.5 replicas' capacity), `U` = 13.9 at 4 pods and 11.4 at 5; desired 10, then 8; right answer 4 | from the metric's source |

The skill's report template has the full layout.

## 6. Hand over

The result for one trigger is a row per property, plus the kind, the role it
can play and the role it plays. The skill combines the rows of every trigger in
the ScaledObject into the verdict.
