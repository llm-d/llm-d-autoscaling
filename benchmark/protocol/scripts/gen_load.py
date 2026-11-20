#!/usr/bin/env python3
"""Write the loads of the evaluation run set (steps 3 and 5) for one scenario.

Every rate follows from the inputs of benchmark/protocol/run-set.md §1 by the
rules of §2, so no rate is chosen by hand.

    gen_load.py knee --mix 8192:256|blend --estimate 2.4 --slo ttft=2,tpot=0.05 --out DIR
    gen_load.py runs --mu 8192:256=2.1,2048:2048=0.9,256:4096=0.5 --mu-blend 0.8 \
        --max-replicas 6 [--min-replicas 1] [--stage 300] --spike 105 --quiet 720 \
        --gap 150 --hold 1125 --down-stage 1020 \
        --slo ttft=2,tpot=0.05 --out DIR [--target inference-perf|blis]

The mix set M defaults to llm-d-benchmark's random_* mixes (--mixes overrides it);
the blend draws each request's mix from M with equal weight.

`knee` writes one knee load (run 1), for one mix or the blend. `runs` writes
runs 2 to 10 once every knee is measured, and index.json with every schedule and
the numbers it came from. Both targets get the same schedules:

- inference-perf (step 5): <run>.csv (an AzurePublicDataset trace for
  inference-perf's trace_replay) and <run>.yaml (its profile); agentic.yaml has
  Poisson stages and no trace. Run 6 (static baseline) replays mixshift.csv
  with mixshift.yaml.
- blis (step 3): <run>.yaml, a blis-signal-lab workload spec with one client per
  mix and one absolute-rate lifecycle window per phase. blis draws the arrivals
  itself, from the same piecewise-constant Poisson rates. Run 6 reruns
  mixshift.yaml with the autoscaler off.

Arrivals are Poisson, piecewise-constant in rate, with a fixed seed per run.
Standard library only.
"""

import argparse
import datetime
import json
import math
import random
from pathlib import Path

EPOCH = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)
# A stuck request becomes a failure after the timeout. The timeout is 1.5 x the
# slowest request that still meets the SLO (TTFT + longest OSL x TPOT), at least
# 60 s; the drain after the last arrival is at least as long.
def timeouts(limits, osl_max):
    t = max(60, math.ceil(1.5 * (limits.get("ttft", 2) + osl_max * limits.get("tpot", 0.05))))
    return t, max(240, t)

WORKERS = 4

# conversation_replay distribution means (agentic profile below), for the
# mean prompt length estimate: system + dynamic + input + half the history.
AG = dict(system=4096, dynamic=2048, turns=10, input=1200, output=360)

# llm-d-benchmark workload/profiles/inference-perf/random_{prefill_heavy,symmetrical,decode_heavy}
MIXES = "8192:256,2048:2048,256:4096"


def mix(spec):
    i, o = spec.split(":")
    return int(i), int(o)


def mixes(spec):
    return [mix(m) for m in spec.split(",")]


def label(ms):
    return "blend" if len(ms) > 1 else f"{ms[0][0]}x{ms[0][1]}"


def mus(spec):
    """8192:256=2.1,... -> [((8192, 256), 2.1), ...] in the given order"""
    out = []
    for part in spec.split(","):
        m, v = part.split("=")
        out.append((mix(m), float(v)))
    return out


def slo(spec):
    out = {}
    for part in spec.split(","):
        k, v = part.split("=")
        if k not in ("ttft", "tpot"):
            raise SystemExit(f"--slo: unknown limit {k} (ttft, tpot)")
        out[k] = float(v)
    return out


def trace(phases, seed):
    """phases: [(duration s, rate req/s, [(isl, osl), ...])] -> [(offset s, isl, osl)]

    One Poisson arrival process; each request's mix is drawn uniformly from the
    phase's mixes (a blend), or is the phase's only mix."""
    rng = random.Random(seed)
    rows, t0 = [], 0.0
    for dur, rate, ms in phases:
        t = t0
        while rate > 0:
            t += rng.expovariate(rate)
            if t >= t0 + dur:
                break
            isl, osl = ms[0] if len(ms) == 1 else rng.choice(ms)
            rows.append((t, isl, osl))
        t0 += dur
    return rows


def write_trace(path, rows):
    with open(path, "w") as f:
        f.write("TIMESTAMP,ContextTokens,GeneratedTokens\n")
        for off, isl, osl in rows:
            ts = EPOCH + datetime.timedelta(seconds=off)
            f.write(f"{ts.strftime('%Y-%m-%d %H:%M:%S.%f')},{isl},{osl}\n")


def concurrency(max_rate, timeout):
    # the worst backlog (max rate x timeout) must fit, or the replay stops being open loop
    return max(512, math.ceil(max_rate * timeout * 1.5 / WORKERS))


def constraints(limits):
    return "".join(f"      {k}: {v:g}\n" for k, v in limits.items())


REPORT = """report:
  request_lifecycle:
    summary: true
    per_stage: true
    per_request: true
    per_request_fields:
      request: false
      response: false
      info: true
      response_chunks: false
      computed_metrics: true
  goodput:
    constraints:
{constraints}storage:
  local_storage:
    path: /workspace
"""

SERVER = """api:
  type: completion
  streaming: true
server:
  type: vllm
  model_name: REPLACE_ENV_LLMDBENCH_DEPLOY_CURRENT_MODEL
  base_url: REPLACE_ENV_LLMDBENCH_HARNESS_STACK_ENDPOINT_URL
  ignore_eos: true
tokenizer:
  pretrained_model_name_or_path: REPLACE_ENV_LLMDBENCH_DEPLOY_CURRENT_TOKENIZER
"""

REPLAY = """# step 5 run {name}, written by gen_load.py; do not edit by hand.
# {what}
# Run with --workload-file-path <this file> --dataset /requests/datasets/{name}.csv
load:
  type: trace_replay
  trace:
    file: REPLACE_ENV_LLMDBENCH_RUN_DATASET_DIR/REPLACE_ENV_LLMDBENCH_RUN_DATASET_FILE
    format: AzurePublicDataset
  stages:
    - rate: 1
      duration: {duration}
  request_timeout: {timeout}
  stage_teardown_grace_seconds: {drain}
  num_workers: {workers}
  worker_max_concurrency: {conc}
""" + SERVER + """data:
  type: random
  trace:
    file: REPLACE_ENV_LLMDBENCH_RUN_DATASET_DIR/REPLACE_ENV_LLMDBENCH_RUN_DATASET_FILE
    format: AzurePublicDataset
""" + REPORT

AGENTIC = """# step 5 run agentic, written by gen_load.py; do not edit by hand.
# {what}
# Run with --workload-file-path <this file> (no --dataset: content is generated).
load:
  type: poisson
  interval: 0
  stages:
    - rate: {low}
      duration: {p1}
    - rate: {high}
      duration: {p2}
  request_timeout: {timeout}
  stage_teardown_grace_seconds: {drain}
  num_workers: {workers}
  worker_max_concurrency: {conc}
""" + SERVER + """data:
  type: conversation_replay
  conversation_replay:
    seed: {seed}
    # enough slots that a slot's serial turns never cap the offered rate
    num_conversations: {slots}
    max_model_len: REPLACE_ENV_LLMDBENCH_DEPLOY_CURRENT_MAX_MODEL_LEN
    shared_system_prompt_len: {system}
    dynamic_system_prompt_len: {{type: lognormal, min: 256, max: 8192, mean: {dynamic}, std_dev: 1500}}
    turns_per_conversation: {{type: lognormal, min: 2, max: 30, mean: {turns}, std_dev: 6}}
    input_tokens_per_turn: {{type: lognormal, min: 64, max: 6000, mean: {input}, std_dev: 1000}}
    output_tokens_per_turn: {{type: lognormal, min: 16, max: 4096, mean: {output}, std_dev: 500}}
    tool_call_latency_sec: {{type: lognormal, min: 0, max: 30, mean: 2, std_dev: 3}}
""" + REPORT


def blis_windows(spans, scale=1.0):
    """[(start s, end s, rate req/s)] -> blis lifecycle windows, rate x scale."""
    return "".join(f"        - {{ start_us: {int(a * 1e6)}, end_us: {int(b * 1e6)}, "
                   f"trace_rate: {r * scale:.6g} }}\n" for a, b, r in spans)


def blis_workload(name, what, phases, seed, timeout):
    """One client per mix; a blend phase gives each of its mixes rate / |mixes|, and the
    superposition of those Poisson streams is the blend's one Poisson stream."""
    spans, t0 = {}, 0.0
    for dur, rate, ms in phases:
        for m in ms:
            spans.setdefault(m, []).append((t0, t0 + dur, rate / len(ms)))
        t0 += dur
    clients = "".join(BLIS_CLIENT.format(
        id=f"{i}x{o}", isl=i, osl=o, timeout_us=int(timeout * 1e6), windows=blis_windows(w))
        for (i, o), w in spans.items())
    return BLIS_WORKLOAD.format(name=name, what=what, seed=seed, clients=clients)


def emit(out, name, phases, seed, limits, what, index, target):
    duration = int(sum(d for d, _, _ in phases))
    timeout, drain = timeouts(limits, max(o for _, _, ms in phases for _, o in ms))
    entry = dict(what=what, seed=seed, duration_s=duration)
    if target == "blis":
        (out / f"{name}.yaml").write_text(blis_workload(name, what, phases, seed, timeout))
        entry["expected_requests"] = round(sum(d * r for d, r, _ in phases))
    else:
        rows = trace(phases, seed)
        write_trace(out / f"{name}.csv", rows)
        (out / f"{name}.yaml").write_text(REPLAY.format(
            name=name, what=what, duration=duration, timeout=timeout, drain=drain,
            workers=WORKERS, conc=concurrency(max(r for _, r, _ in phases), timeout),
            constraints=constraints(limits)))
        entry["requests"] = len(rows)
    entry.update(timeout_s=timeout, drain_s=drain,
                 phases=[dict(seconds=d, rate=round(r, 4),
                              mix=",".join(f"{i}:{o}" for i, o in ms))
                         for d, r, ms in phases])
    index[name] = entry
    print(f"{name}: {entry.get('requests', entry.get('expected_requests'))} requests, {duration} s")


BLIS_WORKLOAD = """# step 3 run {name}, written by gen_load.py; do not edit by hand.
# {what}
# Absolute-rate mode (aggregate_rate: 0): each window's trace_rate is that client's req/s.
version: "2"
aggregate_rate: 0
seed: {seed}
clients:
{clients}"""

BLIS_CLIENT = """  - id: "{id}"
    rate_fraction: 1.0
    arrival: {{ process: poisson }}
    input_distribution: {{ type: constant, params: {{ value: {isl} }} }}
    output_distribution: {{ type: constant, params: {{ value: {osl} }} }}
    timeout: {timeout_us}
    lifecycle:
      windows:
{windows}"""

# The agentic sessions in blis: a shared system prompt (prefix group), then
# `turns` rounds whose context accumulates. blis has no per-conversation dynamic
# system prompt and no cap at --max-model-len, so the per-turn lengths are
# truncated to keep the longest context under 32k tokens. The rate is
# sessions per second: requests per second / turns.
BLIS_AGENTIC = """# step 3 run agentic, written by gen_load.py; do not edit by hand.
# {what}
version: "2"
aggregate_rate: 0
seed: {seed}
category: reasoning
clients:
  - id: "agentic"
    rate_fraction: 1.0
    arrival: {{ process: poisson }}
    prefix_group: system
    prefix_length: {system}
    input_distribution: {{ type: gaussian, params: {{ mean: {input}, std_dev: 1000, min: 64, max: 2400 }} }}
    output_distribution: {{ type: gaussian, params: {{ mean: {output}, std_dev: 500, min: 16, max: 640 }} }}
    timeout: {timeout_us}
    reasoning:
      reason_ratio_distribution: {{ type: constant, params: {{ value: 0 }} }}
      multi_turn: {{ max_rounds: {turns}, think_time_us: 2000000, context_growth: accumulate }}
    lifecycle:
      windows:
{windows}"""


def knee(a, out, limits):
    ms = mixes(a.mixes) if a.mix == "blend" else [mix(a.mix)]
    name = a.name or f"knee-{label(ms)}"
    # The knee is a cliff, so one failing stage brackets it and later ones only
    # add timeouts (run-set.md §2). Stages stay 300 s: shorter ones overestimate
    # KV-bound capacity.
    factors = (0.6, 0.8, 1.0, 1.25)
    phases = [(300, f * a.estimate, ms) for f in factors]
    index = {}
    emit(out, name, phases, a.seed, limits,
         f"knee, {label(ms)} ({','.join(f'{i}:{o}' for i, o in ms)}), "
         f"stages at {factors} x estimate {a.estimate} req/s", index, a.target)
    return name, index


def runs(a, out, limits):
    per_mix = mus(a.mu)
    blend = mixes(a.mixes)
    if sorted(m for m, _ in per_mix) != sorted(blend):
        raise SystemExit(f"--mu must give one μ_SLO per mix of the set {a.mixes}")
    mu = a.mu_blend
    C = a.max_replicas * mu
    T = a.stage
    P = max(600, 2 * T)
    seed = a.seed
    index = {"inputs": dict(mixes=a.mixes, mu={f"{i}:{o}": v for (i, o), v in per_mix},
                            mu_blend=mu, max_replicas=a.max_replicas,
                            min_replicas=a.min_replicas, stage_s=T, phase_s=P,
                            C_req_s=round(C, 4), slo=limits, target=a.target)}

    fr = (0.15, 0.3, 0.5, 0.7, 0.9)
    emit(out, "stair", [(T, f * C, blend) for f in fr], seed + 2, limits,
         f"SO-active staircase, blend, stages at {fr} x C (C = {C:.3f} req/s)", index, a.target)

    q = 0.5 * a.max_replicas * min(v for _, v in per_mix)
    need = ", ".join(f"{i}:{o} needs {math.ceil(q / v)}" for (i, o), v in per_mix)
    emit(out, "mixshift", [(P, q, [m]) for m, _ in per_mix], seed + 3, limits,
         f"mix-shift at {q:.3f} req/s, one phase per mix: {need} replicas", index, a.target)

    warm = 1.2 * mu
    emit(out, "overload-under", [(P, warm, blend), (P, 0.8 * C, blend)], seed + 4, limits,
         f"overload under the cap, blend: {warm:.3f} then 0.8 x C = {0.8 * C:.3f} req/s", index, a.target)
    emit(out, "overload-over", [(P, warm, blend), (P, 1.5 * C, blend)], seed + 5, limits,
         f"overload over the cap, blend: {warm:.3f} then 1.5 x C = {1.5 * C:.3f} req/s", index, a.target)

    burst = [(a.quiet, 0.2 * C, blend)] + [(a.spike, 0.7 * C, blend), (a.quiet, 0.2 * C, blend)] * 3
    emit(out, "bursty", burst, seed + 8, limits,
         f"bursty, blend: {a.quiet} s at 0.2 x C = {0.2 * C:.3f} req/s, then 3 x ({a.spike} s at "
         f"0.7 x C = {0.7 * C:.3f} req/s, {a.quiet} s quiet)", index, a.target)

    short = max(30, a.spike // 2)
    spikes = [(300, 0.2 * C, blend)]
    for i in range(6):
        spikes += [(short, 0.7 * C, blend), (a.gap if i < 5 else a.quiet, 0.2 * C, blend)]
    emit(out, "short-spikes", spikes, seed + 9, limits,
         f"short spikes, blend: 300 s at 0.2 x C, then 6 x ({short} s at 0.7 x C = "
         f"{0.7 * C:.3f} req/s, {a.gap} s gap at {0.2 * C:.3f} req/s), last gap {a.quiet} s", index, a.target)

    down = [(a.hold, 0.9 * C, blend)] + [(a.down_stage, f * C, blend) for f in (0.6, 0.3, 0.1)]
    emit(out, "scale-down", down, seed + 10, limits,
         f"scale-down, blend: {a.hold} s at 0.9 x C = {0.9 * C:.3f} req/s, then {a.down_stage} s "
         f"steps at 0.6, 0.3, 0.1 x C; needs "
         + ", ".join(str(math.ceil(f * C / mu)) for f in (0.9, 0.6, 0.3, 0.1)) + " replicas", index,
         a.target)

    if a.min_replicas == 0:
        emit(out, "bootstrap", [(P, 0.4 * mu, blend)], seed + 7, limits,
             f"scale from zero, blend: {0.4 * mu:.3f} req/s", index, a.target)

    mean_prompt = AG["system"] + AG["dynamic"] + AG["input"] + \
        (AG["turns"] - 1) / 2 * (AG["input"] + AG["output"])
    cap_tokens = C * sum(i for i, _ in blend) / len(blend)
    low, high = 0.1 * cap_tokens / mean_prompt, 0.3 * cap_tokens / mean_prompt
    what = (f"agentic: {low:.3f} then {high:.3f} req/s = 0.1 and 0.3 x C x mean ISL of the blend "
            f"({cap_tokens:.0f} prompt tok/s) / estimated mean prompt {mean_prompt:.0f}")
    turn_s = 2 + 2  # about a turn plus a tool pause, s
    ag_timeout, ag_drain = timeouts(limits, 4096)  # output_tokens_per_turn max
    if a.target == "blis":
        sessions = [(0, P, low / AG["turns"]), (P, 3 * P, high / AG["turns"])]
        (out / "agentic.yaml").write_text(BLIS_AGENTIC.format(
            what=what + f"; {AG['turns']} rounds per session", seed=seed + 6,
            timeout_us=int(ag_timeout * 1e6), windows=blis_windows(sessions), **AG))
        baseline = "static baseline: reruns mixshift.yaml with the autoscaler off"
    else:
        (out / "agentic.yaml").write_text(AGENTIC.format(
            what=what, low=round(low, 3), high=round(high, 3), p1=P, p2=2 * P,
            timeout=ag_timeout, drain=ag_drain, workers=WORKERS, conc=concurrency(high, ag_timeout),
            seed=seed + 6, slots=max(64, math.ceil(high * turn_s * 4)),
            constraints=constraints(limits), **AG))
        baseline = "static baseline: replays mixshift.csv with mixshift.yaml"
    index["agentic"] = dict(what=what, seed=seed + 6, duration_s=3 * P,
                            timeout_s=ag_timeout, drain_s=ag_drain,
                            phases=[dict(seconds=P, rate=round(low, 3)),
                                    dict(seconds=2 * P, rate=round(high, 3))],
                            mean_prompt_estimate=mean_prompt)
    index["baseline"] = dict(what=baseline)
    print(f"agentic: {low:.3f} then {high:.3f} req/s")
    return index


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    k = sub.add_parser("knee")
    k.add_argument("--mix", required=True, help="ISL:OSL, or blend")
    k.add_argument("--estimate", required=True, type=float, help="estimated μ_SLO, req/s")
    k.add_argument("--name", help="default knee-<ISL>x<OSL> or knee-blend")
    k.add_argument("--seed", type=int, default=1)
    r = sub.add_parser("runs")
    r.add_argument("--mu", required=True, help="μ_SLO per mix, ISL:OSL=req/s,...; "
                   "the mix-shift phases follow this order")
    r.add_argument("--mu-blend", required=True, type=float, help="μ_SLO of the blend, req/s")
    r.add_argument("--max-replicas", required=True, type=int)
    r.add_argument("--min-replicas", type=int, default=1)
    r.add_argument("--stage", type=int, default=300, help="stage length T, s (run-set.md §2)")
    r.add_argument("--spike", required=True, type=int, help="burst spike length R, s (run-set.md §2)")
    r.add_argument("--quiet", required=True, type=int, help="burst quiet length Q, s (run-set.md §2)")
    r.add_argument("--gap", required=True, type=int, help="short-spike gap G, s (run-set.md §2)")
    r.add_argument("--hold", required=True, type=int, help="scale-down opening hold H, s (run-set.md §2)")
    r.add_argument("--down-stage", required=True, type=int, help="scale-down step length D, s (run-set.md §2)")
    r.add_argument("--seed", type=int, default=100)
    for p in (k, r):
        p.add_argument("--mixes", default=MIXES, help=f"the mix set M (default {MIXES})")
        p.add_argument("--slo", required=True, type=slo, help="ttft=S,tpot=S")
        p.add_argument("--out", required=True, type=Path)
        p.add_argument("--target", choices=("inference-perf", "blis"), default="inference-perf",
                       help="inference-perf traces and profiles (step 5), or blis workloads (step 3)")
    a = ap.parse_args()
    if a.cmd == "runs" and a.stage < 300:
        ap.error("--stage must be at least 300 s (run-set.md §2)")
    if a.cmd == "runs" and (a.spike < 60 or a.quiet < 300 or a.gap < 60):
        ap.error("--spike and --gap must be at least 60 s, --quiet at least 300 s (run-set.md §2)")
    a.out.mkdir(parents=True, exist_ok=True)
    if a.cmd == "knee":
        name, index = knee(a, a.out, a.slo)
        path = a.out / f"{name}.json"
    else:
        index, path = runs(a, a.out, a.slo), a.out / "index.json"
    path.write_text(json.dumps(index, indent=1) + "\n")


if __name__ == "__main__":
    main()
