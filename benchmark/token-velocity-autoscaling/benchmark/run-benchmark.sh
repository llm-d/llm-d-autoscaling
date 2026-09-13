#!/usr/bin/env bash
#
# run-benchmark.sh — benchmark the ALREADY-DEPLOYED P/D stack with llm-d-benchmark.
#
# RUN-ONLY. This never calls `llmdbenchmark standup` or `teardown`, so it cannot
# redeploy or disturb the stack that deploy-pd-guide.sh created. That distinction
# matters: the older deploy-pd-llmdbench.sh in this repo's parent stands up its OWN
# llmdbenchmark-managed stack, and running that against a guide-deployed namespace
# lands two stacks (two EPPs, two InferencePools) in one namespace whose pods share the
# `llm-d.ai/role` labels the PodMonitors select on — after which the decode autoscaling
# trigger sums both stacks and scales on foreign load.
#
# It follows the guide's documented run-only path
# (guides/pd-disaggregation/README.md -> Benchmarking):
#
#     llmdbenchmark --spec guides/pd-disaggregation run \
#         --endpoint-url <EPP clusterIP> --gateway-class epponly ...
#
# `--gateway-class epponly` is what tells the CLI this is a standalone-router stack.
# Without it the CLI re-renders against the scenario's default topology and measures
# something other than what you deployed.
#
# RESULTS LAND INSIDE THIS FOLDER, under ./benchmark-results/<timestamp>/, with a
# `latest` symlink. Nothing is written to ~/data.
#
# WHAT IT ADDS AROUND THE CLI
#
#   * Preflight that fails loudly instead of benchmarking a broken stack: pods Ready,
#     endpoint actually answers /v1/models, workload profile exists, model name read
#     from the live Deployment rather than assumed.
#   * A replica + trigger-value timeline sampled THROUGHOUT the run into
#     autoscaling-timeline.csv. If KEDA is armed, a benchmark scales the fleet mid-run
#     and the throughput numbers describe a moving target; this records exactly when.
#   * `--pause-autoscaling` freezes both ScaledObjects at their current replica count
#     for a fixed-topology baseline, and restores them on exit (even on Ctrl-C).
#   * EPP counter snapshots before and after, asserting llm_d_epp_disagg_decision_total
#     actually increased — proof the load was served through the P/D split rather than
#     aggregated. A benchmark that silently stopped disaggregating still produces
#     plausible-looking latency numbers.
#
# USAGE
#   ./run-benchmark.sh                                  # guide_pd-disaggregation_2.yaml
#   ./run-benchmark.sh -w sanity_random.yaml            # quick plumbing check
#   ./run-benchmark.sh --workload-file workloads/pd-autoscaling-ramp.yaml
#                                                      # a local multi-stage profile
#   ./run-benchmark.sh -w guide_pd-disaggregation_1.yaml  # the guide's saturation profile
#   ./run-benchmark.sh --pause-autoscaling              # fixed-topology baseline
#   ./run-benchmark.sh --list-endpoints                 # what the CLI detects, then exit
#   ./run-benchmark.sh --dry-run
#
# Environment: NAMESPACE RELEASE MODEL WORKLOAD HARNESS SPEC WAIT_TIMEOUT RESULTS_ROOT
#
set -uo pipefail
cd "$(dirname "$0")" || exit 1

NAMESPACE="${NAMESPACE:-pd-test}"
RELEASE="${RELEASE:-pd-disaggregation}"          # helm release => <release>-epp service
SPEC="${SPEC:-guides/pd-disaggregation}"
HARNESS="${HARNESS:-inference-perf}"
WORKLOAD="${WORKLOAD:-guide_pd-disaggregation_2.yaml}"
WORKLOAD_FILE="${WORKLOAD_FILE:-}"   # local profile path; wins over WORKLOAD
MODEL="${MODEL:-}"                               # empty => read from the live Deployment
GATEWAY_CLASS="${GATEWAY_CLASS:-epponly}"        # standalone router (no k8s Gateway)
WAIT_TIMEOUT="${WAIT_TIMEOUT:-1800}"
RESULTS_ROOT="${RESULTS_ROOT:-${PWD}/benchmark-results}"
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-15}"

DO_ANALYZE=true
PAUSE_AUTOSCALING=false
LIST_ONLY=false
DRY_RUN=false

if [[ -t 2 ]]; then B=$'\033[1m'; R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; Z=$'\033[0m'
else B=; R=; G=; Y=; C=; Z=; fi
hdr()  { printf '\n%s══ %s %s\n' "$C$B" "$*" "$Z" >&2; }
ok()   { printf '   %sPASS%s  %s\n' "$G" "$Z" "$*" >&2; }
info() { printf '   %s\n' "$*" >&2; }
warn() { printf '   %sWARN%s  %s\n' "$Y" "$Z" "$*" >&2; }
die()  { printf '\n   %sFAIL%s  %s\n\n' "$R" "$Z" "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -w|--workload)      WORKLOAD="${2:?}"; shift 2 ;;
    --workload-file)    WORKLOAD_FILE="${2:?}"; shift 2 ;;
    -p|--namespace)     NAMESPACE="${2:?}"; shift 2 ;;
    -m|--model)         MODEL="${2:?}"; shift 2 ;;
    -l|--harness)       HARNESS="${2:?}"; shift 2 ;;
    --pause-autoscaling) PAUSE_AUTOSCALING=true; shift ;;
    --list-endpoints)   LIST_ONLY=true; shift ;;
    --no-analyze)       DO_ANALYZE=false; shift ;;
    --dry-run)          DRY_RUN=true; shift ;;
    --timeout)          WAIT_TIMEOUT="${2:?}"; shift 2 ;;
    -h|--help)          sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//' | head -n -1; exit 0 ;;
    *)                  die "unknown flag: $1  (try --help)" ;;
  esac
done

RUN_ID="$(date +%Y%m%d-%H%M%S)"
# A dry run must not leave an empty timestamped directory behind in the results tree —
# those read later as real runs that produced nothing.
if [[ $DRY_RUN == true ]]; then
  RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/pd-bench-dryrun.XXXXXX")"
else
  RUN_DIR="${RESULTS_ROOT}/${RUN_ID}"
fi
TIMELINE="${RUN_DIR}/autoscaling-timeline.csv"
EPP_BEFORE="${RUN_DIR}/epp-metrics-before.txt"
EPP_AFTER="${RUN_DIR}/epp-metrics-after.txt"
mkdir -p "$RUN_DIR"
LOG="${RUN_DIR}/run-benchmark.log"

SAMPLER_PID=""
PAUSED_AT=""

# Restore autoscaling and stop the sampler no matter how we exit — a Ctrl-C during a
# 20-minute benchmark must not leave the ScaledObjects pinned.
cleanup() {
  [[ -n $SAMPLER_PID ]] && kill "$SAMPLER_PID" 2>/dev/null
  if [[ -n $PAUSED_AT ]]; then
    for so in $PAUSED_AT; do
      kubectl annotate scaledobject "$so" -n "$NAMESPACE" \
        autoscaling.keda.sh/paused-replicas- >/dev/null 2>&1 \
        && printf '   restored autoscaling on %s\n' "$so" >&2
    done
  fi
}
trap cleanup EXIT INT TERM

# =========================================================================
preflight() {
  hdr "STAGE 0  preflight"
  for t in kubectl python3; do
    command -v "$t" >/dev/null 2>&1 || die "required tool not found: $t"
  done
  command -v llmdbenchmark >/dev/null 2>&1 || die "llmdbenchmark not found. Install the CLI:
           curl -sSL https://raw.githubusercontent.com/llm-d/llm-d-benchmark/main/install.sh | bash
           cd llm-d-benchmark && source .venv/bin/activate
         It also provides the config/ and workload/ trees this script needs (see --base-dir)."
  ok "tools: llmdbenchmark $(llmdbenchmark --version 2>&1 | tr -d '\n'), kubectl, python3"

  kubectl get ns default >/dev/null 2>&1 || die "not authenticated — run your 'oc login'"
  kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || die "namespace ${NAMESPACE} not found"

  # The spec templates resolve config/ relative to --base-dir. Derive it from the
  # installed package so this works from any cwd; a mistyped base-dir makes the CLI
  # render defaults instead of failing.
  BASE_DIR="${LLMDBENCH_BASE_DIR:-$(python3 - <<'PY'
import os, sys
try:
    import llmdbenchmark
except Exception:
    sys.exit(0)
root = os.path.dirname(os.path.dirname(os.path.abspath(llmdbenchmark.__file__)))
print(root if os.path.isdir(os.path.join(root, "config", "templates", "values")) else "")
PY
)}"
  [[ -n $BASE_DIR ]] || die "cannot locate the llm-d-benchmark checkout.
         Set LLMDBENCH_BASE_DIR=/path/to/llm-d-benchmark and re-run."
  ok "base-dir: ${BASE_DIR}"

  [[ -f "${BASE_DIR}/config/specification/${SPEC}.yaml.j2" ]] \
    || die "spec not found: ${BASE_DIR}/config/specification/${SPEC}.yaml.j2"
  ok "spec: ${SPEC}"

  if [[ -n $WORKLOAD_FILE ]]; then
    [[ -f $WORKLOAD_FILE ]] || die "workload file not found: ${WORKLOAD_FILE}"
    # Absolutize: the CLI resolves this path from its own cwd, not ours.
    WORKLOAD_FILE="$(cd "$(dirname "$WORKLOAD_FILE")" && pwd)/$(basename "$WORKLOAD_FILE")"
    ok "workload file: ${WORKLOAD_FILE}"
    # Print the stage plan and total duration. A multi-stage profile whose own runtime
    # exceeds --wait-timeout gets killed mid-run and yields partial results, so the
    # timeout is raised to cover it rather than left at a default that cannot.
    local plan
    plan=$(python3 - "$WORKLOAD_FILE" <<'PYEOF'
import sys, yaml
d = yaml.safe_load(open(sys.argv[1])) or {}
st = ((d.get("load") or {}).get("stages")) or []
data = d.get("data") or {}
isl = (data.get("input_distribution") or {}).get("mean")
osl = (data.get("output_distribution") or {}).get("mean")
tot = 0
for i, x in enumerate(st, 1):
    dur = int(x.get("duration") or 0); tot += dur
    print("       stage %d: rate=%-5s %4ds" % (i, x.get("rate"), dur))
print("       ISL=%s OSL=%s stages=%d total=%ds (%.0f min)" % (isl, osl, len(st), tot, tot / 60))
print("TOTAL=%d" % tot)
PYEOF
)
    printf '%s\n' "$plan" | grep -v '^TOTAL=' >&2
    local total; total=$(printf '%s' "$plan" | sed -n 's/^TOTAL=//p')
    if [[ -n $total ]] && (( total + 900 > WAIT_TIMEOUT )); then
      warn "profile runs ${total}s but --wait-timeout was ${WAIT_TIMEOUT}s; raising to $(( total + 900 ))s"
      warn "  (a harness killed mid-ramp returns partial results that look like a finished run)"
      WAIT_TIMEOUT=$(( total + 900 ))
    fi
    ok "wait-timeout: ${WAIT_TIMEOUT}s"
  else
    # Shipped profiles exist as either <name> or <name>.in
    local wdir="${BASE_DIR}/workload/profiles/${HARNESS}"
    if [[ ! -f "${wdir}/${WORKLOAD}" && ! -f "${wdir}/${WORKLOAD}.in" ]]; then
      warn "available profiles in ${wdir}:"
      ls "$wdir" 2>/dev/null | sed 's/\.in$//' | sed 's/^/       /' >&2
      die "workload profile not found: ${WORKLOAD}"
    fi
    ok "workload: ${WORKLOAD}"
  fi

  # Model server pods must all be Ready. Benchmarking a half-rolled fleet produces
  # numbers that describe the rollout, not the stack.
  local pods total ready
  pods=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | grep -E 'prefill|decode' || true)
  [[ -n $pods ]] || die "no prefill/decode pods in ${NAMESPACE} — run ./deploy-pd-guide.sh first"
  total=$(wc -l <<<"$pods" | tr -d ' ')
  ready=$(awk '{split($2,a,"/"); if (a[1]==a[2] && a[1]>0) c++} END {print c+0}' <<<"$pods")
  [[ $ready == "$total" ]] || { sed 's/^/       /' <<<"$pods" >&2
    die "only ${ready}/${total} model server pods Ready — wait for the rollout"; }
  ok "${ready}/${total} model server pods Ready"

  # Model name from the live Deployment. Prefer --served-model-name: that is the id
  # the endpoint answers /v1/models with and the harness must request. `vllm serve
  # <model>` is args[0], but under --model-cache args[0] is the local weight path
  # (/model-cache/...) while --served-model-name carries the real id — using args[0]
  # then makes the harness request a model the server does not expose. Fall back to
  # args[0] only when --served-model-name is absent.
  if [[ -z $MODEL ]]; then
    MODEL=$(kubectl get deploy -n "$NAMESPACE" -o json | python3 -c '
import json, sys
for d in json.load(sys.stdin)["items"]:
    for c in d["spec"]["template"]["spec"]["containers"]:
        if c["name"] != "modelserver":
            continue
        args = c.get("args") or []
        toks = (c.get("command") or []) + args
        served = None
        for i, t in enumerate(toks):
            if t == "--served-model-name" and i + 1 < len(toks):
                served = toks[i + 1]; break
            if t.startswith("--served-model-name="):
                served = t.split("=", 1)[1]; break
        if served:
            print(served); raise SystemExit
        if args:
            print(args[0]); raise SystemExit')
    [[ -n $MODEL ]] || die "cannot read the served model from any Deployment; pass --model"
    ok "model (discovered): ${MODEL}"
  else
    ok "model: ${MODEL}"
  fi

  # Sizing note, not a block: the guide's _1 profile targets a 16-GPU fleet.
  local gpus
  gpus=$(kubectl get pods -n "$NAMESPACE" -o json | python3 -c '
import json, sys
t = 0
for p in json.load(sys.stdin)["items"]:
    for c in p["spec"]["containers"]:
        t += int((c.get("resources", {}).get("requests", {}) or {}).get("nvidia.com/gpu", 0) or 0)
print(t)')
  info "GPUs currently serving in ${NAMESPACE}: ${gpus}"
  case "$WORKLOAD" in
    guide_pd-disaggregation_1*)
      (( gpus < 16 )) && {
        warn "${WORKLOAD} drives rate=45 with 45 workers, sized for the guide's 16-GPU"
        warn "  reference fleet. On ${gpus} GPUs it saturates and the latencies describe"
        warn "  queueing rather than capacity."; } ;;
    sanity_random*)
      warn "${WORKLOAD} is a 1 req/s single-worker plumbing check — not a perf result" ;;
  esac
}

# =========================================================================
resolve_endpoint() {
  hdr "STAGE 1  resolve the endpoint (standalone router)"
  local ip
  ip=$(kubectl get service "${RELEASE}-epp" -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)
  [[ -n $ip ]] || die "service ${RELEASE}-epp not found in ${NAMESPACE} (wrong --namespace, or RELEASE=?)"
  ENDPOINT_URL="http://${ip}"
  ok "endpoint: ${ENDPOINT_URL}   gateway-class: ${GATEWAY_CLASS}"

  # Prove it serves before handing it to the harness: the harness failing on a dead
  # endpoint costs a PVC bind plus a pod start before it says so.
  [[ $DRY_RUN == true ]] && return 0
  local got
  got=$(kubectl run "bench-probe-$RANDOM" --rm -i --restart=Never -n "$NAMESPACE" \
          --image=cfmanteiga/alpine-bash-curl-jq --quiet --command -- \
          sh -c "curl -sS --max-time 30 ${ENDPOINT_URL}/v1/models" 2>/dev/null \
        | python3 -c '
import json, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw[raw.index("{"):])
    print(",".join(m["id"] for m in d.get("data", [])))
except Exception:
    print("")')
  [[ -n $got ]] || die "the endpoint did not answer /v1/models — is the EPP healthy?"
  ok "endpoint answers /v1/models: ${got}"
  [[ $got == *"$MODEL"* ]] || warn "endpoint serves '${got}' but the benchmark will request '${MODEL}'"
}

# =========================================================================
autoscaling_state() {
  hdr "STAGE 2  autoscaling state"
  local sos
  sos=$(kubectl get scaledobject -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    items = json.load(sys.stdin)["items"]
except Exception:
    items = []
for it in items:
    print(it["metadata"]["name"])' 2>/dev/null)
  if [[ -z $sos ]]; then
    info "no ScaledObjects — fixed topology for the whole run"
    return 0
  fi
  kubectl get scaledobject -n "$NAMESPACE" --no-headers 2>/dev/null \
    | awk '{printf "     %-24s min=%s max=%s ready=%s active=%s\n",$1,$4,$5,$6,$7}' >&2

  if [[ $PAUSE_AUTOSCALING == true ]]; then
    [[ $DRY_RUN == true ]] && { info "--dry-run: would pause ${sos}"; return 0; }
    for so in $sos; do
      local tgt cur
      tgt=$(kubectl get scaledobject "$so" -n "$NAMESPACE" -o jsonpath='{.spec.scaleTargetRef.name}')
      cur=$(kubectl get deploy "$tgt" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}')
      # KEDA honours this annotation by pinning the target and ignoring its triggers.
      kubectl annotate scaledobject "$so" -n "$NAMESPACE" \
        "autoscaling.keda.sh/paused-replicas=${cur}" --overwrite >/dev/null \
        || die "could not pause ${so}"
      PAUSED_AT="${PAUSED_AT} ${so}"
      ok "paused ${so} at ${cur} replica(s) — restored on exit"
    done
    info "fixed-topology baseline: the fleet will not move during this run"
  else
    warn "autoscaling is ARMED: a benchmark may scale the fleet mid-run, so throughput"
    warn "  numbers describe a moving target. autoscaling-timeline.csv records when."
    warn "  Use --pause-autoscaling for a fixed-topology baseline."
  fi
}

# =========================================================================
# EPP metrics. Enabling the guide's monitoring values sets --metrics-endpoint-auth=false,
# so an unauthenticated scrape works; without it the endpoint answers 401, and an
# unauthenticated read is indistinguishable from "metric absent". Try plain, then token.
scrape_epp() {
  local out="$1" ip tok
  ip=$(kubectl get pods -n "$NAMESPACE" -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if "epp" in p["metadata"]["name"] and p.get("status", {}).get("phase") == "Running":
        print(p["status"]["podIP"]); raise SystemExit')
  [[ -n $ip ]] || { warn "no running EPP pod; skipping snapshot"; : > "$out"; return; }
  tok=$(oc whoami -t 2>/dev/null || true)
  kubectl run "eppscrape-$RANDOM" --rm -i --restart=Never -n "$NAMESPACE" \
     --image=cfmanteiga/alpine-bash-curl-jq --quiet --env="IP=${ip}" --env="TOK=${tok}" --command -- \
     sh -c 'curl -sS --max-time 25 "http://${IP}:9090/metrics" 2>/dev/null \
            || curl -sS --max-time 25 -H "Authorization: Bearer $TOK" "http://${IP}:9090/metrics"' \
     > "$out" 2>/dev/null
  if [[ ! -s $out ]] || grep -qi '^Unauthorized' "$out"; then
    warn "EPP scrape failed ($( [[ -s $out ]] && head -c 40 "$out" || echo empty ))"
    : > "$out"
  fi
}

counter_of() {
  local f="$1" m="$2"
  [[ -s $f ]] || return 0
  awk -v m="^${m}" '$0 ~ m { v=$NF; if (v+0==v) s+=v } END { if (s != "") printf "%.0f", s }' "$f"
}

# =========================================================================
start_sampler() {
  [[ $DRY_RUN == true ]] && return 0
  {
    printf 'elapsed_s,prefill_replicas,prefill_ready,decode_replicas,decode_ready'
    kubectl get scaledobject -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    for it in json.load(sys.stdin)["items"]:
        print(",%s_metric" % it["metadata"]["name"], end="")
except Exception:
    pass'
    printf '\n'
  } > "$TIMELINE"

  (
    local start; start=$(date +%s)
    while :; do
      local row pr pd prr pdr
      pr=$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[?(@.metadata.labels.llm-d\.ai/role=="prefill")]}{.spec.replicas}{end}' 2>/dev/null)
      prr=$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[?(@.metadata.labels.llm-d\.ai/role=="prefill")]}{.status.readyReplicas}{end}' 2>/dev/null)
      pd=$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[?(@.metadata.labels.llm-d\.ai/role=="decode")]}{.spec.replicas}{end}' 2>/dev/null)
      pdr=$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[?(@.metadata.labels.llm-d\.ai/role=="decode")]}{.status.readyReplicas}{end}' 2>/dev/null)
      row="$(( $(date +%s) - start )),${pr:-0},${prr:-0},${pd:-0},${pdr:-0}"
      # Trigger values as KEDA itself reports them — cheaper and more faithful than
      # re-running the PromQL, since this is what the HPA consumed.
      for so in $(kubectl get scaledobject -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null); do
        v=$(kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/${NAMESPACE}/s0-prometheus?labelSelector=scaledobject.keda.sh%2Fname=${so}" 2>/dev/null \
            | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["items"][0]["value"])
except Exception: print("NA")' 2>/dev/null)
        row="${row},${v:-NA}"
      done
      printf '%s\n' "$row" >> "$TIMELINE"
      sleep "$SAMPLE_INTERVAL"
    done
  ) &
  SAMPLER_PID=$!
  ok "sampling replicas + trigger values every ${SAMPLE_INTERVAL}s -> $(basename "$TIMELINE")"
}

# =========================================================================
run_benchmark() {
  hdr "STAGE 3  EPP snapshot (before)"
  if [[ $DRY_RUN == false ]]; then
    scrape_epp "$EPP_BEFORE"
    D_BEFORE=$(counter_of "$EPP_BEFORE" 'llm_d_epp_disagg_decision_total')
    T_BEFORE=$(counter_of "$EPP_BEFORE" 'llm_d_epp_request_total')
    info "disagg_decision_total=${D_BEFORE:-?}  request_total=${T_BEFORE:-?}"
  else
    info "--dry-run: skipped"
  fi

  hdr "STAGE 4  llmdbenchmark run"
  local args=(
    --spec "$SPEC"
    run
    --base-dir "$BASE_DIR"
    --endpoint-url "$ENDPOINT_URL"
    --gateway-class "$GATEWAY_CLASS"
    --model "$MODEL"
    --namespace "$NAMESPACE"
    --harness "$HARNESS"
  )
  if [[ -n $WORKLOAD_FILE ]]; then
    args+=( --workload-file-path "$WORKLOAD_FILE" )
  else
    args+=( --workload "$WORKLOAD" )
  fi
  args+=(
    --workspace "$RUN_DIR"
    --wait-timeout "$WAIT_TIMEOUT"
  )
  [[ $DO_ANALYZE == true ]] && args+=( --analyze )

  info "llmdbenchmark ${args[*]}"
  if [[ $DRY_RUN == true ]]; then
    warn "--dry-run: not running"
    return 0
  fi

  start_sampler
  local rc=0
  llmdbenchmark "${args[@]}" 2>&1 | tee "${RUN_DIR}/llmdbenchmark.log" >&2 || rc=$?
  [[ -n $SAMPLER_PID ]] && { kill "$SAMPLER_PID" 2>/dev/null; SAMPLER_PID=""; }
  if (( rc != 0 )); then
    warn "llmdbenchmark exited ${rc} — results may be partial (see ${RUN_DIR}/llmdbenchmark.log)"
  else
    ok "benchmark finished"
  fi

  hdr "STAGE 5  EPP snapshot (after) — was the load actually disaggregated?"
  scrape_epp "$EPP_AFTER"
  local d_after t_after
  d_after=$(counter_of "$EPP_AFTER" 'llm_d_epp_disagg_decision_total')
  t_after=$(counter_of "$EPP_AFTER" 'llm_d_epp_request_total')
  if [[ -n ${D_BEFORE:-} && -n ${d_after:-} ]]; then
    local delta=$(( d_after - D_BEFORE ))
    if (( delta > 0 )); then
      ok "disagg decisions during the run: +${delta} — P/D split CONFIRMED"
    else
      warn "disagg decisions did NOT increase (${D_BEFORE} -> ${d_after})"
      warn "  the load may have been served WITHOUT disaggregation; latencies would still look fine"
    fi
  else
    warn "could not read llm_d_epp_disagg_decision_total on both sides"
  fi
  [[ -n ${T_BEFORE:-} && -n ${t_after:-} ]] && info "EPP requests during the run: +$(( t_after - T_BEFORE ))"
}

# =========================================================================
report() {
  hdr "STAGE 6  results"
  [[ $DRY_RUN == true ]] && { info "--dry-run: nothing produced"; return 0; }

  ln -sfn "$RUN_DIR" "${RESULTS_ROOT}/latest"
  local n
  n=$(find "$RUN_DIR" -type f | wc -l | tr -d ' ')
  ok "${n} file(s) under ${RUN_DIR}"
  find "$RUN_DIR" -type f \( -name '*.json' -o -name '*.csv' -o -name '*.md' -o -name '*.yaml' \) \
    | head -20 | sed "s|${RUN_DIR}/|       |" >&2

  # Surface the headline numbers rather than leaving them in a JSON tree.
  python3 - "$RUN_DIR" <<'PY' >&2
import json, os, sys
root = sys.argv[1]
wanted = ("ttft", "tpot", "itl", "latency", "throughput", "request_rate", "output_tokens")
found = False
for dirpath, _, files in os.walk(root):
    for f in files:
        if not f.endswith(".json"):
            continue
        p = os.path.join(dirpath, f)
        try:
            d = json.load(open(p))
        except Exception:
            continue
        flat = {}
        def walk(o, pre=""):
            if isinstance(o, dict):
                for k, v in o.items():
                    walk(v, pre + k + ".")
            elif isinstance(o, (int, float)):
                flat[pre[:-1]] = o
        walk(d)
        hits = {k: v for k, v in flat.items() if any(w in k.lower() for w in wanted)}
        if hits:
            found = True
            print("\n   metrics from %s:" % os.path.relpath(p, root))
            for k in sorted(hits)[:18]:
                print("     %-52s %s" % (k, round(hits[k], 4) if isinstance(hits[k], float) else hits[k]))
if not found:
    print("   (no metric JSON parsed; inspect the files above)")
PY

  if [[ -s $TIMELINE ]]; then
    hdr "Autoscaling during the run"
    python3 - "$TIMELINE" <<'PY' >&2
import csv, sys
rows = list(csv.DictReader(open(sys.argv[1])))
if len(rows) < 2:
    print("   too few samples"); raise SystemExit
for col in rows[0]:
    if col == "elapsed_s":
        continue
    vals = [r[col] for r in rows if r.get(col) not in (None, "", "NA")]
    if not vals:
        continue
    try:
        nums = [float(v) for v in vals]
        print("   %-34s min=%-10.4g max=%-10.4g %s" % (
            col, min(nums), max(nums),
            "MOVED" if max(nums) > min(nums) else "flat"))
    except ValueError:
        print("   %-34s %s" % (col, ",".join(sorted(set(vals)))))
PY
    info "full timeline: $(basename "$TIMELINE")"
  fi

  hdr "SUMMARY"
  cat >&2 <<EOF
   namespace / release: ${NAMESPACE} / ${RELEASE}
   model:               ${MODEL}
   workload:            ${WORKLOAD_FILE:-$WORKLOAD}   (harness ${HARNESS})
   endpoint:            ${ENDPOINT_URL}  gateway-class=${GATEWAY_CLASS}
   autoscaling:         $([[ $PAUSE_AUTOSCALING == true ]] && echo "PAUSED (fixed topology)" || echo "armed")
   results:             ${RUN_DIR}
                        ${RESULTS_ROOT}/latest -> ${RUN_ID}
EOF
}

main() {
  preflight
  resolve_endpoint
  if [[ $LIST_ONLY == true ]]; then
    hdr "Endpoints the CLI detects"
    llmdbenchmark --spec "$SPEC" run --base-dir "$BASE_DIR" -p "$NAMESPACE" \
      --endpoint-url "$ENDPOINT_URL" --gateway-class "$GATEWAY_CLASS" \
      --workspace "${RUN_DIR}/endpoints" --list-endpoints 2>&1 | sed 's/^/   /' >&2
    exit 0
  fi
  autoscaling_state
  run_benchmark
  report
}

main 2>&1 | tee "$LOG"
exit "${PIPESTATUS[0]}"
