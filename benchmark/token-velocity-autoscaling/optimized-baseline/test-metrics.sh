#!/bin/bash
#
# test-metrics.sh — verify token-aware metrics are flowing for optimized-baseline
#
# Confirms the full path the KEDA autoscaler depends on:
#   1. EPP (router) up; its authenticated /metrics endpoint responds
#   2. Model server up; its vLLM /metrics endpoint responds
#   3. The two KEDA trigger metrics exist:
#        prefill: llm_d_epp_inflight_tokens{producer_name="inflight-load-producer"}
#        decode:  vllm:kv_cache_usage_perc
#      NOTE: inflight_tokens is a LAZILY-registered gauge — the series does not
#      exist until traffic has flowed through the EPP at least once. --probe
#      generates traffic so it materializes.
#   4. Both metrics are queryable through the cluster monitoring stack (thanos),
#      which is where KEDA reads them — and the KEDA HPA reflects live values.
#
# Usage:
#   ./test-metrics.sh                 # health + flow check (no traffic)
#   ./test-metrics.sh --probe 90      # send traffic 90s, watch metrics move
#   ./test-metrics.sh --probe 90 --csv timeline.csv
#
# NOTE: deliberately NOT using `pipefail`. This script pipes very large metric
# dumps into grep -q / head; those readers close the pipe early, the writer gets
# SIGPIPE, and under pipefail that turns a successful match into a false failure.
set -eu
cd "$(dirname "$0")" || exit 1

NAMESPACE="${NAMESPACE:-pd-test}"
GUIDE_NAME="${GUIDE_NAME:-optimized-baseline}"
MODEL_ID="${MODEL_ID:-Qwen/Qwen3-32B}"
PROM_ADDR="${PROM_ADDR:-https://thanos-querier.openshift-monitoring.svc.cluster.local:9091}"
METRICS_SA="${METRICS_SA:-keda-epp-metrics-reader}"
V_P="${V_P:-15644}"
PROBE_SECONDS=0
CONCURRENCY="${CONCURRENCY:-24}"
CSV=""
FAILED=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --probe) if [[ ${2:-} =~ ^[0-9]+$ ]]; then PROBE_SECONDS="$2"; shift 2; else PROBE_SECONDS=90; shift; fi ;;
    --csv)   CSV="${2:?}"; shift 2 ;;
    -n|--namespace) NAMESPACE="${2:?}"; shift 2 ;;
    -g|--guide) GUIDE_NAME="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,/^set -euo/{/^set -euo/!p;}' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done
[[ -n $CSV ]] || CSV="metrics-timeline-$(date +%Y%m%d-%H%M%S).csv"

g=$'\033[32m'; r=$'\033[31m'; y=$'\033[33m'; b=$'\033[1m'; o=$'\033[0m'
hdr()  { printf '\n%s%s%s\n' "$b" "$*" "$o"; }
pass() { printf '  %sPASS%s %s\n' "$g" "$o" "$*"; }
fail() { printf '  %sFAIL%s %s\n' "$r" "$o" "$*"; FAILED=$((FAILED+1)); }
note() { printf '  %sNOTE%s %s\n' "$y" "$o" "$*"; }
info() { printf '       %s\n' "$*"; }
# pure-bash substring test — no pipe, so no SIGPIPE/pipefail hazard
contains() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

command -v kubectl >/dev/null || { echo "kubectl not found" >&2; exit 1; }
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || { echo "namespace ${NAMESPACE} not found" >&2; exit 1; }

ROUTER_DEPLOY="${GUIDE_NAME}-router-epp"
MODEL_DEPLOY="${GUIDE_NAME}-modelserver"

# ── a long-lived in-cluster curl pod; we exec into it for every fetch/query ──
HELPER="mtest-helper-$RANDOM"
cleanup() { kubectl delete pod "$HELPER" -n "$NAMESPACE" --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT
kubectl run "$HELPER" -n "$NAMESPACE" --restart=Never --image=curlimages/curl:latest \
  --command -- sleep 3600 >/dev/null 2>&1 || true
kubectl wait --for=condition=Ready "pod/$HELPER" -n "$NAMESPACE" --timeout=60s >/dev/null 2>&1 \
  || { echo "helper pod not ready" >&2; exit 1; }

hx() { kubectl exec -n "$NAMESPACE" "$HELPER" -- sh -c "$1" 2>/dev/null; }
urlenc() { python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$1"; }

# thanos query -> prints the scalar value of the first result (empty if none)
THANOS_TOKEN=$(kubectl create token "$METRICS_SA" -n "$NAMESPACE" --duration=15m 2>/dev/null || echo "")
thanos_q() {
  local enc; enc=$(urlenc "$1")
  hx "curl -sk -H 'Authorization: Bearer ${THANOS_TOKEN}' '${PROM_ADDR}/api/v1/query?query=${enc}'" \
    | python3 -c "import json,sys
try: d=json.load(sys.stdin); r=d['data']['result']; print(r[0]['value'][1] if r else '')
except Exception: print('')" 2>/dev/null
}

# ------- 1. deployments
hdr "1. Deployments in ${NAMESPACE}"
kubectl get deploy "$ROUTER_DEPLOY" -n "$NAMESPACE" >/dev/null 2>&1 && pass "router: ${ROUTER_DEPLOY}" || fail "router deployment missing: ${ROUTER_DEPLOY}"
kubectl get deploy "$MODEL_DEPLOY"  -n "$NAMESPACE" >/dev/null 2>&1 && pass "modelserver: ${MODEL_DEPLOY}" || fail "modelserver deployment missing: ${MODEL_DEPLOY}"

# ------- 2. readiness
hdr "2. Pod readiness"
for d in "$ROUTER_DEPLOY" "$MODEL_DEPLOY"; do
  ready=$(kubectl get deploy "$d" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}/{.status.replicas}' 2>/dev/null || echo "?/?")
  IFS='/' read -r rr tt <<< "$ready"
  [[ "${rr:-0}" -ge 1 && "${rr:-0}" -eq "${tt:-0}" ]] && pass "$d ready: $ready" || fail "$d not ready: $ready"
done

# ------- 3. services + discovery
hdr "3. Services"
# router service: created by deploy as "router" (metrics 8000->9090, inference 80->8081)
ROUTER_SVC=$(kubectl get svc router -n "$NAMESPACE" -o name >/dev/null 2>&1 && echo "router" \
  || kubectl get svc -n "$NAMESPACE" -l "app.kubernetes.io/name=${ROUTER_DEPLOY}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
MODEL_SVC=$(kubectl get svc -n "$NAMESPACE" -l "app=modelserver" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[[ -n $ROUTER_SVC ]] && pass "router service: ${ROUTER_SVC}" || fail "router service not found"
[[ -n $MODEL_SVC ]]  && pass "modelserver service: ${MODEL_SVC}" || fail "modelserver service not found"

# ------- 4. EPP metrics endpoint (authenticated, via router svc :8000 -> 9090)
hdr "4. EPP metrics endpoint"
EPP_TOKEN=$(kubectl get secret "${ROUTER_DEPLOY}-token" -n "$NAMESPACE" -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
EPP_METRICS=$(hx "curl -s -H 'Authorization: Bearer ${EPP_TOKEN}' http://${ROUTER_SVC}:8000/metrics")
if contains "$EPP_METRICS" $'\nllm_d_epp'; then
  pass "EPP /metrics responding (authenticated)"
  info "sample:"; printf '%s\n' "$EPP_METRICS" | grep '^llm_d_epp' | grep -v '^# ' | awk 'NR<=4{print "         " $0}'
else
  fail "EPP /metrics not returning llm_d_epp_* (check token/port)"
fi

# ------- 5. modelserver vLLM endpoint
hdr "5. Model server API + vLLM metrics"
MODELS=$(hx "curl -s http://${MODEL_SVC}:8000/v1/models")
if contains "$MODELS" '"id"'; then
  MID=$(printf '%s' "$MODELS" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | sed -n '1p')
  pass "vLLM API responding"; info "model: ${MID:-$MODEL_ID}"
else
  fail "vLLM API not returning models"
fi
VLLM_METRICS=$(hx "curl -s http://${MODEL_SVC}:8000/metrics")
contains "$VLLM_METRICS" "vllm:kv_cache_usage_perc" && pass "vLLM /metrics exposes kv_cache_usage_perc" || fail "vLLM /metrics missing kv_cache_usage_perc"

# ------- 6. the two KEDA trigger metrics on the raw endpoints
hdr "6. KEDA trigger metrics (raw endpoints)"
if contains "$EPP_METRICS" "llm_d_epp_inflight_tokens{"; then
  pass "prefill: llm_d_epp_inflight_tokens series present"
  info "$(printf '%s\n' "$EPP_METRICS" | grep '^llm_d_epp_inflight_tokens{' | sed -n '1p' | cut -c1-130)"
else
  note "prefill: llm_d_epp_inflight_tokens NOT present yet (lazy gauge — needs traffic; run --probe)"
fi
KV=$(printf '%s\n' "$VLLM_METRICS" | grep '^vllm:kv_cache_usage_perc{' | sed -n '1p' | awk '{print $NF}')
[[ -n $KV ]] && pass "decode: vllm:kv_cache_usage_perc = ${KV}" || fail "decode: vllm:kv_cache_usage_perc missing"

# ------- 7. flow into thanos (what KEDA actually reads)
hdr "7. Metric flow into thanos (KEDA source of truth)"
if [[ -z $THANOS_TOKEN ]]; then
  fail "could not mint ${METRICS_SA} token for thanos"
else
  PREFILL=$(thanos_q "sum(llm_d_epp_inflight_tokens{producer_name=\"inflight-load-producer\"}) / ${V_P}")
  DECODE=$(thanos_q "avg(vllm:kv_cache_usage_perc{namespace=\"${NAMESPACE}\"})")
  if [[ -n $PREFILL ]]; then pass "prefill query resolves in thanos = ${PREFILL} s backlog"; else note "prefill query has no series yet (lazy — run --probe)"; fi
  if [[ -n $DECODE ]];  then pass "decode query resolves in thanos = ${DECODE}"; else fail "decode query has no series in thanos (ServiceMonitor scraping?)"; fi
fi

# ------- 7b. HPA reflects the metrics
hdr "7b. KEDA HPA"
HPA="keda-hpa-${GUIDE_NAME}-autoscale"
if kubectl get hpa "$HPA" -n "$NAMESPACE" >/dev/null 2>&1; then
  pass "HPA present: ${HPA}"
  kubectl get hpa "$HPA" -n "$NAMESPACE" --no-headers | sed 's/^/         /'
  sa=$(kubectl get hpa "$HPA" -n "$NAMESPACE" -o jsonpath='{range .status.conditions[?(@.type=="ScalingActive")]}{.status} {.reason}{end}' 2>/dev/null)
  [[ $sa == True* ]] && pass "ScalingActive=${sa}" || fail "ScalingActive=${sa:-unknown} (metrics not readable by HPA)"
else
  note "no KEDA HPA yet — run ./launch-scaledobject.sh first"
fi

# ------- 8. probe: generate traffic and watch the lazy metrics move
if [[ $PROBE_SECONDS -gt 0 ]]; then
  hdr "8. Traffic probe (${PROBE_SECONDS}s, ${CONCURRENCY} concurrent) — inference via ${ROUTER_SVC}:80"
  LOADER="load-gen-$RANDOM"
  kubectl run "$LOADER" -n "$NAMESPACE" --restart=Never --image=curlimages/curl:latest \
    --overrides='{"spec":{"restartPolicy":"Never","containers":[{"name":"curl","image":"curlimages/curl:latest","command":["sh","-c"],"args":["end=$(($(date +%s) + '"$PROBE_SECONDS"')); for i in $(seq 1 '"$CONCURRENCY"'); do (while [ $(date +%s) -lt $end ]; do curl -s -o /dev/null -m 60 -X POST http://'"$ROUTER_SVC"':80/v1/completions -H \"Content-Type: application/json\" -d \"{\\\"model\\\":\\\"'"$MODEL_ID"'\\\",\\\"prompt\\\":\\\"Write an extensive multi-section technical essay on the history and future of distributed computing, with detailed examples.\\\",\\\"max_tokens\\\":512}\"; done) & done; wait"]}]}}' \
    >/dev/null 2>&1 || true
  note "load generator ${LOADER} started"

  hdr "9. Sampling every 15s (raw + thanos + replicas)"
  printf '%-7s %-14s %-13s %-13s %-11s %-9s %s\n' elapsed epp_inflight vllm_kv_perc thanos_prefill thanos_kv vllm_run replicas
  echo "elapsed_s,epp_inflight_tokens,vllm_kv_perc,thanos_prefill_s,thanos_kv,vllm_running,replicas" > "$CSV"
  START=$(date +%s)
  while [[ $(($(date +%s) - START)) -lt $PROBE_SECONDS ]]; do
    EL=$(($(date +%s) - START))
    EPPM=$(hx "curl -s -H 'Authorization: Bearer ${EPP_TOKEN}' http://${ROUTER_SVC}:8000/metrics")
    VLLMM=$(hx "curl -s http://${MODEL_SVC}:8000/metrics")
    inflight=$(printf '%s\n' "$EPPM" | awk '/^llm_d_epp_inflight_tokens{/{s+=$NF; n++} END{print (n?s:"-")}')
    kvperc=$(printf '%s\n' "$VLLMM" | awk '/^vllm:kv_cache_usage_perc{/{print $NF; exit}')
    vrun=$(printf '%s\n' "$VLLMM" | awk '/^vllm:num_requests_running{/{print $NF; exit}')
    tpre=$(thanos_q "sum(llm_d_epp_inflight_tokens{producer_name=\"inflight-load-producer\"}) / ${V_P}")
    tkv=$(thanos_q "avg(vllm:kv_cache_usage_perc{namespace=\"${NAMESPACE}\"})")
    reps=$(kubectl get deploy "$MODEL_DEPLOY" -n "$NAMESPACE" -o jsonpath='{.status.replicas}' 2>/dev/null)
    printf '%-7s %-14s %-13s %-13s %-11s %-9s %s\n' "$EL" "${inflight:-–}" "${kvperc:-–}" "${tpre:-–}" "${tkv:-–}" "${vrun:-–}" "${reps:-–}"
    echo "${EL},${inflight:-},${kvperc:-},${tpre:-},${tkv:-},${vrun:-},${reps:-}" >> "$CSV"
    sleep 15
  done
  kubectl delete pod "$LOADER" -n "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
  pass "timeline saved: ${CSV}"

  # confirm the lazy prefill gauge materialized at least once during the probe
  if awk -F, 'NR>1 && $2!="" && $2+0>0 {found=1} END{exit !found}' "$CSV"; then
    pass "lazy metric confirmed: llm_d_epp_inflight_tokens moved above 0 under load"
  else
    note "inflight_tokens stayed 0 in samples (requests may have drained between 15s polls; series still registered)"
  fi
fi

hdr "Result"
if (( FAILED == 0 )); then
  printf '  %sall checks passed%s\n' "$g" "$o"
else
  printf '  %s%d check(s) failed%s\n' "$r" "$FAILED" "$o"
fi
exit $(( FAILED > 0 ))
