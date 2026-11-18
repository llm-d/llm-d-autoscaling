#!/usr/bin/env python3
"""Write a SYNTHETIC results directory in the llm-d-benchmark layout (inference-perf, --monitoring).

Use it to exercise the adapter when no real run is at hand. The numbers are made up: arrivals follow a 3-stage schedule, the pool scales on a lagged work-rate trigger.
File names, nesting and field names follow llm-d-benchmark@4c08b9f (collect_metrics.sh L112-L200)
and inference-perf v0.7.0 (reportgen/base.py L867-L912).

    make_mock_llmd_benchmark_run.py OUTDIR
"""
import datetime as dt
import json
import os
import random
import sys

E0 = 1793800000.0           # epoch of stage 0 run.time.start
P_OFF = 8938.0              # perf_counter value at E0 (arbitrary; the harness does not record it)
THRESH = 3500.0             # work tok/s per replica
STAGES = [(0, 300, 2.0), (300, 600, 6.0), (600, 900, 2.0)]    # start, end, req/s
LP, LO, CW = 1024, 128, 3.3
random.seed(7)


def iso(t, fmt="%Y-%m-%dT%H:%M:%S.%fZ"):
    return dt.datetime.fromtimestamp(t, dt.timezone.utc).strftime(fmt)


def main(out):
    os.makedirs(f"{out}/metrics/processed", exist_ok=True)
    reqs, t = [], 1.0
    for lo, hi, rate in STAGES:
        t = max(t, lo + 1.0)
        while t < hi:
            t += random.expovariate(rate)
            if t < hi:
                reqs.append((t, LP, LO))
    # the first request is scheduled ~1 s + first gap after the P0 anchor; keep E0 so the bridge is exact to a gap
    for i, (lo, hi, _) in enumerate(STAGES):
        open(f"{out}/inference-perf.partial.stage_{i}.yaml", "w").write(
            f"version: 0.2.1\nrun:\n  uid: mock-{i}\n  time:\n    start: '{iso(E0 + lo)}'\n"
            f"    end: '{iso(E0 + hi)}'\n    duration: PT{hi - lo}S\n")
    # pool: trigger = true work rate lagged by a 30 s rate window; HPA +1 / 60 s; pod Ready 90 s later
    def work(t):
        return sum(lp + CW * lo for s, lp, lo in reqs if t - 30 < s <= t) / 30
    spec, ready, pend = 1, 1, []
    snaps, trig, desired, auto = [], [], [], []
    for k in range(0, 940, 15):
        tk = k + random.uniform(0, 3)
        w = work(tk)
        want = max(1, min(6, -(-int(w) // int(THRESH))))
        if want > spec and (not pend or tk - pend[-1] >= 60):
            spec += 1
            pend.append(tk)
        if pend and tk - pend[0] >= 90:
            pend.pop(0)
            ready += 1
        if want < spec and k > 700:
            spec = ready = want
        desired.append([E0 + tk, str(spec)])
        auto.append({"timestamp": iso(E0 + tk, "%Y-%m-%dT%H:%M:%SZ"),
                     "hpa": {"keda-hpa-mock": {"name": "keda-hpa-mock", "desiredReplicas": spec,
                                               "currentReplicas": spec, "currentMetrics": [], "conditions": []}},
                     "scaledobjects": {"mock": {"name": "mock", "triggers": [{"name": "work", "type": "prometheus"}]}}})
        snaps.append({"timestamp": iso(E0 + tk, "%Y-%m-%dT%H:%M:%SZ"), "namespace": "mock",
                      "controllers": [{"name": "mock-decode", "kind": "Deployment", "model": "m",
                                       "role": "decode", "desired_replicas": spec,
                                       "ready_replicas": ready, "available_replicas": ready,
                                       "updated_replicas": ready}]})
    for k in range(0, 940, 15):
        trig.append([E0 + k, f"{work(k):.1f}"])
    # per-request records: perf_counter times, no stage_id / scheduled_time (inference-perf v0.7.0).
    # Latency follows the pool: TTFT 0.3 s while Ready keeps up with the load, 0.3 + 1.2 s per missing
    # replica when it does not; while short, 30 % of requests time out (error).
    rdy = [(dt.datetime.strptime(x["timestamp"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc).timestamp() - E0,
            x["controllers"][0]["ready_replicas"]) for x in snaps]
    ent = []
    for t, lp, lo in reqs:
        ready_t = [r for u, r in rdy if u <= t][-1] if rdy[0][0] <= t else rdy[0][1]
        short = max(0, -(-int(work(t)) // int(THRESH)) - ready_t)
        ttft, tpot = 0.3 + 1.2 * short, 0.03
        e2e = ttft + tpot * (lo - 1)
        err = {"type": "timeout", "message": "request timed out"} if short and random.random() < 0.3 else None
        ent.append({"start_time": P_OFF + t, "end_time": P_OFF + t + (120.0 if err else e2e),
                    "info": {"request_metrics": {"text": {"input_tokens": lp}},
                             "response_metrics": {"output_tokens": 0 if err else lo}},
                    "computed_metrics": None if err else {"time_to_first_token": ttft, "time_per_output_token": tpot,
                                                           "request_latency": e2e},
                    "error": err})
    json.dump(ent, open(f"{out}/per_request_lifecycle_metrics.json", "w"))
    json.dump({"snapshots": snaps}, open(f"{out}/metrics/processed/replica_status_timeseries.json", "w"))
    json.dump({"status": "success", "data": {"resultType": "matrix", "result": [
        {"metric": {}, "values": trig}]}}, open(f"{out}/trigger.json", "w"))
    kv = [[E0 + k, f"{min(0.99, 0.2 + work(k) / (4 * THRESH)):.3f}"] for k in range(0, 940, 15)]
    json.dump({"status": "success", "data": {"resultType": "matrix", "result": [
        {"metric": {}, "values": kv}]}}, open(f"{out}/kv.json", "w"))
    json.dump({"status": "success", "data": {"resultType": "matrix", "result": [
        {"metric": {}, "values": desired}]}}, open(f"{out}/hpa_desired.json", "w"))
    # what llm-d-benchmark's collect_metrics.sh writes once it records HPA state (kept outside the
    # results dir, so a test can add it)
    os.makedirs(f"{out}.extra", exist_ok=True)
    json.dump({"snapshots": auto}, open(f"{out}.extra/autoscaler_timeseries.json", "w"))
    # several ScaledObjects in one file: two named triggers of different metricType, and one
    # combined with scalingModifiers
    open(f"{out}/scaledobjects-multi.yaml", "w").write(f"""apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: mock
spec:
  scaleTargetRef:
    name: mock-decode
  triggers:
  - type: prometheus
    name: work
    metadata:
      query: sum(rate(mock_work_total[30s]))
      threshold: "{int(THRESH)}"
    authenticationRef:
      name: prometheus-auth
  - type: prometheus
    name: kv
    metricType: Value
    metadata:
      query: max(vllm:kv_cache_usage_perc)
      threshold: '0.8'
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: mock-composite
spec:
  scaleTargetRef:
    name: mock-other
  advanced:
    scalingModifiers:
      formula: "work / 2 + kv * 1000"
      target: "{int(THRESH)}"
  triggers:
    - type: prometheus
      name: work
      metadata:
        threshold: "{int(THRESH)}"
    - type: prometheus
      name: kv
      metadata:
        threshold: "1"
""")
    open(f"{out}/scaledobject.yaml", "w").write(
        "apiVersion: keda.sh/v1alpha1\nkind: ScaledObject\nmetadata:\n  name: mock\nspec:\n  triggers:\n"
        f"  - type: prometheus\n    metricType: AverageValue\n    metadata:\n      threshold: \"{int(THRESH)}\"\n"
        "      query: sum(rate(mock_work_total[30s]))\n")


if __name__ == "__main__":
    main(sys.argv[1])
