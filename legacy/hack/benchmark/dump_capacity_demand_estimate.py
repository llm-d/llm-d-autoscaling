#!/usr/bin/env python3
"""Estimate model-level capacity and token demand from raw vLLM and EPP
scrape files in a results dir, and write a timeseries to:

    metrics/processed/capacity_demand_estimate.json

Three demand components are computed (matching the WVA V2 analyzer's
breakdown — see internal/engines/analyzers/saturation_v2/analyzer.go):

  inUse        = Σ_pods (kv_cache_usage_perc · num_gpu_blocks · block_size)
  waitingPods  = Σ_pods (num_requests_waiting · avg_input_tokens)
  eppQueue     = epp_queue_size · avg_input_tokens     (model-level)
  demandTotal  = inUse + waitingPods + eppQueue

Capacity is computed from the same `vllm:cache_config_info` series used by
WVA's totalSupply, summed across all ready pods at each scrape timestamp:

  capacityRaw  = Σ_pods (num_gpu_blocks · block_size)

This deliberately overlaps in scope with `dump_wva_target_timeseries.py`
(which extracts the analyzer's own totalSupply / totalDemand from the
controller log). The two are complementary: this script works on any run
that has raw vLLM + EPP scrapes (no controller log dependency); the other
captures the analyzer's exact decision input. Both panels can render
side by side.

Usage
-----
  python hack/benchmark/dump_capacity_demand_estimate.py \
      <results>/<treatment>_<i> [--avg-input-tokens 4000]
"""
import argparse
import json
import re
from collections import defaultdict
from pathlib import Path


# Regexes for the raw Prometheus-text fields we care about.
KV_USAGE = re.compile(r"^vllm:kv_cache_usage_perc\{[^}]*\}\s+([0-9.eE+-]+)")
NUM_RUNNING = re.compile(r"^vllm:num_requests_running\{[^}]*\}\s+([0-9.eE+-]+)")
NUM_WAITING = re.compile(r"^vllm:num_requests_waiting\{[^}]*\}\s+([0-9.eE+-]+)")
CACHE_CFG = re.compile(
    r'^vllm:cache_config_info\{[^}]*num_gpu_blocks="(?P<blk>\d+)"[^}]*'
    r'block_size="(?P<bs>\d+)"[^}]*\}\s+'
)
CACHE_CFG_ALT = re.compile(
    r'^vllm:cache_config_info\{[^}]*block_size="(?P<bs>\d+)"[^}]*'
    r'num_gpu_blocks="(?P<blk>\d+)"[^}]*\}\s+'
)
EPP_QUEUE_SIZE = re.compile(
    r"^inference_extension_flow_control_queue_size\{[^}]*\}\s+([0-9.eE+-]+)"
)

FILE_RE = re.compile(r"^(?P<pod>.+?)_(?P<ts>\d{10})_metrics\.log$")


def parse_pod_log(path: Path):
    """Returns dict with kvUsage, numRunning, numWaiting, capacityTokens — or None."""
    try:
        text = path.read_text()
    except Exception:
        return None
    if '"object":"error"' in text:
        return None
    out = {}
    for line in text.splitlines():
        if "kvUsage" not in out:
            m = KV_USAGE.match(line)
            if m:
                out["kvUsage"] = float(m.group(1))
                continue
        if "numRunning" not in out:
            m = NUM_RUNNING.match(line)
            if m:
                out["numRunning"] = float(m.group(1))
                continue
        if "numWaiting" not in out:
            m = NUM_WAITING.match(line)
            if m:
                out["numWaiting"] = float(m.group(1))
                continue
        if "capacityTokens" not in out:
            m = CACHE_CFG.match(line) or CACHE_CFG_ALT.match(line)
            if m:
                try:
                    out["capacityTokens"] = int(m.group("blk")) * int(m.group("bs"))
                except (ValueError, TypeError):
                    pass
    return out or None


def parse_epp_log(path: Path):
    """Returns dict with eppQueueSize, or None if metric absent."""
    try:
        text = path.read_text()
    except Exception:
        return None
    for line in text.splitlines():
        m = EPP_QUEUE_SIZE.match(line)
        if m:
            return {"eppQueueSize": float(m.group(1))}
    return None


def is_v2(pod_name: str) -> bool:
    return "-decode-v2-" in pod_name


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir")
    ap.add_argument("--avg-input-tokens", type=float, default=4000.0,
                    help="Average input tokens per request, for converting "
                         "queued-request count to token demand. Default 4000 "
                         "matches prefill_heavy.yaml; pass the workload's "
                         "prompt_tokens for other scenarios.")
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()
    raw = rd / "metrics" / "raw"

    decode_by_ts = defaultdict(list)   # ts -> list of (pod, fields)
    epp_by_ts = {}                     # ts -> eppQueueSize

    for f in sorted(raw.glob("*_metrics.log")):
        m = FILE_RE.match(f.name)
        if not m:
            continue
        ts = int(m.group("ts"))
        pod = m.group("pod")
        if "gaie-epp" in pod:
            ed = parse_epp_log(f)
            if ed:
                epp_by_ts[ts] = ed["eppQueueSize"]
        elif "decode" in pod:
            d = parse_pod_log(f)
            if d:
                decode_by_ts[ts].append((pod, d))

    avg_in = float(args.avg_input_tokens)
    rows = []
    for ts in sorted(set(decode_by_ts) | set(epp_by_ts)):
        pods = decode_by_ts.get(ts, [])

        cap_raw = 0
        in_use = 0.0
        waiting_count = 0.0
        running_count = 0.0
        cap_per_variant = {"primary": 0, "v2": 0}
        in_use_per_variant = {"primary": 0.0, "v2": 0.0}

        for pod, m in pods:
            tag = "v2" if is_v2(pod) else "primary"
            cap = m.get("capacityTokens")
            if cap:
                cap_raw += cap
                cap_per_variant[tag] += cap
                if "kvUsage" in m:
                    iu = m["kvUsage"] * cap
                    in_use += iu
                    in_use_per_variant[tag] += iu
            if "numWaiting" in m:
                waiting_count += m["numWaiting"]
            if "numRunning" in m:
                running_count += m["numRunning"]

        epp_q = epp_by_ts.get(ts, 0.0)
        waiting_tokens = waiting_count * avg_in
        epp_tokens = epp_q * avg_in
        demand_total = in_use + waiting_tokens + epp_tokens

        rows.append({
            "timestamp": ts,
            "readyPods": len(pods),
            "capacityRaw": cap_raw,
            "capacityPrimary": cap_per_variant["primary"],
            "capacityV2": cap_per_variant["v2"],
            "demandInUse": round(in_use, 1),
            "demandInUsePrimary": round(in_use_per_variant["primary"], 1),
            "demandInUseV2": round(in_use_per_variant["v2"], 1),
            "demandWaitingPods": round(waiting_tokens, 1),
            "waitingRequestsCount": int(waiting_count),
            "demandEppQueue": round(epp_tokens, 1),
            "eppQueueSize": int(epp_q),
            "demandTotalEstimate": round(demand_total, 1),
            "runningRequestsCount": int(running_count),
        })

    out = rd / "metrics" / "processed" / "capacity_demand_estimate.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps({
        "avg_input_tokens_assumed": avg_in,
        "samples": rows,
    }, indent=2))
    print(f"Wrote {out} ({len(rows)} snapshots)")


if __name__ == "__main__":
    main()
