# Evaluation protocol for autoscaling configurations

This protocol lists the steps for evaluating a KEDA autoscaling configuration (a
`keda:` block in `benchmark/config/scenarios/`) before and after it runs on a
cluster. Each step either rejects the configuration or hands the next step a
list of questions to settle.

| Step | What it decides | Needs a cluster? |
|---|---|---|
| [1. Static signal analysis](1-static-signal-analysis.md) | whether each trigger, and their combination, can compute the right replica count at all | no |

Step 1 uses the [metric traps](traps.md), a list of version-specific metric
pitfalls that is maintained separately.

Later steps (calibration, benchmark runs, efficiency against an oracle) will be
added here.
