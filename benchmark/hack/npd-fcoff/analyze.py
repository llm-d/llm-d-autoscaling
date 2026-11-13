#!/usr/bin/env python3
"""Reduce one npd-fcoff llm-d-benchmark run to the research benchmark's metrics.

  analyze.py run   <results_dir> [--trace <trace.csv>] [--prom URL | --kube-context CTX]
                   --namespace NS [--deployment NAME] [--bin 60] [--out run.json]
  analyze.py knee  <results_dir> --trace <trace.csv> [--out knee.json]

<results_dir> is one experiment's directory (it holds
per_request_lifecycle_metrics.json, or the workspace.tar.zst it is packed in),
e.g. benchmark/results/<session>/results/<experiment-id>_1.

`run` mirrors the research analyze.py for a law-mode run (without --trace, e.g.
the generated agentic workload, the client's offered load is the requests it
actually sent, with the lengths inference-perf recorded):
  - client side, per schedule segment: offered req/s and work tok/s
    nu = lambda * (L_p + 3.3 * L_out), error rate, A = P(TTFT <= 1 s),
    TTFT / ITL P50/P90/P99, TTFT bucketed by prompt length;
  - control side, from Prometheus over the same window: the rendered npd-fcoff
    trigger, its components (D1, D15, L_p, L_out), the HPA's desired/current
    replicas, the Deployment's ready replicas, prefix-cache hit rate;
  - the sync check: trigger / client-offered nu per --bin seconds (the
    research kind validation's "trigger tracks the sent rate" verdict);
  - GPU-seconds = integral of ready replicas over the client window.
`knee` reduces a fixed-replica staircase to C_P and nu_max (A = 0.95 crossing),
interpolating between the stages' measured offered rates (the research reduction
used the nominal ones, so it reads ~3 % higher on the same data).

Per-request timestamps are monotonic (perf_counter) in inference-perf, so the
wall-clock origin is recovered from the stage's start epoch in
inference-perf.partial.stage_0.yaml: the first request is sent at the first
trace offset.
"""
import argparse
import bisect
import csv
import datetime
import io
import json
import math
import subprocess
import sys
import tarfile
import urllib.parse
import urllib.request
from pathlib import Path

C_W = 3.3
SLO_TTFT = 1.0
NS_PLACEHOLDER = "$NS"

TRIGGER = """(
  (sum(rate(llm_d_epp_request_total{namespace="$NS"}[1m])) or vector(0))
  + (sum(rate(llm_d_epp_request_error_total{namespace="$NS",
                                           error_code="ServiceUnavailable"}[1m])) or vector(0))
)
*
(
  ((sum(rate(vllm:prefix_cache_queries_total{namespace="$NS"}[1m]))
    / clamp_min(sum(rate(vllm:request_success_total{namespace="$NS"}[1m])), 0.001)
   ) or vector(8192))
  + 3.3 *
  ((sum(rate(vllm:generation_tokens_total{namespace="$NS"}[1m]))
    / clamp_min(sum(rate(vllm:request_success_total{namespace="$NS"}[1m])), 0.001)
   ) or vector(128))
)"""

SERIES = {
    "trigger": TRIGGER,
    "d1": 'sum(rate(llm_d_epp_request_total{namespace="$NS"}[1m]))',
    "d15": 'sum(rate(llm_d_epp_request_error_total{namespace="$NS",error_code="ServiceUnavailable"}[1m]))',
    "lbar_p": 'sum(rate(vllm:prefix_cache_queries_total{namespace="$NS"}[1m]))'
              ' / clamp_min(sum(rate(vllm:request_success_total{namespace="$NS"}[1m])), 0.001)',
    "lbar_out": 'sum(rate(vllm:generation_tokens_total{namespace="$NS"}[1m]))'
                ' / clamp_min(sum(rate(vllm:request_success_total{namespace="$NS"}[1m])), 0.001)',
    "keda_metric": 'max(keda_scaler_metrics_value{exported_namespace="$NS"})',
    "hpa_desired": 'max(kube_horizontalpodautoscaler_status_desired_replicas{namespace="$NS"})',
    "hpa_current": 'max(kube_horizontalpodautoscaler_status_current_replicas{namespace="$NS"})',
    "ready": 'max(kube_deployment_status_replicas_ready{namespace="$NS",deployment="$DEP"})',
    "spec": 'max(kube_deployment_spec_replicas{namespace="$NS",deployment="$DEP"})',
    "vllm_pods": 'count(count by (pod) (vllm:request_success_total{namespace="$NS"}))',
    "prefix_hit": 'sum(rate(vllm:prefix_cache_hits_total{namespace="$NS"}[1m]))'
                  ' / clamp_min(sum(rate(vllm:prefix_cache_queries_total{namespace="$NS"}[1m])), 0.001)',
    "running": 'sum(vllm:num_requests_running{namespace="$NS"})',
    "waiting": 'sum(vllm:num_requests_waiting{namespace="$NS"})',
}


# --- inputs ---------------------------------------------------------------

def read_results(results_dir):
    """Return (per_request records, stage_0 start epoch) from a results dir."""
    d = Path(results_dir)
    files = {}
    if (d / "per_request_lifecycle_metrics.json").exists():
        files["per_request"] = (d / "per_request_lifecycle_metrics.json").read_text()
        partial = d / "inference-perf.partial.stage_0.yaml"
        files["partial"] = partial.read_text() if partial.exists() else ""
    else:
        archive = d / "workspace.tar.zst"
        if not archive.exists():
            sys.exit(f"{d}: neither per_request_lifecycle_metrics.json nor workspace.tar.zst")
        raw = subprocess.run(["zstd", "-dc", str(archive)], check=True, capture_output=True).stdout
        with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
            for m in tar.getmembers():
                name = Path(m.name).name
                if name == "per_request_lifecycle_metrics.json":
                    files["per_request"] = tar.extractfile(m).read().decode()
                elif name == "inference-perf.partial.stage_0.yaml":
                    files["partial"] = tar.extractfile(m).read().decode()
    start = None
    for line in files.get("partial", "").splitlines():
        s = line.strip()
        if s.startswith("start:") and start is None:
            ts = s.split(":", 1)[1].strip().strip("'\"")
            start = datetime.datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()
    return json.loads(files["per_request"]), start


def read_trace(path):
    epoch = None
    rows = []
    with open(path) as f:
        r = csv.reader(f)
        next(r)
        for ts, n_in, n_out in r:
            t = datetime.datetime.strptime(ts, "%Y-%m-%d %H:%M:%S.%f").replace(
                tzinfo=datetime.timezone.utc).timestamp()
            epoch = t if epoch is None else epoch
            rows.append((t - epoch, int(n_in), int(n_out)))
    return rows


def requests(records):
    """Flatten inference-perf per-request records."""
    out = []
    for r in records:
        info = r.get("info") or {}
        cm = r.get("computed_metrics") or {}
        n_in = (info.get("request_metrics") or {}).get("text", {}).get("input_tokens") or info.get("input_tokens")
        resp = info.get("response_metrics") or {}
        out.append({
            "start": r["start_time"], "end": r["end_time"],
            "in": n_in, "out": resp.get("output_tokens"),
            "ttft": cm.get("time_to_first_token"), "itl": cm.get("inter_token_latency"),
            "err": (r.get("error") or {}).get("error_type"),
        })
    out.sort(key=lambda x: x["start"])
    return out


# --- statistics -----------------------------------------------------------

def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    k = (len(xs) - 1) * p / 100
    lo, hi = math.floor(k), math.ceil(k)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def r3(x):
    return None if x is None else round(x, 3)


def latency_summary(reqs):
    ok = [q for q in reqs if not q["err"]]
    return {
        "requests": len(reqs), "ok": len(ok),
        "error_rate": r3((len(reqs) - len(ok)) / len(reqs)) if reqs else None,
        "errors": sorted({q["err"] for q in reqs if q["err"]}),
        # failed requests count against attainment, as in the research A
        "A_ttft_le_1s": r3(sum(1 for q in ok if q["ttft"] is not None and q["ttft"] <= SLO_TTFT) / len(reqs))
        if reqs else None,
        "ttft_s": {f"p{p}": r3(pct([q["ttft"] for q in ok], p)) for p in (50, 90, 99)},
        "itl_ms": {f"p{p}": r3((pct([q["itl"] for q in ok], p) or 0) * 1000) for p in (50, 90, 99)},
    }


def ttft_by_prompt(reqs):
    edges = [0, 2048, 8192, 16384, 25000, 10 ** 9]
    out = {}
    for lo, hi in zip(edges, edges[1:]):
        b = [q["ttft"] for q in reqs if not q["err"] and q["in"] is not None and lo <= q["in"] < hi]
        if b:
            out[f"{lo}-{hi if hi < 10 ** 9 else 'inf'}"] = {"n": len(b), "p50": r3(pct(b, 50)), "p90": r3(pct(b, 90))}
    return out


def segments(schedule):
    """Segment start offsets from a gen_traces.py schedule ("0:2.0,600:9.47")."""
    return sorted(float(p.split(":")[0]) for p in schedule.split(","))


# --- prometheus -----------------------------------------------------------

class Prom:
    def __init__(self, url=None, context=None):
        self.url, self.context = url, context

    def range(self, q, start, end, step=5):
        params = urllib.parse.urlencode({"query": q, "start": start, "end": end, "step": step})
        if self.url:
            with urllib.request.urlopen(f"{self.url}/api/v1/query_range?{params}", timeout=120) as r:
                body = json.load(r)
        else:
            raw = subprocess.run(
                ["kubectl", "--context", self.context, "get", "--raw",
                 f"/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy/api/v1/query_range?{params}"],
                check=True, capture_output=True, text=True).stdout
            body = json.loads(raw)
        res = body["data"]["result"]
        return [(float(t), float(v)) for t, v in res[0]["values"]] if res else []


def at(series, t):
    i = bisect.bisect_right([s[0] for s in series], t) - 1
    return series[i][1] if i >= 0 else None


def mean_over(series, t0, t1):
    v = [x for t, x in series if t0 <= t < t1 and math.isfinite(x)]
    return sum(v) / len(v) if v else None


def median_over(series, t0, t1):
    return pct([x for t, x in series if t0 <= t < t1 and math.isfinite(x)], 50)


def integral(series, t0, t1):
    pts = [(t, x) for t, x in series if t0 <= t <= t1]
    return sum((b[0] - a[0]) * a[1] for a, b in zip(pts, pts[1:]))


# --- commands -------------------------------------------------------------

def align(reqs, trace, start_epoch):
    """Give each request its trace offset and return the trace's epoch origin.

    The first request goes out at the first trace offset, so its monotonic
    send time anchors the mapping; the stage's start epoch (+1 s, run_stage
    schedules from perf_counter() + 1) places offset 0 in wall-clock time.
    """
    if not reqs:
        return None
    t0_mono = reqs[0]["start"] - trace[0][0]
    for q in reqs:
        q["off"] = q["start"] - t0_mono
    return start_epoch + 1.0 if start_epoch else None


def sent_as_trace(reqs):
    """The requests actually sent, as (offset, prompt, output) trace rows."""
    t0 = reqs[0]["start"]
    return [(q["start"] - t0, q["in"] or 0, q["out"] or 0) for q in reqs]


def cmd_run(a):
    records, start_epoch = read_results(a.results_dir)
    reqs = requests(records)
    trace = read_trace(a.trace) if a.trace else sent_as_trace(reqs)
    epoch0 = align(reqs, trace, start_epoch)
    if epoch0 is None:
        sys.exit("cannot place the run in wall-clock time (no stage start in the results)")
    duration = trace[-1][0]
    ns = a.namespace
    dep = a.deployment or ""
    prom = Prom(a.prom, a.kube_context)
    t_start, t_end = epoch0 - 60, epoch0 + duration + 300
    ts = {k: prom.range(q.replace(NS_PLACEHOLDER, ns).replace("$DEP", dep), t_start, t_end)
          for k, q in SERIES.items()}

    cuts = (segments(a.schedule) if a.schedule else [0.0]) + [duration]
    out = {"results_dir": str(a.results_dir), "trace": str(a.trace), "namespace": ns,
           "window": {"start": epoch0, "end": epoch0 + duration},
           "requests_sent": len(reqs), "trace_rows": len(trace), "segments": []}
    for s0, s1 in zip(cuts, cuts[1:]):
        rows = [r for r in trace if s0 <= r[0] < s1]
        seg_reqs = [q for q in reqs if s0 <= q["off"] < s1]
        lam = len(rows) / (s1 - s0)
        lp = sum(r[1] for r in rows) / len(rows)
        lo = sum(r[2] for r in rows) / len(rows)
        nu = lam * (lp + C_W * lo)
        # steady part of the segment: skip the 1 m rate window + 60 s of lag
        w0, w1 = epoch0 + s0 + 120, epoch0 + s1
        out["segments"].append({
            "t": [round(s0), round(s1)], "offered_rps": r3(lam), "mix": f"{round(lp)}:{round(lo)}",
            "offered_nu": round(nu),
            "trigger_median": r3(median_over(ts["trigger"], w0, w1)),
            "trigger_over_offered": r3((median_over(ts["trigger"], w0, w1) or 0) / nu),
            "d1_median": r3(median_over(ts["d1"], w0, w1)),
            "d15_max": r3(max([x for t, x in ts["d15"] if w0 <= t < w1], default=0.0)),
            "lbar_p_median": r3(median_over(ts["lbar_p"], w0, w1)),
            "lbar_out_median": r3(median_over(ts["lbar_out"], w0, w1)),
            "ceil_nu_over_threshold": math.ceil(nu / a.threshold) if a.threshold else None,
            "ready_replicas": sorted({int(x) for t, x in ts["ready"] if epoch0 + s0 <= t < epoch0 + s1}),
            "ready_at_end": at(ts["ready"], epoch0 + s1),
            "prefix_hit_median": r3(median_over(ts["prefix_hit"], w0, w1)),
            **latency_summary(seg_reqs),
        })

    # sync check: client-offered nu vs trigger per bin (trigger at bin end + 60 s
    # of rate-window lag, the research kind validation's convention)
    bins = []
    for b0 in range(0, int(duration) - a.bin + 1, a.bin):
        rows = [r for r in trace if b0 <= r[0] < b0 + a.bin]
        if not rows:
            continue
        nu = sum(r[1] + C_W * r[2] for r in rows) / a.bin
        trig = at(ts["trigger"], epoch0 + b0 + a.bin + 30)
        bins.append({"t": b0, "offered_nu": round(nu), "trigger": r3(trig),
                     "ratio": r3(trig / nu) if trig is not None and nu else None})
    ratios = [b["ratio"] for b in bins if b["ratio"] is not None and b["t"] >= 60]
    out["sync"] = {"bin_s": a.bin, "n": len(ratios), "ratio_median": r3(pct(ratios, 50)),
                   "ratio_p10": r3(pct(ratios, 10)), "ratio_p90": r3(pct(ratios, 90)), "bins": bins}
    out["replicas"] = {
        "ready_max": max([x for _, x in ts["ready"]], default=None),
        "replica_seconds": round(integral(ts["ready"], epoch0, epoch0 + duration)),
        "decisions": [{"t": round(t - epoch0), "desired": int(x)} for (t, x), prev in
                      zip(ts["hpa_desired"][1:], ts["hpa_desired"]) if x != prev[1]],
    }
    out["overall"] = latency_summary(reqs)
    out["ttft_by_prompt_len"] = ttft_by_prompt(reqs)
    out["series"] = {k: [[round(t - epoch0), r3(x)] for t, x in v] for k, v in ts.items()}
    text = json.dumps(out, indent=1)
    if a.out:
        Path(a.out).write_text(text)
    printable = {k: v for k, v in out.items() if k != "series"}
    printable["sync"] = {k: v for k, v in out["sync"].items() if k != "bins"}
    print(json.dumps(printable, indent=1))


def cmd_knee(a):
    records, start_epoch = read_results(a.results_dir)
    trace = read_trace(a.trace)
    reqs = requests(records)
    align(reqs, trace, start_epoch)
    duration = trace[-1][0]
    stage = a.stage
    stages = []
    for s0 in range(0, int(math.ceil(duration)), stage):
        rows = [r for r in trace if s0 <= r[0] < s0 + stage]
        seg = [q for q in reqs if s0 <= q["off"] < s0 + stage]
        if not rows:
            continue
        lam = len(rows) / stage
        w = sum(r[1] + C_W * r[2] for r in rows) / len(rows)
        stages.append({"t": s0, "offered_rps": r3(lam), "work_per_req": round(w, 1),
                       "nu": round(lam * w), **latency_summary(seg)})
    # interpolate the A = 0.95 crossing in req/s
    cp = None
    for lo, hi in zip(stages, stages[1:]):
        if lo["A_ttft_le_1s"] >= 0.95 > hi["A_ttft_le_1s"]:
            f = (lo["A_ttft_le_1s"] - 0.95) / (lo["A_ttft_le_1s"] - hi["A_ttft_le_1s"])
            cp = lo["offered_rps"] + f * (hi["offered_rps"] - lo["offered_rps"])
            break
    w = stages[0]["work_per_req"] if stages else None
    out = {"stages": stages, "C_P_rps": r3(cp), "nu_max": round(cp * w) if cp else None,
           "threshold_work_0p7": round(0.7 * cp * w) if cp else None}
    text = json.dumps(out, indent=1)
    if a.out:
        Path(a.out).write_text(text)
    print(text)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("results_dir")
    r.add_argument("--trace", help="the replayed trace (omit for generated workloads)")
    r.add_argument("--namespace", required=True)
    r.add_argument("--deployment", help="the scaled Deployment (default: any in the namespace)")
    r.add_argument("--threshold", type=float, help="npdFcoff.thresholdWork, for ceil(nu/threshold)")
    g = r.add_mutually_exclusive_group(required=True)
    g.add_argument("--prom", help="Prometheus base URL")
    g.add_argument("--kube-context", help="reach monitoring/prometheus-operated via the API-server proxy")
    r.add_argument("--schedule", help="the trace's schedule (gen_traces.py --list), to report per segment")
    r.add_argument("--bin", type=int, default=60)
    r.add_argument("--out")
    k = sub.add_parser("knee")
    k.add_argument("results_dir")
    k.add_argument("--trace", required=True)
    k.add_argument("--stage", type=int, default=300)
    k.add_argument("--out")
    a = ap.parse_args()
    {"run": cmd_run, "knee": cmd_knee}[a.cmd](a)


if __name__ == "__main__":
    main()
