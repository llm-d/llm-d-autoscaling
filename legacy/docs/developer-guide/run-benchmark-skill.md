# The `run-benchmark` Skill

`run-benchmark` is a Claude Code skill (defined in
[`.claude/skills/run-benchmark/SKILL.md`](../../.claude/skills/run-benchmark/SKILL.md))
that drives the autoscaling test bed under [`benchmark/`](../../benchmark)
through its full lifecycle — `standup → smoketest → run → teardown` — and
optionally renders the per-session Grafana/HTML report.

It exists so that running a scenario is a single request instead of a sequence
of hand-assembled `llmdbenchmark` invocations that must all carry the same
flags. The skill encodes the operational workflow and the guardrails; it does
**not** replace [`benchmark/README.md`](../../benchmark/README.md), which
remains the source of truth for the directory's design.

## When it applies

The skill covers the `benchmark/` test bed only — the `llmdbenchmark` CLI
driven directly against `benchmark/config/` (specifications, backend-agnostic
scenarios, and `cluster-config` backend overlays), using a sibling
`../llm-d-benchmark` clone.

It is **not** the `make benchmark-*` workflow described in
[`benchmark-guide.md`](benchmark-guide.md). That workflow is a separate path:
it clones `llm-d-benchmark` into the repo root, reads scenarios from
`test/benchmark/scenarios/`, and targets an OpenShift cluster. The two do not
share configuration. Use `run-benchmark` for the `benchmark/` KEDA test bed;
use the make targets for the single-/multi-model WVA scaling benchmarks.

## Invoking it

```
/run-benchmark [spec] [backend] [-p <namespace>] [phase]
```

All arguments are free-form and optional; anything missing is resolved
interactively. Natural-language requests work too ("stand up
pd-disaggregation on the sim backend in namespace bench", "dry-run the
kv-early experiment", "tear down my benchmark run").

| Argument | Meaning | Default |
|---|---|---|
| `spec` | Scenario to run — a bare name (`pd-disaggregation`, `staging/pd-disaggregation/kv-early`) or a full `benchmark/config/specification/**/*.yaml.j2` path | asks if more than one guide exists |
| `backend` | `--cluster-config` overlay — `ocp/model-sim-qwen3-32b`, `k8s/inference-sim`, or `ocp/vllm` | `ocp/model-sim-qwen3-32b` (CPU/Kind); `ocp/vllm` only with GPUs |
| `-p <namespace>` | Target namespace | **required — never guessed** |
| `phase` | `dry-run`, `standup`, `smoketest`, `run`, `teardown`, `report`, or `all` | resolved from the request |
| harness + workload | For `run`: `-l <harness> -w <workload>` | `-l inference-perf -w sanity_random.yaml` |

`all` runs `dry-run → standup → smoketest → run → report`, stopping on the
first failure. Teardown is never part of `all` — it happens only on an
explicit request.

## What the skill does on your behalf

1. **Resolves inputs** and restates the plan in one line before acting.
2. **Puts `llmdbenchmark` on PATH** by activating the sibling clone's venv
   (`source ../llm-d-benchmark/.venv/bin/activate`), and surfaces the clone +
   install commands if it is missing.
3. **Preflights the cluster** for anything past `--dry-run`: confirms
   `kubectl config current-context` with you before the first write, and
   checks the KEDA / kube-prometheus-stack dependencies the scenario expects.
4. **Runs a dry-run composition check** before any standup — renders the
   merged `plan/<scenario>/config.yaml` and verifies the backend overlay
   applied (image, `accelerator.count`, per-role resources, init containers,
   vLLM command, `extraObjects`), that lists appear in full, and that the
   scenario's `keda:` block is untouched.
5. **Standup / smoketest / run**, carrying the *same* `--spec`,
   `--cluster-config`, `--workspace benchmark/results`, and `-p` to every
   phase, and records the session directory
   (`benchmark/results/<user>-<timestamp>/`) from each command's output.
6. **Report** (optional): runs `benchmark/hack/benchmark_report.sh all
   <session_dir>` to render `report.html` and capture a Grafana snapshot — see
   [`benchmark/docs/benchmark-report.md`](../../benchmark/docs/benchmark-report.md).
7. **Teardown** only when you ask for it.

## Guardrails it enforces

- **Namespace is required input**, never a default.
- **Confirms the kube context** before every cluster write.
- **Same `--spec` / `--cluster-config` / `--workspace` / `-p` on every
  phase** — a mismatch between standup and teardown orphans resources.
- **`--workspace benchmark/results`** on every phase, so sessions land in one
  repo-local directory instead of scattered temp dirs.
- **Dry-run first**, always, before a standup.
- **Backends never set `keda:`.** The scaling strategy lives in the scenario;
  if an overlay grows a `keda:` block, that is a bug.
- **`model.name`** in a scenario must equal the vLLM `--served-model-name` and
  the HuggingFace id; `shortName` is only the k8s label. The skill will not
  "fix" one without the others.
- **`ocp/vllm.yaml` backend needs real GPUs** — it defaults to
  `ocp/model-sim-qwen3-32b` for CPU/Kind.

## Prerequisites

- A sibling `../llm-d-benchmark` clone with its venv installed
  (`git clone https://github.com/llm-d/llm-d-benchmark.git ../llm-d-benchmark
  && cd ../llm-d-benchmark && ./install.sh`). If it lives elsewhere, the skill
  passes `--base-dir <path>` so the spec's sibling paths resolve.
- For cluster phases: a reachable cluster with KEDA ≥ 2.20 and
  kube-prometheus-stack installed (`make create-kind-cluster` for a local
  Kind cluster). Install commands are in
  [`benchmark/README.md`](../../benchmark/README.md) under "Prepare a
  Kubernetes cluster"; the skill surfaces them rather than installing
  silently.
- For the report step: a local Grafana at `http://localhost:3000`, a
  port-forward to the cluster's Prometheus, and a one-time
  `benchmark/hack/benchmark_report.sh configure`.

## Related

- [`benchmark/README.md`](../../benchmark/README.md) — directory design,
  scenario/backend model, KEDA experiment naming convention.
- [`benchmark/docs/benchmark-report.md`](../../benchmark/docs/benchmark-report.md)
  — the per-session report and Grafana snapshot tooling.
- [`benchmark-guide.md`](benchmark-guide.md) — the separate `make benchmark-*`
  WVA scaling benchmark workflow.
