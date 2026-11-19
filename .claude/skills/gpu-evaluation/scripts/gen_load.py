#!/usr/bin/env python3
"""Write the step 5 traces and inference-perf profiles for one scenario.

Every rate follows from the inputs of benchmark/protocol/5-gpu-evaluation.md §5.2
by the rules of §5.3, so no rate is chosen by hand.

    gen_load.py knee --mix 8192:128 --estimate 2.4 --slo ttft=2,tpot=0.05 \
        --name knee-ref --out DIR
    gen_load.py runs --mix 8192:128 --mu 2.42 --mix2 128:512 --mu2 11.4 \
        --max-replicas 6 [--min-replicas 1] [--stage 300] \
        --slo ttft=2,tpot=0.05 --out DIR

`knee` writes one knee trace and profile (run 1a or 1b). `runs` writes runs 2 to 7
once both knees are measured: <run>.csv (an AzurePublicDataset trace for
inference-perf's trace_replay) and <run>.yaml (its profile), agentic.yaml
(Poisson stages, no trace), and index.json with every schedule and the numbers
it came from. Run 6 (static baseline) replays mixshift.csv with mixshift.yaml.
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
TIMEOUT = 60      # request timeout, s: a stuck request becomes a failure
DRAIN = 240       # grace after the last arrival, s
WORKERS = 4

# conversation_replay distribution means (agentic profile below), for the
# mean prompt length estimate: system + dynamic + input + half the history.
AG = dict(system=4096, dynamic=2048, turns=10, input=1200, output=360)


def mix(spec):
    i, o = spec.split(":")
    return int(i), int(o)


def slo(spec):
    out = {}
    for part in spec.split(","):
        k, v = part.split("=")
        if k not in ("ttft", "tpot"):
            raise SystemExit(f"--slo: unknown limit {k} (ttft, tpot)")
        out[k] = float(v)
    return out


def trace(phases, seed):
    """phases: [(duration s, rate req/s, (isl, osl))] -> [(offset s, isl, osl)]"""
    rng = random.Random(seed)
    rows, t0 = [], 0.0
    for dur, rate, (isl, osl) in phases:
        t = t0
        while rate > 0:
            t += rng.expovariate(rate)
            if t >= t0 + dur:
                break
            rows.append((t, isl, osl))
        t0 += dur
    return rows


def write_trace(path, rows):
    with open(path, "w") as f:
        f.write("TIMESTAMP,ContextTokens,GeneratedTokens\n")
        for off, isl, osl in rows:
            ts = EPOCH + datetime.timedelta(seconds=off)
            f.write(f"{ts.strftime('%Y-%m-%d %H:%M:%S.%f')},{isl},{osl}\n")


def concurrency(max_rate):
    # the worst backlog (max rate x timeout) must fit, or the replay stops being open loop
    return max(512, math.ceil(max_rate * TIMEOUT * 1.5 / WORKERS))


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


def emit(out, name, phases, seed, limits, what, index):
    rows = trace(phases, seed)
    write_trace(out / f"{name}.csv", rows)
    duration = int(sum(d for d, _, _ in phases))
    (out / f"{name}.yaml").write_text(REPLAY.format(
        name=name, what=what, duration=duration, timeout=TIMEOUT, drain=DRAIN,
        workers=WORKERS, conc=concurrency(max(r for _, r, _ in phases)),
        constraints=constraints(limits)))
    index[name] = dict(what=what, seed=seed, duration_s=duration, requests=len(rows),
                       phases=[dict(seconds=d, rate=round(r, 4), mix=f"{i}:{o}")
                               for d, r, (i, o) in phases])
    print(f"{name}: {len(rows)} requests, {duration} s")


def knee(a, out, limits):
    m = mix(a.mix)
    factors = (0.4, 0.7, 1.0, 1.4, 1.9, 2.5)
    phases = [(300, f * a.estimate, m) for f in factors]
    index = {}
    emit(out, a.name, phases, a.seed, limits,
         f"knee, {a.mix}, stages at {factors} x estimate {a.estimate} req/s", index)
    return index


def runs(a, out, limits):
    ref, second = mix(a.mix), mix(a.mix2)
    C = a.max_replicas * a.mu
    T = a.stage
    P = max(600, 2 * T)
    seed = a.seed
    index = {"inputs": dict(mix=a.mix, mu=a.mu, mix2=a.mix2, mu2=a.mu2,
                            max_replicas=a.max_replicas, min_replicas=a.min_replicas,
                            stage_s=T, phase_s=P, C_req_s=round(C, 4), slo=limits)}

    fr = (0.15, 0.3, 0.5, 0.7, 0.9)
    emit(out, "stair", [(T, f * C, ref) for f in fr], seed + 2, limits,
         f"SO-active staircase, {a.mix}, stages at {fr} x C (C = {C:.3f} req/s)", index)

    q = 0.5 * a.max_replicas * min(a.mu, a.mu2)
    emit(out, "mixshift", [(P, q, ref), (P, q, second)], seed + 3, limits,
         f"mix-shift at {q:.3f} req/s, {a.mix} then {a.mix2}; "
         f"need {math.ceil(q / a.mu)} then {math.ceil(q / a.mu2)} replicas", index)

    warm = 1.2 * a.mu
    emit(out, "overload-under", [(P, warm, ref), (P, 0.8 * C, ref)], seed + 4, limits,
         f"overload under the cap: {warm:.3f} then 0.8 x C = {0.8 * C:.3f} req/s", index)
    emit(out, "overload-over", [(P, warm, ref), (P, 1.5 * C, ref)], seed + 5, limits,
         f"overload over the cap: {warm:.3f} then 1.5 x C = {1.5 * C:.3f} req/s", index)

    if a.min_replicas == 0:
        emit(out, "bootstrap", [(P, 0.4 * a.mu, ref)], seed + 7, limits,
             f"scale from zero: {0.4 * a.mu:.3f} req/s", index)

    mean_prompt = AG["system"] + AG["dynamic"] + AG["input"] + \
        (AG["turns"] - 1) / 2 * (AG["input"] + AG["output"])
    cap_tokens = C * ref[0]
    low, high = 0.1 * cap_tokens / mean_prompt, 0.3 * cap_tokens / mean_prompt
    what = (f"agentic: {low:.3f} then {high:.3f} req/s = 0.1 and 0.3 x C x ISL "
            f"({cap_tokens:.0f} prompt tok/s) / estimated mean prompt {mean_prompt:.0f}")
    turn_s = 2 + 2  # about a turn plus a tool pause, s
    (out / "agentic.yaml").write_text(AGENTIC.format(
        what=what, low=round(low, 3), high=round(high, 3), p1=P, p2=2 * P,
        timeout=TIMEOUT, drain=DRAIN, workers=WORKERS, conc=concurrency(high),
        seed=seed + 6, slots=max(64, math.ceil(high * turn_s * 4)),
        constraints=constraints(limits), **AG))
    index["agentic"] = dict(what=what, seed=seed + 6, duration_s=3 * P,
                            phases=[dict(seconds=P, rate=round(low, 3)),
                                    dict(seconds=2 * P, rate=round(high, 3))],
                            mean_prompt_estimate=mean_prompt)
    index["baseline"] = dict(what="static baseline: replays mixshift.csv with mixshift.yaml")
    print(f"agentic: {low:.3f} then {high:.3f} req/s")
    return index


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    k = sub.add_parser("knee")
    k.add_argument("--mix", required=True, help="ISL:OSL")
    k.add_argument("--estimate", required=True, type=float, help="estimated μ_SLO, req/s")
    k.add_argument("--name", default="knee")
    k.add_argument("--seed", type=int, default=1)
    r = sub.add_parser("runs")
    r.add_argument("--mix", required=True, help="reference ISL:OSL")
    r.add_argument("--mu", required=True, type=float, help="μ_SLO of the reference mix, req/s")
    r.add_argument("--mix2", required=True, help="second ISL:OSL")
    r.add_argument("--mu2", required=True, type=float, help="μ_SLO of the second mix, req/s")
    r.add_argument("--max-replicas", required=True, type=int)
    r.add_argument("--min-replicas", type=int, default=1)
    r.add_argument("--stage", type=int, default=300, help="stage length T, s (§5.3)")
    r.add_argument("--seed", type=int, default=100)
    for p in (k, r):
        p.add_argument("--slo", required=True, type=slo, help="ttft=S,tpot=S")
        p.add_argument("--out", required=True, type=Path)
    a = ap.parse_args()
    if a.cmd == "runs" and a.stage < 300:
        ap.error("--stage must be at least 300 s (§5.3)")
    a.out.mkdir(parents=True, exist_ok=True)
    index = (knee if a.cmd == "knee" else runs)(a, a.out, a.slo)
    path = a.out / ("index.json" if a.cmd == "runs" else f"{a.name}.json")
    path.write_text(json.dumps(index, indent=1) + "\n")


if __name__ == "__main__":
    main()
