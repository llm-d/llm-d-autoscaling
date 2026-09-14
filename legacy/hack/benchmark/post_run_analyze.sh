#!/usr/bin/env bash
# Post-run analyzer for two-variant WVA benchmarks.
# Wraps the three steps that should always run after `make benchmark-run`:
#   1. dump WVA controller decisions + V2 saturation analysis numbers from logs
#      (must run while the controller pod's log buffer still covers the run
#       window — kubectl rotates, so do this promptly after the benchmark)
#   2. compute capacity & 3-component demand estimate from raw vLLM/EPP scrapes
#   3. render the pipeline plot
#
# Usage:
#   ./hack/benchmark/post_run_analyze.sh <results_dir> [namespace] [suffix]
#
# Where:
#   <results_dir> is e.g. biran-20260531-130812-164/results/guidellm-1780222131-3ew5uw_1
#   [namespace]   defaults to $BENCHMARK_NAMESPACE or `biran`
#   [suffix]      optional title suffix for the plot
set -euo pipefail

RESULTS_DIR="${1:?usage: $0 <results_dir> [namespace] [suffix]}"
NS="${2:-${BENCHMARK_NAMESPACE:-biran}}"
SUFFIX="${3:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "[1/5] dump_wva_target_timeseries.py (decisions + V2 analyzer numbers)"
python3 "$SCRIPT_DIR/dump_wva_target_timeseries.py" "$RESULTS_DIR" -n "$NS" || \
    echo "  (skipping: log dump failed; raw-scrape estimate will still render)"

echo "[2/5] dump_capacity_demand_estimate.py (raw scrape estimate)"
python3 "$SCRIPT_DIR/dump_capacity_demand_estimate.py" "$RESULTS_DIR"

echo "[3/5] dump_epp_throughput.py (request rate from EPP counters)"
python3 "$SCRIPT_DIR/dump_epp_throughput.py" "$RESULTS_DIR" || true

echo "[4/5] dump_wva_full_timeseries.py (WVA Prometheus metrics — empty if collect_metrics.sh predates the WVA scrape patch)"
python3 "$SCRIPT_DIR/dump_wva_full_timeseries.py" "$RESULTS_DIR" || true

echo "[5/5] plot_two_variant_pipeline.py"
if [ -n "$SUFFIX" ]; then
    python3 "$SCRIPT_DIR/plot_two_variant_pipeline.py" "$RESULTS_DIR" --suffix "$SUFFIX"
else
    python3 "$SCRIPT_DIR/plot_two_variant_pipeline.py" "$RESULTS_DIR"
fi

echo "Done. Outputs:"
echo "  $RESULTS_DIR/metrics/processed/wva_target_timeseries.json"
echo "  $RESULTS_DIR/metrics/processed/capacity_demand_estimate.json"
echo "  $RESULTS_DIR/metrics/processed/epp_throughput.json"
echo "  $RESULTS_DIR/metrics/processed/wva_metrics_timeseries.json"
echo "  $RESULTS_DIR/metrics/graphs/two_variant_v2_full_pipeline.png"
