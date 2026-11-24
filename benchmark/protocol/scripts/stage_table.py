#!/usr/bin/env python3
"""The per-stage table of one run of the evaluation run set (steps 3 and 5).

    stage_table.py <results dir> --index INDEX.json --run NAME --slo ttft=S,tpot=S \\
        [--trace RUN.csv] [--deployment NAME] [--knee X] [--gpus-per-replica G] [--json OUT]

One row per stage or phase of the run's schedule (gen_load.py's index.json, or
knee-<mix>.json): offered req/s, λ_in and λ_out, requests sent, errors by type,
attainment A, TTFT and TPOT p50/p90/p99, Ready replicas (min-max), requested and
Ready replica-seconds, and, for runs 2 to 10, the replicas the stage needs,
ceil(q / μ_SLO). The rules are benchmark/protocol/run-set.md §1 and §3.

It reads the run through the load-vs-supply adapter, so the table and the
figure place every request and replica on the same clock. That clock puts the
first request at t = 1 s; the schedule is aligned to it by the first request's
scheduled offset, read from the run's trace (--trace, step 5) or from
blis-run.json (step 3).

--knee X adds μ_SLO: the rate where A crosses X, interpolated linearly between
the two stages around it, and says when the crossing is outside the stages:
stage 1 fails or the last stage passes (run-set.md §2, run 1).

For a pool that serves only part of the load (run-set.md §1: prefill behind a decider
that keeps short prompts on decode), --min-input-tokens N counts only the requests with
at least N input tokens in A, errors, TTFT and TPOT; offered req/s, λ and sent stay
the whole load's, so μ_SLO stays in requests per second of the load. --mu MIX=X and
--mu-blend X replace the index's μ_SLO in the need column with the pool's own, and a
phase whose mixes all fall under N needs minReplicaCount. Standard library only.
"""

import argparse
import collections
import datetime as dt
import glob
import json
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "..", "..", ".claude", "skills", "load-vs-supply", "scripts"))
from lvs_adapters import LlmdBenchmark  # noqa: E402

EPOCH = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc)  # gen_load.py's trace epoch


def slo(spec):
    out = {}
    for part in spec.split(","):
        k, v = part.split("=")
        out[k] = float(v)
    return out


def first_offset(root, trace):
    if trace:
        with open(trace) as f:
            next(f)
            ts = dt.datetime.strptime(next(f).split(",")[0], "%Y-%m-%d %H:%M:%S.%f")
        return (ts.replace(tzinfo=dt.timezone.utc) - EPOCH).total_seconds(), f"first row of {trace}"
    meta = glob.glob(os.path.join(root, "**", "blis-run.json"), recursive=True)
    if meta:
        return json.load(open(meta[0]))["first_arrival_s"], "blis-run.json"
    sys.exit("give --trace <run>.csv (step 5) to align the schedule with the records")


def pct(xs, q):
    if not xs:
        return None
    xs = sorted(xs)
    return xs[min(len(xs) - 1, max(0, math.ceil(q * len(xs)) - 1))]


def at(series, t):
    v = None
    for u, n in series:
        if u > t:
            break
        v = n
    return v


def integral(series, a, b):
    """Replica-seconds of a step series over [a, b)."""
    pts = [(a, at(series, a) or 0)] + [(u, n) for u, n in series if a < u < b] + [(b, None)]
    return sum((pts[i + 1][0] - pts[i][0]) * pts[i][1] for i in range(len(pts) - 1))


def window_range(series, a, b):
    vals = [n for u, n in series if a <= u < b]
    v0 = at(series, a)
    if v0 is not None:
        vals.append(v0)
    return (min(vals), max(vals)) if vals else (None, None)


def pool_requests(reqs, min_in):
    """The requests the pool serves: those with at least min_in input tokens, or all."""
    return reqs if min_in is None else [r for r in reqs if r["in"] >= min_in]


def serves(mix, min_in):
    """Whether a phase's mix (one or a comma list) sends the pool any request."""
    return min_in is None or any(int(m.split(":")[0]) >= min_in for m in mix.split(","))


def need(rate, mix, mu, mu_blend, min_in, min_replicas):
    """Replicas a phase needs, ceil(q / μ_SLO); minReplicaCount when the pool serves none of it."""
    if not (mu and mu_blend and mix):
        return None
    if not serves(mix, min_in):
        return min_replicas
    m = mu.get(mix) if "," not in mix else mu_blend
    return math.ceil(rate / m) if m else None


def capacities(inputs, mu_flags, mu_blend_flag):
    """The index's μ_SLO per mix and for the blend, with --mu MIX=X and --mu-blend X applied."""
    mu = dict(inputs.get("mu") or {}) or None
    for spec in mu_flags or []:
        k, v = spec.rsplit("=", 1)
        mu = {**(mu or {}), k: float(v)}
    return mu, (mu_blend_flag if mu_blend_flag is not None else inputs.get("mu_blend"))


def fmt(x, nd=2):
    if x is None:
        return "–"
    if isinstance(x, int):
        return f"{x:,}"
    return f"{x:,.{nd}f}"


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("results")
    ap.add_argument("--index", required=True)
    ap.add_argument("--run", required=True)
    ap.add_argument("--slo", required=True, type=slo, help="ttft=S,tpot=S")
    ap.add_argument("--trace", help="the run's trace (step 5)")
    ap.add_argument("--deployment")
    ap.add_argument("--knee", type=float, metavar="X", help="attainment objective: report μ_SLO")
    ap.add_argument("--gpus-per-replica", type=float, default=1)
    ap.add_argument("--json")
    ap.add_argument("--min-input-tokens", type=int, metavar="N",
                    help="count only requests with at least N input tokens (the pool's) in A, errors, TTFT, TPOT")
    ap.add_argument("--mu", action="append", metavar="MIX=X", help="the pool's μ_SLO for MIX (repeat)")
    ap.add_argument("--mu-blend", type=float, metavar="X", help="the pool's μ_SLO for the blend")
    a = ap.parse_args()

    index = json.load(open(a.index))
    entry = index.get(a.run) or sys.exit(f"no run {a.run} in {a.index}")
    inputs = index.get("inputs") or {}
    mu, mu_blend = capacities(inputs, a.mu, a.mu_blend)
    min_replicas = inputs.get("min_replicas", 1)
    run = LlmdBenchmark(a.results, deployment=a.deployment).build()
    o_first, src = first_offset(a.results, a.trace)
    shift = 1.0 - o_first          # schedule offset b is t = b + shift on the run's clock

    rows, b = [], 0.0
    for k, ph in enumerate(entry["phases"]):
        lo, hi = b + shift, b + ph["seconds"] + shift
        b += ph["seconds"]
        dur = ph["seconds"]
        reqs = [r for r in run.requests if lo <= r["t"] < hi]
        pool = pool_requests(reqs, a.min_input_tokens)
        ok = [r for r in pool if r["ok"]]
        good = [r for r in ok if (r["ttft"] is not None and r["ttft"] <= a.slo.get("ttft", math.inf))
                and (r["tpot"] is None or r["tpot"] <= a.slo.get("tpot", math.inf))]
        errs = collections.Counter(r["err"] or "error" for r in pool if not r["ok"])
        ttft = [r["ttft"] for r in ok if r["ttft"] is not None]
        tpot = [r["tpot"] for r in ok if r["tpot"] is not None]
        rmin, rmax = window_range(run.ready, lo, hi)
        mix = ph.get("mix", "")
        rows.append({
            "stage": k + 1, "seconds": dur, "rate": ph["rate"], "mix": mix,
            "offered_req_s": len(reqs) / dur,
            "lambda_in": sum(r["in"] for r in reqs) / dur,
            "lambda_out": sum(r["out"] for r in reqs) / dur,
            "sent": len(reqs), "served": len(pool), "errors": dict(errs),
            "A": len(good) / len(pool) if pool else None,
            "ttft": [pct(ttft, q) for q in (0.5, 0.9, 0.99)],
            "tpot": [pct(tpot, q) for q in (0.5, 0.9, 0.99)],
            "ready": [rmin, rmax], "need": need(ph["rate"], mix, mu, mu_blend, a.min_input_tokens, min_replicas),
            "requested_replica_s": integral(run.spec, lo, hi) if run.spec else None,
            "ready_replica_s": integral(run.ready, lo, hi)})

    t_lo, t_hi = shift, b + shift
    before = sum(1 for r in run.requests if r["t"] < t_lo)
    after = sum(1 for r in run.requests if r["t"] >= t_hi)
    # the agentic run's phases carry no mix: their rounds follow a session past the last phase
    sessions = not any(ph.get("mix") for ph in entry["phases"])
    outside = before + (0 if sessions else after)
    off = [r["stage"] for r in rows
           if abs(r["sent"] - r["rate"] * r["seconds"]) > 3 * math.sqrt(r["rate"] * r["seconds"])]
    placement = {"before_first_stage": before, "after_last_stage": after, "stages_off_rate": off}
    print(f"{a.run}: {len(run.requests)} requests in the records; schedule aligned by {src} "
          f"(first request at offset {o_first:.3f} s)")
    head = ("| stage | s | set req/s | offered req/s | λ_in tok/s | λ_out tok/s | sent | errors | A "
            "| TTFT p50/p90/p99 s | TPOT p50/p90/p99 ms | Ready | need | requested replica-s "
            "| Ready replica-s |")
    if a.min_input_tokens is not None:
        print(f"A, errors, TTFT and TPOT count the {sum(r['served'] for r in rows):,} requests with at least "
              f"{a.min_input_tokens:,} input tokens (the pool's); offered req/s, λ and sent count every request")
    print(head)
    print("|" + "---|" * (head.count("|") - 1))
    for r in rows:
        errs = ", ".join(f"{n} {k}" for k, n in sorted(r["errors"].items())) or "0"
        print(f"| {r['stage']} | {r['seconds']} | {fmt(r['rate'], 3)} | {fmt(r['offered_req_s'], 3)} "
              f"| {fmt(r['lambda_in'], 0)} | {fmt(r['lambda_out'], 0)} | {r['sent']:,} | {errs} "
              f"| {fmt(r['A'] * 100 if r['A'] is not None else None, 1)} % "
              f"| {'/'.join(fmt(x) for x in r['ttft'])} "
              f"| {'/'.join(fmt(x * 1000 if x is not None else None, 1) for x in r['tpot'])} "
              f"| {r['ready'][0]}–{r['ready'][1]} | {fmt(r['need'])} "
              f"| {fmt(r['requested_replica_s'], 0)} | {fmt(r['ready_replica_s'], 0)} |")
    tot_req = sum(r["requested_replica_s"] or 0 for r in rows)
    sent = sum(r["sent"] for r in rows)
    served = sum(r["served"] for r in rows)
    good = sum((r["A"] or 0) * r["served"] for r in rows)
    errs = collections.Counter()
    for r in rows:
        errs.update(r["errors"])
    summary = {"sent": sent, "served": served, "A": good / served if served else None, "errors": dict(errs),
               "requested_replica_s": tot_req, "gpu_s": tot_req * a.gpus_per_replica,
               "placement": placement}
    print(f"\nrun: {sent:,} sent, A {summary['A'] * 100:.1f} %, errors {dict(errs) or 0}, "
          f"requested {tot_req:,.0f} replica-s = {summary['gpu_s']:,.0f} GPU-s "
          f"at {a.gpus_per_replica:g} GPU per replica")

    print(f"placement: {before} request(s) before the first stage, {after} after the last"
          + (" (session rounds)" if sessions else "") + f"; stages more than 3 sd off their set "
          f"rate: {off or 'none'}: {'FAIL' if outside or off else 'ok'}")

    if a.knee is not None:
        x = a.knee
        knee = None
        for i in range(1, len(rows)):
            p, q = rows[i - 1], rows[i]
            if p["A"] is not None and q["A"] is not None and p["A"] >= x > q["A"]:
                f = (p["A"] - x) / (p["A"] - q["A"])
                knee = {"mu_slo": p["offered_req_s"] + f * (q["offered_req_s"] - p["offered_req_s"]),
                        "between_stages": [p["stage"], q["stage"]]}
                break
        if knee is None:
            print(f"knee: A does not cross {x:g} between two stages; move the estimate and rerun")
        else:
            inside = rows[0]["A"] >= x and rows[-1]["A"] < x
            knee["inside"] = inside
            print(f"knee: μ_SLO = {knee['mu_slo']:.3f} req/s (A crosses {x:g} between stages "
                  f"{knee['between_stages'][0]} and {knee['between_stages'][1]}"
                  + ("" if inside else "; stage 1 fails or the last stage passes: the crossing is "
                     "outside the stages, move the estimate and rerun") + ")")
        summary["knee"] = knee
    if a.json:
        with open(a.json, "w") as f:
            json.dump({"run": a.run, "aligned_by": src, "rows": rows, "summary": summary}, f, indent=1)
            f.write("\n")


if __name__ == "__main__":
    main()
