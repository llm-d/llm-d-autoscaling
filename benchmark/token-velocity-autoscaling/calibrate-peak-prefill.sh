#!/usr/bin/env bash
#
# calibrate-peak-prefill.sh — measure peakPrefillThroughput with the UPSTREAM
#                             calibrate.sh, under conditions that make the
#                             number mean something.
#
#   guide:  https://github.com/llm-d/llm-d/blob/main/guides/recipes/router/calibration/README.md
#   tool:   llm-d/guides/recipes/router/calibration/calibrate.sh   (called, never modified)
#
# WHAT UPSTREAM DOES
#   Runs a Job that sends NUM_WARMUP + NUM_MEASUREMENTS requests of exactly
#   CHUNK_SIZE random token IDs through the EPP, records TTFT, and prints
#
#       peakPrefillThroughput = CHUNK_SIZE / median(TTFT)
#
#   It measures and prints. It changes nothing.
#
# WHY THIS WRAPPER EXISTS
#
# That formula is a ratio of two numbers the operator supplies implicitly, and
# each has a way of being silently wrong:
#
#   1. CHUNK_SIZE MUST EQUAL vLLM's --max-num-batched-tokens. Upstream documents
#      this and then defaults to 8192 with no check. The pd-disaggregation guide
#      never sets the flag, so the effective value comes from vLLM's own default
#      and can move with the image version. If CHUNK_SIZE exceeds it, vLLM splits
#      the prompt across scheduler steps and the measured TTFT covers several
#      prefill passes — the throughput comes out LOW and looks plausible. This
#      script reads the effective value out of every prefill pod's own startup
#      log and refuses to run on a mismatch.
#
#   2. THE STACK MUST BE IDLE. TTFT includes queue wait. Any concurrent traffic
#      inflates it, which understates throughput — again plausibly. This script
#      asserts vllm:num_requests_running + num_requests_waiting == 0 on every
#      model-server pod before it starts, and re-checks after.
#
#   3. ONE RUN IS NOT A MEASUREMENT. Upstream takes a median of 20 requests in a
#      single Job, which captures request-to-request noise but not run-to-run
#      drift (compile caches warming, other tenants on the node, clock-throttle).
#      This script runs the Job REPEATS times and reports the spread, so you can
#      see whether the number is stable before you hardcode it in a config.
#
# It also compares the result against the two numbers already in play: the value
# your EPP is running right now (read from its live ConfigMap) and the upstream
# configuration-matrix reference for this exact (model, accelerator, TP, chunk).
#
# USAGE
#   ./calibrate-peak-prefill.sh                  # measure, 3 repeats, change nothing
#   ./calibrate-peak-prefill.sh --repeats 5
#   ./calibrate-peak-prefill.sh --apply          # + patch the EPP and verify it took
#   ./calibrate-peak-prefill.sh --dry-run        # preflight + asserts only
#   ./calibrate-peak-prefill.sh --allow-load     # measure anyway on a busy stack
#   ./calibrate-peak-prefill.sh --chunk-size N   # override, with a loud warning
#
# Environment overrides:
#   NAMESPACE GUIDE_NAME MODEL_NAME REPEATS NUM_WARMUP NUM_MEASUREMENTS T_MAX_SECONDS
#
set -uo pipefail
cd "$(dirname "$0")" || exit 1

NAMESPACE="${NAMESPACE:-pd-test}"
GUIDE_NAME="${GUIDE_NAME:-pd-disaggregation}"
# Where the llm-d checkout lives. Overridable so the folder can be dropped next to an
# existing checkout instead of cloning a second copy: LLMD_DIR=/path/to/llm-d ./calibrate-peak-prefill.sh
# Default: ./llm-d inside this folder, falling back to ../llm-d when that already exists
# (the layout in the repo this folder was extracted from).
if [[ -z ${LLMD_DIR:-} ]]; then
  if [[ -d "${PWD}/llm-d/.git" ]]; then LLMD_DIR="${PWD}/llm-d"
  elif [[ -d "${PWD}/../llm-d/.git" ]]; then LLMD_DIR="$(cd "${PWD}/../llm-d" && pwd)"
  else LLMD_DIR="${PWD}/llm-d"; fi
fi
CAL_DIR="${LLMD_DIR}/guides/recipes/router/calibration"

MODEL_NAME="${MODEL_NAME:-}"        # empty => discovered from the live pod
CHUNK_SIZE="${CHUNK_SIZE:-}"        # empty => discovered from the live pod
REPEATS="${REPEATS:-3}"
NUM_WARMUP="${NUM_WARMUP:-5}"
NUM_MEASUREMENTS="${NUM_MEASUREMENTS:-20}"
T_MAX_SECONDS="${T_MAX_SECONDS:-18}"

# Upstream's configuration-matrix reference for this deployment's exact path
# (gpu/vllm, Qwen3-32B, H100 80GB, TP=2, chunk 8192) — also the plugin default.
MATRIX_REFERENCE="${MATRIX_REFERENCE:-15928}"

WORKDIR="${WORKDIR:-${PWD}/.pd-guide-workspace/calibration}"
DRY_RUN=false; APPLY=false; ALLOW_LOAD=false; FORCED_CHUNK=false

if [[ -t 2 ]]; then B=$'\033[1m'; R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; Z=$'\033[0m'
else B=; R=; G=; Y=; C=; Z=; fi
stage() { printf '\n%s══ %s %s\n' "$C$B" "$*" "$Z" >&2; }
info()  { printf '   %s\n' "$*" >&2; }
ok()    { printf '   %sPASS%s  %s\n' "$G" "$Z" "$*" >&2; }
warn()  { printf '   %sWARN%s  %s\n' "$Y" "$Z" "$*" >&2; }
die()   { printf '\n   %sFAIL%s  %s\n\n' "$R" "$Z" "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)     DRY_RUN=true; shift ;;
    --apply)       APPLY=true; shift ;;
    --allow-load)  ALLOW_LOAD=true; shift ;;
    --repeats)     REPEATS="${2:?}"; shift 2 ;;
    --chunk-size)  CHUNK_SIZE="${2:?}"; FORCED_CHUNK=true; shift 2 ;;
    --namespace|-n) NAMESPACE="${2:?}"; shift 2 ;;
    --guide)       GUIDE_NAME="${2:?}"; shift 2 ;;
    -h|--help)     sed -n '2,/^set -uo/{/^set -uo/!p;}' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             die "unknown argument: $1  (try --help)" ;;
  esac
done

mkdir -p "$WORKDIR"
RUN_ID="$(date +%Y%m%d-%H%M%S)"
LOG="${WORKDIR}/calibrate.${RUN_ID}.log"

# =========================================================================
# STAGE 0 — preflight
# =========================================================================
preflight() {
  stage "STAGE 0  preflight"

  for t in kubectl envsubst python3; do
    command -v "$t" >/dev/null 2>&1 \
      || die "missing required tool: $t $([[ $t == envsubst ]] && echo '(brew install gettext)')"
  done
  ok "tools present: kubectl envsubst python3"

  [[ -f "${CAL_DIR}/calibrate.sh" ]] \
    || die "upstream calibrate.sh not found at ${CAL_DIR}/calibrate.sh — run ./deploy-pd-guide.sh first"
  # Upstream tracks calibrate.sh as mode 100644 (its sibling
  # calibrate-min-cached-token-delta.sh is 100755), so the README's own
  # `./calibrate.sh` fails with "permission denied" on a fresh clone. Invoke it
  # through bash rather than chmod-ing someone else's checkout.
  [[ -x "${CAL_DIR}/calibrate.sh" ]] \
    || warn "upstream calibrate.sh is not executable (mode $(ls -l "${CAL_DIR}/calibrate.sh" | cut -c1-10)); invoking it via bash"
  [[ -f "${CAL_DIR}/calibration-peak-throughput.yaml" ]] \
    || die "upstream Job template missing at ${CAL_DIR}/calibration-peak-throughput.yaml"
  info "upstream tool: $(cd "$LLMD_DIR" && git log -1 --format='%h %ad' --date=short 2>/dev/null) ${CAL_DIR#$PWD/}/calibrate.sh"

  kubectl auth whoami >/dev/null 2>&1 || die "not authenticated to the cluster. Run your 'oc login ...'."
  kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || die "namespace ${NAMESPACE} does not exist"

  # The Job talks to the EPP service, so the whole P/D path must be serving —
  # this measures throughput THROUGH the router, not against a bare vLLM.
  local epp_ip epp_port
  epp_ip=$(kubectl get service "${GUIDE_NAME}-epp" -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)
  [[ -n $epp_ip ]] || die "service ${GUIDE_NAME}-epp not found in ${NAMESPACE} — is the guide deployed?"
  epp_port=$(kubectl get service "${GUIDE_NAME}-epp" -n "$NAMESPACE" \
              -o jsonpath='{.spec.ports[?(@.name=="http")].port}' 2>/dev/null)
  [[ -n $epp_port ]] || die "service ${GUIDE_NAME}-epp has no port named 'http' — upstream auto-discovery needs it"
  ok "EPP endpoint: http://${epp_ip}:${epp_port}"
  ok "$(kubectl get deploy -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}={.status.readyReplicas}/{.spec.replicas} {end}')"
}

# =========================================================================
# STAGE 1 — discover the truth from the live pods
# =========================================================================
PREFILL_PODS=(); DECODE_PODS=()
discover() {
  stage "STAGE 1  discover the live serving configuration"

  # Portable instead of `mapfile`, which needs bash >= 4 (macOS still ships 3.2 as
  # /bin/bash, where the array would come back empty and this would misreport "no pods").
  PREFILL_PODS=(); DECODE_PODS=()
  while IFS= read -r _p; do [[ -n $_p ]] && PREFILL_PODS+=("$_p"); done < <(
    kubectl get pods -n "$NAMESPACE" -l llm-d.ai/role=prefill \
      --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
  while IFS= read -r _p; do [[ -n $_p ]] && DECODE_PODS+=("$_p"); done < <(
    kubectl get pods -n "$NAMESPACE" -l llm-d.ai/role=decode \
      --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
  [[ ${#PREFILL_PODS[@]} -gt 0 ]] || die "no running prefill pods in ${NAMESPACE}"
  [[ ${#DECODE_PODS[@]}  -gt 0 ]] || die "no running decode pods in ${NAMESPACE}"
  info "prefill pods: ${#PREFILL_PODS[@]}  decode pods: ${#DECODE_PODS[@]}"

  # --- effective --max-num-batched-tokens, read from each pod's own startup log.
  # vLLM logs "Chunked prefill is enabled with max_num_batched_tokens=N". This is
  # the ground truth: the guide sets no flag, so the value is vLLM's default and
  # travels with the image tag.
  local chunks=() c
  for p in "${PREFILL_PODS[@]}"; do
    c=$(kubectl logs -n "$NAMESPACE" "$p" -c modelserver 2>/dev/null \
         | grep -oE 'max_num_batched_tokens=[0-9]+' | tail -1 | cut -d= -f2)
    [[ -n $c ]] || die "cannot read max_num_batched_tokens from ${p} (log rotated? try restarting the pod)"
    chunks+=("$c")
    info "  ${p}: max_num_batched_tokens=${c}"
  done
  local uniq
  uniq=$(printf '%s\n' "${chunks[@]}" | sort -u | tr '\n' ' ')
  [[ $(printf '%s\n' "${chunks[@]}" | sort -u | wc -l) -eq 1 ]] \
    || die "prefill pods disagree on max_num_batched_tokens (${uniq}) — a single CHUNK_SIZE cannot be right for all"
  local effective_chunk="${chunks[0]}"

  # --- model id + max_model_len, straight from the engine
  local served maxlen
  served=$(kubectl exec -n "$NAMESPACE" "${PREFILL_PODS[0]}" -c modelserver -- python3 -c '
import urllib.request, json
d = json.load(urllib.request.urlopen("http://localhost:8000/v1/models", timeout=15))
m = d["data"][0]
print(m["id"], m.get("max_model_len", 0))' 2>/dev/null)
  [[ -n $served ]] || die "cannot query /v1/models on ${PREFILL_PODS[0]}"
  maxlen=$(printf '%s' "$served" | awk '{print $2}')
  served=$(printf '%s' "$served" | awk '{print $1}')
  info "  serving model: ${served}  max_model_len=${maxlen}"

  # --- reconcile with what we were asked to send
  if [[ -z $MODEL_NAME ]]; then
    MODEL_NAME="$served"
    info "MODEL_NAME discovered: ${MODEL_NAME}"
  elif [[ $MODEL_NAME != "$served" ]]; then
    die "MODEL_NAME=${MODEL_NAME} but the pod serves ${served} — the request would 404"
  fi

  if [[ -z $CHUNK_SIZE ]]; then
    CHUNK_SIZE="$effective_chunk"
    ok "CHUNK_SIZE=${CHUNK_SIZE}, matching the effective --max-num-batched-tokens"
  elif [[ $CHUNK_SIZE != "$effective_chunk" ]]; then
    if [[ $FORCED_CHUNK == true ]]; then
      warn "CHUNK_SIZE=${CHUNK_SIZE} does NOT match the effective max_num_batched_tokens=${effective_chunk}"
      if (( CHUNK_SIZE > effective_chunk )); then
        warn "  LARGER than the batch budget: vLLM splits the prompt across scheduler steps, so the"
        warn "  measured TTFT spans several prefill passes and UNDERSTATES throughput"
      else
        warn "  SMALLER than the batch budget: the prompt is one pass, but it does not fill the"
        warn "  batch, so this measures throughput at that size, not the ceiling upstream defines"
      fi
      warn "  proceeding because --chunk-size was given explicitly"
    else
      die "CHUNK_SIZE=${CHUNK_SIZE} != effective max_num_batched_tokens=${effective_chunk}.
         Upstream requires these to match. Drop the override, or pass --chunk-size ${CHUNK_SIZE} to force it."
    fi
  else
    ok "CHUNK_SIZE=${CHUNK_SIZE} matches the effective --max-num-batched-tokens"
  fi

  (( CHUNK_SIZE < maxlen )) \
    || die "CHUNK_SIZE=${CHUNK_SIZE} >= max_model_len=${maxlen}; the prompt plus 1 output token will not fit"
  ok "CHUNK_SIZE=${CHUNK_SIZE} fits inside max_model_len=${maxlen}"

  # --- TP and replica shape, for the record: the value is per (model x accel x TP x chunk)
  local tp
  tp=$(kubectl get pod "${PREFILL_PODS[0]}" -n "$NAMESPACE" \
        -o jsonpath='{range .spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null \
        | grep -oE 'tensor-parallel-size=[0-9]+' | cut -d= -f2)
  info "  prefill TP=${tp:-?}, replicas=${#PREFILL_PODS[@]} (the value is specific to model x accelerator x TP x chunk)"
  if [[ ${#PREFILL_PODS[@]} -gt 1 ]]; then
    warn "more than one prefill pod: requests spread across them, so the median reflects"
    warn "  whichever pods the EPP picked, not one pod's ceiling. Scale prefill to 1 for a clean read."
  fi

  # --- what the EPP is running right now
  LIVE_VALUE=$(read_live_epp_value)
  if [[ -n $LIVE_VALUE ]]; then
    info "  EPP currently runs peakPrefillThroughput=${LIVE_VALUE}"
  else
    warn "could not read peakPrefillThroughput from the EPP ConfigMap"
  fi
}

read_live_epp_value() {
  kubectl get cm "${GUIDE_NAME}-epp" -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys, yaml
try:
    data = json.load(sys.stdin).get("data", {})
except Exception:
    sys.exit(0)
for k, v in data.items():
    if not k.endswith(".yaml"):
        continue
    try:
        cfg = yaml.safe_load(v)
    except Exception:
        continue
    for p in (cfg or {}).get("plugins", []) or []:
        ppt = (p.get("parameters") or {}).get("peakPrefillThroughput")
        if ppt is not None:
            print(ppt); sys.exit(0)
' 2>/dev/null
}

# =========================================================================
# STAGE 2 — idle gate
# =========================================================================
assert_idle() {
  local phase="$1"
  local busy=0 line
  for p in "${PREFILL_PODS[@]}"; do
    line=$(vllm_load "$p" 8000) || continue
    info "  ${p}: ${line}"
    [[ $line == *"running=0"* && $line == *"waiting=0"* ]] || busy=$((busy + 1))
  done
  for p in "${DECODE_PODS[@]}"; do
    # decode vLLM listens on 8200; 8000 belongs to the routing sidecar
    line=$(vllm_load "$p" 8200) || continue
    info "  ${p}: ${line}"
    [[ $line == *"running=0"* && $line == *"waiting=0"* ]] || busy=$((busy + 1))
  done

  if (( busy > 0 )); then
    if [[ $ALLOW_LOAD == true ]]; then
      warn "${busy} pod(s) busy ${phase} — TTFT includes queue wait, so the result UNDERSTATES throughput"
    else
      die "${busy} pod(s) are serving traffic ${phase}. TTFT would include queue wait and the
         measurement would understate throughput. Wait for idle, or pass --allow-load."
    fi
  else
    ok "all model-server pods idle ${phase}"
  fi
}

vllm_load() {
  local pod="$1" port="$2"
  kubectl exec -n "$NAMESPACE" "$pod" -c modelserver -- python3 -c "
import urllib.request
try:
    m = urllib.request.urlopen('http://localhost:${port}/metrics', timeout=15).read().decode()
except Exception as e:
    print('unreachable:', e); raise SystemExit(0)
def g(name):
    for l in m.splitlines():
        if l.startswith(name + '{'):
            return float(l.rsplit(' ', 1)[1])
    return -1.0
print('running=%g waiting=%g kv=%.3f' % (g('vllm:num_requests_running'),
                                         g('vllm:num_requests_waiting'),
                                         g('vllm:kv_cache_usage_perc')))" 2>/dev/null
}

# =========================================================================
# STAGE 3 — run the upstream tool, REPEATS times
# =========================================================================
RESULTS=()
run_calibration() {
  stage "STAGE 3  run upstream calibrate.sh  (${REPEATS} repeat(s))"

  if [[ $DRY_RUN == true ]]; then
    info "would run, per repeat:"
    info "  GUIDE_NAME=${GUIDE_NAME} NAMESPACE=${NAMESPACE} MODEL_NAME=${MODEL_NAME} \\"
    info "  CHUNK_SIZE=${CHUNK_SIZE} NUM_WARMUP=${NUM_WARMUP} NUM_MEASUREMENTS=${NUM_MEASUREMENTS} \\"
    info "  bash ${CAL_DIR#$PWD/}/calibrate.sh"
    warn "--dry-run: not running"
    return 0
  fi

  local i
  for (( i = 1; i <= REPEATS; i++ )); do
    info "repeat ${i}/${REPEATS} ..."
    local out="${WORKDIR}/repeat-${RUN_ID}-${i}.out"
    local joblog="${WORKDIR}/repeat-${RUN_ID}-${i}.joblog"

    # The upstream script, called as-is. Nothing here edits it or its Job.
    if ! env GUIDE_NAME="$GUIDE_NAME" NAMESPACE="$NAMESPACE" MODEL_NAME="$MODEL_NAME" \
             CHUNK_SIZE="$CHUNK_SIZE" T_MAX_SECONDS="$T_MAX_SECONDS" \
             NUM_WARMUP="$NUM_WARMUP" NUM_MEASUREMENTS="$NUM_MEASUREMENTS" \
             bash "${CAL_DIR}/calibrate.sh" > "$out" 2>&1; then
      tail -25 "$out" >&2
      die "upstream calibrate.sh failed on repeat ${i} (full output: ${out})"
    fi

    # Keep the Job's own stdout: it holds every TTFT sample and the RNG seed,
    # which is the only way to see the distribution behind the median.
    kubectl logs -n "$NAMESPACE" job/calibrate-peak-throughput > "$joblog" 2>/dev/null || true

    local val
    val=$(grep -oE 'peakPrefillThroughput = [0-9]+' "$out" | tail -1 | awk '{print $NF}')
    [[ -n $val ]] || val=$(grep -oE '^PEAK_PREFILL_THROUGHPUT=[0-9]+' "$joblog" | tail -1 | cut -d= -f2)
    [[ -n $val ]] || { tail -20 "$out" >&2; die "repeat ${i}: no peakPrefillThroughput in the output"; }
    RESULTS+=("$val")

    local med n
    med=$(grep -oE 'T\(B\) *= *[0-9.]+' "$joblog" | tail -1 | grep -oE '[0-9.]+')
    n=$(grep -c 'measure ' "$joblog" 2>/dev/null || echo 0)
    ok "repeat ${i}: peakPrefillThroughput=${val} tok/s   median TTFT=${med:-?}s over ${n} samples"
  done
}

# =========================================================================
# STAGE 4 — analyse
# =========================================================================
FINAL_VALUE=
analyse() {
  stage "STAGE 4  analysis"
  [[ ${#RESULTS[@]} -gt 0 ]] || { warn "nothing measured"; return 0; }

  FINAL_VALUE=$(python3 - "$CHUNK_SIZE" "$MATRIX_REFERENCE" "${LIVE_VALUE:-0}" "$WORKDIR" "$RUN_ID" "${RESULTS[@]}" <<'PY'
import sys, glob, os, re, statistics as st
chunk = int(sys.argv[1]); ref = int(sys.argv[2]); live = int(sys.argv[3] or 0)
workdir, run_id = sys.argv[4], sys.argv[5]
vals = [int(v) for v in sys.argv[6:]]

# every TTFT sample from every repeat, so the spread is visible
samples = []
for f in sorted(glob.glob(os.path.join(workdir, f"repeat-{run_id}-*.joblog"))):
    for line in open(f, errors="replace"):
        m = re.search(r"measure \d+: TTFT=([0-9.]+)s", line)
        if m:
            samples.append(float(m.group(1)))

out = []
out.append("   repeats: " + ", ".join(str(v) for v in vals) + "  tok/s")
if len(vals) > 1:
    spread = (max(vals) - min(vals)) / st.median(vals) * 100
    out.append(f"   run-to-run spread: {spread:.1f}% of the median "
               f"(min {min(vals)}, median {int(st.median(vals))}, max {max(vals)})")
    if spread > 10:
        out.append("   WARN  >10% run-to-run drift — the stack is not in a steady state;")
        out.append("         re-run when it is before hardcoding a value")
if samples:
    s = sorted(samples)
    p = lambda q: s[min(len(s) - 1, int(q * len(s)))]
    cv = st.stdev(s) / st.mean(s) * 100 if len(s) > 1 else 0.0
    out.append(f"   TTFT over {len(s)} samples: p50={st.median(s):.3f}s p90={p(0.9):.3f}s "
               f"min={s[0]:.3f}s max={s[-1]:.3f}s  CV={cv:.1f}%")
    out.append(f"   implied from the pooled p50: {int(chunk / st.median(s))} tok/s")

final = int(st.median(vals))
out.append("")
out.append(f"   MEASURED peakPrefillThroughput = {final} tok/s   (median of {len(vals)} run(s))")
out.append("")
d = lambda a, b: (a - b) / b * 100
if live:
    out.append(f"   vs the value your EPP runs now ({live}): {d(final, live):+.0f}%")
out.append(f"   vs the upstream matrix reference ({ref}):   {d(final, ref):+.0f}%")
print("\n".join(out), file=sys.stderr)
print(final)
PY
)
  [[ -n $FINAL_VALUE ]] || warn "analysis produced no final value"
}

# =========================================================================
# STAGE 5 — optionally apply
# =========================================================================
apply_value() {
  [[ $APPLY == true ]] || {
    stage "STAGE 5  apply (skipped)"
    info "upstream deliberately does not auto-apply. To apply the measured value:"
    info "  ./$(basename "$0") --apply        # patches the guide values, upgrades, restarts, verifies"
    return 0
  }
  stage "STAGE 5  apply ${FINAL_VALUE} to the EPP"
  [[ -n ${FINAL_VALUE:-} ]] || die "no measured value to apply"
  command -v helm >/dev/null 2>&1 || die "helm not found, cannot apply"

  local base_values="${LLMD_DIR}/guides/recipes/router/base.values.yaml"
  local guide_values="${LLMD_DIR}/guides/${GUIDE_NAME}/router/${GUIDE_NAME}.values.yaml"
  local override="${WORKDIR}/router-calibrated.values.yaml"
  [[ -f $guide_values ]] || die "guide values not found: ${guide_values}"

  # peakPrefillThroughput lives INSIDE the pd-config.yaml string embedded in the
  # guide's values, so it cannot be reached with a --set. Rewrite that one number
  # inside the embedded document and emit a minimal override layered on top.
  python3 - "$guide_values" "$override" "$FINAL_VALUE" <<'PY' || die "could not build the override values file"
import sys, re, yaml
src, dst, val = sys.argv[1], sys.argv[2], sys.argv[3]
v = yaml.safe_load(open(src))
custom = v["router"]["epp"]["pluginsCustomConfig"]
key = next(k for k in custom if k.endswith(".yaml"))
body, n = re.subn(r"(peakPrefillThroughput:\s*)\d+", r"\g<1>" + val, custom[key])
if n != 1:
    sys.exit(f"expected exactly one peakPrefillThroughput in {key}, found {n}")
yaml.safe_dump({"router": {"epp": {"pluginsCustomConfig": {key: body}}}},
               open(dst, "w"), default_flow_style=False, width=10**6)
print(f"   wrote {dst} (peakPrefillThroughput -> {val})")
PY
  info "$(cat <<EOF
helm upgrade ${GUIDE_NAME} oci://ghcr.io/llm-d/charts/llm-d-router-standalone \\
  -f ${base_values} -f ${guide_values} -f ${override} -n ${NAMESPACE} --version v0
EOF
)"
  helm upgrade "$GUIDE_NAME" oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
    -f "$base_values" -f "$guide_values" -f "$override" \
    -n "$NAMESPACE" --version v0 --wait --timeout 5m >/dev/null \
    || die "helm upgrade failed"
  ok "router upgraded"

  # The EPP reads --config-file once at startup, so the ConfigMap change is inert
  # until the pod restarts.
  kubectl rollout restart -n "$NAMESPACE" "deployment/${GUIDE_NAME}-epp" >/dev/null \
    || die "rollout restart failed"
  kubectl rollout status -n "$NAMESPACE" "deployment/${GUIDE_NAME}-epp" --timeout=5m >/dev/null \
    || die "EPP did not come back after the restart"
  ok "EPP restarted"

  # Assert the value actually landed, rather than trusting helm's exit code.
  local now
  now=$(read_live_epp_value)
  [[ $now == "$FINAL_VALUE" ]] \
    || die "EPP ConfigMap still reports peakPrefillThroughput=${now:-<none>}, expected ${FINAL_VALUE}"
  ok "verified in the live EPP ConfigMap: peakPrefillThroughput=${now}"
}

summary() {
  stage "SUMMARY"
  cat >&2 <<EOF
   namespace / guide:   ${NAMESPACE} / ${GUIDE_NAME}
   model:               ${MODEL_NAME}
   chunk size:          ${CHUNK_SIZE}  (= effective --max-num-batched-tokens)
   repeats:             ${REPEATS} x (${NUM_WARMUP} warmup + ${NUM_MEASUREMENTS} measured)
   measured value:      ${FINAL_VALUE:-<none>} tok/s
   applied to EPP:      $([[ $APPLY == true ]] && echo yes || echo "no (measure-only, as upstream intends)")
   artifacts:           ${WORKDIR}/
                          repeat-${RUN_ID}-*.out      upstream calibrate.sh output
                          repeat-${RUN_ID}-*.joblog   every TTFT sample + RNG seed
EOF
  [[ $APPLY == false && -n ${FINAL_VALUE:-} ]] && cat >&2 <<EOF

   to apply:  ./$(basename "$0") --apply
   or by hand, per the upstream README:
     - type: prefix-cache-affinity-filter
       parameters:
         peakPrefillThroughput: ${FINAL_VALUE}
     then helm upgrade the router and: kubectl rollout restart -n ${NAMESPACE} deployment/${GUIDE_NAME}-epp
EOF
  return 0
}

main() {
  printf '%s\n' "logging to ${LOG}" >&2
  preflight
  discover
  stage "STAGE 2  idle gate (before)"
  assert_idle "before the run"
  run_calibration
  if [[ $DRY_RUN == false ]]; then
    stage "STAGE 2b  idle gate (after)"
    assert_idle "after the run"
  fi
  analyse
  apply_value
  summary
}

main 2>&1 | tee "$LOG"
exit "${PIPESTATUS[0]}"
