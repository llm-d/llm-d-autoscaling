#!/usr/bin/env python3
"""Write the blis-signal-lab policy for the rendered KEDA ScaledObjects of a scenario.

blis-signal-lab runs KEDA's scale loop and a Kubernetes HPA per ScaledObject:
one for a single-pool scenario, one per role (prefill, decode) for P/D
(benchmark/protocol/3-blis-evaluation.md §3.2). This script carries over every
field that translates mechanically: replica bounds, pollingInterval, the
cooldowns, idleReplicaCount, fallback, scalingModifiers, the HPA behavior (or
its absence: KEDA's legacy path), each trigger's threshold, metricType and
activationThreshold, and useCachedMetrics. A trigger's PromQL query does not
translate: its blis signal is written by hand in TERMS.

    so_to_policy.py --scaledobject PLAN.yaml [--name SO ...] [--role SO=prefill|decode ...] \
        --terms TERMS.json --cold-start S --metrics-delay S [--threshold [SO/]NAME=VALUE ...] \
        --scrape S [--sync 15] --out policy.json

TERMS.json maps each trigger to its signal, keyed by trigger name for one
ScaledObject, or by ScaledObject name, then trigger name, for several. A signal
is a list of terms (autoscaling-kit README, "Signals"), a linear sum:

    {"epp-offered-work-rate": [{"metric": "arrived_prompt_tokens", "function": "rate", "window_s": 60}]}

or, for a query that multiplies or divides series, helper signals and an
expression over them, which becomes the HPA's composite metric
(scaling_modifiers). This is allowed only for a ScaledObject with one trigger
and no scalingModifiers of its own:

    {"epp-offered-work-rate": {"helpers": {"d": [...], "lp": [...], "s": [...]},
                               "expr": "d * (lp / max(s, 0.001))"}}

A ScaledObject's role is --role, else the suffix -prefill or -decode of its
scaleTargetRef; several ScaledObjects need a role each (hpa_by_role). Next to
policy.json the script writes scaledobjects.json (each ScaledObject's role,
name, target and triggers), which blis_run.py reads. --threshold replaces a
trigger's threshold (a capacity-derived one, set from the step's own knee).
The script refuses what blis cannot run and prints what it changed.
Standard library only.
"""

import argparse
import json
import os
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


def rules(src, notes, direction):
    """A behavior.scaleUp or scaleDown block -> blis scale_up or scale_down."""
    if src is None:
        return None
    out = {}
    for k, d in (("tolerance", "tolerance"), ("stabilizationWindowSeconds", "stabilization_window_s"),
                 ("selectPolicy", "select_policy")):
        if k in src:
            out[d] = src[k]
    if src.get("policies"):
        out["policies"] = [{"type": p["type"], "value": p["value"], "period_s": p["periodSeconds"]}
                           for p in src["policies"]]
    return out


def role_of(so, roles):
    name = so["metadata"]["name"]
    if name in roles:
        return roles[name]
    target = so["spec"]["scaleTargetRef"]["name"]
    for r in ("prefill", "decode"):
        if target.endswith("-" + r):
            return r
    return None


def signal_terms(name, spec, notes):
    """-> (terms for a linear signal, or None) and (helpers, expr) for a composite one."""
    if isinstance(spec, list):
        return spec, None
    if isinstance(spec, dict) and spec.get("helpers") and spec.get("expr"):
        return None, (spec["helpers"], spec["expr"])
    sys.exit(f"terms for {name}: a list of terms, or {{\"helpers\": ..., \"expr\": ...}}")


def translate(so, terms, override, notes, refused, a):
    spec, name = so["spec"], so["metadata"]["name"]
    adv = spec.get("advanced") or {}
    hpa = {"sync_period_s": a.sync, "tolerance": a.tolerance,
           "min_replicas": spec.get("minReplicaCount", 0),
           "max_replicas": spec.get("maxReplicaCount", 100),
           "metrics_delay_s": a.metrics_delay}
    for k, d in (("pollingInterval", "polling_interval_s"), ("cooldownPeriod", "cooldown_period_s"),
                 ("initialCooldownPeriod", "initial_cooldown_period_s"),
                 ("idleReplicaCount", "idle_replica_count")):
        if spec.get(k) is not None:
            hpa[d] = spec[k]
    fb = spec.get("fallback")
    if fb:
        hpa["fallback"] = {"failure_threshold": fb["failureThreshold"], "replicas": fb["replicas"]}
        if fb.get("behavior"):
            hpa["fallback"]["behavior"] = fb["behavior"]
    behavior = (adv.get("horizontalPodAutoscalerConfig") or {}).get("behavior")
    if behavior is None:
        notes.append(f"{name}: no behavior block: the HPA runs KEDA's default, the legacy path, "
                     "with a 300 s downscale stabilization")
    else:
        hpa["behavior"] = {}
        for src, dst in (("scaleUp", "scale_up"), ("scaleDown", "scale_down")):
            r = rules(behavior.get(src), notes, src)
            if r is not None:
                hpa["behavior"][dst] = r
    sm = adv.get("scalingModifiers")
    if sm:
        hpa["scaling_modifiers"] = {"formula": sm["formula"].strip(), "target": float(sm["target"]),
                                    "metric_type": sm.get("metricType", "AverageValue")}
        if sm.get("activationTarget") is not None:
            hpa["scaling_modifiers"]["activation_target"] = float(sm["activationTarget"])

    so_terms = terms.get(name, terms) if isinstance(terms.get(name), dict) else terms
    triggers, cached = [], set()
    src = spec.get("triggers") or []
    for i, t in enumerate(src):
        tname = t.get("name") or f"s{i}-{t.get('type')}"
        if t.get("type") != "prometheus":
            refused.append(f"{name}/{tname}: type {t.get('type')} is not a Prometheus trigger")
            continue
        if tname not in so_terms:
            refused.append(f"{name}/{tname}: no terms in {a.terms}")
            continue
        md = t.get("metadata") or {}
        thr = override.pop(f"{name}/{tname}", override.pop(tname, md.get("threshold")))
        mtype = t.get("metricType", "AverageValue")
        act = float(md.get("activationThreshold") or 0)
        cached.add(bool(t.get("useCachedMetrics")))
        linear, composite = signal_terms(tname, so_terms[tname], notes)
        if linear is not None:
            trig = {"name": tname, "target_type": mtype, "target": float(thr), "terms": linear}
            if act:
                trig["activation_threshold"] = act
            triggers.append(trig)
            continue
        if len(src) != 1 or sm:
            refused.append(f"{name}/{tname}: a composite signal needs a ScaledObject with one "
                           "trigger and no scalingModifiers; write a linear approximation")
            continue
        helpers, expr = composite
        for h, ht in helpers.items():
            triggers.append({"name": h, "target_type": "AverageValue", "target": 1, "terms": ht})
        hpa["scaling_modifiers"] = {"formula": expr, "target": float(thr), "metric_type": mtype,
                                    "activation_target": act}
        notes.append(f"{name}/{tname}: written as helper triggers {sorted(helpers)} and the "
                     "composite metric; the HPA log's composite_value is the trigger's value")
    if len(cached) > 1:
        notes.append(f"{name}: useCachedMetrics differs between triggers; blis applies one value "
                     "to all, here true")
    if True in cached:
        hpa["use_cached_metrics"] = True
    hpa["triggers"] = triggers
    extra = set(so_terms) - {t.get("name") for t in src}
    if extra:
        refused.append(f"{name}: terms for unknown trigger(s) {sorted(extra)}")
    return hpa, [t.get("name") for t in src]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scaledobject", required=True,
                    help="rendered manifest: the plan/<stack>/*.yaml file with kind: ScaledObject")
    ap.add_argument("--name", action="append", default=[], help="the ScaledObjects to use (default: all)")
    ap.add_argument("--role", action="append", default=[], metavar="SO=ROLE")
    ap.add_argument("--terms", required=True, help="JSON: each trigger's blis signal")
    ap.add_argument("--cold-start", required=True, type=float,
                    help="seconds from a replica requested to Ready (instance_lifecycle.loading_delay)")
    ap.add_argument("--metrics-delay", required=True, type=float,
                    help="seconds from a scrape to the HPA reading it (metrics_delay_s)")
    ap.add_argument("--threshold", action="append", default=[], metavar="[SO/]NAME=VALUE")
    ap.add_argument("--scrape", required=True, type=float,
                    help="scrape interval, s: the rendered PodMonitor's interval")
    ap.add_argument("--sync", type=float, default=15, help="HPA sync period, s (default 15)")
    ap.add_argument("--tolerance", type=float, default=0.1)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    sos = scaledobjects(a.scaledobject)
    if a.name:
        sos = [s for s in sos if s["metadata"]["name"] in a.name]
    if not sos:
        sys.exit(f"no ScaledObject {a.name or ''} in {a.scaledobject}")
    terms = json.load(open(a.terms))
    override = dict(t.split("=", 1) for t in a.threshold)
    roles = dict(r.split("=", 1) for r in a.role)
    notes, refused, blocks, meta = [], [], {}, []
    for so in sos:
        role = role_of(so, roles) if len(sos) > 1 or so["metadata"]["name"] in roles else None
        if len(sos) > 1 and role not in ("prefill", "decode"):
            refused.append(f"{so['metadata']['name']}: several ScaledObjects need a role each "
                           "(--role SO=prefill|decode)")
            continue
        if role in blocks:
            refused.append(f"two ScaledObjects for role {role}")
            continue
        hpa, names = translate(so, terms, override, notes, refused, a)
        blocks[role] = hpa
        meta.append({"role": role, "name": so["metadata"]["name"],
                     "target": so["spec"]["scaleTargetRef"]["name"], "triggers": names,
                     "composite": "scaling_modifiers" in hpa,
                     "min_replicas": hpa["min_replicas"], "max_replicas": hpa["max_replicas"]})
    for k in override:
        refused.append(f"--threshold {k}: no such trigger")
    if refused:
        sys.exit("cannot translate:\n  " + "\n  ".join(refused))

    auto = {"hpa": blocks[None]} if None in blocks else {"hpa_by_role": blocks}
    policy = {"autoscaler": auto,
              "instance_lifecycle": {"loading_delay": {"mean": a.cold_start, "stddev": 0}},
              "signals": {"scrape_interval_s": a.scrape}}
    with open(a.out, "w") as f:
        json.dump(policy, f, indent=2)
        f.write("\n")
    side = os.path.join(os.path.dirname(os.path.abspath(a.out)), "scaledobjects.json")
    with open(side, "w") as f:
        json.dump(meta, f, indent=2)
        f.write("\n")
    for n in notes:
        print("note:", n)
    for m in meta:
        print(f"{m['name']}" + (f" ({m['role']})" if m["role"] else "") + f": {len(m['triggers'])} "
              f"trigger(s), replicas {m['min_replicas']}-{m['max_replicas']}")
    print(f"wrote {a.out} and {side}")


if __name__ == "__main__":
    main()
