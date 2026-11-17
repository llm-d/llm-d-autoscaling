---
name: scenario-sanity-check
description: Sanity-check a benchmark scenario in this repo's benchmark/ test bed end to end — standup succeeds, every pod is Running and healthy, sending requests increases a role's replicas by 1, and stopping the requests scales it back down to minReplicas. Use before trusting a new or edited scenario/staging variant/backend overlay, or when asked to "sanity check", "smoke test the autoscaling of", or "verify scaling for" a scenario. Invoke with /scenario-sanity-check [spec] [backend] [-p namespace].
---

# Scenario sanity check

**Arguments:** `$ARGUMENTS` — spec, backend, and `-p <namespace>` as in
`/run-benchmark`. Namespace is required; never guess it.

This is a pass/fail gate, not a measurement. It answers four questions, in
order, and stops at the first failure:

1. **Standup** — did the scenario stand up?
2. **Health** — are all pods Running, Ready, and not restarting?
3. **Scale up** — does load increase a scaled role's replicas by at least 1?
4. **Scale down** — does removing load bring it back to `minReplicas`?

Use the `run-benchmark` skill for the mechanics it already documents (Steps 1–4:
resolving inputs, putting `llmdbenchmark` on PATH, cluster preflight, dry-run;
Step 5 standup; Step 7 ScaledObject checks; Step 10 teardown). Do not repeat
them here — follow them, with the same flags on every command. Confirm
`kubectl config current-context` with the user before the first cluster write.

## Speed: patch the timings first

The check must finish in minutes, not the 30+ the scenario's production timings
need. Before Check 1, generate a fast copy of the spec and scenario (never edit
the originals):

```bash
ROOT=$(git rev-parse --show-toplevel)
PY="$ROOT/../llm-d-benchmark/.venv/bin/python"     # has PyYAML
"$PY" -I .claude/skills/scenario-sanity-check/patch_scenario.py \
  <spec.yaml.j2> benchmark/results/sanity/<ns>
```

The last line it prints is the patched spec; **use it as `--spec` for every
command** (standup, smoketest, run, teardown) and tell the user the original
was not touched. For each ScaledObject it sets `pollingInterval: 5`, scale-up
policy `periodSeconds: 10` and stabilization 0, scale-down stabilization 10s
and policy `periodSeconds: 10` (plus `cooldownPeriod: 10` when scaling to
zero). It does not touch triggers, thresholds, min/max, or `scaleTargetRef`:
the check must still exercise the scenario's own signal. The output dir is under
the gitignored `benchmark/results/`.

Other time sinks, in order of payoff:

- **Reuse a stack.** If the scenario is already up in `<ns>` from the same
  patched spec, skip standup; if only the original-timing ScaledObjects are
  up, teardown and standup with the patched spec (no in-place update).
- **Smallest backend.** Prefer a CPU sim / inference-sim backend; the model
  download and pod startup dominate Check 2.
- **Shrink the load run.** Use a short stage (about 60–90s) at a rate that
  crosses the trigger threshold, not a full workload profile.
- **Poll tightly.** 5–10s polls, and stop as soon as the condition holds.
- **Not patchable:** pod startup/image pull, Prometheus scrape interval
  (ServiceMonitor `interval`, 15s in these scenarios) and the cluster's HPA sync
  period. Budget for them; a pod-level count change is typically 1–3 minutes
  after load starts.

Record each check as PASS/FAIL with the evidence (command output excerpt) and
print the summary table at the end.

## Check 1 — Standup

Dry-run first with the patched spec (`run-benchmark` Step 4), then:

```bash
llmdbenchmark standup --spec <spec.j2> --cluster-config <backend.yaml> \
  --workspace benchmark/results -p <ns>
```

PASS: exit code 0 and the last line is a workspace path (record it). FAIL: any
non-zero exit or failed step; show the failing step's log from the session
directory and stop.

## Check 2 — Pods up and healthy

```bash
kubectl get pods -n <ns> -o wide
kubectl wait --for=condition=Ready pod --all -n <ns> --timeout=<budget>
```

Budget: 20m for real vLLM, 10m for CPU sim / inference-sim (model download
dominates). Then check:

```bash
# any container restarting or not ready; Completed Jobs are fine
kubectl get pods -n <ns> -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.containerStatuses[*]}{.ready}{"/"}{.restartCount}{" "}{end}{"\n"}{end}'
kubectl get events -n <ns> --field-selector type=Warning
```

PASS: every non-Job pod is `Running` with all containers ready and
`restartCount` 0 (or unchanged across two looks a minute apart); no
`CrashLoopBackOff`, `ImagePullBackOff`, `Pending`, `OOMKilled`. Include the
gateway/EPP and decode/prefill (or equivalent) pods — "all" means all. Also run
`llmdbenchmark smoketest` (same flags) and require it to pass.

Then gate on `run-benchmark` Step 7: every ScaledObject `READY=True` and its HPA
showing real (non-`<unknown>`) metrics. A scenario whose ScaledObject cannot read
its metric cannot pass Check 3, so fail here with the cause.

## Check 3 — Load increases replicas by 1

Pick the scaled role(s) from `kubectl get scaledobject -n <ns>`; note each
Deployment's starting replicas (expect `minReplicas`). If a target starts
above 1, wait for it to settle first or report the baseline.

Generate load strong enough to cross the scenario's trigger thresholds (read
them from the ScaledObject — e.g. queue size 20, KV 0.80) — the `sanity_random`
workload is usually too light. Use `llmdbenchmark run` with a heavier profile
or a custom `--workload-file-path`, in the background, so you can watch scaling
while it runs:

```bash
llmdbenchmark run --spec <spec.j2> --cluster-config <backend.yaml> \
  --workspace benchmark/results -p <ns> -l inference-perf -w <heavy workload> &
```

If no profile saturates the sim backend, raise the rate/concurrency until the
trigger metric (the PromQL in the ScaledObject) goes above threshold; report
the load used.

Watch, polling every 15–30s:

```bash
kubectl get hpa,deploy -n <ns>
kubectl get scaledobject -n <ns>        # ACTIVE should turn True
```

Timeout (with patched timings): scrape lag (~15s) + polling (5s) + policy
period (10s) + pod startup — usually 3–5 minutes; give up at 8. PASS: a scaled Deployment reaches `readyReplicas >= start + 1`. FAIL: it never
does; report the trigger metric value vs. threshold (query it) to tell "load too
light" from "scaling broken", and the HPA conditions.

## Check 4 — Idle scales back down

Stop the load (let the run finish or kill it; confirm requests have stopped,
e.g. `vllm:num_requests_running` is 0). Then poll as above.

Timeout (with patched timings): 10s stabilization + 10s per pod removed +
scrape lag, usually 1–2 minutes; give up at 5. (With unpatched timings expect
the original `stabilizationWindowSeconds` plus a `periodSeconds` per pod.)
PASS: the Deployment drops to the ScaledObject's `minReplicas` (read it from
`spec.minReplicaCount`; 0 is valid). FAIL: still above it after the timeout; report the HPA
`ScalingActive`/`AbleToScale` conditions and current metric.

## Output

```
Scenario: <spec> / <backend>   ns: <ns>   session: <dir>
1 standup      PASS|FAIL  <evidence>
2 pods healthy PASS|FAIL  <n pods, restarts, ScaledObjects>
3 scale up     PASS|FAIL  <role: start -> start+N after Xm, load used>
4 scale down   PASS|FAIL  <role: N -> M after Xm>
```

Leave the stack up unless asked; teardown only on explicit request
(`run-benchmark` Step 10). If a check failed, say which and the next debugging
step; if the cluster was unavailable, say the check was not run rather than
inferring a result.

## Gotchas

- Fail fast: do not run Check 3 against pods that failed Check 2.
- Load must be sent through the gateway/stack endpoint (what `llmdbenchmark
  run` does), not to a single pod, or the EPP-driven metrics won't move
  correctly.
- Only the timing patch is allowed. Never change triggers, thresholds, or
  replica bounds to make a check pass; report the failure.
- A pass on patched timings says the signal and wiring work, not that the
  production timings are right.
