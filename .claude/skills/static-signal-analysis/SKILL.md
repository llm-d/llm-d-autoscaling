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

The verdict has five parts:

1. per trigger: kind, role, each required property with its result, rendered
   line and evidence tag, and the estimated lag;
2. the combination: which trigger drives in normal operation, which win when,
   and whether that is sound;
3. predicted behavior in one or two sentences;
4. what needs measurement (per-replica capacity, mix range, cold start) — the
   questions for the next step;
5. fixes, each tied to a failed check.

A configuration with no trigger that passes P1–P4 in its intended load range is
**rejected** at this step; running it measures a known failure.

The worked example in §1.5 of the protocol shows the expected depth.

Where to put the verdict: if the user asked for a report, in the chat; if the
scenario is a staging variant being written, offer to add a summary to its
header comment. Do not edit the scenario without asking.
