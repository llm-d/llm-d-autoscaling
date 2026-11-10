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
mix?

**How.**

1. Write the value as Little's law gives it: rate × time counted, for a gauge;
   just the rate, for a counter under `rate()`.
2. Decide whether the time counted depends on load. Queue wait does: it is
   near 0 at low load and grows without limit near capacity. Prefill time and
   decode time depend on load only mildly.
3. If it depends on load, compute the value at 20 %, 50 % and 80 % of capacity
   and the elasticity between each pair (§1.2, P1).

**Passes when** the time counted does not include queue wait, or `ε` stays
within about 0.8–1.25 over 20–80 % of capacity.

**Example.** The gauge counts until first token, so the time counted includes
queue wait. With `S = 3.04 s`:

| Utilization | 20 % | 50 % | 80 % |
|---|---:|---:|---:|
| `s` (seconds of work per pod) | 0.684 | 2.279 | 7.293 |

`ε = ln(2.279 / 0.684) / ln(2.5) = 1.31` from 20 % to 50 %, and
`ln(7.293 / 2.279) / ln(1.6) = 2.47` from 50 % to 80 %. **Fail**, from 50 % of
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

**Example.** Load of 2.5 pods' worth (6,740 prompt tokens per second):

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
   served, overstates what a replica sustains at the SLO.

**Passes when** the utilization is the same across the mix and leaves room for
bursts (70 % leaves 43 %), and the constant is a capacity measured at the SLO
on the deployed stack. The constant's value always *needs measurement*.

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

**Question.** If the pool is at zero and requests arrive, does the trigger read
above `activationThreshold`?

**How.** Skip if `minReplicaCount` is above 0, and record "n/a, min 1". Else,
from step 3, find the process that exports the series. A vLLM pod's series
disappear with the pod; the router's and the gateway's exist with no pods,
unless step 3 found that the series is deleted when no endpoint exists (trap
T5).

**Passes when** the series exists at zero replicas and reads above
`activationThreshold` for the smallest load that should wake the pool.

**Example.** `minReplicas: 1` (`token-aware.yaml:299`). **n/a.**

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
**Fail.** EPP `v0.11.0` is in the range of trap T3b. The query's liveness gate
(`and on (target_pod)` with `per_endpoint_queue_size`, `token-aware.yaml:315`)
drops the series of deleted endpoints, which is half of T3b's workaround; it
does not `clamp_min` each series at 0. *From the metric's source.*

### Recording the result

For each property, record a row:

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P2 coupled to actuation | fail | at 2.5 pods' worth, `U` = 13.9 at 4 pods and 11.4 at 5; desired 10, then 8; right answer 4 | from the metric's source |

The skill's report template has the full layout.

## 6. Hand over

The result for one trigger is a row per property, plus the kind, the role it
can play and the role it plays. The skill combines the rows of every trigger in
the ScaledObject into the verdict.
