"""Per-phase table of a step 5 run from its results, trace, schedule and series.

    phases.py <results dir> <trace.csv> <schedule.json> <triggers dir>
              [--mu-blend Q] [--mu ISL:OSL=Q,...] [--x 0.95] [--gpus 1]

Requests are sent in trace order, so the per-request records sorted by start
map one to one onto the trace rows: each request gets its scheduled offset,
ISL and OSL from the trace. The phase of a request is the one its scheduled
offset falls in. TTFT and TPOT come from inference-perf's computed_metrics or,
when the profile leaves them out, from the same formulas on the raw token times. A counts a request as good when it completed without error
and met TTFT and TPOT; failures and completions without latency are misses.

Prometheus series (replicas-ready.json, replicas-spec.json) are placed on the
same clock from the stage-0 partial report's run.time.start (+1 s, the
inference-perf lead) plus the first row's offset.

Prints JSON: per-phase rows, the schedule delay (sent offset minus scheduled
offset), and the whole run's totals.
"""
import csv
import json
import math
import sys
from datetime import datetime
from pathlib import Path


def pct(v, q):
    if not v:
        return None
    v = sorted(v)
    return v[min(len(v) - 1, max(0, int(math.ceil(q * len(v))) - 1))]


def arg(name, default=None):
    return sys.argv[sys.argv.index(name) + 1] if name in sys.argv else default


def series(path):
    d = json.load(open(path))["data"]["result"]
    return [(float(t), float(v)) for t, v in d[0]["values"]] if d else []


def main():
    res, trace, sched, trig = (Path(p) for p in sys.argv[1:5])
    x = float(arg("--x", 0.95))
    gpus = float(arg("--gpus", 1))
    mu_blend = float(arg("--mu-blend")) if arg("--mu-blend") else None
    mu = {k: float(v) for k, v in (p.split("=") for p in arg("--mu", "").split(",") if p)}
    spec = json.load(open(sched))
    spec = next(iter(spec.values())) if "phases" not in spec else spec
    phases = spec["phases"]
    slo = {"ttft": 2.0, "tpot": 0.05}
    for p in (arg("--slo") or "").split(","):
        if p:
            k, v = p.split("=")
            slo[k] = float(v)

    rows = []
    with open(trace) as f:
        for r in csv.DictReader(f):
            rows.append((datetime.fromisoformat(r["TIMESTAMP"]).timestamp(),
                         int(r["ContextTokens"]), int(r["GeneratedTokens"])))
    o0 = rows[0][0]
    rows = [(t - o0, i, o) for t, i, o in rows]
    recs = sorted(json.load(open(res / "per_request_lifecycle_metrics.json")),
                  key=lambda r: r["start_time"])
    if len(recs) != len(rows):
        print(f"warning: {len(recs)} records against {len(rows)} trace rows", file=sys.stderr)
    s0 = recs[0]["start_time"]

    partial = next(res.glob("inference-perf.partial.stage_0.yaml"))
    import yaml
    run_start = datetime.fromisoformat(
        yaml.safe_load(open(partial))["run"]["time"]["start"].replace("Z", "+00:00")).timestamp()
    epoch0 = run_start + 1.0  # first-row offset is 0 by construction (o0 subtracted)
    ready = series(trig / "replicas-ready.json")
    spec_r = series(trig / "replicas-spec.json")

    edges, t = [], 0.0
    for p in phases:
        edges.append((t, t + p["seconds"], p))
        t += p["seconds"]

    def phase_of(off):
        for k, (lo, hi, _) in enumerate(edges):
            if lo <= off < hi:
                return k
        return len(edges) - 1

    out = [dict(sent=0, good=0, errors={}, ttft=[], tpot=[], lin=0, lout=0) for _ in edges]
    delays = []
    for rec, (off, isl, osl) in zip(recs, rows):
        k = phase_of(off)
        o = out[k]
        o["sent"] += 1
        o["lin"] += isl
        o["lout"] += osl
        delays.append((off, (rec["start_time"] - s0) - off))
        e = rec.get("error")
        if e:
            name = e.get("error_type", "error") if isinstance(e, dict) else str(e)[:40]
            o["errors"][name] = o["errors"].get(name, 0) + 1
            continue
        m = rec.get("computed_metrics") or {}
        a, b = m.get("time_to_first_token"), m.get("time_per_output_token")
        if not m:
            # Profiles without computed_metrics: inference-perf's own formulas on the
            # raw token times (TTFT = first - start, TPOT = (last - first) / (n - 1)).
            rm = (rec.get("info") or {}).get("response_metrics") or {}
            ct, n = rm.get("chunk_times") or [], rm.get("output_tokens") or 0
            if len(ct) > 1 and n > 1:
                a, b = ct[0] - rec["start_time"], (ct[-1] - ct[0]) / (n - 1)
        if a is None or b is None:
            o["errors"]["completed without latency"] = o["errors"].get("completed without latency", 0) + 1
            continue
        o["ttft"].append(a)
        o["tpot"].append(b)
        if a <= slo["ttft"] and b <= slo["tpot"]:
            o["good"] += 1

    def integ(s, lo, hi):
        tot, vals = 0.0, []
        for (t1, v1), (t2, _) in zip(s, s[1:] + [(s[-1][0] + 15, 0)]):
            a, b = max(t1, lo), min(t2, hi)
            if b > a:
                tot += v1 * (b - a)
                vals.append(v1)
        return tot, vals

    table = []
    for (lo, hi, p), o in zip(edges, out):
        dur = hi - lo
        rs, rv = integ(ready, epoch0 + lo, epoch0 + hi)
        ss, sv = integ(spec_r, epoch0 + lo, epoch0 + hi)
        mixes = p.get("mix", "")
        need = None
        q = p["rate"]
        if "," in mixes and mu_blend:
            need = math.ceil(q / mu_blend)
        elif mixes in mu:
            need = math.ceil(q / mu[mixes])
        d = [dl for off, dl in delays if lo <= off < hi]
        table.append({
            "phase_s": [lo, hi], "mix": mixes, "scheduled_rps": q, "sent": o["sent"],
            "sent_rps": o["sent"] / dur, "lambda_in": o["lin"] / dur, "lambda_out": o["lout"] / dur,
            "errors": o["errors"], "A": o["good"] / o["sent"] if o["sent"] else None,
            "ttft": [pct(o["ttft"], q_) for q_ in (.5, .9, .99)],
            "tpot": [pct(o["tpot"], q_) for q_ in (.5, .9, .99)],
            "ready_min_max": [min(rv), max(rv)] if rv else None,
            "requested_min_max": [min(sv), max(sv)] if sv else None,
            "requested_replica_s": ss, "ready_replica_s": rs, "gpu_s": ss * gpus,
            "need": need,
            "schedule_delay_s": {"p50": pct(d, .5), "max": max(d) if d else None,
                                 "last": d[-1] if d else None},
        })
    sent = sum(o["sent"] for o in out)
    good = sum(o["good"] for o in out)
    errs = {}
    for o in out:
        for k, v in o["errors"].items():
            errs[k] = errs.get(k, 0) + v
    print(json.dumps({
        "phases": table, "epoch0": epoch0, "slo": slo, "x": x,
        "total": {"sent": sent, "A": good / sent if sent else None, "errors": errs,
                  "requested_replica_s": sum(r["requested_replica_s"] for r in table),
                  "ready_replica_s": sum(r["ready_replica_s"] for r in table),
                  "gpu_s": sum(r["gpu_s"] for r in table)},
    }, indent=1))


main()
