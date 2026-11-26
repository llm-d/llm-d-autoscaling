# BLIS evaluation: coord-disaggregation/decode-slot-guard

[Step 1](decode-slot-guard.md) · [Step 2](decode-slot-guard-step2.md) ·
[Evaluation protocol](../../3-blis-evaluation.md)

<!-- markdownlint-disable MD013 -->

| | |
| --- | --- |
| Official plant | Qwen3-VL-32B BF16, H100 TP2 DP1 per role, as in [coordinator-kv](coordinator-kv-step3.md) |
| Protocol tooling | blis-signal-lab `25f2dfb`, `signals-hpa` branch |
| Model data | blis-substrate v0.14.0 plus [catalog PR #415](https://github.com/inference-sim/blis-substrate/pull/415) at `722c4c85be153697f1372f5ad2df1cd3e387d748` |
| Harness | llm-d-benchmark `61e9e47b3c6df6e26c1575539be84d81e12e9f71` |
| Primary cold start | 120 s |
| SLO | Joint TTFT <= 2 s and TPOT <= 50 ms on at least 95% of arrivals |
| Slot targets | 716.8 primary, 665.6 and 768 sensitivity, conditional on a 1024-slot decode cap |
| Verdict | **NOT RUN** |

<!-- markdownlint-enable MD013 -->

The [pinned simulator fork](https://github.com/vishakha-ramani/blis-signal-lab)
returned repository-not-found over Git and HTTP 404 for both the commit
API and source archive on 2026-11-26. The public inference-sim repository
does not resolve `25f2dfb`. The control's
[archived Step 3 inputs and traces](coordinator-kv-step3/) and
[TTFT projection patch](coordinator-kv-step3/overload-over/lab-patch.diff)
do not provide the lab source. Catalog PR #415 exposes the same model
entry used by the control and remains open. A maintainer-provided build
or accessible checkout of the pinned lab with that catalog entry would
allow the paired runs. Verify the build, then verify the model before
using any results.

## Reproduction

Run from the repository root with Python 3 and an accessible lab checkout.
Set `LAB` to its absolute path, `case` to an indexed active run directory
(for example, `mixshift`), and `run_dir` to a writable output directory.
Build the pinned lab with its Go toolchain. Follow the control's catalog
procedure and use the same model and plant in both arms. The archived plant
uses engine version 0.32.0, leaves the sequence limit implicit and uses the
EPP prefix decider to approximate Coordinator routing. Confirm that each
arm resolves to a 1024-slot decode cap before evaluating these thresholds.

```sh
git -C "$LAB" rev-parse HEAD
(cd "$LAB" && GOTOOLCHAIN=auto go build -o blis main.go)
"$LAB/blis" catalog resolve --model Qwen/Qwen3-VL-32B-Instruct --json

control="$PWD/benchmark/protocol/verdicts/coord-disaggregation/coordinator-kv-step3"
python3 - "$control/$case/policy.json" "$run_dir" <<'PYTHON'
import copy
import json
import sys
from pathlib import Path

base = json.loads(Path(sys.argv[1]).read_text())
output = Path(sys.argv[2])
output.mkdir(parents=True, exist_ok=True)
for ratio in (0.65, 0.70, 0.75):
    policy = copy.deepcopy(base)
    policy["autoscaler"]["hpa_by_role"]["decode"]["triggers"].append({
        "name": "decode-running-slots",
        "target_type": "AverageValue",
        "target": round(ratio * 1024, 1),
        "terms": [{"metric": "running", "aggregation": "sum", "role": "decode"}],
    })
    (output / f"policy-{ratio:.2f}.json").write_text(
        json.dumps(policy, indent=2) + "\n"
    )
PYTHON
```

For each indexed run, use its `workload.yaml`, seed, instance counts,
horizon and other arguments from `command.txt`. Generate a candidate
policy for that case and pass `--policy-config "$run_dir/policy-0.70.json"`.
For the pinned knees, keep the fixed replica bounds in each arm and
verify the guard translation against the running gauge on the first
knee before proceeding. For run 6, pin each role to the peak reached in
the corresponding active run 3 arm. Keep each run's outputs in its own
directory. The lab's Ready-only running term approximates the gauge
sum. Verify inventory and stale telemetry through the real scrape path.

For the shared-prefix supplement, start with the pinned benchmark's
`shared_prefix_synthetic_heavy.yaml.in` generator schema. Use one
12,000-token shared prefix, 288 unique tokens per request, 32 prompt
variants per group, 1,024 output tokens and seed 1595. Pin decode at one
for the four-rung knee sweep. Freeze an active script at 0.5x, 1x and 2x
the measured knee with 900 s per phase, then replay both policies with
120 s startup. Record actual prefix hits and verify locality in the lab
before scoring this supplement.

## Runs

<!-- markdownlint-disable MD013 -->

| # | Run | Candidate disposition |
| --- | --- | --- |
| 1a–1f | Pinned knees across regular mixes | NOT RUN, pinned simulator unavailable |
| 2 | Active staircase | NOT RUN, same dependency |
| 3 | Mix shift | NOT RUN, same dependency |
| 4a–4b | Overload under and over cap | NOT RUN, same dependency |
| 5 | Agentic | NOT RUN, same dependency |
| 6 | Static control pinned to active peak | NOT RUN, same dependency |
| 7 | Scale from zero | Inapplicable, minReplicaCount 1 |
| 8–9 | Bursty and short spikes | NOT RUN, pinned simulator unavailable |
| 10 | Scale-down | NOT RUN, same dependency |
| Additional | Pinned and active shared-prefix workload | NOT RUN, same dependency |

<!-- markdownlint-enable MD013 -->

## Substitute results

The research packet screened 789 runs and performed 64 targeted replays
with public
[inference-sim](https://github.com/inference-sim/inference-sim/tree/e7597a8c9510f876653d7d87bd8baf195c37c5c7)
with experimental patches. The plant models colocated Qwen3-14B H100
TP2 with 64 slots, instantaneous 15 s observations and a controller
that accounts for loading replicas. The archived 48-slot arm targeted
75% of that cap. At 120 s startup, 25 new 44.8-slot replays (70% of 64)
were matched to 50 archived control and 48-slot runs. The table gives
five-seed means per arm across five workloads.

The simulator patches, per-seed inputs, commands and outputs are in a
local research archive. External reproduction of the substitute
numbers requires that archive. The harness and commands above are for
the separate Coordinator evaluation.

<!-- markdownlint-disable MD013 -->

| Workload | Attainment control / 48 / 44.8 (%) | Replica-s control / 48 / 44.8 | TTFT p95 control / 48 / 44.8 (s) |
| --- | --- | --- | --- |
| Shared-prefix shift | 74.93 / 79.26 / 79.70 | 2100 / 2088 / 2082 | 34.57 / 26.04 / 25.40 |
| Cold long shift | 74.88 / 74.88 / 74.88 | 2382 / 2382 / 2382 | 102.29 / 102.29 / 102.29 |
| Short burst | 59.17 / 59.17 / 59.17 | 2016 / 2025 / 2025 | 4.45 / 4.45 / 4.45 |
| Closed-loop fast turns | 74.61 / 82.76 / 83.24 | 1998 / 2028 / 2031 | 3.71 / 3.39 / 3.35 |
| Healthy short context, long output | 100 / 100 / 100 | 720 / 720 / 831 | 0.084 / 0.084 / 0.084 |

<!-- markdownlint-enable MD013 -->

The 44.8 arm added a replica in one healthy long-output seed with no
attainment gain. The historical 32 target used 1400 replica-s on the
healthy long-output sensitivity at 30 s with 100% in both arms.
At 30 s on shared-prefix, target 48 raised mean overall attainment
from 91.04% to 95.51%, with 1698 to 1425 replica-s. Every peak stage
still missed 95%.

At 120 s the first extra Ready pod appeared on average 192, 168 and 165 s
after the shared-prefix shift for the control, 48 and 44.8 arms respectively.
Single-replica service carried most of the 270 s peak. The control and
44.8 arm met the TTFT limit for 497/1909 and 507/1647 peak requests arriving
before their first extra Ready pod, and 774/791 and 1036/1053 arriving
after Ready, respectively. Every peak request passed TPOT in those samples.
Peak joint attainment was 47.07%, 56.22% and 57.15% across the three arms.
Mean run p95 ITL was 39.54, 39.53 and 39.53 ms. Requests retained past the
substitute client
deadline can distort drain and replica cost. The closed-loop case also
missed the peak gate (67.80%, 78.14% and 78.74%).

The substitute shows sensitivity to startup delay and workload shape.
The shared-prefix case missed the 95% SLO at 120 s. Workload quietness
and latency on the official plant remain open. The full run set needs the pinned
simulator and model entry, then
paired execution without changing the predeclared targets.
