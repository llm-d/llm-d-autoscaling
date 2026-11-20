"""Scale events of a step 5 run, placed in its phases.

    scale_events.py <phases.json> <triggers dir> [<results dir>]

Lists each change of the requested replicas (replicas-spec.json) with the
phase it falls in; for a scale-up, the time the Ready count reached the new
value and whether that was after the phase ended. With the results dir, also
counts the failed requests whose stream ended within 30 s after each
scale-down (requests cut when a replica was removed).
"""
import json
import sys
from pathlib import Path


def series(path):
    d = json.load(open(path))["data"]["result"]
    return [(float(t), float(v)) for t, v in d[0]["values"]] if d else []


ph = json.load(open(sys.argv[1]))
trig = Path(sys.argv[2])
e0 = ph["epoch0"]
spec = series(trig / "replicas-spec.json")
ready = series(trig / "replicas-ready.json")
edges = [p["phase_s"] for p in ph["phases"]]


def phase_of(t):
    for k, (lo, hi) in enumerate(edges):
        if lo <= t < hi:
            return k + 1
    return None


recs, s0 = None, None
if len(sys.argv) > 3:
    recs = json.load(open(Path(sys.argv[3]) / "per_request_lifecycle_metrics.json"))
    s0 = min(r["start_time"] for r in recs)

events = []
for (t1, v1), (t2, v2) in zip(spec, spec[1:]):
    if v2 == v1:
        continue
    t = t2 - e0
    ev = {"t_s": round(t), "from": v1, "to": v2, "phase": phase_of(t)}
    if v2 > v1:
        hit = next((tr - e0 for tr, vr in ready if tr >= t2 and vr >= v2), None)
        ev["ready_at_s"] = round(hit) if hit is not None else None
        p = ev["phase"]
        if p and hit is not None:
            ev["ready_after_phase_end"] = hit >= edges[p - 1][1]
    elif recs is not None:
        ev["failed_ending_within_30s"] = sum(
            1 for r in recs if r.get("error") and t - 15 <= r["end_time"] - s0 <= t + 30)
    events.append(ev)
print(json.dumps(events, indent=1))
