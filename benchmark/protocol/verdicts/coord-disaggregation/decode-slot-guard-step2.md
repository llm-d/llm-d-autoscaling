# Step 2 simulator verification for coord-disaggregation/decode-slot-guard

[Step 1 verdict](decode-slot-guard.md) | [Protocol](../../2-simulator-verification.md)

<!-- markdownlint-disable MD013 -->

| | |
| --- | --- |
| Date | 2026-11-26 |
| Namespace and cluster version | `step2-decode-slot-guard-20261126-live`, Kubernetes v1.31.12 |
| Control | [coordinator-kv Step 2](coordinator-kv-step2.md), CPU vLLM model simulator |
| Supplemental backend | Direct requests to `llm-d-inference-sim:v1.2.0` (`6cc170fcec957148d3adfc7fc9125428b75d2899`), digest `sha256:9992453829c0dea8d120b8e0322bb0e4237cbb3c6d9ea3bdce060410c40c91c9` |
| Control path | KEDA v2.19.0, LWS v1.0.0, Prometheus v3.5.0, kube-state-metrics v2.15.0 |
| Historical live policy | Decode ScaledObject at target 716.8 from `4e4493ef`, adapted to the Kind namespace and Prometheus address |
| Profile | Qwen3-VL-32B-Instruct model name, no weights, 5 simulated running slots, 18 s readiness delay and 5 s scrapes |
| Capacity knee | Unmeasured on this backend |
| Result | **Partial evidence.** The historical policy exercised scraping and actuation. Prometheus fixtures cover the revised inventory checks. Scheduler-faithful Step 2 remains pending. |

<!-- markdownlint-enable MD013 -->

## Prior Kind checks

The fixture run applied control and candidate with running target 48 at
`a247bddeca7063eed04b0e9abe6e6eda6ee28585`. Its remote-written samples
bypassed Kubernetes discovery. The query in `4e4493ef` matched that
run's query and passed seven Prometheus rule fixtures.

<!-- markdownlint-disable MD013 -->

| Check | Observation |
| --- | --- |
| Running-only scale-up | Running 64, KV 0.01 and waiting 0 held the control at 1. The candidate requested 2. |
| Pending and scope | LWS reported 2 replicas with 1 Ready. Pending, prefill and foreign namespace samples were excluded. Duplicate engine scrapes yielded 64. |
| Independent waiting | Waiting 16 requested 4 at target 5 while 1 served. Removing the test taint allowed 4 pods to become Ready. |
| Independent KV | KV 0.95, waiting 2 and running 16 requested 3 from 1. |
| Missing and stale samples | Missing engine data, stale engine data and stale observed readiness each produced an empty vector. The HPA held 4 during errors and current-replica fallback. |
| Recovery and scale-down | Fresh zero cleared fallback and the role returned to 1 with a test-only zero stabilization window. |
| Error arbitration | With invalid slot telemetry, waiting 16 held 3 inside tolerance. Waiting 18 requested 4. |

<!-- markdownlint-enable MD013 -->

## Prometheus fixtures for the staging query

Prometheus v3.5.0 fixtures cover fresh zero, duplicate engine scrapes,
pending pods with and without a running gauge, unscheduled pods without
a Ready condition, missing or stale running
gauges, missing and stale readiness and pod inventory, UID mismatch, no
Ready pods, and exclusion of other roles, namespaces and similarly named
sets. These use synthetic series. The live observations below used the
historical query without the `kube_pod_info` cross-check.

## Historical live simulator observations

The control observations for the inherited triggers are recorded in
[coordinator-kv Step 2](coordinator-kv-step2.md). This run exercised the
historical guard and inherited decode triggers against a live scrape of
Kubernetes readiness and model metrics.

<!-- markdownlint-disable MD013 -->

| Property | Step 1 prediction | Observation and outcome |
| --- | --- | --- |
| P1 proportionality | n/a for guard | Not verified by this skill. |
| P2 actuation coupling | n/a for guard | Not verified by this skill. |
| P3 fixed busyness | n/a for guard | Not verified by this skill. The control simulation supplies the mix comparison. |
| P4 metric type | Pass under DP1 | The waiting sum of 7 at target 5 proposed 2 from 1 and LWS actuated it. The fixture run independently exercised the running metric. |
| P5 bounds | Conditional | Not reproducible here. The fake engine's cap was 5 and the production guard target was 716.8. |
| P6 zero | n/a with min 1 | Not verified by this skill. |
| P7 visible failure | Conditional | The fixture evidence covers missing and stale samples. During the live run the startup query was empty before a decode pod reported Ready. The historical query could miss a pod omitted from readiness inventory. |
| P8 lag and noise | Needs measurement | The live scrape resolves nonzero running and waiting under requests. The 5 s scrape and synthetic engine do not establish production lag or batching noise. |
| P9 semantics | Conditional | Three concurrent decode requests yielded running 3, waiting 0, and one replica. A 12-request burst yielded running 5 and waiting 7. The new pod emitted a running gauge before it was Ready and did not contribute to the guard. |
| P10 pool isolation | Conditional | Three concurrent prefill requests drove prefill running to 3 while all three decode triggers read zero. The prior fixtures cover duplicate scrapes and a foreign namespace. A second stack with overlapping pod names was not exercised. |
| P11 EPP configuration | Pass for guard | Not verified by this skill. The supplemental path sent traffic directly to simulator pods. |

<!-- markdownlint-enable MD013 -->

KEDA exposed all three metrics to HPA. At 07:29:40 UTC the waiting
alarm requested two replicas at target 5. The first sample covering
both Ready pods arrived at 07:30:03 UTC. The Kubernetes Ready set
matched the scraped set at 59 of 60 observations. The final sample
retained the terminating pod in Prometheus for one observation after
Kubernetes dropped it. A subsequent instant query showed the correct
one-pod roster. The removed pod's last running value was zero.

With the configured 300 s stabilization window and one-pod scale-down
policy, HPA reduced decode to one at 07:34:56 UTC, 316 s after scaling
out. The count held at two for over 200 s after all candidate metrics
were zero. This checks a single stabilized reduction.

## Reproduction

Render both arms with llm-d-benchmark at
`61e9e47b3c6df6e26c1575539be84d81e12e9f71`. Use a dedicated namespace
and start one simulator in each role. Set the decode RoleScaler to
External and point the rendered decode ScaledObject to the local
Prometheus. Keep the bounds and 300 s scale-down behavior. Scrape
simulator `/metrics` and kube-state-metrics at 5 s. Keep namespace and
pod labels on both sources and the UID on both Kubernetes metrics. Use the
simulator's readiness probe `/health/ready`.
Send three concurrent direct OpenAI chat requests of 250 output tokens,
then twelve of 1,000 tokens, with 40 ms simulated ITL and seed 1595.
Sample the three queries, the matching Ready pod names, HPA desired and
RoleScaler counts through recovery. For the isolation check, send three
400-token requests only to prefill after decode drains.

## Simulator gaps

This supplement verifies discovery, labels, backlog actuation and
readiness inventory using stock Kubernetes and stock KEDA. It bypasses
the Coordinator and EPP, and the fake engine cannot validate guard
quietness on KV-bound mixes, the effective vLLM cap, real prefix
sharing or SLO knees. The CPU vLLM simulator requires roughly 20 GiB
per pod and exceeds the 7.7 GiB Docker VM. Run the four fixed mixes and
shared-prefix load on a suitably sized cluster, verify the effective
scheduler and admission limits, then measure proposals, scrape lag,
steady-state noise and inventory coverage with the real monitoring
stack. Guide PRs llm-d/llm-d#3111 and #3114 supply the PD-only overlay
and model PodMonitors.
