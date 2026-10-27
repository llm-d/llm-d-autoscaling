# async-batch-autoscaling — prototype manifests and validation runbook

Companion manifests for [the proposal](../async-batch-autoscaling.md).
Validated against a GKE Autopilot cluster running llm-d-async (Redis
sorted-set transport, v0.10.0 for M1 and v0.11.0 for M2), llm-d-router EPP
(`optimized-baseline`), a single `Deployment/vllm`, and kube-prometheus-stack.
Adjust names/namespaces for other environments.

Validated M1 and M2 manifests are included below. M3 manifests and runbook
steps land in a follow-up change once M3 is validated on a cluster.

| File | Purpose | Status |
| --- | --- | --- |
| `worker-pools-readiness-gate.json` | **Validated hold-back config**: fail-closed `prometheus-query` inner gate on `llm_d_epp_ready_endpoints`. Replaces `worker-pools.json` in the llm-d-async ConfigMap. | committed |
| `m1-cron-scaledobject.yaml` | M1: cron-windowed 0↔N scaling (fixed N for the window). | committed |
| `m2-backlog-scaledobject.yaml` | M2: backlog-driven 0↔N scaling (`activationThreshold: "500"`, `threshold: "2000"`, 4-layer `llm-d-async` v0.11.0 PromQL query). | committed |
| `backlog-source-alert-prometheusrule.yaml` | M2/M3 guardrail alert (`AsyncBrokerBacklogSourceUnavailable`) when `llm_d_async_async_broker_backlog_source_available == 0` for >5m. | committed |

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
#   llm_d_async_async_gate_waiting_requests -> equals min(enqueued, workers) (on >= v0.11.0)
#   llm_d_async_async_gate_wait_requeues_total -> climbs every gate-wait-timeout
# and confirm nothing reaches the router (EPP logs quiet).
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

### M2 — backlog-driven 0↔N

1. Remove any M1 ScaledObject (`kubectl -n llm-d-async delete scaledobject vllm-batch-window --ignore-not-found`), then apply the guardrail alert and M2 ScaledObject:
   ```sh
   kubectl apply -f backlog-source-alert-prometheusrule.yaml
   kubectl apply -f m2-backlog-scaledobject.yaml
   ```
2. **Idle scale-to-zero (`1 → 0`)**: With an empty queue (`broker_backlog + queue_depth + gate_waiting_requests + inflight_requests == 0`), confirm KEDA marks `vllm-backlog` inactive (`READY=True`, `ACTIVE=False`) and scales `Deployment/vllm` to `0` replicas once `cooldownPeriod` elapses.
3. **Batch accumulation below `activationThreshold` (`<= 500`)**: Enqueue `200` requests (`< 500`). Verify that the 4-layer PromQL sum reports `200`, the dispatch gate holds all requests back (`gate_waiting_requests` + `broker_backlog` = 200), and `Deployment/vllm` remains at `0` replicas because `200 <= activationThreshold (500)`.
4. **Wake from zero (`0 → 1` at `> 500`)**: Enqueue `400` additional requests (total `600 > 500`). Verify `vllm-backlog` becomes active (`ACTIVE=True`), HPA computes `ceil(600 / 2000) = 1`, and `Deployment/vllm` scales `0 → 1`.
5. **Proportional scale-out (`1 → 2` at `> 2000`)**: Enqueue `2000` additional requests (or `2500` from zero) so total pending work exceeds `2000`. Verify HPA computes `ceil(total / 2000) = 2` and scales `Deployment/vllm` to `2` replicas (`maxReplicaCount`).
6. **Drain-rate calibration (`R`) and scale-to-zero (`2 → 0`)**:
   - Record the time from when the replicas become Ready and the gate opens until all requests drain (`LLEN` on the result list reaches the total enqueued count). Compute per-replica throughput:
     ```
     R = completed_requests / (drain_wall_seconds * ready_replicas)
     ```
   - To size `threshold` (`X`) for a target drain time `T_target` (in seconds), set `threshold = T_target * R`, and ensure `cooldownPeriod >= activationThreshold / R` so the tail below `activationThreshold` finishes draining before KEDA scales `1 → 0`.
   - Confirm `llm_d_async_async_exceeded_deadline_requests_total` does not increase and `Deployment/vllm` scales back `2 → 0` after the queue drains and `cooldownPeriod` / HPA stabilization elapse.

