#!/usr/bin/env python3
"""Draw a load-vs-supply figure from an llm-d-benchmark results directory, in raw units only.

Stacked panels on one time axis, each in its own unit:
  offered load      input and/or output tokens/s (or requests/s) clients sent, per --bin seconds
  autoscaler input  one panel per trigger: its value, with its threshold as a reference line
  replicas          Ready, requested and (if given) the HPA's desired replicas
  SLO attainment    with --slo: share of requests sent in each bin that met every SLO

Nothing is normalised by a capacity or a threshold, and no series is rescaled to fit another.
Any scenario; one pool at a time (a P/D stack has a prefill and a decode pool).

  load_vs_supply.py RESULTS_DIR --loop open --role decode \\
      --scaledobject so.yaml --trigger trigger.json --title ... --out fig.svg

Writes OUT.svg plus OUT.csv (every plotted series, with its source) and prints the provenance of
every series. See SKILL.md.
"""

import argparse
import csv
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lvs_adapters as A  # noqa: E402
import lvs_render as R  # noqa: E402

LOAD_NAMES = {"in": "input tokens/s", "out": "output tokens/s", "req": "requests/s"}


def parse_trigger(spec):
    """FILE[=THRESHOLD][:NAME] -> (file, threshold or None, name or None)."""
    name = thr = None
    if ":" in os.path.basename(spec):
        spec, name = spec.rsplit(":", 1)
    if "=" in os.path.basename(spec):
        spec, thr = spec.rsplit("=", 1)
    return spec, (float(thr) if thr else None), name


def threshold_refs(pts, thr, mtype):
    """Reference lines drawing the HPA's own rule for this metricType -> (refs, label suffix)."""
    if thr is None:
        return [], ""
    if mtype == "AverageValue":
        # the HPA asks for ceil(value / threshold) replicas, so mark k x threshold
        top = max((v for _, v in pts), default=thr) * 1.12
        n = max(1, min(8, int(top // thr)))
        refs = [(thr, f"threshold {thr:g}: 2 replicas above")]
        refs += [(k * thr, f"{k + 1} above") for k in range(2, n + 1)]
        return refs, f"; AverageValue: HPA asks ceil(value ÷ {thr:g})"
    if mtype == "Value":
        return ([(thr, f"threshold {thr:g}")],
                f"; Value: HPA asks ceil(current replicas × value ÷ {thr:g})")
    return [(thr, f"target {thr:g}")], f"; {mtype}"


def pick_scaledobject(ap, a):
    """The ScaledObject of the plotted pool, from --scaledobject (may hold several)."""
    if not a.scaledobject:
        return None
    sos = A.parse_scaledobjects(a.scaledobject)
    if a.scaledobject_name:
        sos = [s for s in sos if s["name"] == a.scaledobject_name]
    elif len(sos) > 1 and a.deployment:
        sos = [s for s in sos if s["target"] == a.deployment] or sos
    if len(sos) != 1:
        ap.error(f"{a.scaledobject}: {len(sos)} matching ScaledObjects "
                 f"{[(s['name'], s['target']) for s in A.parse_scaledobjects(a.scaledobject)]}; "
                 "pick one with --scaledobject-name (or --deployment matching its scaleTargetRef)")
    return sos[0]


def detect(path):
    for name in ("per_request_lifecycle_metrics.json", "profile_export.jsonl",
                 "replica_status_timeseries.json"):
        if A._find(path, name):
            return True
    return False


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("path", help="llm-d-benchmark results dir")
    g = ap.add_argument_group("which pool")
    g.add_argument("--deployment", help="plot this Deployment/StatefulSet (needed when the stack has several pools)")
    g.add_argument("--role", help="or the pool with this llm-d.ai/role (decode, prefill, ...)")
    g = ap.add_argument_group("offered load")
    g.add_argument("--loop", choices=["open", "closed"], required=True,
                   help="how the load generator paces requests. open (inference-perf constant/poisson): the "
                        "send rate is the load. closed (concurrent): the send rate falls when the pool slows, "
                        "so it is plotted but labelled as not the load")
    g.add_argument("--load", default=None, metavar="in,out,req",
                   help="which offered-load panels: in (input tokens/s), out (output tokens/s), req (requests/s). "
                        "Default: in for --role prefill, out for --role decode, else in,out")
    g.add_argument("--bin", type=int, default=R.BIN, help="bin length, seconds (>= 30 on the llm-d-benchmark clock bridge)")
    g = ap.add_argument_group("SLO attainment")
    g.add_argument("--slo", action="append", default=[], metavar="KEY=SECONDS",
                   help="an SLO a request must meet to count as good: ttft, tpot or e2e, in seconds; repeat for "
                        "several (a request is good if it meets all). Adds the attainment panel")
    g.add_argument("--attainment-target", type=float, metavar="SHARE",
                   help="draw the attainment objective as a reference line, e.g. 0.95")
    g = ap.add_argument_group("what the autoscaler read")
    g.add_argument("--trigger", action="append", default=[], metavar="FILE[=THRESHOLD][:NAME]",
                   help="Prometheus range-query result (API body or result list) of one trigger expression; "
                        "repeat per trigger, one panel each. THRESHOLD overrides the ScaledObject's; NAME labels "
                        "the panel and matches the trigger's name in --scaledobject")
    g.add_argument("--scaledobject", help="rendered ScaledObject YAML (may hold several documents): supplies each "
                   "--trigger's threshold and metricType, matched by :NAME to the trigger's name (a lone trigger "
                   "matches without a name; :composite matches advanced.scalingModifiers)")
    g.add_argument("--scaledobject-name", help="which ScaledObject in the file, when it holds several")
    g.add_argument("--hpa-desired", metavar="FILE",
                   help="Prometheus range result of kube_horizontalpodautoscaler_status_desired_replicas for this "
                        "pool's HPA; drawn in the replicas panel. Default: metrics/processed/autoscaler_timeseries.json if present")
    g.add_argument("--recompute-metric", help="gauge summed over pods from metrics/raw scrapes, labelled as recomputed")
    g = ap.add_argument_group("other")
    g.add_argument("--replicas-json", help="fallback when the harness recorded no replicas: Prometheus range result of Ready replicas")
    g.add_argument("--t-end", type=float)
    g.add_argument("--title", default="Load vs supply")
    g.add_argument("--subtitle", default="")
    g.add_argument("--out", required=True, help="output .svg")
    a = ap.parse_args(argv)

    if not detect(a.path):
        ap.error(f"{a.path} holds no per-request records or replica series: not an llm-d-benchmark results dir")
    keys = a.load.split(",") if a.load else {"prefill": ["in"], "decode": ["out"]}.get(a.role, ["in", "out"])
    if any(k not in LOAD_NAMES for k in keys):
        ap.error(f"--load takes in, out, req; got {a.load}")

    run = A.LlmdBenchmark(a.path, a.deployment, a.role).build(a.bin, a.t_end, a.recompute_metric, a.replicas_json)
    if not run.requests:
        ap.error("no client records: the offered-load panel has no source")

    panels, rows = [], []
    # --- offered load: what clients sent, every request by its scheduled time, completed or not
    if a.loop == "open":
        how = f"client records, sent per {a.bin} s"
    else:
        how = f"client records, sent per {a.bin} s; closed loop: falls when the pool slows, not the load"
        run.notes.append("closed loop: the send rate depends on the replica count, so this panel is not the load")
    for k in keys:
        pts = R.bins(run.requests, run.t_end, k, a.bin)
        panels.append({"label": f"Offered {LOAD_NAMES[k]} ({how})",
                       "series": [{"pts": pts, "cls": "s2", "width": 1.75, "name": "offered"}]})
        rows += [(f"offered_{k}", t, v, how) for t, v in pts]

    # --- what the autoscaler read, one panel per trigger, in its own unit
    so = pick_scaledobject(ap, a)
    if so and so["modifiers"]:
        m = so["modifiers"]
        run.notes.append(f"{so['name']} uses scalingModifiers: the HPA reads one composite metric "
                         f"({m['formula']}, target {m['target']:g}, {m['metricType']}), not each trigger. "
                         "Pass its series as --trigger FILE:composite to draw what the HPA used")
    trig_src = []
    for spec in a.trigger:
        f, thr, name = parse_trigger(spec)
        label = name or os.path.splitext(os.path.basename(f))[0]
        mtype, how = None, "explicit"
        if so:
            if name == "composite" and so["modifiers"]:
                cfg = {"threshold": so["modifiers"]["target"], "metricType": so["modifiers"]["metricType"]}
            else:
                cfg = next((t for t in so["triggers"] if name and t["name"] == name), None)
                if cfg is None and len(so["triggers"]) == 1 and len(a.trigger) == 1:
                    cfg = so["triggers"][0]
            if cfg is None:
                run.notes.append(f"--trigger {spec}: no trigger named {name!r} in ScaledObject {so['name']} "
                                 f"(has {[t['name'] for t in so['triggers']]}); name the panel after one with :NAME")
            else:
                mtype = cfg["metricType"]
                if thr is None:
                    thr, how = cfg["threshold"], f"from ScaledObject {so['name']}"
        if thr is not None and mtype is None:
            mtype = "AverageValue"
            how += ", metricType assumed AverageValue (KEDA's default)"
        pts = [(t - run.e0, v) for t, v in A.prom_series(json.load(open(f)))]
        refs, rule = threshold_refs(pts, thr, mtype)
        panels.append({"label": f"What KEDA read: trigger {label} (Prometheus range query{rule})", "refs": refs,
                       "series": [{"pts": pts, "cls": "s3", "width": 1.25, "name": "what KEDA read"}]})
        rows += [(f"trigger_{label}", t, v, "Prometheus range query") for t, v in pts]
        trig_src.append(f"{label}: Prometheus range query" + (f"; threshold {thr:g} {mtype} ({how})" if thr is not None else ""))
    if run.trigger:
        panels.append({"label": f"Trigger input {a.recompute_metric} ({run.trigger_source})",
                       "series": [{"pts": run.trigger, "cls": "s3", "width": 1.25, "name": "recomputed"}]})
        rows += [("trigger_recomputed", t, v, run.trigger_source) for t, v in run.trigger]
        trig_src.append(run.trigger_source)
    if not trig_src:
        run.notes.append("no trigger series: llm-d-benchmark does not record what the autoscaler read; "
                         "pass --trigger FILE (Prometheus range query) or --recompute-metric")

    # --- replicas
    rep = [{"pts": run.ready, "cls": "s1", "width": 2.5, "step": True, "name": "Ready"}]
    if run.spec:
        rep.append({"pts": run.spec, "cls": "s1", "width": 1.5, "dash": "5 4", "step": True, "name": "requested"})
    hpa_src = None
    if a.hpa_desired:
        des = [(t - run.e0, v) for t, v in A.prom_series(json.load(open(a.hpa_desired)))]
        hpa_src = "Prometheus kube_horizontalpodautoscaler_status_desired_replicas"
    else:
        got = A.LlmdBenchmark(a.path).hpa_desired(run.e0, so["hpa"] if so else None)
        des, hpa_src = (got[0], f"autoscaler_timeseries.json ({got[1]})") if got else (None, None)
    if des:
        rep.append({"pts": des, "cls": "s3", "width": 1.25, "dash": "2 3", "step": True, "name": "HPA desired"})
        rows += [("hpa_desired", t, v, hpa_src) for t, v in des]
    panels.append({"label": f"Replicas ({run.ready_source})", "series": rep, "integer": True})
    rows += [("ready", t, v, run.ready_source) for t, v in run.ready]
    rows += [("requested", t, v, run.ready_source) for t, v in run.spec or []]

    # --- SLO attainment: share of requests sent in the bin that met every SLO; errors are misses
    if a.slo:
        slo = {}
        for item in a.slo:
            k, _, v = item.partition("=")
            if k not in R.SLO_KEYS or not v:
                ap.error(f"--slo {item}: use ttft=S, tpot=S or e2e=S (seconds)")
            slo[k] = float(v)
        pts, unjudged = R.attainment(run.requests, run.t_end, slo, a.bin)
        if unjudged:
            run.notes.append(f"attainment: {unjudged} completed request(s) lack a latency the SLO needs and are "
                             "left out (inference-perf: per_request_fields.computed_metrics: true)")
        if all(v is None for _, v in pts):
            ap.error("--slo given but no request could be judged: the records carry no latencies")
        def ms(x):
            return f"{x * 1000:g} ms" if x < 1 else f"{x:g} s"
        cond = ", ".join(f"{R.SLO_KEYS[k]} ≤ {ms(v)}" for k, v in slo.items())
        refs = [(a.attainment_target, f"objective {a.attainment_target * 100:g}%")] if a.attainment_target else []
        panels.append({"label": f"SLO attainment (requests sent in the bin meeting {cond}; errors are misses)",
                       "series": [{"pts": pts, "cls": "s4", "width": 1.75, "name": "attainment"}],
                       "refs": refs, "ymax": 1.0, "step": 0.25, "percent": True})
        rows += [("attainment", t, v, f"client records; {cond}") for t, v in pts if v is not None]
        att_src = f"client records; {cond}"
    else:
        att_src = None

    with open(a.out, "w") as fh:
        fh.write(R.panels_svg(a.title, a.subtitle, run.t_end, panels, run.phases))
    base = os.path.splitext(a.out)[0]
    with open(base + ".csv", "w", newline="") as fh:
        w = csv.writer(fh)
        w.writerow(["series", "t_s", "value", "source"])
        for name, t, v, s in rows:
            w.writerow([name, round(t, 2), round(v, 4), s])

    print(f"wrote {a.out} and {base}.csv", file=sys.stderr)
    print(f"  offered load   : {run.load_source} ({a.loop} loop): {', '.join(LOAD_NAMES[k] for k in keys)}", file=sys.stderr)
    print(f"  ready replicas : {run.ready_source}", file=sys.stderr)
    print(f"  requested      : {'yes' if run.spec else 'not available'}", file=sys.stderr)
    print(f"  HPA desired    : {hpa_src or 'not available'}", file=sys.stderr)
    print(f"  trigger        : {'; '.join(trig_src) or 'not available'}", file=sys.stderr)
    print(f"  SLO attainment : {att_src or 'not drawn (no --slo)'}", file=sys.stderr)
    for n in run.notes:
        print(f"  note: {n}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
