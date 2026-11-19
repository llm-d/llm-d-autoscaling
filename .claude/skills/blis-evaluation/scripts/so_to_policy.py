#!/usr/bin/env python3
"""Write the blis-signal-lab policy that stands in for a rendered KEDA ScaledObject.

blis-signal-lab models KEDA only as the ScaledObject-to-HPA translation: each
Prometheus trigger becomes an HPA External metric with its threshold and
metricType, and the HPA follows Kubernetes' autoscaling/v2 behavior path
(benchmark/protocol/3-blis-evaluation.md §3.2). This script carries over what
translates mechanically: min and max replicas, every trigger's threshold and
metricType, and the HPA behavior. A trigger's PromQL query does not translate:
its blis signal terms are written by hand, one entry per trigger, in TERMS.

    so_to_policy.py --scaledobject PLAN.yaml [--name SO] --terms TERMS.json \\
        --cold-start S [--threshold NAME=VALUE ...] [--scrape 15] [--sync 15] --out policy.json

TERMS.json maps each trigger name to its blis terms (autoscaling-kit README,
"Signals"):

    {"epp-offered-work-rate": [{"metric": "arrived_prompt_tokens", "function": "rate", "window_s": 60},
                               {"metric": "generated_tokens", "function": "rate", "window_s": 60,
                                "coef": 3.3}]}

--threshold replaces a trigger's threshold (a capacity-derived one, set from
the step's own knee). The script refuses what blis cannot run: no trigger
terms, scalingModifiers, a non-Prometheus trigger; and it says what it changed:
minReplicaCount 0 becomes 1, a missing behavior block is run on the behavior
path with Kubernetes' defaults. Standard library only.
"""

import argparse
import json
import re
import sys


def _scalar(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        return v[1:-1]
    if re.fullmatch(r"-?\d+", v):
        return int(v)
    if re.fullmatch(r"-?\d*\.\d+(e-?\d+)?", v):
        return float(v)
    return {"true": True, "false": False, "null": None, "{}": {}, "[]": []}.get(v, v)


def load_yaml(text):
    """The block-YAML subset llm-d-benchmark renders: mappings, sequences, quoted and plain
    scalars, `|` block scalars. Comments and flow collections other than {} and [] are not read."""
    lines = []
    for ln in text.splitlines():
        if ln.strip() and not ln.lstrip().startswith("#"):
            lines.append(ln)

    def indent(ln):
        return len(ln) - len(ln.lstrip(" "))

    def block_scalar(i, ind):
        out = []
        while i < len(lines) and (indent(lines[i]) > ind):
            out.append(lines[i])
            i += 1
        cut = min((indent(x) for x in out), default=0)
        return "\n".join(x[cut:] for x in out) + "\n", i

    def node(i, ind):
        if lines[i].lstrip().startswith("- "):
            return seq(i, indent(lines[i]))
        return mapping(i, ind)

    def mapping(i, ind):
        out = {}
        while i < len(lines):
            ln = lines[i]
            if indent(ln) != ind or ln.lstrip().startswith("- "):
                break
            key, _, rest = ln.strip().partition(":")
            rest = rest.strip()
            i += 1
            if rest in ("|", "|-", ">", ">-"):
                out[key], i = block_scalar(i, ind)
            elif rest:
                out[key] = _scalar(rest)
            elif i < len(lines) and (indent(lines[i]) > ind or
                                     (indent(lines[i]) == ind and lines[i].lstrip().startswith("- "))):
                out[key], i = node(i, indent(lines[i]))
            else:
                out[key] = None
        return out, i

    def seq(i, ind):
        out = []
        while i < len(lines) and indent(lines[i]) == ind and lines[i].lstrip().startswith("- "):
            item = lines[i].lstrip()[2:]
            inner = ind + 2
            if ":" in item and not item.startswith(("'", '"')):
                lines[i] = " " * inner + item   # the item's first key, at its mapping's indent
                val, i = mapping(i, inner)
            else:
                val, i = _scalar(item), i + 1
            out.append(val)
        return out, i

    doc, _ = mapping(0, 0) if lines else ({}, 0)
    return doc


def scaledobjects(path):
    out = []
    for doc in re.split(r"(?m)^---\s*$", open(path).read()):
        if re.search(r"(?m)^kind:\s*ScaledObject\s*$", doc):
            out.append(load_yaml(doc))
    return out


def rules(src, direction, notes):
    """KEDA/HPA scaleUp or scaleDown -> blis scale_up or scale_down."""
    if src is None:
        return None
    out = {}
    if "stabilizationWindowSeconds" in src:
        out["stabilization_window_s"] = src["stabilizationWindowSeconds"]
    if "selectPolicy" in src:
        out["select_policy"] = src["selectPolicy"]
    if src.get("policies"):
        out["policies"] = [{"type": p["type"], "value": p["value"], "period_s": p["periodSeconds"]}
                           for p in src["policies"]]
    if "tolerance" in src:
        notes.append(f"{direction}.tolerance {src['tolerance']} is not modelled: blis uses one "
                     "tolerance for both directions")
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scaledobject", required=True, help="rendered manifest (plan/<stack>/*keda-scaledobject*.yaml)")
    ap.add_argument("--name", help="the ScaledObject, when the file has several")
    ap.add_argument("--terms", required=True, help="JSON: trigger name -> list of blis terms")
    ap.add_argument("--cold-start", required=True, type=float,
                    help="seconds from a replica requested to Ready (instance_lifecycle.loading_delay)")
    ap.add_argument("--threshold", action="append", default=[], metavar="NAME=VALUE")
    ap.add_argument("--scrape", type=float, default=15, help="scrape interval, s (default 15)")
    ap.add_argument("--sync", type=float, default=15, help="HPA sync period, s (default 15)")
    ap.add_argument("--tolerance", type=float, default=0.1)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    sos = scaledobjects(a.scaledobject)
    if a.name:
        sos = [s for s in sos if s["metadata"]["name"] == a.name]
    if len(sos) != 1:
        names = [s["metadata"]["name"] for s in scaledobjects(a.scaledobject)]
        sys.exit(f"pick one ScaledObject with --name; the file has {names}")
    so = sos[0]
    spec = so["spec"]
    terms = json.load(open(a.terms))
    override = dict(t.split("=", 1) for t in a.threshold)
    notes, refused = [], []

    adv = spec.get("advanced") or {}
    if adv.get("scalingModifiers"):
        refused.append("advanced.scalingModifiers: blis takes the max over triggers, not a formula")
    behavior = (adv.get("horizontalPodAutoscalerConfig") or {}).get("behavior")
    if behavior is None:
        notes.append("no advanced.horizontalPodAutoscalerConfig.behavior: KEDA leaves the HPA on the "
                     "legacy path, blis runs the behavior path with Kubernetes' defaults")
    lo = spec.get("minReplicaCount", 0)
    if lo == 0:
        notes.append("minReplicaCount 0 runs as 1: blis does not scale to zero")
        lo = 1
    if spec.get("idleReplicaCount") is not None:
        notes.append("idleReplicaCount is not modelled")
    if "pollingInterval" in spec or "cooldownPeriod" in spec:
        notes.append("pollingInterval and cooldownPeriod are not modelled (KEDA's activation loop)")
    if spec.get("fallback"):
        notes.append("fallback is not modelled: blis queries never fail")

    triggers = []
    for i, t in enumerate(spec.get("triggers") or []):
        name = t.get("name") or f"s{i}-{t.get('type')}"
        if t.get("type") != "prometheus":
            refused.append(f"trigger {name}: type {t.get('type')} is not a Prometheus trigger")
            continue
        if name not in terms:
            refused.append(f"trigger {name}: no terms in {a.terms}")
            continue
        md = t.get("metadata") or {}
        if md.get("activationThreshold") not in (None, 0, "0"):
            notes.append(f"trigger {name}: activationThreshold is not modelled")
        if t.get("useCachedMetrics"):
            notes.append(f"trigger {name}: useCachedMetrics is not modelled (always live)")
        thr = float(override.pop(name, md.get("threshold")))
        triggers.append({"name": name, "target_type": t.get("metricType", "AverageValue"),
                         "target": thr, "terms": terms[name]})
    for name in override:
        refused.append(f"--threshold {name}: no such trigger")
    extra = set(terms) - {t["name"] for t in triggers}
    if extra:
        refused.append(f"terms for unknown trigger(s): {sorted(extra)}")
    if refused:
        sys.exit("cannot translate:\n  " + "\n  ".join(refused))

    hpa = {"sync_period_s": a.sync, "tolerance": a.tolerance, "min_replicas": lo,
           "max_replicas": spec["maxReplicaCount"], "triggers": triggers}
    for src, dst in (("scaleUp", "scale_up"), ("scaleDown", "scale_down")):
        r = rules((behavior or {}).get(src), src, notes)
        if r:
            hpa[dst] = r
    policy = {"autoscaler": {"hpa": hpa},
              "instance_lifecycle": {"loading_delay": {"mean": a.cold_start, "stddev": 0}},
              "signals": {"scrape_interval_s": a.scrape}}
    with open(a.out, "w") as f:
        json.dump(policy, f, indent=2)
        f.write("\n")
    for n in notes:
        print("note:", n)
    print(f"{a.out}: {len(triggers)} trigger(s), replicas {lo}-{spec['maxReplicaCount']}")


if __name__ == "__main__":
    main()
