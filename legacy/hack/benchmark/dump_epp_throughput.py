#!/usr/bin/env python3
"""Extract gateway-side throughput counters from EPP raw scrapes and write a
per-window-rate timeseries to:

    metrics/processed/epp_throughput.json

Source counters (cumulative on the EPP, finite-differenced into rates here):
    inference_objective_request_total                 -> request_rate   (rps)
    inference_objective_request_error_total           -> error_rate     (rps)
    inference_objective_input_tokens_count            -> input_tps
    inference_objective_output_tokens_count           -> output_tps
    inference_extension_scheduler_attempts_total      -> sched_attempts_per_s

These signals are already present in the existing EPP raw files; this dumper
just exposes them in a plot-friendly form. Works on any prior run that has
EPP scrapes.

Usage
-----
  python hack/benchmark/dump_epp_throughput.py <results>/<treatment>_<i>
"""
import argparse
import json
import re
from pathlib import Path

PATTERNS = {
    "request_total": re.compile(
        r'^inference_objective_request_total\{[^}]*model_name="(?P<m>[^"]+)"[^}]*\}\s+([0-9.eE+-]+)'
    ),
    "request_error_total": re.compile(
        r'^inference_objective_request_error_total\{[^}]*\}\s+([0-9.eE+-]+)'
    ),
    "input_tokens_count": re.compile(
        r'^inference_objective_input_tokens_count\{[^}]*\}\s+([0-9.eE+-]+)'
    ),
    "output_tokens_count": re.compile(
        r'^inference_objective_output_tokens_count\{[^}]*\}\s+([0-9.eE+-]+)'
    ),
    "sched_attempts_total": re.compile(
        r'^inference_extension_scheduler_attempts_total\{[^}]*\}\s+([0-9.eE+-]+)'
    ),
}

# Match files like "<pod>_<unixts>_metrics.log"
FILE_RE = re.compile(r"^(?P<pod>.+?)_(?P<ts>\d{10})_metrics\.log$")


def parse_one(path: Path):
    """Parse one EPP scrape file. Sums by metric (ignoring labels) for
    monotonic counters that are aggregated across label sets."""
    out = {}
    try:
        text = path.read_text()
    except Exception:
        return None
    for line in text.splitlines():
        if line.startswith("#") or not line:
            continue
        for key, rx in PATTERNS.items():
            m = rx.match(line)
            if m:
                # Take the last numeric group regardless of named groups
                val = float(m.group(m.lastindex or 1))
                out[key] = out.get(key, 0.0) + val
                break
    return out or None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir")
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()
    raw = rd / "metrics" / "raw"

    # One file per pod per scrape — we want EPP files only.
    by_ts = {}
    for f in sorted(raw.glob("*_metrics.log")):
        m = FILE_RE.match(f.name)
        if not m or "gaie-epp" not in m.group("pod"):
            continue
        ts = int(m.group("ts"))
        d = parse_one(f)
        if d:
            by_ts[ts] = d

    # Compute per-window rates via finite difference.
    samples = []
    sorted_ts = sorted(by_ts)
    for i, t in enumerate(sorted_ts):
        cur = by_ts[t]
        rates = {}
        if i > 0:
            prev_t = sorted_ts[i - 1]
            dt = t - prev_t
            if dt > 0:
                prev = by_ts[prev_t]
                for key in PATTERNS:
                    if key in cur and key in prev:
                        delta = cur[key] - prev[key]
                        if delta < 0:  # counter reset (pod restart)
                            delta = 0.0
                        rates[f"{key}_per_s"] = round(delta / dt, 3)
        samples.append({
            "timestamp": t,
            "raw": cur,
            "rates": rates,
        })

    out = rd / "metrics" / "processed" / "epp_throughput.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps({"samples": samples}, indent=2))
    print(f"Wrote {out} ({len(samples)} snapshots)")


if __name__ == "__main__":
    main()
