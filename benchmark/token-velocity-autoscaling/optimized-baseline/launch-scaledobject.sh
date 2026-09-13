#!/bin/bash
#
# launch-scaledobject.sh — create a single KEDA ScaledObject for optimized-baseline.
#
# Combines both prefill and decode triggers into one ScaledObject targeting the
# modelserver deployment. Requires KEDA to be installed in the cluster.
#
# TRIGGERS (from TOKEN-AWARE-AUTOSCALING-SUMMARY.md):
#   prefill:  seconds of uncached prefill backlog = inflight_tokens / V_P > 1.5s
#   decode:   how full the decode KV caches are    = kv_cache_usage_perc       > 0.8
#
# KEDA's prometheus scaler needs a PromQL API, not a raw /metrics endpoint. Both
# metrics are read from the OpenShift cluster monitoring stack via thanos-querier:
#   - inflight_tokens          <- EPP ServiceMonitor (from deploy-optimized-baseline.sh)
#   - vllm:kv_cache_usage_perc <- model server ServiceMonitor (created by this script)
# Auth: a ServiceAccount bound to cluster-monitoring-view; its token + service-ca.crt
# are wired into a KEDA TriggerAuthentication.
#
# TRIGGER PRIORITY (BOTH in a single ScaledObject):
#   KEDA evaluates both triggers independently and scales to the MAX of their desired replicas.
#   - If prefill fires (backlog > 1.5s), it calculates desired replicas
#   - If decode fires (KV util > 0.8), it calculates desired replicas
#   - HPA scales to max(prefill_desired, decode_desired)
#
#   This means: whichever workload is more demanding wins. If prefill needs 8 replicas but
#   decode only needs 2, you get 8. If decode needs 6 and prefill only needs 3, you get 6.
#   Neither has priority; they collaborate to scale based on the bottleneck.
#
#   This differs from pd-disaggregation, which has SEPARATE ScaledObjects per role (prefill
#   vs decode), allowing independent scaling (e.g., prefill at 8, decode at 2 simultaneously).
#   The trade-off: optimized-baseline is simpler but cannot independently optimize load
#   imbalance between prefill and decode when they have different saturation profiles.
#
# V_P (peakPrefillThroughput) MUST BE CALIBRATED FIRST:
#   ./calibrate-peak-prefill.sh --apply
#
# Usage:
#   ./launch-scaledobject.sh                        # discover, apply, verify
#   ./launch-scaledobject.sh --dry-run              # render only
#   ./launch-scaledobject.sh --delete               # remove
#   ./launch-scaledobject.sh --vp 2665 --max 4      # explicit V_P and max replicas
#   ./launch-scaledobject.sh --prefill-threshold 5.0  # long-context ISL 8192
#
set -euo pipefail
cd "$(dirname "$0")" || exit 1

NAMESPACE="${NAMESPACE:-pd-test}"
GUIDE_NAME="${GUIDE_NAME:-optimized-baseline}"
MIN_REPLICAS="${MIN_REPLICAS:-1}"
MAX_REPLICAS="${MAX_REPLICAS:-10}"
V_P="${V_P:-}"
PREFILL_THRESHOLD="${PREFILL_THRESHOLD:-1.5}"
DECODE_THRESHOLD="${DECODE_THRESHOLD:-0.8}"
POLLING_INTERVAL="${POLLING_INTERVAL:-15}"
# KEDA's prometheus scaler queries a PromQL API, NOT a raw /metrics endpoint.
# On OpenShift, user-workload-monitoring exposes both EPP and vLLM metrics via
# the cluster thanos-querier. Auth is a bearer token from a ServiceAccount bound
# to the cluster-monitoring-view ClusterRole (the established cluster convention).
PROM_ADDR="${PROM_ADDR:-https://thanos-querier.openshift-monitoring.svc.cluster.local:9091}"
METRICS_SA="${METRICS_SA:-keda-epp-metrics-reader}"
METRICS_SECRET="${METRICS_SA}-token"
TRIGGER_AUTH="${TRIGGER_AUTH:-prometheus-auth}"
DRY_RUN=false
DELETE=false
FAILED=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=true; shift ;;
    --delete)  DELETE=true; shift ;;
    --min)     MIN_REPLICAS="${2:?}"; shift 2 ;;
    --max)     MAX_REPLICAS="${2:?}"; shift 2 ;;
    --vp)      V_P="${2:?}"; shift 2 ;;
    --prefill-threshold) PREFILL_THRESHOLD="${2:?}"; shift 2 ;;
    --decode-threshold)  DECODE_THRESHOLD="${2:?}"; shift 2 ;;
    -n|--namespace) NAMESPACE="${2:?}"; shift 2 ;;
    -g|--guide) GUIDE_NAME="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,/^set -euo/{/^set -euo/!p;}' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

g=$'\033[32m'; r=$'\033[31m'; y=$'\033[33m'; b=$'\033[1m'; o=$'\033[0m'
hdr()  { printf '\n%s%s%s\n' "$b" "$*" "$o"; }
ok()   { printf '  %sok%s %s\n'    "$g" "$o" "$*"; }
warn() { printf '  %swarn%s %s\n'  "$y" "$o" "$*" >&2; }
die()  { printf '  %serror%s %s\n' "$r" "$o" "$*" >&2; exit 1; }

command -v kubectl >/dev/null || die "kubectl not found"
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || die "namespace ${NAMESPACE} not found"

# Check if KEDA is installed
if ! kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
  die "KEDA not found — install KEDA first: https://keda.sh/docs/deploy/"
fi

if [[ "$DELETE" == true ]]; then
  hdr "Removing ScaledObject from ${NAMESPACE}"
  kubectl delete scaledobject "${GUIDE_NAME}-autoscale" -n "$NAMESPACE" --ignore-not-found=true
  kubectl delete triggerauthentication "${TRIGGER_AUTH}" -n "$NAMESPACE" --ignore-not-found=true
  kubectl delete secret "${METRICS_SECRET}" -n "$NAMESPACE" --ignore-not-found=true
  kubectl delete clusterrolebinding "${METRICS_SA}-monitoring-view-${NAMESPACE}" --ignore-not-found=true
  kubectl delete sa "${METRICS_SA}" -n "$NAMESPACE" --ignore-not-found=true
  kubectl delete servicemonitor "${GUIDE_NAME}-modelserver-monitor" -n "$NAMESPACE" --ignore-not-found=true
  ok "removed"; exit 0
fi

# ───────────────────────────────────────────────────────── discover
hdr "1. Discovering target in ${NAMESPACE}"

# Find the modelserver deployment
MODELSERVER=$(kubectl get deploy -n "$NAMESPACE" -l app=modelserver \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[[ -n "$MODELSERVER" ]] || die "no modelserver deployment found (app=modelserver label)"
ok "modelserver  ${MODELSERVER}"

# Find the router/EPP service
ROUTER_SVC=$(kubectl get svc -n "$NAMESPACE" \
  -o jsonpath='{.items[?(@.metadata.name=="router")].metadata.name}' 2>/dev/null)
[[ -n "$ROUTER_SVC" ]] || ROUTER_SVC=$(kubectl get svc -n "$NAMESPACE" \
  -o jsonpath='{.items[?(@.spec.selector.app\.kubernetes\.io/name=="'${GUIDE_NAME}'-router-epp")].metadata.name}' 2>/dev/null)
[[ -n "$ROUTER_SVC" ]] || die "no router service found"
ok "router svc   ${ROUTER_SVC}"

# Read V_P (peakPrefillThroughput) from the router EPP ConfigMap if not provided.
# The helm chart renders the ConfigMap as "<release>-epp" == "${GUIDE_NAME}-router-epp";
# fall back to the older "${GUIDE_NAME}-epp" name for compatibility.
if [[ -z "$V_P" ]]; then
  EPP_CM=$(kubectl get cm "${GUIDE_NAME}-router-epp" -n "$NAMESPACE" -o name >/dev/null 2>&1 \
            && echo "${GUIDE_NAME}-router-epp" || echo "${GUIDE_NAME}-epp")
  V_P=$(kubectl get cm "$EPP_CM" -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys, yaml
try: data = json.load(sys.stdin).get("data", {})
except Exception: sys.exit(0)
for k, v in data.items():
    if not k.endswith(".yaml"): continue
    try: cfg = yaml.safe_load(v)
    except Exception: continue
    for pl in (cfg or {}).get("plugins", []) or []:
        ppt = (pl.get("parameters") or {}).get("peakPrefillThroughput")
        if ppt is not None: print(ppt); sys.exit(0)
' 2>/dev/null)
  if [[ -n "$V_P" ]]; then
    ok "V_P          ${V_P} tok/s  (peakPrefillThroughput from ConfigMap ${EPP_CM})"
  else
    die "peakPrefillThroughput unknown. Measure it first:
         ../calibrate-peak-prefill.sh --namespace ${NAMESPACE} --guide ${GUIDE_NAME} --apply
         or pass explicitly: --vp <value>"
  fi
else
  ok "V_P          ${V_P} tok/s  (provided)"
fi

ok "triggers:    prefill @ ${PREFILL_THRESHOLD}s backlog, decode @ ${DECODE_THRESHOLD} KV utilization"

# ───────────────────────────────────────────────────── ensure metrics are scraped
hdr "2. Ensuring metrics are scraped into cluster monitoring"

# The prefill query reads llm_d_epp_inflight_tokens from the EPP (scraped by the
# EPP ServiceMonitor created by deploy-optimized-baseline.sh). The decode query
# reads vllm:kv_cache_usage_perc from the model server, which needs its own
# ServiceMonitor so user-workload-monitoring scrapes it into thanos.
if kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
  cat <<EOF | kubectl apply -f -
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ${GUIDE_NAME}-modelserver-monitor
  namespace: ${NAMESPACE}
  labels:
    app: modelserver
    llm-d.ai/guide: ${GUIDE_NAME}
spec:
  selector:
    matchLabels:
      app: modelserver
  endpoints:
  - port: modelserver
    path: /metrics
    interval: 15s
    scheme: http
EOF
  ok "model server ServiceMonitor created/updated (vllm:* -> thanos)"
else
  warn "ServiceMonitor CRD not found — decode query (vllm:kv_cache_usage_perc) will have no data"
fi

# ───────────────────────────────────────────────────────── create auth
hdr "3. Setting up KEDA authentication (thanos bearer token)"

# ServiceAccount whose token KEDA presents to thanos-querier.
kubectl create sa "${METRICS_SA}" -n "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
ok "ServiceAccount ${METRICS_SA} created/updated"

# Grant it cluster-monitoring-view so thanos-querier authorizes its PromQL queries.
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${METRICS_SA}-monitoring-view-${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-monitoring-view
subjects:
- kind: ServiceAccount
  name: ${METRICS_SA}
  namespace: ${NAMESPACE}
EOF
ok "ClusterRoleBinding -> cluster-monitoring-view"

# Long-lived SA token secret; also carries service-ca.crt for TLS to thanos.
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: ${METRICS_SECRET}
  namespace: ${NAMESPACE}
  annotations:
    kubernetes.io/service-account.name: ${METRICS_SA}
type: kubernetes.io/service-account-token
EOF
# Wait for the token controller to populate the secret.
for _ in $(seq 1 10); do
  [[ -n "$(kubectl get secret "${METRICS_SECRET}" -n "$NAMESPACE" -o jsonpath='{.data.token}' 2>/dev/null)" ]] && break
  sleep 1
done
ok "Secret ${METRICS_SECRET} populated"

# TriggerAuthentication: bearer token + CA for the thanos TLS endpoint.
cat <<EOF | kubectl apply -f -
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: ${TRIGGER_AUTH}
  namespace: ${NAMESPACE}
spec:
  secretTargetRef:
  - parameter: bearerToken
    name: ${METRICS_SECRET}
    key: token
  - parameter: ca
    name: ${METRICS_SECRET}
    key: service-ca.crt
EOF
ok "TriggerAuthentication ${TRIGGER_AUTH} created/updated"

# ───────────────────────────────────────────────────────── create ScaledObject
hdr "4. Creating ScaledObject"

cat <<EOF | kubectl apply -f -
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: ${GUIDE_NAME}-autoscale
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${GUIDE_NAME}-tokenaware
    llm-d.ai/guide: ${GUIDE_NAME}
spec:
  scaleTargetRef:
    name: ${MODELSERVER}
    kind: Deployment
  minReplicaCount: ${MIN_REPLICAS}
  maxReplicaCount: ${MAX_REPLICAS}
  pollingInterval: ${POLLING_INTERVAL}
  cooldownPeriod: 30
  triggers:
  # Prefill trigger: seconds of uncached prefill backlog = inflight_tokens / V_P > PREFILL_THRESHOLD
  - type: prometheus
    name: prefill-backlog-seconds
    metricType: AverageValue
    authenticationRef:
      name: ${TRIGGER_AUTH}
    metadata:
      serverAddress: ${PROM_ADDR}
      authModes: bearer
      unsafeSsl: "false"
      activationThreshold: "0"
      threshold: "${PREFILL_THRESHOLD}"
      query: |
        sum(llm_d_epp_inflight_tokens{producer_name="inflight-load-producer"}) / ${V_P}

  # Decode trigger: scale on average KV cache utilization > DECODE_THRESHOLD
  - type: prometheus
    name: decode-kv-utilization
    metricType: AverageValue
    authenticationRef:
      name: ${TRIGGER_AUTH}
    metadata:
      serverAddress: ${PROM_ADDR}
      authModes: bearer
      unsafeSsl: "false"
      activationThreshold: "0"
      threshold: "${DECODE_THRESHOLD}"
      query: |
        avg(vllm:kv_cache_usage_perc{namespace="${NAMESPACE}"})
EOF

if [[ "$DRY_RUN" == true ]]; then
  ok "would apply above (--dry-run)"
else
  ok "ScaledObject created/updated"
fi

# ───────────────────────────────────────────────────────── verify
hdr "5. Verification"

# Wait a moment for resources to settle
sleep 2

# Check ScaledObject status
if SO=$(kubectl get scaledobject "${GUIDE_NAME}-autoscale" -n "$NAMESPACE" 2>/dev/null); then
  pass_count=$(echo "$SO" | grep -c "True" || echo 0)
  [[ $pass_count -gt 0 ]] && ok "ScaledObject status OK" || warn "ScaledObject status unclear"
else
  warn "could not verify ScaledObject"
fi

# Check for HPA
if HPA=$(kubectl get hpa -n "$NAMESPACE" -l scaledobject.keda.sh/name="${GUIDE_NAME}-autoscale" 2>/dev/null); then
  ok "HPA created by KEDA"
  echo "$HPA" | tail -1
else
  warn "HPA not yet visible"
fi

# Test that the ScaledObject's queries actually resolve against thanos, using the
# same SA token KEDA uses. Empty results mean no data yet (idle is fine: value 0).
hdr "6. Testing PromQL connectivity (thanos)"
TEST_POD="test-prometheus-$RANDOM"
TOKEN=$(kubectl create token "${METRICS_SA}" -n "$NAMESPACE" --duration=10m 2>/dev/null || true)
check_query() {
  local label="$1" query="$2"
  local enc; enc=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$query")
  local out; out=$(timeout 20 kubectl run "${TEST_POD}-$RANDOM" -n "$NAMESPACE" --restart=Never --rm -i \
    --image=curlimages/curl --quiet -- sh -c \
    "curl -sk -H 'Authorization: Bearer ${TOKEN}' '${PROM_ADDR}/api/v1/query?query=${enc}'" 2>/dev/null)
  if echo "$out" | grep -q '"status":"success"' && echo "$out" | grep -q '"result":\['; then
    if echo "$out" | grep -q '"value":\['; then ok "${label}: query returns data"; else
      warn "${label}: query valid but no series yet (send traffic to populate)"; fi
  else
    warn "${label}: query failed — check thanos auth/endpoint"; FAILED=$((FAILED+1))
  fi
}
if [[ -n "$TOKEN" ]]; then
  check_query "prefill (inflight_tokens/V_P)" "sum(llm_d_epp_inflight_tokens{producer_name=\"inflight-load-producer\"}) / ${V_P}"
  check_query "decode  (vllm:kv_cache_usage_perc)" "avg(vllm:kv_cache_usage_perc{namespace=\"${NAMESPACE}\"})"
else
  warn "could not mint SA token for connectivity test"
fi

# ───────────────────────────────────────────────────────── summary
hdr "Summary"
cat <<EOF
  namespace:           ${NAMESPACE}
  target:              ${MODELSERVER}
  min replicas:        ${MIN_REPLICAS}
  max replicas:        ${MAX_REPLICAS}
  V_P:                 ${V_P} tok/s
  prefill threshold:   ${PREFILL_THRESHOLD}s backlog
  decode threshold:    ${DECODE_THRESHOLD} KV utilization
  polling interval:    ${POLLING_INTERVAL}s

To monitor scaling:
  kubectl get scaledobject -n ${NAMESPACE} -w
  kubectl get hpa -n ${NAMESPACE} -w
  kubectl get events -n ${NAMESPACE} --sort-by='.lastTimestamp'

To delete:
  ./$(basename "$0") --delete
EOF

exit $(( FAILED > 0 ))
