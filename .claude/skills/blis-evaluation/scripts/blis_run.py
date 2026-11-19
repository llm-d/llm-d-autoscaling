#!/usr/bin/env python3
"""Run one step 3 run in blis-signal-lab and leave it in llm-d-benchmark's layout.

    blis_run.py --lab DIR --scenario FILE --load DIR --run NAME --policy policy.json \\
        (--pin N | --active) --so-name SO --out RUNDIR [--scenarios DIR] [--seed N]

NAME is a load gen_load.py --target blis wrote: its workload is DIR/NAME.yaml
and its schedule is the entry NAME of DIR/index.json (or DIR/NAME.json for a
knee). The horizon is the load plus its drain.

Arms (benchmark/protocol/3-blis-evaluation.md §3.4):
  --pin N    min = max = N: the HPA still reads and logs every trigger, but
             cannot move the pool (KEDA's paused-replicas holds it the same way)
  --active   the policy as written, starting from min_replicas Ready pods

RUNDIR gets:
  sim/          blis's own output: result.json (the Result, with request_rows),
                signals.csv, signals-pods.csv, signals-hpa.csv, signals-summary.json,
                stderr.txt, command.txt, and the policy and workload as run
  results/      the same run as an llm-d-benchmark results directory:
                per_request_lifecycle_metrics.json, inference-perf.partial.stage_N.yaml
                (one per phase), metrics/processed/replica_status_timeseries.json and
                autoscaler_timeseries.json, and blis-run.json (the clock and the inputs)
  triggers/     one Prometheus API body per trigger, from the HPA log (what the HPA
                read at each sync), the format load-vs-supply takes with --trigger

Simulated time t s is placed at epoch EPOCH + t. Standard library only.
"""

import argparse
import csv
import datetime as dt
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path

EPOCH = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc).timestamp()
PERF0 = 1000.0           # perf_counter of simulated t = 0 in the per-request records
DEPLOYMENT = "blis-decode"


def iso(t):
    return dt.datetime.fromtimestamp(EPOCH + t, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def schedule(load, run):
    idx = load / "index.json"
    entry = json.load(open(idx)).get(run) if idx.exists() else None
    if entry is None and (load / f"{run}.json").exists():
        entry = json.load(open(load / f"{run}.json")).get(run)
    if entry is None or "phases" not in entry:
        sys.exit(f"no schedule for {run} in {idx} or {load / (run + '.json')}")
    return entry


def simulate(a, entry, rundir):
    sim = rundir / "sim"
    sim.mkdir(parents=True, exist_ok=True)
    policy = json.load(open(a.policy))
    hpa = policy["autoscaler"]["hpa"]
    if a.pin is not None:
        hpa["min_replicas"] = hpa["max_replicas"] = a.pin
        pods = a.pin
    else:
        pods = hpa.get("min_replicas", 1)
    (sim / "policy.json").write_text(json.dumps(policy, indent=2) + "\n")
    workload = Path(a.load) / f"{a.run}.yaml"
    (sim / "workload.yaml").write_text(workload.read_text())
    horizon = entry["duration_s"] + entry["drain_s"]
    blis = Path(a.lab) / "blis"
    # run in sim/, every file by its name there, so command.txt reruns it as it ran
    cmd = [str(blis), "run", "--scenario", a.scenario, "--scenarios", a.scenarios,
           "--workload-spec", "workload.yaml", "--policy-config", "policy.json",
           "--num-instances", str(pods), "--horizon", str(int(horizon * 1e6)),
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
    return pods, horizon


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


def convert(a, entry, rundir, pods, horizon):
    sim, res, trig = rundir / "sim", rundir / "results", rundir / "triggers"
    (res / "metrics" / "processed").mkdir(parents=True, exist_ok=True)
    trig.mkdir(exist_ok=True)
    result = json.load(open(sim / "result.json"))
    rows = result["request_rows"]
    if not rows:
        sys.exit("the run generated no request")

    a0 = min(r["arrival_us"] for r in rows) / 1e6
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
        s = PERF0 + r["arrival_us"] / 1e6
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
        start = a0 - 1 if k == 0 else t0
        t0 += ph["seconds"]
        (res / f"inference-perf.partial.stage_{k}.yaml").write_text(
            f"version: 0.2.1\nrun:\n  uid: blis-{a.run}-{k}\n  time:\n"
            f"    start: '{iso(start)}'\n    end: '{iso(t0)}'\n")

    with open(sim / "signals.csv") as f:
        scrapes = list(csv.DictReader(f))
    with open(sim / "signals-hpa.csv") as f:
        syncs = list(csv.DictReader(f))
    snaps = []
    for s in scrapes:
        t = float(s["t_s"])
        ready = int(float(s["ready_pods"]))
        held = ready + int(float(s["loading_pods"])) + int(float(s["pending_pods"]))
        desired = int(float(s["desired_pods"])) or held
        snaps.append({"timestamp": iso(t), "namespace": "blis", "controllers": [{
            "name": DEPLOYMENT, "kind": "Deployment", "role": "decode",
            "desired_replicas": desired, "ready_replicas": ready, "available_replicas": ready,
            "updated_replicas": held, "draining_replicas": int(float(s["draining_pods"])),
            "gpu_pods": int(float(s["gpu_pods"]))}]})
    json.dump({"snapshots": snaps}, open(res / "metrics" / "processed" / "replica_status_timeseries.json", "w"))

    hpa_name = f"keda-hpa-{a.so_name}"
    names = [c[:-len("_value")] for c in (syncs[0].keys() if syncs else []) if c.endswith("_value")]
    auto = [{"timestamp": iso(float(r["t_s"])),
             "hpa": {hpa_name: {"name": hpa_name, "desiredReplicas": int(float(r["final"])),
                                "currentReplicas": int(float(r["current"])), "reason": r["reason"]}},
             "scaledobjects": {a.so_name: {"name": a.so_name,
                                           "triggers": [{"name": n, "type": "prometheus"} for n in names]}}}
            for r in syncs]
    json.dump({"snapshots": auto}, open(res / "metrics" / "processed" / "autoscaler_timeseries.json", "w"))
    for n in names:
        body = {"status": "success", "data": {"resultType": "matrix", "result": [{
            "metric": {"__name__": n, "source": "blis-signal-lab HPA log (value at each sync)"},
            "values": [[EPOCH + float(r["t_s"]), r[f"{n}_value"]] for r in syncs]}]}}
        json.dump(body, open(trig / f"{n}.json", "w"))

    lab_commit = subprocess.run(["git", "-C", a.lab, "rev-parse", "HEAD"], capture_output=True,
                                text=True).stdout.strip()
    meta = {"run": a.run, "epoch0": EPOCH, "first_arrival_s": a0, "horizon_s": horizon,
            "arm": f"pin {a.pin}" if a.pin is not None else f"active from {pods}",
            "lab_commit": lab_commit, "scenario": a.scenario, "seed": a.seed,
            "fates": result["requests"],
            "hpa_log_sha256": hashlib.sha256((sim / "signals-hpa.csv").read_bytes()).hexdigest(),
            "schedule": entry}
    (res / "blis-run.json").write_text(json.dumps(meta, indent=1) + "\n")
    return meta, names


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--lab", required=True, help="blis-signal-lab checkout with a built ./blis")
    ap.add_argument("--scenario", required=True, help="scenario file name within --scenarios")
    ap.add_argument("--scenarios", default=None, help="default <lab>/testdata/kernel/aisimulate")
    ap.add_argument("--load", required=True, type=Path, help="gen_load.py --target blis output")
    ap.add_argument("--run", required=True)
    ap.add_argument("--policy", required=True, help="so_to_policy.py output")
    arm = ap.add_mutually_exclusive_group(required=True)
    arm.add_argument("--pin", type=int, metavar="N")
    arm.add_argument("--active", action="store_true")
    ap.add_argument("--so-name", required=True, help="the ScaledObject's name (names the HPA)")
    ap.add_argument("--seed", type=int, help="overrides the workload's seed")
    ap.add_argument("--out", required=True, type=Path)
    a = ap.parse_args()
    a.lab = str(Path(a.lab).resolve())
    a.scenarios = a.scenarios or os.path.join(a.lab, "testdata", "kernel", "aisimulate")
    a.policy = str(Path(a.policy).resolve())
    a.load = a.load.resolve()
    a.out = a.out.resolve()

    entry = schedule(a.load, a.run)
    pods, horizon = simulate(a, entry, a.out)
    meta, names = convert(a, entry, a.out, pods, horizon)
    f = meta["fates"]
    print(f"{a.run}: {f['generated']} requests, {f['completed']} completed, "
          f"{f.get('timed_out', 0)} timed out, dropped {f.get('dropped', {})}, "
          f"{f.get('in_flight_at_horizon', 0) + f.get('queued_at_horizon', 0)} unfinished at {horizon:.0f} s; "
          f"triggers {names}; {a.out}")


if __name__ == "__main__":
    main()
