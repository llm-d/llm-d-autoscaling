#!/usr/bin/env python3
"""
Post-process llm-d-benchmark results into a markdown table.

Produces the exact table format used in docs/benchmark.md.

Usage:
    # Single run:
    python hack/benchmark/postprocess.py results/guidellm-*_1

    # Three runs (Run 1 | Run 2 | Run 3 | Avg):
    python hack/benchmark/postprocess.py results/guidellm-*_1 \\
                                          results/guidellm-*_2 \\
                                          results/guidellm-*_3

    # With scenario header:
    python hack/benchmark/postprocess.py --scenario "Prefill Heavy — Qwen/Qwen3-32B (600s)" \\
        results/guidellm-*_1 results/guidellm-*_2 results/guidellm-*_3

    # Two-variant run (primary TP=2, secondary TP=1 with suffix "v2"):
    python hack/benchmark/postprocess.py --secondary-suffix v2 \\
        --gpus-per-primary 2 --gpus-per-secondary 1 \\
        results/guidellm-*_1
"""

import argparse
import json
import os
import re
import sys
from statistics import mean

METRICS = [
    "P99 TTFT (ms)",
    "P99 ITL (ms/token)",
    "Avg replicas",
    "Max replicas",
    "Avg KV cache utilization",
    "Avg queue depth (EPP)",
    "Error count",
    "Avg pod startup (s)",
]

# Metrics list used for two-variant runs; replica rows are split per variant
# and a weighted-cost row replaces the plain replica rows.
def _variant_metrics(primary_label, secondary_label):
    return [
        "P99 TTFT (ms)",
        "P99 ITL (ms/token)",
        f"Avg {primary_label} replicas",
        f"Max {primary_label} replicas",
        f"Avg {secondary_label} replicas",
        f"Max {secondary_label} replicas",
        "Avg KV cache utilization",
        "Avg queue depth (EPP)",
        "Error count",
        "Avg pod startup (s)",
        "Cost (weighted avg replicas × GPU/hr)",
    ]


def _read_tensor_from_yaml(yaml_path):
    """Return the first `tensor: N` value found in a YAML file, or 1 if absent."""
    if not yaml_path or not os.path.isfile(yaml_path):
        return 1
    with open(yaml_path) as f:
        for line in f:
            m = re.match(r'\s*tensor:\s*(\d+)', line)
            if m:
                return int(m.group(1))
    return 1


def _parse_prometheus_value(line, metric_name):
    """Extract a float from a Prometheus exposition-format line."""
    if not line.startswith(metric_name):
        return None
    rest = line[len(metric_name):]
    if rest and rest[0] not in ("{", " "):
        return None
    if rest.startswith("{"):
        close = rest.find("}")
        if close < 0:
            return None
        rest = rest[close + 1:]
    try:
        return float(rest.strip())
    except (ValueError, IndexError):
        return None


def _extract_latency(results_dir):
    """P99 TTFT and P99 ITL from results.json."""
    path = os.path.join(results_dir, "results.json")
    if not os.path.isfile(path):
        return None, None

    with open(path) as f:
        data = json.load(f)

    metrics = data["benchmarks"][0]["metrics"]

    def _p99(section_key):
        section = metrics.get(section_key, {}).get("successful", {})
        pcts = section.get("percentiles", {})
        if isinstance(pcts, list):
            pcts = {p["percentile"]: p["value"] for p in pcts}
        return pcts.get("p99") or pcts.get(99) or section.get("max")

    return _p99("time_to_first_token_ms"), _p99("inter_token_latency_ms")


def _extract_error_count(results_dir):
    """Error count from results.json."""
    path = os.path.join(results_dir, "results.json")
    if not os.path.isfile(path):
        return 0
    with open(path) as f:
        data = json.load(f)
    return data["benchmarks"][0]["metrics"]["request_totals"].get("errored", 0)


def _extract_replica_stats(results_dir):
    """Avg and max ready replicas from replica_status_timeseries.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "replica_status_timeseries.json")
    if not os.path.isfile(path):
        return None, None

    with open(path) as f:
        data = json.load(f)

    totals = []
    for snap in data["snapshots"]:
        ready = sum(
            (c.get("ready_replicas", 0) or 0) for c in snap["controllers"]
            if "decode" in c.get("name", "")
        )
        totals.append(ready)

    if not totals:
        return None, None
    return mean(totals), max(totals)


def _extract_variant_replica_stats(results_dir, secondary_suffix):
    """Per-variant avg/max ready replicas from replica_status_timeseries.json.

    Returns (primary_avg, primary_max, secondary_avg, secondary_max).
    Controllers whose name ends with '-<secondary_suffix>' are secondary;
    all other decode controllers are primary.
    """
    path = os.path.join(results_dir, "metrics", "processed",
                        "replica_status_timeseries.json")
    if not os.path.isfile(path):
        return None, None, None, None

    with open(path) as f:
        data = json.load(f)

    primary_totals, secondary_totals = [], []
    for snap in data["snapshots"]:
        p, s = 0, 0
        for c in snap.get("controllers", []):
            name = c.get("name", "")
            ready = c.get("ready_replicas", 0) or 0
            if "decode" not in name:
                continue
            if f"-{secondary_suffix}" in name:
                s += ready
            else:
                p += ready
        primary_totals.append(p)
        secondary_totals.append(s)

    if not primary_totals:
        return None, None, None, None
    return (mean(primary_totals), max(primary_totals),
            mean(secondary_totals), max(secondary_totals))


def _extract_kv_cache_avg(results_dir):
    """Average KV cache utilization (%) from raw vLLM metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    values = []
    for fname in os.listdir(raw_dir):
        if not fname.endswith(".log") or "router-epp" in fname:
            continue
        if fname == "collection_debug.log":
            continue
        fpath = os.path.join(raw_dir, fname)
        with open(fpath) as f:
            for line in f:
                val = _parse_prometheus_value(line.strip(),
                                              "vllm:kv_cache_usage_perc")
                if val is not None:
                    values.append(val * 100)
                    break

    return mean(values) if values else None


def _extract_queue_depth_avg(results_dir):
    """Average EPP queue depth from raw EPP metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    metric_names = [
        "llm_d_epp_average_queue_size",
    ]

    values = []
    for fname in sorted(os.listdir(raw_dir)):
        if not fname.endswith(".log") or "router-epp" not in fname:
            continue
        fpath = os.path.join(raw_dir, fname)
        found = False
        with open(fpath) as f:
            for line in f:
                stripped = line.strip()
                for metric_name in metric_names:
                    val = _parse_prometheus_value(stripped, metric_name)
                    if val is not None:
                        values.append(val)
                        found = True
                        break
                if found:
                    break

    return mean(values) if values else None


def _extract_pod_startup_avg(results_dir):
    """Average pod startup time (s) from pod_startup_times.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "pod_startup_times.json")
    if not os.path.isfile(path):
        return None

    with open(path) as f:
        data = json.load(f)

    times = [p["startup_seconds"] for p in data.get("pods", [])
             if p.get("startup_seconds") is not None]
    return mean(times) if times else None


def _fmt(metric, value):
    """Format a value to match the benchmark.md number style."""
    if value is None:
        return "?"

    if metric == "P99 TTFT (ms)":
        return f"{value:,.0f}"
    if metric == "P99 ITL (ms/token)":
        return f"{value:.2f}" if (value * 100) % 10 != 0 else f"{value:.1f}"
    if metric in ("Avg replicas",) or metric.startswith("Avg ") and "replicas" in metric:
        return f"{value:.2f}"
    if metric in ("Max replicas",) or metric.startswith("Max ") and "replicas" in metric:
        return str(int(value))
    if metric == "Avg KV cache utilization":
        return f"{value:.1f}%"
    if metric == "Avg queue depth (EPP)":
        return f"{value:.1f}"
    if metric == "Error count":
        return f"{int(value):,}"
    if metric == "Avg pod startup (s)":
        return str(round(value))
    if metric == "Cost (weighted avg replicas × GPU/hr)":
        return f"{value:.2f}"
    return str(value)


def process_one(results_dir, secondary_suffix=None, gpus_per_primary=1,
                gpus_per_secondary=1, primary_label="primary",
                secondary_label="secondary"):
    """Extract all benchmark.md metrics from one results directory.

    When secondary_suffix is given, replica stats are split per variant and a
    weighted cost row is included.
    """
    p99_ttft, p99_itl = _extract_latency(results_dir)
    kv_avg = _extract_kv_cache_avg(results_dir)
    queue_avg = _extract_queue_depth_avg(results_dir)
    startup_avg = _extract_pod_startup_avg(results_dir)
    error_count = _extract_error_count(results_dir)

    if secondary_suffix:
        p_avg, p_max, s_avg, s_max = _extract_variant_replica_stats(
            results_dir, secondary_suffix)
        cost = None
        if p_avg is not None and s_avg is not None:
            cost = p_avg * gpus_per_primary + s_avg * gpus_per_secondary
        return {
            "P99 TTFT (ms)": p99_ttft,
            "P99 ITL (ms/token)": p99_itl,
            f"Avg {primary_label} replicas": p_avg,
            f"Max {primary_label} replicas": p_max,
            f"Avg {secondary_label} replicas": s_avg,
            f"Max {secondary_label} replicas": s_max,
            "Avg KV cache utilization": kv_avg,
            "Avg queue depth (EPP)": queue_avg,
            "Error count": error_count,
            "Avg pod startup (s)": startup_avg,
            "Cost (weighted avg replicas × GPU/hr)": cost,
        }

    avg_rep, max_rep = _extract_replica_stats(results_dir)
    return {
        "P99 TTFT (ms)": p99_ttft,
        "P99 ITL (ms/token)": p99_itl,
        "Avg replicas": avg_rep,
        "Max replicas": max_rep,
        "Avg KV cache utilization": kv_avg,
        "Avg queue depth (EPP)": queue_avg,
        "Error count": error_count,
        "Avg pod startup (s)": startup_avg,
    }


def _compute_avg(runs, metrics):
    """Compute average column across multiple runs (raw numeric values)."""
    avg = {}
    for m in metrics:
        vals = [r[m] for r in runs if r.get(m) is not None]
        avg[m] = mean(vals) if vals else None
    return avg


def format_table(runs, labels, metrics=None):
    """Render a markdown table matching the benchmark.md style."""
    if metrics is None:
        metrics = METRICS
    show_avg = len(runs) > 1
    cols = list(labels)
    data_cols = list(runs)

    if show_avg:
        cols.append("Avg")
        data_cols.append(_compute_avg(runs, metrics))

    header = "| Metric | " + " | ".join(cols) + " |"
    sep = "|--------|" + "|".join(["------"] * len(cols)) + "|"

    rows = []
    for m in metrics:
        cells = [_fmt(m, run.get(m)) for run in data_cols]
        rows.append(f"| {m} | " + " | ".join(cells) + " |")

    return "\n".join([header, sep] + rows)


def main():
    """CLI entry point."""
    ap = argparse.ArgumentParser(
        description="Post-process llm-d-benchmark results into a markdown table")
    ap.add_argument("results_dirs", nargs="+",
                    help="One or more benchmark results directories")
    ap.add_argument("--scenario", type=str, default=None,
                    help="Scenario heading (e.g. 'Prefill Heavy — Qwen/Qwen3-32B (600s)')")
    ap.add_argument("--json", action="store_true",
                    help="Output raw JSON instead of markdown")
    ap.add_argument("--secondary-suffix", type=str, default=None,
                    help="Controller-name suffix that identifies the secondary variant "
                         "(e.g. 'v2'); enables per-variant replica rows and weighted cost")
    ap.add_argument("--primary-label", type=str, default="primary",
                    help="Label for the primary variant in table rows (default: primary)")
    ap.add_argument("--secondary-label", type=str, default="secondary",
                    help="Label for the secondary variant in table rows (default: secondary)")
    ap.add_argument("--scenario-yaml", type=str, default=None,
                    help="Path to the primary scenario YAML; tensor: value sets gpus-per-primary")
    ap.add_argument("--variant-config", type=str, default=None,
                    help="Path to the secondary variant config YAML; tensor: value sets gpus-per-secondary")
    args = ap.parse_args()

    gpus_per_primary = _read_tensor_from_yaml(args.scenario_yaml)
    gpus_per_secondary = _read_tensor_from_yaml(args.variant_config)

    metrics = (
        _variant_metrics(args.primary_label, args.secondary_label)
        if args.secondary_suffix
        else METRICS
    )

    runs = []
    labels = []
    for d in args.results_dirs:
        if not os.path.isdir(d):
            print(f"WARNING: {d} is not a directory, skipping", file=sys.stderr)
            continue
        print(f"Processing: {d}", file=sys.stderr)
        runs.append(process_one(
            d,
            secondary_suffix=args.secondary_suffix,
            gpus_per_primary=gpus_per_primary,
            gpus_per_secondary=gpus_per_secondary,
            primary_label=args.primary_label,
            secondary_label=args.secondary_label,
        ))
        labels.append(f"Run {len(runs)}")

    if not runs:
        print("ERROR: No valid results directories found", file=sys.stderr)
        sys.exit(1)

    if args.json:
        print(json.dumps(runs, indent=2))
        return

    print()
    if args.scenario:
        print(f"### {args.scenario}\n")
    print(format_table(runs, labels, metrics))
    print()


if __name__ == "__main__":
    main()
