# Evaluation protocol for autoscaling configurations

This protocol lists the steps for evaluating a KEDA autoscaling configuration (a
`keda:` block in `benchmark/config/scenarios/`) before and after it runs on a
cluster. Each step either rejects the configuration or hands the next step a
list of questions to settle.

| Step | What it decides | Needs a cluster? |
|---|---|---|
| [1. Static signal analysis](1-static-signal-analysis.md) | whether each trigger, and their combination, can compute the right replica count at all | no |

Step 1 has three supporting documents:

- [How to analyze one trigger](1-per-trigger-how-to.md): the per-trigger
  procedure, with where to look and what counts as evidence;
- [Metric traps](traps.md): version-specific metric pitfalls, maintained
  separately;
- the [`static-signal-analysis` skill](../../.claude/skills/static-signal-analysis/SKILL.md),
  which runs the step end to end and writes the verdict.

Later steps (calibration, benchmark runs, efficiency against an oracle) will be
added here.
