---
name: static-signal-analysis
description: Run step 1 of this repo's autoscaling evaluation protocol (benchmark/protocol/) on a KEDA scaling configuration — judge, without a cluster, whether each trigger and their combination can compute the right replica count, and write the verdict. Use when asked to review, assess or analyze a scenario's keda block, a ScaledObject, a new staging variant, or a PromQL scaling trigger before running it. Invoke with /static-signal-analysis <scenario.yaml> [backend overlay].
---

# Static signal analysis

**Arguments:** `$ARGUMENTS`: a scenario under `benchmark/config/scenarios/`,
optionally a backend overlay under `benchmark/config/cluster-configs/`. If no
overlay is given, use the one the scenario's header names, or ask.

The definitions this skill applies live in the protocol, not here:

- [`benchmark/protocol/1-static-signal-analysis.md`](../../../benchmark/protocol/1-static-signal-analysis.md):
  what the HPA computes (§1.1), the properties (§1.2), kinds and roles (§1.3);
- [`benchmark/protocol/1-per-trigger-how-to.md`](../../../benchmark/protocol/1-per-trigger-how-to.md):
  how to establish each of them for one trigger;
- [`benchmark/protocol/traps.md`](../../../benchmark/protocol/traps.md): known
  metric traps, by component version.

Read all three before starting. Do not restate their content in the output;
cite the property or section.

## Step 1: Analyze each trigger

For every ScaledObject and every trigger in it, follow
`1-per-trigger-how-to.md` §1–§6. Rendering with `--dry-run` is local and safe;
anything else that would touch a cluster needs the user's confirmation.

Clone exporters' source into a temporary directory, never into this
repository. When a trigger reads the same metric as an earlier one, reuse that
analysis.

## Step 2: Check the combination

Once every trigger has its rows, check the ScaledObject as a whole. With this
repo's template, `scalingModifiers` is not rendered (see the how-to, §1), so
the default maximum applies unless the rendered object shows otherwise.

- **Who decides where.** For each trigger, decide under which conditions it
  sets the replica count: when it wins the maximum, or when the formula's
  branch or weight gives it control. Sound: one proportional driver, with
  guards and alarms that stay out of the result in normal operation. Unsound:
  a guard or alarm that decides in steady state, or a configuration with no
  proportional trigger at all. The maximum of non-proportional signals is
  still non-proportional.
- **Scale-in.** A backlog that collapses after a scale-up can drive the pool
  below what the driver asks. A trigger that stays high after load drops (a
  frozen gauge, a long window) blocks scale-in.
- **`scalingModifiers.formula`.** The formula replaces the maximum, so the
  HPA sees one metric and the per-trigger thresholds and `metricType` no
  longer apply. Apply P1–P4 to the formula's result, check its own `target` and
  `metricType`, and check each operator across the load range:
  - a `min` lets the smallest trigger win, so a backlog that collapses after a
    scale-up drags the pool down;
  - a weighted sum is proportional only if every term is;
  - a conditional switches between laws, and each branch must pass on the
    range where it is taken.
- **Disaggregated pools.** Each pool's trigger must measure that pool's work. On
  prefill/decode, `inflight_tokens` on the prefill pool counts until the first
  token, which comes from the decode pool, so it carries the KV transfer and
  decode delays.
- **`behavior`.** Slow scale-up policies bound the swing of a non-proportional
  signal but do not stop it oscillating. An exploding signal pins at
  `maxReplicaCount`; that is harmless only if the true need is already at the
  cap.

## Step 3: Write the verdict

Write the verdict as a Markdown report, using the template below. Save it as
`benchmark/protocol/verdicts/<guide>/<strategy>.md` (for example
`verdicts/pd-disaggregation/token-aware.md`), so it sits in version control
next to the protocol, and show it in the chat. Do not edit the scenario itself
without asking.

### Evidence rules

Every finding carries one evidence tag and the evidence itself. A finding
without its evidence is not finished.

| Tag | Evidence required | Example |
|---|---|---|
| `config` | the rendered file and line, quoted | `27_keda-scaledobjects.yaml:42`: `metricType: AverageValue` |
| `source` | `repo@commit:path#Lline`, at the deployed version, with the line quoted or paraphrased | `vllm-project/vllm@3facf82:vllm/v1/metrics/loggers.py#L537-L542`: `vllm:num_requests_waiting` is a gauge |
| `measure` | the question to answer, phrased so step 2 can answer it | "sustained prefill tokens/s of one replica at the TTFT SLO, for the benchmark's mix" |

Also:

- **Show the arithmetic.** When a finding rests on numbers, write the
  computation with the configuration's own values: `1.0 / 0.95 = 1.05 < 1.1`,
  not "too close to the ceiling".
- **Give a concrete case for each failure.** A load level, a pod count or an
  event where the trigger asks for the wrong number, and what it asks for.
- **Quote, don't summarize, the query.** Write it out as rendered, with
  placeholders resolved.
- **Say what was not established.** If a source could not be read at the
  deployed version (a floating tag, an unreachable repository), mark the
  finding `measure` and say why.

### Template

Replace every `<…>`. Keep the headings, so verdicts can be compared across
variants.

~~~markdown
# Static signal analysis: <guide>/<strategy>

| | |
|---|---|
| Scenario | `benchmark/config/scenarios/staging/<guide>/<strategy>.yaml` |
| Backend overlay | `benchmark/config/cluster-configs/<platform>/<backend>.yaml` |
| Rendered with | `llmdbenchmark standup --dry-run`, llm-d-benchmark@<commit>, or "by hand, defaults from 27_keda-scaledobjects.yaml.j2@<commit>" |
| Date | <YYYY-MM-DD> |
| Verdict | **Accepted** / **Accepted with conditions** / **Rejected** |

<One paragraph: the verdict, the reason in one sentence, and the predicted
behavior in one sentence.>

## Deployed versions

| Component | Version | Source of the version |
|---|---|---|
| <EPP> | <v0.11.0> | <llm-d-benchmark defaults.yaml@commit:line> |

## ScaledObject `<name>`

<minReplicaCount, maxReplicaCount, pollingInterval, behavior: one line each,
with rendered line numbers.>

### Trigger `<trigger name>`

```promql
<query, as rendered>
```

`metricType: <…>`, `threshold: <…>`, `activationThreshold: <…>`
(`<file>:<line>`).

- **What it counts:** <the metric's type, and the events at which a request is
  added and removed, or at which the counter is incremented>. `source`:
  <repo@commit:path#L…>.
- **Kind:** <kind from §1.3>. **Role it can play:** <role>. **Role it plays
  here:** <role>.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P1 proportional | pass / fail / n/a | <evidence, with arithmetic> | config / source / measure |
| … | | | |

<Only properties the role requires, plus any other that fails. n/a must say
why.>

**Failure case:** <a concrete load and pod count where this trigger asks for
the wrong replica count, with the computation.>

## Combination

- **Driver in normal operation:** <trigger, or "none">.
- **Who decides where:** <per trigger: the conditions under which it sets the
  count>.
- **Scale-in:** <what drives the pool down, and whether it can go below need>.
- **behavior and bounds:** <effect of the scale-up/scale-down policies and of
  maxReplicaCount>.

**Predicted behavior:** <two or three sentences: what the pool does at low
load, near capacity, past capacity, and at constant load>.

## Needs measurement

1. <question for step 2>, needed by <finding>.

## Fixes

| Fails | Fix | Effect |
|---|---|---|
| <trigger, property> | <concrete change to the keda block, as YAML or PromQL> | <which check it makes pass> |
~~~

### Example

An extract of a verdict for `staging/pd-disaggregation/baseline.yaml`, prefill
pool, showing the expected depth. Line numbers are from the scenario, since it
was rendered by hand.

~~~markdown
### Trigger `vllm-kv-utilization`

```promql
max(vllm:kv_cache_usage_perc{model_name="Qwen/Qwen3-32B",role="prefill"})
```

`metricType: AverageValue`, `threshold: "0.95"`, `activationThreshold: "0"`
(`baseline.yaml:206-211`).

- **What it counts:** a gauge, the fraction of the KV-cache blocks in use on
  one vLLM engine; 1 means full. `source`:
  `vllm-project/vllm@3facf82:vllm/v1/metrics/loggers.py#L619-L624`.
- **Kind:** capped saturation, ceiling 1. **Role it can play:** guard. **Role
  it plays here:** guard, next to the queue trigger.

| Property | Result | Evidence | Tag |
|---|---|---|---|
| P4 metric type | fail | `max(...)` is intensive, but `metricType: AverageValue` (`baseline.yaml:210`) gives `ceil(max / 0.95)`, whatever the pod count: at most `ceil(1 / 0.95) = 2` | config |
| P5 bounds | fail | ceiling 1, threshold 0.95: one full pod gives `1.0 / 0.95 = 1.05`, inside the HPA's 10 % tolerance. The threshold must be below `1 / 1.1 = 0.909` | config |
| P8 lag | pass | instant gauge; lag = vLLM scrape 30 s (llm-d-benchmark `defaults.yaml`, `monitoring.scrapeInterval`) + KEDA polling 30 s (not rendered, KEDA default) = 60 s, against a cold start of minutes | config |
| P9 measures what it claims | pass | no trap on `vllm:kv_cache_usage_perc` in `traps.md` | source |

**Failure case:** one prefill pod with its KV cache full. The HPA computes
`ratio = 1.0 / 0.95 = 1.05`, within `[0.9, 1.1]`, so it does nothing. With 3
pods all full it computes `ceil(1.0 / 0.95) = 2`: a scale-in to 2, unless
another trigger asks for more.
~~~

Before finishing, check the report against the template: every trigger has
its table, every `fail` has a failure case, every `measure` appears under
"Needs measurement", and every failure has a fix.
