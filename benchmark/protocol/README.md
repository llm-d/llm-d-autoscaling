# Evaluation protocol for autoscaling configurations

This protocol lists the steps for evaluating a KEDA autoscaling configuration (a
`keda:` block in `benchmark/config/scenarios/`) before and after it runs on a
cluster. Each step either rejects the configuration or hands the next step a
list of questions to settle.

| Step | What it decides | Needs a cluster? |
|---|---|---|
| [1. Static signal analysis](1-static-signal-analysis.md) | whether each trigger, and their combination, can compute the right replica count at all | no |
| [2. Verification with a simulator](2-simulator-verification.md) | whether each failure step 1 predicted shows up on a real cluster, with `llm-d-inference-sim` or `vllm-model-sim` in place of GPUs | yes (CPU only) |
| [3. Evaluation in blis-signal-lab](3-blis-evaluation.md) | how the pool follows the run set in a simulated plant, before any GPU is held: replicas against load, SLO attainment, and cost against a static fleet, in the simulator's numbers | no (CPU, local) |
| [5. Evaluation on GPU](5-gpu-evaluation.md) | how the pool follows a fixed run set on the target GPU plant: capacity, replicas against load, SLO attainment, and cost against a static fleet | yes (GPU) |

Step 1 has three supporting documents:

- [How to analyze one trigger](1-per-trigger-how-to.md): the per-trigger
  procedure, with where to look and what counts as evidence;
- [Metric traps](traps.md): version-specific metric pitfalls, maintained
  separately;
- the [`static-signal-analysis` skill](../../.claude/skills/static-signal-analysis/SKILL.md),
  which runs the step end to end and writes the verdict.

Step 2 is run by the [`simulator-verification` skill](../../.claude/skills/simulator-verification/SKILL.md).

Steps 3 and 5 run the same [evaluation run set](run-set.md): the loads, the
runs, the reports, the gate after each run, and the commands they share
([`scripts/`](scripts/): `gen_load.py` writes the loads for either plant,
`stage_table.py` the per-stage table). Each step adds only its plant.

Step 3 is run by the [`blis-evaluation` skill](../../.claude/skills/blis-evaluation/SKILL.md),
step 5 by the [`gpu-evaluation` skill](../../.claude/skills/gpu-evaluation/SKILL.md).

Step 4, and efficiency against an oracle, are not written yet.
