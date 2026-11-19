"""Per-stage A, TTFT and TPOT of a step 5 knee run, and mu_SLO at A = X.

    knee.py <results dir> <knee json from gen_load.py> [--x 0.95]

A request belongs to the stage its scheduled offset falls in; offsets are
measured from the earliest request start. A counts a request as good when it
completed without error with TTFT <= ttft and TPOT <= tpot.
"""
import json
import math
import sys


def pct(v, q):
    if not v:
        return float("nan")
    v = sorted(v)
    return v[min(len(v) - 1, int(math.ceil(q * len(v))) - 1)]


def main():
    res, kj = sys.argv[1], sys.argv[2]
    x = float(sys.argv[sys.argv.index("--x") + 1]) if "--x" in sys.argv else 0.95
    spec = next(iter(json.load(open(kj)).values()))
    phases = spec["phases"]
    slo = spec.get("slo") or {"ttft": 2.0, "tpot": 0.05}
    recs = json.load(open(f"{res}/per_request_lifecycle_metrics.json"))
    t0 = min(r["start_time"] for r in recs)
    edges, t = [], 0.0
    for p in phases:
        edges.append((t, t + p["seconds"], p["rate"]))
        t += p["seconds"]
    rows = []
    for lo, hi, rate in edges:
        sel = [r for r in recs if lo <= r["start_time"] - t0 < hi]
        good, ttft, tpot, errs = 0, [], [], {}
        for r in sel:
            if r.get("error"):
                e = r["error"]
                k = e.get("error_type", str(e)[:40]) if isinstance(e, dict) else str(e)[:40]
                errs[k] = errs.get(k, 0) + 1
                continue
            m = (r.get("info") or {}).get("computed_metrics") or r.get("computed_metrics") or {}
            a = m.get("ttft") if m.get("ttft") is not None else m.get("time_to_first_token")
            b = m.get("tpot") if m.get("tpot") is not None else m.get("time_per_output_token")
            if a is None or b is None:
                errs["no_metrics"] = errs.get("no_metrics", 0) + 1
                continue
            ttft.append(a)
            tpot.append(b)
            if a <= slo["ttft"] and b <= slo["tpot"]:
                good += 1
        n = len(sel)
        rows.append({
            "stage_s": [lo, hi], "rate": rate, "sent": n, "achieved_rps": n / (hi - lo),
            "A": good / n if n else float("nan"), "errors": errs,
            "ttft_p50": pct(ttft, .5), "ttft_p90": pct(ttft, .9), "ttft_p99": pct(ttft, .99),
            "tpot_p50": pct(tpot, .5), "tpot_p90": pct(tpot, .9), "tpot_p99": pct(tpot, .99),
        })
    mu, idx = None, None
    for i in range(1, len(rows)):
        a0, a1 = rows[i - 1]["A"], rows[i]["A"]
        if a0 >= x > a1:
            q0, q1 = rows[i - 1]["rate"], rows[i]["rate"]
            mu = q0 + (a0 - x) * (q1 - q0) / (a0 - a1)
            idx = i + 1  # crossing ends at stage idx (1-based)
            break
    print(json.dumps({"rows": rows, "x": x, "mu_slo": mu, "crossing_into_stage": idx,
                      "first_stage_A": rows[0]["A"]}, indent=1))


main()
