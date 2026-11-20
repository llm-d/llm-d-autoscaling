#!/usr/bin/env python3
"""Run one step 3 run in blis-signal-lab and leave it in llm-d-benchmark's layout.

    blis_run.py --lab DIR --scenario FILE --load DIR --run NAME --policy policy.json \
        (--pin N|ROLE=N,... | --active) [--static ROLE=N] --out RUNDIR [--scenarios DIR] [--seed N]

NAME is a load gen_load.py --target blis wrote: its workload is DIR/NAME.yaml
and its schedule is the entry NAME of DIR/index.json (or DIR/NAME.json for a
knee). The horizon is the load plus its drain. The ScaledObjects come from the
scaledobjects.json so_to_policy.py wrote next to the policy: one HPA for a
single pool, one per role (prefill, decode) for P/D.

Arms (benchmark/protocol/3-blis-evaluation.md §3.3), per pool:
  --pin N          min = max = N: the HPA still reads and logs every trigger, but
  --pin ROLE=N,..  cannot move the pool (KEDA's paused-replicas holds it the same
                   way); a P/D role not named runs active
  --active         the policy as written, from max(1, minReplicaCount) pods
  --static ROLE=N  a P/D role no ScaledObject scales, held at N
blis starts no pool at 0 pods. When an active pool's minReplicaCount is 0, the
load is shifted by a quiet lead-in (polling interval + initial cooldown + 30 s)
so KEDA scales it to zero first; every series is placed on the load's clock,
and blis-run.json records the Ready pods at the load's start.

RUNDIR gets:
  sim/          blis's own output: result.json (the Result, with request_rows),
                signals.csv, signals-pods.csv, signals-hpa[-<role>].csv,
                signals-summary.json, stderr.txt, command.txt, and the policy and
                workload as run
  results/      the same run as an llm-d-benchmark results directory:
                per_request_lifecycle_metrics.json, inference-perf.partial.stage_N.yaml
                (one per phase), metrics/processed/replica_status_timeseries.json (one
                controller per pool, named after its ScaledObject's target) and
                autoscaler_timeseries.json (one HPA per ScaledObject), and blis-run.json
                (the clock, the arm and the inputs)
  triggers/     one Prometheus API body per trigger, from the HPA log (what the HPA
                read at each sync; composite.json with scalingModifiers), the format
                load-vs-supply takes with --trigger; for P/D, one directory per role

Load time t s is placed at epoch EPOCH + t. Standard library only.
"""

import argparse
import csv
import datetime as dt
import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

EPOCH = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc).timestamp()
PERF0 = 1000.0           # perf_counter of load time 0 in the per-request records
READY = ("Active", "WarmingUp")


def iso(t):
    return dt.datetime.fromtimestamp(EPOCH + t, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def counts(spec):
    """'3' -> {None: 3}; 'prefill=1,decode=4' -> {'prefill': 1, 'decode': 4}."""
    if spec is None:
        return {}
    if "=" not in spec:
        return {None: int(spec)}
    return {k: int(v) for k, v in (p.split("=") for p in spec.split(","))}


def schedule(load, run):
    idx = load / "index.json"
    entry = json.load(open(idx)).get(run) if idx.exists() else None
    if entry is None and (load / f"{run}.json").exists():
        entry = json.load(open(load / f"{run}.json")).get(run)
    if entry is None or "phases" not in entry:
        sys.exit(f"no schedule for {run} in {idx} or {load / (run + '.json')}")
    return entry


def plan(a, policy, sos):
    """-> {role: start pods}, lead-in s, and the policy with the arm applied."""
    auto = policy["autoscaler"]
    blocks = {None: auto["hpa"]} if "hpa" in auto else dict(auto["hpa_by_role"])
    pins, static = counts(a.pin), counts(a.static)
    if set(pins) - set(blocks) or set(static) & set(blocks):
        sys.exit(f"--pin names pools with a ScaledObject ({sorted(map(str, blocks))}); "
                 "--static names the others")
    start, lead = dict(static), 0.0
    for role, hpa in blocks.items():
        if role in pins:
            hpa["min_replicas"] = hpa["max_replicas"] = pins[role]
            start[role] = pins[role]
        else:
            start[role] = max(1, hpa.get("min_replicas", 0))
            if hpa.get("min_replicas", 0) == 0:
                lead = max(lead, hpa.get("polling_interval_s", 30)
                           + hpa.get("initial_cooldown_period_s", 0) + 30)
    if None not in blocks and set(start) != {"prefill", "decode"}:
        sys.exit("a P/D run needs a start count for both roles: --static ROLE=N for the one "
                 "no ScaledObject scales")
    return start, 15 * -(-lead // 15)


def shifted(text, lead):
    """The workload with every lifecycle window moved lead s later."""
    return re.sub(r"(start_us|end_us): (\d+)",
                  lambda m: f"{m.group(1)}: {int(m.group(2)) + int(lead * 1e6)}", text)


def simulate(a, entry, rundir, sos):
    sim = rundir / "sim"
    sim.mkdir(parents=True, exist_ok=True)
    policy = json.load(open(a.policy))
    start, lead = plan(a, policy, sos)
    (sim / "policy.json").write_text(json.dumps(policy, indent=2) + "\n")
    workload = Path(a.load) / f"{a.run}.yaml"
    (sim / "workload.yaml").write_text(shifted(workload.read_text(), lead))
    horizon = lead + entry["duration_s"] + entry["drain_s"]
    blis = Path(a.lab) / "blis"
    pods = ["--num-instances", str(sum(start.values()))]
    if None not in start:
        pods += ["--prefill-instances", str(start["prefill"]), "--decode-instances", str(start["decode"])]
    # run in sim/, every file by its name there, so command.txt reruns it as it ran
    cmd = [str(blis), "run", "--scenario", a.scenario, "--scenarios", a.scenarios,
           "--workload-spec", "workload.yaml", "--policy-config", "policy.json", *pods,
           "--horizon", str(int(horizon * 1e6)),
           "--timeout", str(entry["timeout_s"]), "--signals-output", "signals",
           "--output", "result.json"]
    if a.seed is not None:
        cmd += ["--seed", str(a.seed)]
    shown = " ".join(cmd).replace(a.lab, "<lab>")
    (sim / "command.txt").write_text("# run in the run's sim/ directory; <lab> is the "
                                     f"blis-signal-lab checkout\n{shown}\n")
    with open(sim / "stderr.txt", "w") as err:
        rc = subprocess.run(cmd, cwd=sim, stdout=subprocess.DEVNULL, stderr=err).returncode
    if rc:
        sys.exit(f"blis exited {rc}; see {sim / 'stderr.txt'}")
    return start, lead, horizon


def error_of(row, timeout_s):
    """inference-perf's error for a blis request. blis enforces no client timeout on a
    single-shot client with lifecycle windows (3-blis-evaluation.md §3.2), so a request that
    ran past it is counted as the timeout the client would have raised."""
    fate = row["fate"]
    e2e = row.get("e2e_us")
    if fate == "completed" and e2e / 1e6 <= timeout_s:
        return None
    if fate in ("completed", "timed_out", "in_flight_at_horizon", "queued_at_horizon"):
        return {"type": "timeout", "message": f"{fate}: past the {timeout_s} s client timeout"}
    if fate == "dropped":
        return {"type": row.get("drop_reason", "dropped"), "message": "dropped"}
    return {"type": fate, "message": f"{fate} when the run ended"}


def read_csv(path):
    with open(path) as f:
        return list(csv.DictReader(f))


def prom(name, points):
    return {"status": "success", "data": {"resultType": "matrix", "result": [{
        "metric": {"__name__": name, "source": "blis-signal-lab HPA log (value at each sync)"},
        "values": [[EPOCH + t, v] for t, v in points]}]}}


def convert(a, entry, rundir, sos, start, lead, horizon):
    sim, res, trig = rundir / "sim", rundir / "results", rundir / "triggers"
    (res / "metrics" / "processed").mkdir(parents=True, exist_ok=True)
    trig.mkdir(exist_ok=True)
    result = json.load(open(sim / "result.json"))
    rows = result["request_rows"]
    if not rows:
        sys.exit("the run generated no request")

    def t_of(sim_s):     # simulated time -> load time
        return sim_s - lead

    a0 = t_of(min(r["arrival_us"] for r in rows) / 1e6)
    # A request that did not complete has emitted fewer tokens than it asked for. Where the
    # schedule's mixes tell the asked length from the prompt length, the record carries that,
    # so the offered output tokens count every request sent.
    osl = {}
    for ph in entry["phases"]:
        for m in filter(None, ph.get("mix", "").split(",")):
            i, o = map(int, m.split(":"))
            osl.setdefault(i, set()).add(o)
    entries = []
    for r in rows:
        s = PERF0 + t_of(r["arrival_us"] / 1e6)
        err = error_of(r, entry["timeout_s"])
        ok = err is None
        e2e = r.get("e2e_us")
        entries.append({
            "scheduled_time": s, "start_time": s,
            "end_time": s + (e2e / 1e6 if ok else entry["timeout_s"]),
            "info": {"request_metrics": {"text": {"input_tokens": r.get("prompt_tokens", 0)}},
                     "response_metrics": {"output_tokens": r.get("output_tokens", 0) if ok or
                                          len(osl.get(r.get("prompt_tokens"), ())) != 1 else
                                          next(iter(osl[r["prompt_tokens"]]))}},
            "computed_metrics": {
                "time_to_first_token": r["ttft_us"] / 1e6 if r.get("ttft_us") is not None else None,
                "time_per_output_token": r["tpot_us"] / 1e6 if r.get("tpot_us") is not None else None,
                "request_latency": e2e / 1e6 if e2e is not None else None} if ok else None,
            "error": err})
    json.dump(entries, open(res / "per_request_lifecycle_metrics.json", "w"))

    # One stage per phase. load-vs-supply anchors the records on stage 0's start, 1 s
    # before the first request (inference-perf's P0), so stage 0 starts at a0 - 1.
    t0 = 0.0
    for k, ph in enumerate(entry["phases"]):
        begin = a0 - 1 if k == 0 else t0
        t0 += ph["seconds"]
        (res / f"inference-perf.partial.stage_{k}.yaml").write_text(
            f"version: 0.2.1\nrun:\n  uid: blis-{a.run}-{k}\n  time:\n"
            f"    start: '{iso(begin)}'\n    end: '{iso(t0)}'\n")

    by_role = {m["role"]: m for m in sos}
    pd = None not in start
    logs = {role: read_csv(sim / (f"signals-hpa-{role}.csv" if pd else "signals-hpa.csv"))
            for role in by_role}
    scrapes = read_csv(sim / "signals.csv")
    pods = read_csv(sim / "signals-pods.csv")
    ready = {}
    for p in pods:
        if p["state"] in READY:
            key = (p["t_s"], p.get("role") if pd else None)
            ready[key] = ready.get(key, 0) + 1

    def requested(role, t):
        """The ScaledObject's replica count at t: the last decision of either loop."""
        n = start[role]
        for r in logs.get(role, []):
            if float(r["t_s"]) > t:
                break
            n = int(float(r["final"]))
        return n

    snaps, at_start = [], {}
    for sc in scrapes:
        t = float(sc["t_s"])
        ctrls = []
        for role in start:
            name = by_role[role]["target"] if role in by_role else f"blis-{role}"
            n_ready = ready.get((sc["t_s"], role), 0)
            ctrls.append({"name": name, "kind": "Deployment", "role": role or "decode",
                          "desired_replicas": requested(role, t), "ready_replicas": n_ready,
                          "available_replicas": n_ready, "updated_replicas": n_ready})
            if t <= lead:
                at_start[role or "pool"] = n_ready
        snaps.append({"timestamp": iso(t_of(t)), "namespace": "blis", "controllers": ctrls})
    json.dump({"snapshots": snaps}, open(res / "metrics" / "processed" / "replica_status_timeseries.json", "w"))

    events, written = {}, []
    for role, m in by_role.items():
        hpa = f"keda-hpa-{m['name']}"
        log = logs[role]
        cols = [c[:-len("_value")] for c in (log[0].keys() if log else []) if c.endswith("_value")]
        for r in log:
            snap = events.setdefault(r["t_s"], {"timestamp": iso(t_of(float(r["t_s"]))),
                                                "hpa": {}, "scaledobjects": {}})
            snap["hpa"][hpa] = {"name": hpa, "desiredReplicas": int(float(r["final"])),
                                "currentReplicas": int(float(r["current"])), "reason": r["reason"],
                                "loop": r.get("loop", "hpa")}
            snap["scaledobjects"][m["name"]] = {"name": m["name"], "triggers": [
                {"name": n, "type": "prometheus"} for n in m["triggers"]]}
        out = trig / role if pd else trig
        out.mkdir(exist_ok=True)
        for c in cols:
            pts = [(t_of(float(r["t_s"])), r[f"{c}_value"]) for r in log
                   if r.get("loop", "hpa") == "hpa" and r[f"{c}_value"] != ""]
            json.dump(prom(c, pts), open(out / f"{c}.json", "w"))
            written.append(str((out / f"{c}.json").relative_to(rundir)))
    auto = [events[k] for k in sorted(events, key=float)]
    json.dump({"snapshots": auto}, open(res / "metrics" / "processed" / "autoscaler_timeseries.json", "w"))

    lab_commit = subprocess.run(["git", "-C", a.lab, "rev-parse", "HEAD"], capture_output=True,
                                text=True).stdout.strip()
    hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(sim.glob("signals-hpa*.csv"))}
    meta = {"run": a.run, "epoch0": EPOCH, "first_arrival_s": a0, "horizon_s": horizon,
            "lead_in_s": lead, "ready_at_load_start": at_start,
            "arm": {"pin": a.pin, "static": a.static, "start": {(k or "pool"): v for k, v in start.items()}},
            "lab_commit": lab_commit, "scenario": a.scenario, "seed": a.seed,
            "fates": result["requests"], "hpa_log_sha256": hashes, "schedule": entry}
    (res / "blis-run.json").write_text(json.dumps(meta, indent=1) + "\n")
    return meta, written


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--lab", required=True, help="blis-signal-lab checkout with a built ./blis")
    ap.add_argument("--scenario", required=True, help="scenario file name within --scenarios")
    ap.add_argument("--scenarios", default=None, help="default <lab>/testdata/kernel/aisimulate")
    ap.add_argument("--load", required=True, type=Path, help="gen_load.py --target blis output")
    ap.add_argument("--run", required=True)
    ap.add_argument("--policy", required=True, help="so_to_policy.py output")
    ap.add_argument("--scaledobjects", help="default: scaledobjects.json next to the policy")
    arm = ap.add_mutually_exclusive_group(required=True)
    arm.add_argument("--pin", metavar="N|ROLE=N,...")
    arm.add_argument("--active", action="store_true")
    ap.add_argument("--static", metavar="ROLE=N", help="a P/D role no ScaledObject scales")
    ap.add_argument("--seed", type=int, help="overrides the workload's seed")
    ap.add_argument("--out", required=True, type=Path)
    a = ap.parse_args()
    a.lab = str(Path(a.lab).resolve())
    a.scenarios = str(Path(a.scenarios).resolve()) if a.scenarios else \
        os.path.join(a.lab, "testdata", "kernel", "aisimulate")
    a.policy = str(Path(a.policy).resolve())
    a.load = a.load.resolve()
    a.out = a.out.resolve()
    sos = json.load(open(a.scaledobjects or Path(a.policy).parent / "scaledobjects.json"))

    entry = schedule(a.load, a.run)
    start, lead, horizon = simulate(a, entry, a.out, sos)
    meta, written = convert(a, entry, a.out, sos, start, lead, horizon)
    f = meta["fates"]
    print(f"{a.run}: {f['generated']} requests, {f['completed']} completed, "
          f"{f.get('timed_out', 0)} timed out, dropped {f.get('dropped', {})}, "
          f"{f.get('in_flight_at_horizon', 0) + f.get('queued_at_horizon', 0)} unfinished at the horizon; "
          f"start {meta['arm']['start']}" + (f", lead-in {lead:.0f} s, Ready at the load's start "
                                             f"{meta['ready_at_load_start']}" if lead else "")
          + f"; {len(written)} trigger series; {a.out}")


if __name__ == "__main__":
    main()
