#!/usr/bin/env python3
"""Parse WVA controller raw scrapes (one file per scrape, written by the
patched collect_metrics.sh) into a per-metric timeseries:

    metrics/processed/wva_metrics_timeseries.json

Captures all `wva_*` Prometheus series. Per-variant gauges are bucketed by
their `variant_name` (or `exported_namespace` for namespace-scoped ones);
model-level metrics (no variant label) go under "_model".

Output shape:
    {
      "samples": [
        {
          "timestamp": <unix>,
          "primary": { "wva_saturation_utilization": 0.78, ... },
          "v2":      { ... },
          "_model":  { "wva_required_capacity": 0, ... }
        },
        ...
      ]
    }

Works on runs whose collect_metrics.sh includes the WVA scrape
block. For older runs, output is empty.

Usage
-----
  python hack/benchmark/dump_wva_full_timeseries.py <results>/<treatment>_<i>
"""
import argparse
import json
import re
from pathlib import Path

# Filenames from the patched harness: <wva-pod>_<unixts>_metrics.log
FILE_RE = re.compile(r"^(?P<pod>.+?)_(?P<ts>\d{10})_metrics\.log$")
WVA_POD_PATTERN = re.compile(r"workload-variant-autoscaler-controller-manager")

# Capture: name, label-set, value
LINE_RE = re.compile(
    r"^(?P<name>wva_[a-z_]+)(?P<labels>\{[^}]*\})?\s+(?P<value>[0-9eE.+\-]+)$"
)
# Pull a specific label out of a label set
LABEL_RE = re.compile(r'(\w+)="([^"]*)"')


def variant_key(label_dict):
    """Return the bucket key for this metric sample."""
    vname = label_dict.get("variant_name")
    if not vname:
        return "_model"
    if vname.endswith("-v2"):
        return "v2"
    return "primary"


def parse_one(path: Path):
    sample = {"primary": {}, "v2": {}, "_model": {}}
    try:
        text = path.read_text()
    except Exception:
        return None
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        m = LINE_RE.match(line)
        if not m:
            continue
        name = m.group("name")
        # Skip obvious go-runtime / process gauges that happen to use the
        # wva_ prefix in some helper libs (none currently, but defensive).
        if name.startswith("wva_metrics_"):
            # collection-internal gauges are useful but not per-variant; keep
            # them at model level.
            pass
        labels_str = m.group("labels") or ""
        labels = dict(LABEL_RE.findall(labels_str))
        value = float(m.group("value"))
        bucket = variant_key(labels)
        # Some metrics carry multiple values for the same name across labels
        # (e.g. wva_replica_scaling_total{action="scale-up"} vs scale-down).
        # Distinguish by appending the discriminating label to the key.
        disc = []
        for k in ("action", "result", "controller", "kind", "model_id",
                  "accelerator_name", "scale_target", "namespace"):
            if k in labels and labels[k]:
                disc.append(f"{k}={labels[k]}")
        if disc and name in sample[bucket]:
            sample[bucket][f"{name}|{','.join(disc)}"] = value
        else:
            sample[bucket][name] = value
    return sample


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir")
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()
    raw = rd / "metrics" / "raw"

    samples = []
    for f in sorted(raw.glob("*_metrics.log")):
        m = FILE_RE.match(f.name)
        if not m or not WVA_POD_PATTERN.search(m.group("pod")):
            continue
        s = parse_one(f)
        if not s:
            continue
        s["timestamp"] = int(m.group("ts"))
        samples.append(s)

    out = rd / "metrics" / "processed" / "wva_metrics_timeseries.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps({"samples": samples}, indent=2))
    print(f"Wrote {out} ({len(samples)} WVA snapshots)")


if __name__ == "__main__":
    main()
