#!/usr/bin/env python3
"""Generate the npd-fcoff request traces replayed by inference-perf.

The research benchmark (llm-d-autoscaling-research task 0426) drove load with
its own open-loop Poisson client, loadgen.py, from two specs:

  --schedule "0:4.9,600:23.22"            piecewise-constant arrival rate (req/s)
  --mix "0=8192:128:1;600=128:512:1"      time-varying prompt:output:weight mix

inference-perf can replay a request trace (load.type: trace_replay with the
AzurePublicDataset CSV format): every row is one request's send time, prompt
length and output length. This script turns a (schedule, mix, duration, seed)
into such a trace with loadgen.py's own sampling code — the same RNG, the same
call order (one expovariate per arrival, then one random() for the mix draw) —
so a given seed reproduces the research client's arrival times and lengths.

usage:
  gen_traces.py --out-dir DIR [--set kind|gpu|all] [NAME ...]
  gen_traces.py --list
"""
import argparse
import datetime
import json
import random
import sys
from pathlib import Path

# name -> research run it reproduces, schedule, mix, duration (s), seed.
# GPU rows are the task-0426 runs verbatim (raw/<run>/run.json). kind rows use
# the research kind plant's shapes (experiments/epp-input-rate/kind), sized for
# the simulated replica's capacity; see benchmark/config/workloads/npd-fcoff/README.md.
WORKLOADS = {
    # --- GPU plant (Qwen3-8B, 1x H100 per replica) ---------------------------
    "gpu-knee": dict(source="raw/knee", schedule="0:1.0,300:2.0,600:3.0,900:4.0,1200:5.0,1500:6.0",
                     mix="8192:128:1", duration=1800, seed=1),
    "gpu-stair": dict(source="raw/synth-stair-r3", schedule="0:0.8,300:1.6,600:2.4,900:3.2,1200:4.0",
                      mix="8192:128:1", duration=1500, seed=5),
    "gpu-mixshift": dict(source="raw/synth-mix + raw/baseline-mix", schedule="0:4.9,600:23.22",
                         mix="0=8192:128:1;600=128:512:1", duration=1200, seed=4),
    "gpu-overload": dict(source="raw/synth-over", schedule="0:2.9,600:6.0",
                         mix="8192:128:1", duration=1200, seed=6),
    "gpu-overload-hard": dict(source="raw/synth-over2", schedule="0:2.9,600:16.0",
                              mix="8192:128:1", duration=1200, seed=7),
    "gpu-bootstrap": dict(source="kind raw/npd-fcoff-r1 bootstrap phase", schedule="0:1.0",
                          mix="8192:128:1", duration=600, seed=2),
    # --- kind plant (vLLM CPU + SimulatedForCausalLM) ------------------------
    "kind-stair": dict(source="kind sync check, staircase form", schedule="0:1.0,300:2.0,600:3.0",
                       mix="8192:128:1", duration=900, seed=5),
    "kind-mixshift": dict(source="kind raw/ms2", schedule="0:2.0,600:9.4773",
                          mix="0=8192:128:1;600=128:512:1", duration=1200, seed=4),
    "kind-overload": dict(source="raw/synth-over2 shape", schedule="0:2.0,300:12.0",
                          mix="8192:128:1", duration=900, seed=7),
    "kind-bootstrap": dict(source="kind raw/npd-fcoff-r1 bootstrap phase", schedule="0:1.0",
                           mix="8192:128:1", duration=420, seed=2),
}

EPOCH = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)


# --- loadgen.py's sampling, unchanged -------------------------------------
def parse_schedule(spec):
    segs = sorted((float(t), float(r)) for t, r in (p.split(":") for p in spec.split(",")))
    if segs[0][0] != 0:
        segs.insert(0, (0.0, 0.0))
    return segs


def arrivals(segs, duration, rng):
    bounds = [s[0] for s in segs[1:]] + [duration]
    for (t0, rate), t1 in zip(segs, bounds):
        t, t1 = t0, min(t1, duration)
        while rate > 0:
            t += rng.expovariate(rate)
            if t >= t1:
                break
            yield t


def parse_mix1(spec):
    out = []
    for p in spec.split(","):
        i, o, w = p.split(":")
        out.append((int(i), int(o), float(w)))
    return out


def parse_mix(spec):
    if "=" not in spec:
        return [(0.0, parse_mix1(spec))]
    segs = []
    for part in spec.split(";"):
        t, m = part.split("=", 1)
        segs.append((float(t), parse_mix1(m)))
    return sorted(segs)


def mix_at(segs, t):
    cur = segs[0][1]
    for t0, m in segs:
        if t >= t0:
            cur = m
    return cur
# ---------------------------------------------------------------------------


def sample(spec):
    rng = random.Random(spec["seed"])
    mixes = parse_mix(spec["mix"])
    rows = []
    for off in arrivals(parse_schedule(spec["schedule"]), spec["duration"], rng):
        mix = mix_at(mixes, off)
        u = rng.random() * sum(w for *_, w in mix)
        for n_in, n_out, w in mix:
            u -= w
            if u <= 0:
                break
        rows.append((off, n_in, n_out))
    return rows


def write_trace(path, rows):
    with open(path, "w") as f:
        f.write("TIMESTAMP,ContextTokens,GeneratedTokens\n")
        for off, n_in, n_out in rows:
            ts = EPOCH + datetime.timedelta(seconds=off)
            f.write(f"{ts.strftime('%Y-%m-%d %H:%M:%S.%f')},{n_in},{n_out}\n")


def summary(name, spec, rows):
    return {"name": name, **spec, "requests": len(rows),
            "mean_rate": round(len(rows) / spec["duration"], 4),
            "first_s": round(rows[0][0], 3) if rows else None,
            "last_s": round(rows[-1][0], 3) if rows else None}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("names", nargs="*", help="workloads to generate (default: the --set)")
    ap.add_argument("--set", choices=["kind", "gpu", "all"], default="all")
    ap.add_argument("--out-dir", type=Path)
    ap.add_argument("--list", action="store_true", help="print the workload table and exit")
    a = ap.parse_args()

    if a.list:
        for name, spec in WORKLOADS.items():
            print(f"{name:20s} {spec['schedule']:48s} {spec['mix']:28s} {spec['duration']:5d}s  {spec['source']}")
        return
    if not a.out_dir:
        ap.error("--out-dir is required")
    names = a.names or [n for n in WORKLOADS if a.set == "all" or n.startswith(a.set + "-")]
    unknown = [n for n in names if n not in WORKLOADS]
    if unknown:
        ap.error(f"unknown workload(s): {', '.join(unknown)}")

    a.out_dir.mkdir(parents=True, exist_ok=True)
    index = []
    for name in names:
        rows = sample(WORKLOADS[name])
        write_trace(a.out_dir / f"{name}.csv", rows)
        index.append(summary(name, WORKLOADS[name], rows))
        print(f"{name}: {len(rows)} requests -> {a.out_dir / (name + '.csv')}", file=sys.stderr)
    with open(a.out_dir / "index.json", "w") as f:
        json.dump(index, f, indent=1)


if __name__ == "__main__":
    main()
