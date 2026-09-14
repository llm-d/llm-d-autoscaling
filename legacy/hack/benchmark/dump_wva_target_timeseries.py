#!/usr/bin/env python3
"""Extract WVA controller decisions and V2 saturation analysis numbers from
the controller logs within a given results dir's run window. Output:

  metrics/processed/wva_target_timeseries.json

Captured per reconcile timestamp:
  - per-variant `target` (from "Applied saturation decision via shared cache")
  - model-level totalSupply / totalDemand / utilization / requiredCapacity /
    spareCapacity (from "V2 saturation analysis completed")

Both lines fire at the same reconcile, so we group by integer timestamp.

Usage
-----
  python hack/benchmark/dump_wva_target_timeseries.py \
      <results>/<treatment>_<i> -n NAMESPACE
"""
import argparse
import json
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)


DECISION_PAT = re.compile(
    r'^(?P<ts>\S+)\t\S+\tsaturation/engine\.go:\d+\t'
    r'Applied saturation decision via shared cache\t'
    r'(?P<json>\{.*\})$'
)
ANALYSIS_PAT = re.compile(
    r'^(?P<ts>\S+)\t\S+\tsaturation/engine_v2\.go:\d+\t'
    r'V2 saturation analysis completed\t'
    r'(?P<json>\{.*\})$'
)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    args = ap.parse_args()

    rd = Path(args.results_dir).resolve()
    meta_path = rd / "run_metadata.yaml"
    if not meta_path.is_file():
        print(f"ERROR: run_metadata.yaml not found in {rd}", file=sys.stderr)
        sys.exit(1)
    meta = yaml.safe_load(meta_path.read_text())

    def parse_iso(s):
        return datetime.fromisoformat(s.replace("Z", "+00:00"))

    start = parse_iso(meta["harness_start"])
    stop = parse_iso(meta["harness_stop"])

    # Pull WVA logs covering the run window. We query "since" relative to now
    # plus a small buffer to ensure we capture the harness-start tick.
    now = datetime.now(timezone.utc)
    since_seconds = int((now - start).total_seconds()) + 90

    logs = subprocess.run(
        ["kubectl", "logs", "-n", args.namespace,
         "-l", "app.kubernetes.io/name=workload-variant-autoscaler",
         f"--since={since_seconds}s", "--tail=200000"],
        capture_output=True, text=True,
    ).stdout

    samples_by_ts = {}

    # Bucket reconciles by integer timestamp. Some reconciles fire both
    # "V2 saturation analysis" and per-variant "Applied decision" lines at the
    # same wall-clock second; we want them merged into one sample.
    def bucket(ts_dt):
        return samples_by_ts.setdefault(int(ts_dt.timestamp()), {})

    for line in logs.splitlines():
        m = DECISION_PAT.match(line)
        if m:
            try:
                ts_dt = parse_iso(m.group("ts"))
                if ts_dt < start or ts_dt > stop:
                    continue
                d = json.loads(m.group("json"))
            except (ValueError, json.JSONDecodeError):
                continue
            variant = d.get("variant", "")
            target = d.get("target")
            if target is None:
                continue
            tag = "v2" if variant.endswith("-v2") else "primary"
            bucket(ts_dt)[tag] = int(target)
            continue

        m = ANALYSIS_PAT.match(line)
        if m:
            try:
                ts_dt = parse_iso(m.group("ts"))
                if ts_dt < start or ts_dt > stop:
                    continue
                d = json.loads(m.group("json"))
            except (ValueError, json.JSONDecodeError):
                continue
            b = bucket(ts_dt)
            for k in ("totalSupply", "totalDemand", "utilization",
                      "requiredCapacity", "spareCapacity"):
                if k in d:
                    b[k] = d[k]

    samples = []
    for ts, b in sorted(samples_by_ts.items()):
        samples.append({
            "timestamp": ts,
            "primary":         b.get("primary"),
            "v2":              b.get("v2"),
            "totalSupply":     b.get("totalSupply"),
            "totalDemand":     b.get("totalDemand"),
            "utilization":     b.get("utilization"),
            "requiredCapacity": b.get("requiredCapacity"),
            "spareCapacity":   b.get("spareCapacity"),
        })

    out = rd / "metrics" / "processed" / "wva_target_timeseries.json"
    out.parent.mkdir(parents=True, exist_ok=True)

    # Don't clobber an existing non-empty file with zero new samples — typically
    # means the controller log buffer rotated past the run window. Preserve
    # whatever was previously captured.
    if not samples and out.is_file():
        try:
            existing = json.loads(out.read_text()).get("samples", [])
        except (OSError, json.JSONDecodeError):
            existing = []
        if existing:
            print(f"Skipped overwriting {out}: 0 new snapshots, "
                  f"existing file has {len(existing)}.")
            return

    out.write_text(json.dumps({"samples": samples}, indent=2))
    print(f"Wrote {out} ({len(samples)} snapshots, "
          f"window {start.isoformat()} -> {stop.isoformat()})")


if __name__ == "__main__":
    main()
