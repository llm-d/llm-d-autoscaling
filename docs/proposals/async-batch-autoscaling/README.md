# async-batch-autoscaling — prototype manifests and validation runbook

Companion manifests for [the proposal](../async-batch-autoscaling.md).
Validated against a GKE Autopilot cluster running llm-d-async v0.10.0
(Redis sorted-set transport), llm-d-router EPP, a single `Deployment/vllm`
(Qwen3-8B, 1×L4 via a DWS flex-start/spot ComputeClass), and
kube-prometheus-stack. Adjust names/namespaces for other environments.

Only the validated M1 manifests are committed so far. The M2/M3 manifests and
the `prometheus-budget` gate are authored but not yet applied to a cluster;
they land in follow-up commits once each is validated (see the file table).

| File | Purpose | Status |
| --- | --- | --- |
| `worker-pools-readiness-gate.json` | **Validated hold-back config**: fail-closed `prometheus-query` inner gate on `llm_d_epp_ready_endpoints`. Replaces `worker-pools.json` in the llm-d-async ConfigMap. | committed |
| `m1-cron-scaledobject.yaml` | M1: cron-windowed 0↔N scaling (fixed N for the window). | committed |
| `worker-pools-budget-gate.json` | `prometheus-budget` variant — preferred once llm-d-async's cascade queries match the EPP's exported metric names (v0.10.0 queries deprecated `inference_pool_*` names; recent EPPs export only `llm_d_epp_*`, leaving this gate closed even with backends ready — [llm-d-async#460](https://github.com/llm-d/llm-d-async/issues/460)). | after #460 |
| `m2-backlog-scaledobject.yaml` | M2: backlog-driven 0↔N scaling (raw metrics, single-trigger KEDA). | after M2 validation |
| `m3-deadline-scaledobject.yaml` | M3: deadline-driven ScaledObject on the producer's `deadline_required_replicas` metric (+ M2 backlog fallback trigger). | after M3 validation |
| `m3-deadline-prometheusrule.yaml` | M3 **fallback**: recording-rule chain that computes the same metric consumer-side, for llm-d-async builds without the producer metric. | after M3 validation |
| `backlog-source-alert-prometheusrule.yaml` | Alert when the backlog source is untrusted. | after M2 validation |

M3 requires an llm-d-async build that emits `llm_d_async_async_deadline_required_replicas`
and accepts the `deadline_scaling.*` config; the recording-rule file is the
fallback for older builds. M1 and M2 need no llm-d-async code change.

Apply exactly one ScaledObject per scale target at a time (M1 *or* M2 *or*
M3) — multiple ScaledObjects fighting over one Deployment is undefined
behavior in KEDA.

## 0. Install KEDA

Without helm, from the official release manifests:

```sh
kubectl apply --server-side -f \
  https://github.com/kedacore/keda/releases/download/v2.20.2/keda-2.20.2.yaml
kubectl -n keda get pods   # operator, metrics-apiserver, admission-webhooks Ready
```

## 1. Configure hold-back (prerequisite for every milestone)

The gate change is the load-bearing config: without it, llm-d-async
dispatches into a scaled-to-zero pool (see the fail-open caution in the
proposal).

```sh
# Back up, then replace worker-pools.json with worker-pools-readiness-gate.json
kubectl -n llm-d-async get cm llm-d-async-config -o yaml > /tmp/llm-d-async-config.bak.yaml
kubectl -n llm-d-async create cm llm-d-async-config \
  --from-file=worker-pools.json=worker-pools-readiness-gate.json \
  --from-file=request-merge-policy.json=<existing file> \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n llm-d-async rollout restart deploy/llm-d-async   # pool config is read at startup
```

Verify hold-back with the pool at zero:

```sh
kubectl -n llm-d-async scale deploy/vllm --replicas=0
# enqueue requests (see §2), then check:
#   llm_d_async_async_gate_decisions_total{reason="gate_closed"} -> climbing
#   llm_d_async_async_gate_wait_requeues_total -> climbs every gate-wait-timeout
# and confirm nothing reaches the router (EPP logs quiet).
# NOTE: broker_backlog reads 0 for bursts smaller than the pool's worker
# count — claimed requests gate-waiting in workers are invisible to it (see
# the proposal's gate-wait blind spot). Enqueue more than `workers` items if
# you want to see the backlog gauge itself move.
```

## 2. Enqueueing test requests

Messages are ZADDed with the deadline (Unix seconds) as the score, alongside
a request-token key — use the llm-d-async `producer` Go module rather than
hand-rolling redis-cli. Minimal enqueue program (run with
`kubectl -n llm-d-async port-forward svc/redis 6379:6379` active):

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/llm-d/llm-d-async/api"
	"github.com/llm-d/llm-d-async/producer"
)

// usage: enqueue <queue_name> <count> <deadline_offset_seconds>
func main() {
	queue := os.Args[1]
	n, _ := strconv.Atoi(os.Args[2])
	offset, _ := strconv.Atoi(os.Args[3])

	p, err := producer.NewRedisSortedSetProducer(producer.RedisSortedSetConfig{
		RedisURL:         "redis://localhost:6379",
		RequestQueueName: queue,
		ResultQueueName:  "results-a-list",
	})
	if err != nil {
		panic(err)
	}
	defer p.Close()

	deadline := time.Now().Unix() + int64(offset)
	for i := 0; i < n; i++ {
		err := p.SubmitRequest(context.Background(), &api.RequestMessage{
			ID:       fmt.Sprintf("proto-%s-%d-%d", queue, deadline, i),
			Created:  time.Now().Unix(),
			Deadline: deadline,
			Payload: map[string]any{
				"model":      "Qwen/Qwen3-8B",
				"prompt":     "Write one sentence about autoscaling.",
				"max_tokens": 64,
			},
			Metadata: map[string]string{"team": "team-a"},
		})
		if err != nil {
			panic(err)
		}
	}
	fmt.Printf("enqueued %d to %s, deadline %d\n", n, queue, deadline)
}
```

Check the RedisSortedSetConfig field names and SubmitRequest signature
against the `producer` module version in use. Results land in the queue's configured
result list (`LRANGE results-a-list 0 -1`); a request whose deadline passes
while held produces a `DEADLINE_EXCEEDED` result there.

## 3. Milestone validation

### M1 — cron window

1. Edit `m1-cron-scaledobject.yaml`: set `start` ~10 minutes out, `end` ~25
   minutes out, `timezone` to the cluster's zone. Apply it.
2. Confirm KEDA scales `vllm` to 0 immediately (outside window, backlog
   empty after cooldown).
3. Enqueue requests with deadlines beyond the window start (§2). Verify
   hold-back signatures (§1).
4. At window start: watch `kubectl -n llm-d-async get pods -w`. Record
   **cold-start**: time from ScaledObject activation to first successful
   completion (node provisioning + image pull + model load + gate reopen).
   Repeat the measurement ≥3 times across separate windows — DWS/spot
   provisioning variance is the point. This distribution is M3's `C`.
5. Verify: backlog drains to 0, results delivered, no
   `exceeded_deadline_requests_total` increase, replicas 1→0 at
   window end + cooldown.
6. Negative test: enqueue one request whose deadline expires before the
   window; expect a `DEADLINE_EXCEEDED` result and a counter increment.

### M2 — backlog-driven

1. Delete the M1 ScaledObject; apply `m2-backlog-scaledobject.yaml` and
   `backlog-source-alert-prometheusrule.yaml`.
2. With the pool at 0, enqueue 100 requests (deadline: hours out). Expect
   desired replicas = ceil(100/50) = 2 (or the maxReplicaCount cap).
3. During the drain, measure per-replica throughput `R` =
   completed requests ÷ (wall-clock seconds × replicas). This calibrates M3.
4. Verify the pool does NOT deactivate while
   `llm_d_async_async_inflight_requests > 0`, then scales to 0 after
   drain + cooldown.

### M3 — deadline-proximity

1. Configure the producer's `deadline_scaling.*` constants (R from M2,
   C = p90 from M1, max_replicas, optional safety_factor) in the llm-d-async
   pool/queue config and restart. **Verify the derived metric evaluates**
   (`llm_d_async_async_deadline_required_replicas` returns a value in
   Prometheus) before applying the ScaledObject.
   *Fallback (older llm-d-async):* apply `m3-deadline-prometheusrule.yaml`
   with the constants set as literals, verify `asyncq:deadline_replicas_required:max`
   evaluates (mismatched `ruleSelector` labels fail silently), and point
   trigger 1 of the ScaledObject at that series.
2. Delete the M2 ScaledObject; apply `m3-deadline-scaledobject.yaml`.
3. Two-cohort test with the pool at 0: enqueue cohort A (deadline +30 min)
   and cohort B (deadline +6 h). Expect: pool stays at 0 while
   requirement < 0.85; wakes ≈ `deadline_A − C − |A|/R`; both cohorts
   complete before deadline; pool re-sleeps and re-wakes for cohort B.
4. Report GPU-minutes vs. an always-on baseline for the same job.
