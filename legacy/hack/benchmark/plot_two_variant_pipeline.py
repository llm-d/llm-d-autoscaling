#!/usr/bin/env python3
"""Generate the two-variant V2 full-pipeline 5-panel timeseries plot.

Mirrors `two_variant_v2_full_pipeline_v3.png` from biran-20260527-101013-246.
Panels: Replica Count | KV Cache Util (avg per variant) | Requests Running
(sum per variant) | vLLM Requests Waiting (sum per variant) | EPP Queue Metrics.
"""
import argparse
import json
import os
import re
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

import matplotlib.dates as mdates
import matplotlib.pyplot as plt

PRIMARY_COLOR = "#1f77b4"
V2_COLOR = "#d62728"

VLLM_METRICS = {
    "kv": re.compile(r"^vllm:kv_cache_usage_perc\{[^}]*\}\s+([0-9.eE+-]+)"),
    "running": re.compile(r"^vllm:num_requests_running\{[^}]*\}\s+([0-9.eE+-]+)"),
    "waiting": re.compile(r"^vllm:num_requests_waiting\{[^}]*\}\s+([0-9.eE+-]+)"),
}
EPP_METRICS = {
    "fc_queue": re.compile(
        r"^inference_extension_flow_control_queue_size\{[^}]*\}\s+([0-9.eE+-]+)"
    ),
    "pool_avg": re.compile(
        r"^inference_pool_average_queue_size\{[^}]*\}\s+([0-9.eE+-]+)"
    ),
    "per_pod": re.compile(
        r'^inference_pool_per_pod_queue_size\{model_server_pod="([^"]+)"[^}]*\}\s+([0-9.eE+-]+)'
    ),
}

FILE_RE = re.compile(r"^(?P<pod>.+?)_(?P<ts>\d{10})_metrics\.log$")


def parse_pod_log(path: Path):
    """Extract vllm metrics from a single decode pod log. Returns dict or None."""
    try:
        text = path.read_text()
    except Exception:
        return None
    if '"object":"error"' in text:
        return None
    out = {}
    for line in text.splitlines():
        for k, rx in VLLM_METRICS.items():
            if k in out:
                continue
            m = rx.match(line)
            if m:
                out[k] = float(m.group(1))
    return out or None


def parse_epp_log(path: Path):
    try:
        text = path.read_text()
    except Exception:
        return None
    fc = pa = None
    per_pod = defaultdict(float)
    for line in text.splitlines():
        if fc is None:
            m = EPP_METRICS["fc_queue"].match(line)
            if m:
                fc = float(m.group(1))
                continue
        if pa is None:
            m = EPP_METRICS["pool_avg"].match(line)
            if m:
                pa = float(m.group(1))
                continue
        m = EPP_METRICS["per_pod"].match(line)
        if m:
            per_pod[m.group(1)] = float(m.group(2))
    return {
        "fc_queue": fc or 0.0,
        "pool_avg": pa or 0.0,
        "per_pod": dict(per_pod),
    }


def collect(raw_dir: Path):
    decode_series = defaultdict(list)  # ts -> list of (pod, metrics dict)
    epp_series = []  # list of (ts, epp_dict)
    for f in sorted(raw_dir.glob("*_metrics.log")):
        m = FILE_RE.match(f.name)
        if not m:
            continue
        ts = int(m.group("ts"))
        pod = m.group("pod")
        if "gaie-epp" in pod or "router-epp" in pod:
            ed = parse_epp_log(f)
            if ed:
                epp_series.append((ts, ed))
        elif "decode" in pod:
            md = parse_pod_log(f)
            if md is None:
                continue
            decode_series[ts].append((pod, md))
    return decode_series, epp_series


def is_v2(pod_name: str) -> bool:
    return "-decode-v2-" in pod_name


def aggregate_decode(decode_series):
    """Per timestamp: avg KV (per variant), sum running, sum waiting."""
    rows = []
    for ts in sorted(decode_series.keys()):
        kvs = {"primary": [], "v2": []}
        runs = {"primary": 0.0, "v2": 0.0}
        waits = {"primary": 0.0, "v2": 0.0}
        for pod, m in decode_series[ts]:
            tag = "v2" if is_v2(pod) else "primary"
            if "kv" in m:
                kvs[tag].append(m["kv"])
            if "running" in m:
                runs[tag] += m["running"]
            if "waiting" in m:
                waits[tag] += m["waiting"]
        rows.append({
            "ts": ts,
            "kv_primary": (sum(kvs["primary"]) / len(kvs["primary"]) * 100.0)
                if kvs["primary"] else None,
            "kv_v2": (sum(kvs["v2"]) / len(kvs["v2"]) * 100.0)
                if kvs["v2"] else None,
            "run_primary": runs["primary"],
            "run_v2": runs["v2"],
            "wait_primary": waits["primary"],
            "wait_v2": waits["v2"],
        })
    return rows


def epp_panels(epp_series):
    rows = []
    for ts, ed in sorted(epp_series, key=lambda x: x[0]):
        per_pod = ed["per_pod"]
        sum_p = sum(v for k, v in per_pod.items() if not is_v2(k))
        sum_v = sum(v for k, v in per_pod.items() if is_v2(k))
        rows.append({
            "ts": ts,
            "fc_queue": ed["fc_queue"],
            "pool_avg": ed["pool_avg"],
            "per_pod_primary": sum_p,
            "per_pod_v2": sum_v,
        })
    return rows


def replica_timeseries(results_dir: Path):
    p = results_dir / "metrics" / "processed" / "replica_status_timeseries.json"
    snaps = json.loads(p.read_text())["snapshots"]
    out = []
    for s in snaps:
        ts = int(datetime.fromisoformat(s["timestamp"].replace("Z", "+00:00")).timestamp())
        prim = v2 = 0
        for c in s["controllers"]:
            if c["name"].endswith("-v2"):
                v2 = c["ready_replicas"]
            else:
                prim = c["ready_replicas"]
        out.append((ts, prim, v2))
    return out


def wva_target_timeseries(results_dir: Path):
    """Optional overlay: WVA's per-variant target decisions. Returns [] if not present."""
    p = results_dir / "metrics" / "processed" / "wva_target_timeseries.json"
    if not p.is_file():
        return []
    samples = json.loads(p.read_text()).get("samples", [])
    return [(int(s["timestamp"]), s.get("primary"), s.get("v2")) for s in samples]


def wva_supply_demand_timeseries(results_dir: Path):
    """WVA-side analyzer numbers (totalSupply/Demand etc.). Returns [] if absent.
    Output rows: (ts, supply, demand, util, required, spare).
    """
    p = results_dir / "metrics" / "processed" / "wva_target_timeseries.json"
    if not p.is_file():
        return []
    samples = json.loads(p.read_text()).get("samples", [])
    rows = []
    for s in samples:
        if s.get("totalSupply") is None and s.get("totalDemand") is None:
            continue
        rows.append((
            int(s["timestamp"]),
            s.get("totalSupply"),
            s.get("totalDemand"),
            s.get("utilization"),
            s.get("requiredCapacity"),
            s.get("spareCapacity"),
        ))
    return rows


def capacity_demand_estimate(results_dir: Path):
    """Estimated capacity & 3-component demand from raw vLLM/EPP scrapes.
    Returns [] if not present."""
    p = results_dir / "metrics" / "processed" / "capacity_demand_estimate.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def epp_throughput(results_dir: Path):
    """Per-window request and token rates derived from EPP counters."""
    p = results_dir / "metrics" / "processed" / "epp_throughput.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def wva_metrics_per_variant(results_dir: Path):
    """Per-variant WVA Prometheus metrics over time. Returns:
       [{ts, primary: {metric_name: value, ...}, v2: {...}, _model: {...}}, ...]"""
    p = results_dir / "metrics" / "processed" / "wva_metrics_timeseries.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def to_dt(ts):
    return datetime.fromtimestamp(ts, tz=timezone.utc)


def plot(results_dir: Path, out_path: Path, title_suffix: str):
    decode_series, epp_series = collect(results_dir / "metrics" / "raw")
    drows = aggregate_decode(decode_series)
    erows = epp_panels(epp_series)
    repls = replica_timeseries(results_dir)
    wva_targets = wva_target_timeseries(results_dir)
    wva_sd = wva_supply_demand_timeseries(results_dir)
    cd_est = capacity_demand_estimate(results_dir)

    has_supply_demand = bool(wva_sd or cd_est)
    epp_rates = epp_throughput(results_dir)
    has_rates = bool(epp_rates)
    wva_full = wva_metrics_per_variant(results_dir)
    has_wva_full = bool(wva_full)
    # Skip the EPP-queue panel when it carries no real data (e.g. on v0.7.0
    # the renamed router-epp isn't scraped by the gaie-keyed collector), so the
    # plot doesn't show a null panel.
    has_epp = bool(erows) and any(
        (r.get("fc_queue") or r.get("pool_avg")
         or r.get("per_pod_primary") or r.get("per_pod_v2")) for r in erows)
    epp_shift = 0 if has_epp else 1
    n_extra = (1 if has_supply_demand else 0) + (1 if has_rates else 0) + (2 if has_wva_full else 0)
    n_panels = (5 if has_epp else 4) + n_extra
    fig, axes = plt.subplots(
        n_panels, 1,
        figsize=(8, 11 + 2 * n_extra),
        sharex=True,
    )
    # Panel offset for the original 5 panels: increment for each optional panel inserted before them.
    base = 1 if has_supply_demand else 0

    # 1. Replica Count (actual ready) + optional overlay of WVA target decisions
    ax = axes[0]
    title = "Replica Count"
    if wva_targets:
        title += " — solid: ready,  dashed: WVA desired"
    ax.set_title(title)
    if repls:
        x = [to_dt(r[0]) for r in repls]
        ax.step(x, [r[1] for r in repls], where="post", color=PRIMARY_COLOR, label="primary (ready)", linewidth=2)
        ax.step(x, [r[2] for r in repls], where="post", color=V2_COLOR, label="v2 (ready)", linewidth=2)
    if wva_targets:
        xt = [to_dt(t[0]) for t in wva_targets]
        prim_t = [t[1] for t in wva_targets]
        v2_t = [t[2] for t in wva_targets]
        ax.step(xt, prim_t, where="post", color=PRIMARY_COLOR, linestyle="--", linewidth=1.4,
                label="primary (WVA target)", alpha=0.8)
        ax.step(xt, v2_t, where="post", color=V2_COLOR, linestyle="--", linewidth=1.4,
                label="v2 (WVA target)", alpha=0.8)
    ax.set_ylabel("Replicas")
    ax.legend(loc="upper left", fontsize=7)
    ax.grid(alpha=0.3)

    # 1b. (Optional) Estimated Capacity & Demand — tokens
    # Stacked bars per scrape snapshot show the 3-component demand
    # decomposition (in-use / vLLM waiting / EPP queue). Capacity is a
    # step line on top — bars exceeding it indicate over-saturation.
    # WVA-analyzer numbers from the controller log overlay as markers
    # when present (typically a sparse subset of reconciles).
    if has_supply_demand:
        ax = axes[1]
        ax.set_title("Estimated Demand (stacked) vs Capacity  "
                     "[bars from raw vLLM+EPP scrapes; ●  = WVA analyzer]")
        if cd_est:
            xs = [to_dt(r["timestamp"]) for r in cd_est]
            in_use = [r["demandInUse"] for r in cd_est]
            waiting = [r["demandWaitingPods"] for r in cd_est]
            eppq = [r["demandEppQueue"] for r in cd_est]
            cap = [r["capacityRaw"] for r in cd_est]
            # Bar width based on sample cadence (matplotlib date units = days).
            if len(xs) >= 2:
                interval_sec = max(
                    (cd_est[i + 1]["timestamp"] - cd_est[i]["timestamp"])
                    for i in range(len(cd_est) - 1)
                ) or 30
                width_days = (interval_sec * 0.9) / 86400.0
            else:
                width_days = 15.0 / 86400.0
            base_lower = [0.0] * len(xs)
            base_mid = [a + b for a, b in zip(base_lower, in_use)]
            base_top = [a + b for a, b in zip(base_mid, waiting)]
            ax.bar(xs, in_use, width=width_days, bottom=base_lower,
                   color="#1f77b4", edgecolor="none",
                   label="in-use (KV occupancy)")
            ax.bar(xs, waiting, width=width_days, bottom=base_mid,
                   color="#ff7f0e", edgecolor="none",
                   label="+ vLLM waiting queue")
            ax.bar(xs, eppq, width=width_days, bottom=base_top,
                   color="#d62728", edgecolor="none",
                   label="+ EPP queue (gateway)")
            ax.step(xs, cap, where="post", color="black", linewidth=2,
                    label="capacity (Σ num_gpu_blocks·block_size)")
        if wva_sd:
            xs_sup = [to_dt(r[0]) for r in wva_sd if r[1] is not None]
            sup = [r[1] for r in wva_sd if r[1] is not None]
            xs_dem = [to_dt(r[0]) for r in wva_sd if r[2] is not None]
            dem = [r[2] for r in wva_sd if r[2] is not None]
            if sup:
                ax.scatter(xs_sup, sup, color="black", marker="o", s=24, zorder=5,
                           label="WVA totalSupply")
            if dem:
                ax.scatter(xs_dem, dem, edgecolor="black", facecolor="#d62728",
                           marker="o", s=24, linewidths=0.6, zorder=5,
                           label="WVA totalDemand")
        ax.set_ylabel("Tokens")
        ax.legend(loc="upper left", fontsize=7, ncol=2)
        ax.grid(alpha=0.3, axis="y")
        ax.ticklabel_format(axis="y", style="sci", scilimits=(0, 0))

    # 2. KV Cache Utilization
    ax = axes[1 + base]
    ax.set_title("KV Cache Utilization (avg per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["kv_primary"] for r in drows], color=PRIMARY_COLOR, label="primary")
        ax.plot(x, [r["kv_v2"] for r in drows], color=V2_COLOR, label="v2")
    ax.set_ylabel("KV %")
    ax.set_ylim(0, 100)
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(alpha=0.3)

    # 3. Requests Running
    ax = axes[2 + base]
    ax.set_title("Requests Running (sum per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["run_primary"] for r in drows], color=PRIMARY_COLOR, label="primary")
        ax.plot(x, [r["run_v2"] for r in drows], color=V2_COLOR, label="v2")
    ax.set_ylabel("Running")
    ax.legend(loc="upper left", fontsize=8)
    ax.grid(alpha=0.3)

    # 4. Requests Waiting
    ax = axes[3 + base]
    ax.set_title("vLLM Requests Waiting (sum per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["wait_primary"] for r in drows], color=PRIMARY_COLOR, label="primary")
        ax.plot(x, [r["wait_v2"] for r in drows], color=V2_COLOR, label="v2")
    ax.set_ylabel("Waiting")
    ax.legend(loc="upper left", fontsize=8)
    ax.grid(alpha=0.3)

    # 5. EPP Queue (skipped entirely when it has no real data)
    if has_epp:
        ax = axes[4 + base]
        ax.set_title("EPP Queue Metrics (single y-axis, all in same units)")
        x = [to_dt(r["ts"]) for r in erows]
        ax.plot(x, [r["fc_queue"] for r in erows], color="black", label="flow_control_queue (gateway)")
        ax.plot(x, [r["pool_avg"] for r in erows], color="orange", label="pool_average_queue", alpha=0.8)
        ax.plot(x, [r["per_pod_primary"] for r in erows], color=PRIMARY_COLOR, linestyle="--", label="per pod sum: primary")
        ax.plot(x, [r["per_pod_v2"] for r in erows], color=V2_COLOR, linestyle="--", label="per pod sum: v2")
        ax.set_ylabel("Requests in queue")
        ax.legend(loc="upper left", fontsize=7)
        ax.grid(alpha=0.3)

    # 6. (Optional) Request rate from EPP counters
    if has_rates:
        ax = axes[5 + base - epp_shift]
        ax.set_title("Gateway throughput  (EPP counters → finite-difference rate)")
        x = [to_dt(s["timestamp"]) for s in epp_rates]
        rps = [s.get("rates", {}).get("request_total_per_s", 0.0) for s in epp_rates]
        err_ps = [s.get("rates", {}).get("request_error_total_per_s", 0.0) for s in epp_rates]
        ax.plot(x, rps, color="black", linewidth=2, label="requests/s (offered)")
        if any(v for v in err_ps if v):
            ax.plot(x, err_ps, color="#d62728", linewidth=1.4, label="errors/s")
        ax.set_ylabel("req / s")
        ax.legend(loc="upper left", fontsize=7)
        ax.grid(alpha=0.3)

    # 7. + 8. (Optional) WVA-analyzer per-variant metrics — only when the
    # patched harness scraped the WVA controller during the run.
    if has_wva_full:
        x_wva = [to_dt(s["timestamp"]) for s in wva_full]

        # Panel 7: per-variant wva_saturation_utilization (the analyzer's own
        # internal "how loaded is each variant" reading; differs from the
        # KV-only panel because it folds in queue contributions and uses the
        # capacity-weighted formula).
        ax = axes[5 + base + (1 if has_rates else 0) - epp_shift]
        ax.set_title(
            "WVA Saturation Utilization  (per variant, analyzer-internal)")
        sat_pri = [s.get("primary", {}).get("wva_saturation_utilization") for s in wva_full]
        sat_v2  = [s.get("v2", {}).get("wva_saturation_utilization") for s in wva_full]
        ax.plot(x_wva, sat_pri, color=PRIMARY_COLOR, label="primary", linewidth=2)
        ax.plot(x_wva, sat_v2,  color=V2_COLOR,      label="v2",      linewidth=2)
        # Reference lines from the saturation config: 0.85 scale-up, 0.70 scale-down
        ax.axhline(0.85, color="black", linestyle=":", linewidth=0.8, alpha=0.6)
        ax.axhline(0.70, color="black", linestyle=":", linewidth=0.8, alpha=0.6)
        ax.text(x_wva[-1] if x_wva else 0, 0.85, " 0.85 scaleUp",
                fontsize=7, va="center")
        ax.text(x_wva[-1] if x_wva else 0, 0.70, " 0.70 scaleDown",
                fontsize=7, va="center")
        ax.set_ylabel("utilization")
        ax.legend(loc="upper left", fontsize=7)
        ax.grid(alpha=0.3)
        ax.set_ylim(bottom=0)

        # Panel 8: per-variant tokens used vs capacity (analyzer view).
        # Solid = wva_kv_cache_tokens_used, dashed = wva_kv_cache_tokens_capacity.
        ax = axes[6 + base + (1 if has_rates else 0) - epp_shift]
        ax.set_title("WVA KV Tokens In Use vs Capacity  (per variant)")
        used_pri = [s.get("primary", {}).get("wva_kv_cache_tokens_used") for s in wva_full]
        cap_pri  = [s.get("primary", {}).get("wva_kv_cache_tokens_capacity") for s in wva_full]
        used_v2  = [s.get("v2", {}).get("wva_kv_cache_tokens_used") for s in wva_full]
        cap_v2   = [s.get("v2", {}).get("wva_kv_cache_tokens_capacity") for s in wva_full]
        ax.plot(x_wva, used_pri, color=PRIMARY_COLOR, label="primary used",     linewidth=2)
        ax.plot(x_wva, cap_pri,  color=PRIMARY_COLOR, label="primary capacity",
                linewidth=1.2, linestyle="--", alpha=0.7)
        ax.plot(x_wva, used_v2,  color=V2_COLOR,      label="v2 used",          linewidth=2)
        ax.plot(x_wva, cap_v2,   color=V2_COLOR,      label="v2 capacity",
                linewidth=1.2, linestyle="--", alpha=0.7)
        ax.set_ylabel("tokens")
        ax.legend(loc="upper left", fontsize=7, ncol=2)
        ax.grid(alpha=0.3)
        ax.ticklabel_format(axis="y", style="sci", scilimits=(0, 0))

    # Bound the x-axis to the active window (load + scale-down), clipping the
    # dead/zero tail after collection so the load isn't squished into the left.
    act = [r["timestamp"] for r in cd_est] if cd_est else []
    if repls:
        act += [t[0] for t in repls if (t[1] or 0) > 1 or (t[2] or 0) > 1]
    if act:
        lo, hi = min(act), max(act)
        span = max(hi - lo, 60)
        axes[-1].set_xlim(to_dt(lo - span * 0.03), to_dt(hi + span * 0.05))

    axes[-1].set_xlabel("Time (UTC)")
    axes[-1].xaxis.set_major_formatter(mdates.DateFormatter("%H:%M", tz=timezone.utc))

    final_prim = repls[-1][1] if repls else 0
    final_v2 = repls[-1][2] if repls else 0
    fig.suptitle(
        f"Two-Variant V2 — FULL PIPELINE {title_suffix}\n"
        f"primary={final_prim}, v2={final_v2}  cost-aware",
        fontsize=10,
    )
    fig.tight_layout(rect=[0, 0, 1, 0.97])
    fig.savefig(out_path, dpi=120)
    print(f"Wrote {out_path}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("--name", default="two_variant_v2_full_pipeline.png")
    ap.add_argument("--suffix", default="")
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()
    out = rd / "metrics" / "graphs" / args.name
    out.parent.mkdir(parents=True, exist_ok=True)
    plot(rd, out, args.suffix)


if __name__ == "__main__":
    main()
