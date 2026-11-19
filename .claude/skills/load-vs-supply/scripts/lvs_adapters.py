"""Read an llm-d-benchmark results directory into the raw series of the load-vs-supply figure.

    LlmdBenchmark   an llm-d-benchmark results directory (inference-perf or aiperf)

Returns a Run: the client records (one per request, by scheduled time), Ready and requested
replicas, and optionally a trigger recomputed from raw scrapes, each with where it came from.
Nothing is binned, weighted or normalised here. Standard library only.

Honesty contract (SKILL.md): offered load comes from client records only, never from a trigger
or from completed-request throughput, and every recomputed or reconstructed series says so in
its `*_source` string, which the figure prints.
"""

import datetime as dt
import glob
import gzip
import json
import os
import re
from dataclasses import dataclass, field



@dataclass
class Run:
    """Raw series of one run, each with where it came from. Times are seconds since t = 0."""
    t_end: float
    requests: list                  # [{t (scheduled), in, out, ok, err, ttft, tpot, e2e}] client records
    load_source: str                # where the records came from
    ready: list                     # [(t, replicas)]
    ready_source: str
    spec: list = None               # [(t, replicas)] requested (spec.replicas)
    trigger: list = None            # [(t, value)] recomputed from scrapes, in the metric's own unit
    trigger_source: str = ""
    phases: list = field(default_factory=list)   # [(t, label)] stage boundaries
    notes: list = field(default_factory=list)    # caveats printed by the CLI
    e0: float = 0.0                 # epoch seconds of t = 0, to place external Prometheus series


# ---------------------------------------------------------------------------
# Shared readers.

def jsonl(path, opener=open):
    out = []
    with opener(path, "rt") as f:
        for ln in f:
            try:
                out.append(json.loads(ln))
            except ValueError:  # truncated tail line
                pass
    return out


def iso(s):
    return dt.datetime.fromisoformat(str(s).replace("Z", "+00:00")).timestamp()


def prom_series(result):
    """Prometheus range-query `result` list (or a full API body) -> [(t, v)] of the first series."""
    if isinstance(result, dict):
        result = result.get("data", {}).get("result", [])
    return [(float(t), float(v)) for t, v in result[0]["values"]] if result else []


# ---------------------------------------------------------------------------
# llm-d-benchmark results directory.

def inference_perf_latency(e, sm):
    """ttft, tpot, e2e (seconds) of one inference-perf record. computed_metrics when present;
    else from start_time, end_time and the first token time (perf_counter, so differences are exact)."""
    cm = e.get("computed_metrics") or {}
    if cm.get("time_to_first_token") is not None:
        return {"ttft": cm.get("time_to_first_token"), "tpot": cm.get("time_per_output_token"),
                "e2e": cm.get("request_latency"), "derived": 0}
    start, end = e.get("start_time"), e.get("end_time")
    first = (sm.get("output_token_times") or sm.get("chunk_times") or [None])[0]
    if e.get("error") is not None or start is None or end is None or first is None:
        return {"ttft": None, "tpot": None, "e2e": (end - start) if start is not None and end is not None else None,
                "derived": 0}
    out = sm.get("output_tokens") or 0
    ttft, e2e = first - start, end - start
    return {"ttft": ttft, "e2e": e2e, "tpot": (e2e - ttft) / (out - 1) if out > 1 else None, "derived": 1}


def aiperf_requests(path, t0):
    """aiperf profile_export.jsonl. request_start_ns is epoch time, so no clock bridge is
    needed; t0 (epoch s) is the time origin."""
    reqs = []
    for r in jsonl(path):
        m, md = r["metrics"], r["metadata"]

        def v(k):
            x = m.get(k)
            return x.get("value") if isinstance(x, dict) else x
        err = r.get("error")
        ttft = v("time_to_first_token")
        reqs.append({"t": md["request_start_ns"] / 1e9 - t0,
                     "in": v("input_sequence_length") or v("error_isl") or 0,
                     "out": v("output_sequence_length") or 0,
                     "ok": err is None,
                     "err": None if err is None else f"{err.get('code')} context overflow"
                     if "maximum context length" in str(err) else str(err)[:60],
                     "ttft": ttft / 1000 if ttft is not None else None,
                     "tpot": v("inter_token_latency") / 1000 if v("inter_token_latency") is not None else None,
                     "e2e": v("request_latency") / 1000 if v("request_latency") is not None else None,
                     "itl": [g / 1000 for g in (v("inter_chunk_latency") or [])]})
    return reqs


def _find(root, name):
    hits = sorted(glob.glob(os.path.join(root, "**", name), recursive=True))
    return hits


def _first_time_block(path):
    """run.time.start / end of an inference-perf benchmark-report fragment (no YAML library:
    the block is `time:` then indented `start:` and `end:` ISO strings)."""
    txt = open(path).read()
    m = re.search(r"^\s*time:\s*\n\s*start:\s*'?([^'\n]+)'?\s*\n\s*end:\s*'?([^'\n]+)'?", txt, re.M)
    return (iso(m.group(1)), iso(m.group(2))) if m else None


def _scalar(v):
    v = v.split(" #")[0].strip()
    return v[1:-1] if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'" else v


def _block(lines, key, indent):
    """The lines of the mapping under `key:` at `indent`, until the next key at <= indent."""
    out, inside = [], False
    for ln in lines:
        stripped = ln.lstrip(" ")
        ind = len(ln) - len(stripped)
        if not stripped or stripped.startswith("#"):
            continue
        if inside:
            # a block sequence may sit at its key's own indent ("triggers:\n- type: ...")
            if ind < indent or (ind == indent and not stripped.startswith("- ")):
                break
            out.append(ln)
        elif ind == indent and stripped.split(":", 1)[0] == key:
            inside = True
    return out


def _keys(lines, indent):
    """Scalar `key: value` pairs at exactly `indent`."""
    out = {}
    for ln in lines:
        stripped = ln.lstrip(" ")
        if len(ln) - len(stripped) == indent and ":" in stripped and not stripped.startswith("- "):
            k, v = stripped.split(":", 1)
            if v.strip():
                out[k.strip()] = _scalar(v)
    return out


def parse_scaledobjects(path):
    """Every ScaledObject in a rendered (multi-document) YAML file, without a YAML library.

    -> [{"name", "target", "hpa", "triggers": [{"name", "type", "metricType", "threshold"}],
         "modifiers": {"formula", "target", "metricType"} or None}]
    hpa is advanced.horizontalPodAutoscalerConfig.name, else KEDA's keda-hpa-<name>.

    Reads only what block-style manifests (as llm-d-benchmark renders them) contain: triggers as
    a list of mappings, each with `type`, optional `name` and `metricType`, and `metadata.threshold`.
    KEDA's default metricType is AverageValue.
    """
    out = []
    for doc in re.split(r"(?m)^---\s*$", open(path).read()):
        lines = doc.splitlines()
        if not re.search(r"(?m)^kind:\s*ScaledObject\s*$", doc):
            continue
        meta = _keys(_block(lines, "metadata", 0), 2)
        spec = _block(lines, "spec", 0)
        target = _keys(_block(spec, "scaleTargetRef", 2), 4).get("name")
        trig_lines = _block(spec, "triggers", 2)
        items, cur = [], None
        for ln in trig_lines:
            stripped = ln.lstrip(" ")
            if stripped.startswith("- "):
                cur = []
                items.append((len(ln) - len(stripped), cur))
                ln = " " * (len(ln) - len(stripped) + 2) + stripped[2:]
            if cur is not None:
                cur.append(ln)
        triggers = []
        for ind, item in items:
            top = _keys(item, ind + 2)
            md = _keys(_block(item, "metadata", ind + 2), ind + 4)
            thr = md.get("threshold") or md.get("targetValue") or md.get("value")
            triggers.append({"name": top.get("name"), "type": top.get("type"),
                             "metricType": top.get("metricType", "AverageValue"),
                             "threshold": float(thr) if thr not in (None, "") else None})
        sm = _block(_block(spec, "advanced", 2), "scalingModifiers", 4)
        mods = None
        if sm:
            k = _keys(sm, 6)
            mods = {"formula": k.get("formula"), "metricType": k.get("metricType", "AverageValue"),
                    "target": float(k["target"]) if k.get("target") else None}
        hpa = _keys(_block(_block(spec, "advanced", 2), "horizontalPodAutoscalerConfig", 4), 6).get("name")
        out.append({"name": meta.get("name"), "target": target, "triggers": triggers, "modifiers": mods,
                    "hpa": hpa or (f"keda-hpa-{meta['name']}" if meta.get("name") else None)})
    return out


class LlmdBenchmark:
    """An llm-d-benchmark results directory (the `results/<exp_id>/` tree of `llmdbenchmark run`).

    Looked up by file name, anywhere below `root`:

      per_request_lifecycle_metrics.json  inference-perf per-request records (perf_counter times)
      profile_export.jsonl                aiperf per-request records (epoch ns)
      inference-perf.partial.stage_N.yaml stage wall-clock window (the clock anchor)
      metrics/processed/replica_status_timeseries.json   desired/ready every tick (--monitoring)
      metrics/raw/<pod>_<epoch>_metrics.log               raw /metrics scrapes (trigger recompute)

    A trigger series is NOT collected by the harness. Pass a Prometheus range-query result
    (`trigger_json`) of the ScaledObject trigger expression, or name a gauge
    (`recompute_metric`) to be summed across pods from the raw scrapes.
    """

    def __init__(self, root, deployment=None, role=None):
        self.root, self.deployment, self.role = root, deployment, role

    def stage_windows(self):
        files = _find(self.root, "inference-perf.partial.stage_*.yaml") or \
            _find(self.root, "benchmark_report_v0.2*stage_*.yaml")
        out = []
        for f in files:
            m = re.search(r"stage_(\d+)", os.path.basename(f))
            w = _first_time_block(f)
            if m and w:
                out.append((int(m.group(1)), w[0], w[1]))
        return sorted(out)

    def requests(self, notes):
        """-> (requests with t seconds since the first stage start, epoch of that origin)."""
        pr = _find(self.root, "per_request_lifecycle_metrics.json")
        if pr:
            entries = json.load(open(pr[0]))
            stages = self.stage_windows()
            if not stages:
                raise SystemExit("per_request_lifecycle_metrics.json needs inference-perf.partial.stage_0.yaml "
                                 "(its run.time.start is the only wall-clock anchor); not found")
            e0 = stages[0][1]
            has_sched = any("scheduled_time" in e for e in entries)
            ref = [e.get("scheduled_time", e["start_time"]) for e in entries]
            p0 = min(ref) - 1.0     # load_generator sets P0 = perf_counter() + 1 after stamping run.time.start
            notes.append("clock bridge: perf_counter -> epoch via stage-0 run.time.start; "
                         + ("scheduled_time present" if has_sched else
                            "no scheduled_time in the records (inference-perf v0.7.0): error up to the first inter-arrival gap"))
            reqs, derived = [], 0
            for e, s in zip(entries, ref):
                info = e.get("info") or {}
                rm = (info.get("request_metrics") or {}).get("text") or {}
                sm = info.get("response_metrics") or {}
                err = e.get("error")
                r = {"t": s - p0, "in": rm.get("input_tokens") or 0,
                     "out": sm.get("output_tokens") or 0, "ok": err is None,
                     "err": None if err is None else
                     (err.get("type") if isinstance(err, dict) else None) or "error"}
                r.update(inference_perf_latency(e, sm))
                derived += r.pop("derived")
                reqs.append(r)
            if derived:
                notes.append(f"latency of {derived} request(s) derived from start/end and token times "
                             "(no computed_metrics: set per_request_fields.computed_metrics: true for "
                             "inference-perf's tokenizer-corrected values)")
            return reqs, e0
        ap = _find(self.root, "profile_export.jsonl")
        if ap:
            first = min(r["metadata"]["request_start_ns"] for r in jsonl(ap[0])) / 1e9
            return aiperf_requests(ap[0], first), first
        raise SystemExit("no per_request_lifecycle_metrics.json or profile_export.jsonl under "
                         f"{self.root}: re-run with report.request_lifecycle.per_request: true and "
                         "--data-collect default|fast (--data-collect results drops it)")

    def replicas(self, e0):
        """-> (ready, spec) summed over the serving controllers that pass the filters, or None.
        A P/D or multi-model stack has several; plot one pool at a time."""
        ts = _find(self.root, "replica_status_timeseries.json")
        if not ts:
            return None
        snaps = json.load(open(ts[0]))["snapshots"]
        pools = sorted({(c["name"], c.get("role", "")) for s in snaps for c in s["controllers"]
                        if not str(c.get("role", "")).endswith("-dryrun")})
        pick = [p for p in pools if (self.deployment in (None, p[0])) and (self.role in (None, p[1]))]
        if len(pick) > 1 and self.deployment is None and self.role is None:
            raise SystemExit("several serving controllers in replica_status_timeseries.json: "
                             + ", ".join(f"{n} (role {r})" for n, r in pools)
                             + "; plot one pool with --deployment NAME or --role ROLE")
        if not pick:
            raise SystemExit(f"no controller matches --deployment {self.deployment} --role {self.role}; "
                             f"have: {pools}")
        names = {n for n, _ in pick}
        ready, spec = [], []
        for s in snaps:
            cs = [c for c in s["controllers"] if c["name"] in names]
            if not cs:
                continue
            t = iso(s["timestamp"]) - e0
            ready.append((t, sum(c["ready_replicas"] for c in cs)))
            spec.append((t, sum(c["desired_replicas"] for c in cs)))
        return (ready, spec) if ready else None

    def hpa_desired(self, e0, hpa=None):
        """HPA status.desiredReplicas per harness tick, from metrics/processed/autoscaler_timeseries.json
        (written by collect_metrics.sh once it records HPA state). -> ([(t, n)], hpa name) or None."""
        ts = _find(self.root, "autoscaler_timeseries.json")
        if not ts:
            return None
        snaps = json.load(open(ts[0])).get("snapshots", [])
        names = sorted({n for s in snaps for n in s.get("hpa", {})})
        if hpa is None:
            if len(names) != 1:
                return None
            hpa = names[0]
        pts = [(iso(s["timestamp"]) - e0, s["hpa"][hpa]["desiredReplicas"]) for s in snaps
               if hpa in s.get("hpa", {}) and s["hpa"][hpa].get("desiredReplicas") is not None]
        return (pts, hpa) if pts else None

    def raw_scrapes(self, metric, e0):
        """Sum of a gauge over all pods per scrape tick, from metrics/raw/*_<epoch>_metrics.log."""
        by_t = {}
        pat = re.compile(r"^" + re.escape(metric) + r"(?:\{[^}]*\})?\s+([0-9.eE+-]+)\s*$", re.M)
        for f in glob.glob(os.path.join(self.root, "**", "metrics", "raw", "*_metrics.log"), recursive=True):
            m = re.search(r"_(\d{9,})_metrics\.log$", f)
            if not m:
                continue
            vals = [float(x) for x in pat.findall(open(f, errors="replace").read())]
            if vals:
                by_t[int(m.group(1))] = by_t.get(int(m.group(1)), 0.0) + sum(vals)
        return sorted((t - e0, v) for t, v in by_t.items())

    def build(self, bin_s=30, t_end=None, recompute_metric=None, replicas_json=None):
        notes = []
        reqs, e0 = self.requests(notes)
        stages = self.stage_windows()
        end = t_end or (max(s[2] for s in stages) - e0 if stages else max(r["t"] for r in reqs) + bin_s)
        rep = self.replicas(e0)
        rsrc = "replica_status_timeseries.json"
        if rep is None and replicas_json:
            ready = prom_series(json.load(open(replicas_json)))
            rep, rsrc = ([(t - e0, v) for t, v in ready], None), "Prometheus kube_deployment_status_replicas_ready"
        if rep is None:
            raise SystemExit("no replica source: metrics/processed/replica_status_timeseries.json missing "
                             "(run with --monitoring) and no --replicas-json given")
        ready, spec = rep
        trig, tsrc = None, ""
        if recompute_metric:
            trig = self.raw_scrapes(recompute_metric, e0)
            tsrc = f"recomputed from /metrics scrapes (sum of {recompute_metric}), not what KEDA read"
        phases = [(s[1] - e0, "") for s in stages[1:]]
        return Run(end, reqs, "client records", ready, rsrc, spec if spec else None, trig, tsrc,
                   phases, notes, e0)
