"""Per-stage table of the step 5 agentic run (Poisson stages, no trace).

    agentic.py <results dir> <triggers dir> [--slo ttft=2,tpot=0.05]

Stages are placed from the stage partial reports' run.time windows. Request
times are perf_counter values; they are placed on the same clock from the
earliest request, taken as stage 0's start + 1 s (inference-perf's lead).
TTFT and TPOT use inference-perf's formulas on the raw token times. A counts
failures and completions without latency as misses.
"""
import json
import math
import sys
from datetime import datetime
from pathlib import Path

import yaml


def pct(v, q):
    if not v:
        return None
    v = sorted(v)
    return v[min(len(v) - 1, max(0, int(math.ceil(q * len(v))) - 1))]


def epoch(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def series(path):
    d = json.load(open(path))["data"]["result"]
    return [(float(t), float(v)) for t, v in d[0]["values"]] if d else []


def main():
    res, trig = Path(sys.argv[1]), Path(sys.argv[2])
    slo = {"ttft": 2.0, "tpot": 0.05}
    stages = []
    for p in sorted(res.glob("inference-perf.partial.stage_*.yaml")):
        t = yaml.safe_load(open(p))["run"]["time"]
        stages.append((epoch(t["start"]), epoch(t["end"])))
    recs = sorted(json.load(open(res / "per_request_lifecycle_metrics.json")), key=lambda r: r["start_time"])
    shift = stages[0][0] + 1.0 - recs[0]["start_time"]
    ready = series(trig / "replicas-ready.json")
    spec = series(trig / "replicas-spec.json")
    buckets = [(0, 8192), (8192, 16384), (16384, 24576), (24576, 10**9)]
    out = []
    for k, (lo, hi) in enumerate(stages):
        sel = [r for r in recs if lo <= r["start_time"] + shift < hi]
        good, errs, ttft, tpot, isl = 0, {}, [], [], []
        byb = {b: [] for b in buckets}
        for r in sel:
            e = r.get("error")
            if e:
                name = e.get("error_type", "error")
                if "maximum context length" in (e.get("error_msg") or "") or "context length" in (e.get("error_msg") or ""):
                    name = "context overflow"
                errs[name] = errs.get(name, 0) + 1
                continue
            rm = (r.get("info") or {}).get("response_metrics") or {}
            ct, n = rm.get("chunk_times") or [], rm.get("output_tokens") or 0
            i = ((r.get("info") or {}).get("request_metrics") or {}).get("text", {}).get("input_tokens") or 0
            if len(ct) < 2 or n < 2:
                errs["completed without latency"] = errs.get("completed without latency", 0) + 1
                continue
            a, b = ct[0] - r["start_time"], (ct[-1] - ct[0]) / (n - 1)
            ttft.append(a); tpot.append(b); isl.append(i)
            for bl, bh in buckets:
                if bl <= i < bh:
                    byb[(bl, bh)].append(a)
            if a <= slo["ttft"] and b <= slo["tpot"]:
                good += 1
        rv = [v for t, v in ready if lo <= t < hi]
        def integ(s):
            tot = 0.0
            for (t1, v1), (t2, _) in zip(s, s[1:] + [(s[-1][0] + 15, 0)]):
                a, b = max(t1, lo), min(t2, hi)
                if b > a:
                    tot += v1 * (b - a)
            return tot
        out.append({
            "stage": k, "window_utc": [datetime.utcfromtimestamp(lo).isoformat(), datetime.utcfromtimestamp(hi).isoformat()],
            "seconds": hi - lo, "sent": len(sel), "sent_rps": len(sel) / (hi - lo),
            "mean_isl_completed": sum(isl) / len(isl) if isl else None, "max_isl_completed": max(isl) if isl else None,
            "errors": errs, "A": good / len(sel) if sel else None,
            "ttft": [pct(ttft, q) for q in (.5, .9, .99)], "tpot": [pct(tpot, q) for q in (.5, .9, .99)],
            "ttft_by_isl": {f"{bl}-{bh}": {"n": len(v), "p50": pct(v, .5), "p90": pct(v, .9)} for (bl, bh), v in byb.items()},
            "ready_min_max": [min(rv), max(rv)] if rv else None,
            "requested_replica_s": integ(spec), "ready_replica_s": integ(ready),
        })
    print(json.dumps({"stages": out, "clock_shift_s": shift}, indent=1))


main()
