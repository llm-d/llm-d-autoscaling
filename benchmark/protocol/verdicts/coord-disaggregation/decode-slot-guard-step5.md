# Step 5 GPU evaluation plan for maintainer review

[Step 1 verdict](decode-slot-guard.md) | [GPU protocol](../../5-gpu-evaluation.md)
| [Run set](../../run-set.md)

Status **NOT RUN**. Review the plan on issue #1595 before GPU execution.

Use Qwen/Qwen3-VL-32B-Instruct, H100 TP2 DP1 in both roles and the
Coordinator PD-only guide, with a dedicated namespace per arm. Record
vLLM, Coordinator, EPP, LWS, KEDA, monitoring, gateway and harness versions,
image digests, rendered pod arguments and the exact guide prerequisite
patches. Capture the startup-resolved `max_num_seqs` with vLLM debug logging on
both arms and confirm that running admission defaults to that same
1024-slot limit. Check each decode engine, HPA
access to the role scaler and readiness roster coverage. Confirm that
each source reports during warmup and reconcile the readiness roster against
the LWS pod list. Any GPU execution requires separate approval.

Run the protocol in order using the same llmdbenchmark workload profiles,
seeds and phase durations in each arm. Pin the test role to one replica and hold
the other role at sufficient fixed capacity for each knee
and measure the four regular mix knees and a shared-prefix knee. Freeze
the paired active loads after knee measurement and before policy trials.
Use the [Step 3 shared-prefix profile
recipe](decode-slot-guard-step3.md#reproduction)
with the GPU knee measured on this plant.
Use TTFT <= 2 s and TPOT <= 50 ms jointly on 95% of offered requests.
Run active staircase, mix shift, both overloads, agentic, fixed replicas
at active mix-shift peaks, bursty, short spikes and scale-down. Record
scale-from-zero as inapplicable with minReplicas 1. Use the measured cold
start to size holds and report departures from the 120-second evaluation.
Measure desired-replica-to-Ready, pod-scheduled-to-Ready and backlog-drain
times separately. Record image pulls, model loading and unschedulable time.

Run the control and 716.8 candidate on the same plant. Allocate the same
GPU quota and node type to both arms. Record install, run and teardown
commands, guide source, rendered specs and workload checksums. Predeclare 665.6
and 768 as sensitivity arms with the same profiles. Include a held-out
shared-prefix run built from the pinned llm-d-benchmark shared-prefix
profile and report actual hits, running cap, independent trigger crossing,
request, Ready and drain times. Require the guard to stay below the
winning control proposal during KV-bound steady stages. Report a
shared-prefix benefit only when attainment improves and each peak stage
reaches the SLO. Preserve startup misses and report additional cost.

Retain timestamped request rows, PodMonitor and kube-state-metrics queries,
HPA and RoleScaler events, offered load, replica supply plots, TTFT/TPOT/ITL
tails, joint attainment, requested/Ready replica-seconds and overshoot
above the fixed knee-based target. Record infrastructure discards using
the run-set gate. Publish the exact run commands and plan for review
before booking or using GPUs.
