#!/usr/bin/env python3
"""Export what the autoscaler read during a run, from Prometheus.

Range-queries each Prometheus trigger of a ScaledObject, exactly as the live
object states it, over a run's window, plus the Ready and requested replicas of
its target from kube-state-metrics. Writes one Prometheus API response body per
series, the format the load-vs-supply skill takes with --trigger and
--replicas-json.

    trigger_series.py --scaledobject scaledobject.json --prom URL \
        --start EPOCH --end EPOCH --out DIR [--step 15] [--insecure]

scaledobject.json is `kubectl get scaledobject <name> -o json`. The bearer
token, when Prometheus needs one, is read from $PROM_TOKEN. Standard library only.
"""

import argparse
import json
import os
import re
import ssl
import sys
import urllib.parse
import urllib.request


def range_query(prom, query, start, end, step, insecure):
    url = prom.rstrip("/") + "/api/v1/query_range?" + urllib.parse.urlencode(
        {"query": query, "start": start, "end": end, "step": step})
    req = urllib.request.Request(url)
    token = os.environ.get("PROM_TOKEN")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    ctx = ssl._create_unverified_context() if insecure else None
    with urllib.request.urlopen(req, context=ctx, timeout=120) as resp:
        body = json.load(resp)
    if body.get("status") != "success":
        raise RuntimeError(f"query failed: {body.get('error')}: {query}")
    return body


def safe_name(name):
    return re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-") or "trigger"


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scaledobject", required=True)
    ap.add_argument("--prom", required=True, help="Prometheus or Thanos Querier base URL")
    ap.add_argument("--start", required=True, type=float, help="run start, epoch seconds")
    ap.add_argument("--end", required=True, type=float, help="run end, epoch seconds")
    ap.add_argument("--step", default=15, type=int, help="seconds (default 15)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--insecure", action="store_true", help="skip TLS verification")
    a = ap.parse_args()

    with open(a.scaledobject) as f:
        so = json.load(f)
    ns = so["metadata"]["namespace"]
    target = so["spec"]["scaleTargetRef"]
    kind = target.get("kind", "Deployment")
    os.makedirs(a.out, exist_ok=True)

    series = []
    for i, trig in enumerate(so["spec"].get("triggers", [])):
        name = trig.get("name") or f"s{i}-{trig.get('type')}"
        if trig.get("type") != "prometheus":
            print(f"skip {name}: type {trig.get('type')} is not a Prometheus trigger",
                  file=sys.stderr)
            continue
        series.append((safe_name(name), trig["metadata"]["query"]))

    obj = "statefulset" if kind == "StatefulSet" else "deployment"
    sel = f'namespace="{ns}",{obj}="{target["name"]}"'
    series.append(("replicas-ready", f"kube_{obj}_status_replicas_ready{{{sel}}}"))
    series.append(("replicas-spec", f"kube_{obj}_spec_replicas{{{sel}}}"))

    failed = 0
    for name, query in series:
        try:
            body = range_query(a.prom, query, a.start, a.end, a.step, a.insecure)
        except Exception as e:  # report every series, then fail
            print(f"FAIL {name}: {e}", file=sys.stderr)
            failed += 1
            continue
        path = os.path.join(a.out, name + ".json")
        with open(path, "w") as f:
            json.dump(body, f)
        result = body["data"]["result"]
        points = sum(len(r.get("values", [])) for r in result)
        nonzero = sum(1 for r in result for _, v in r.get("values", []) if float(v) != 0)
        print(f"{path}: {len(result)} series, {points} points, {nonzero} non-zero")
        if not result:
            print(f"WARN {name}: empty result", file=sys.stderr)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
