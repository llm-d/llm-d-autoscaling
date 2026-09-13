#!/usr/bin/env bash
#
# launch-scaledobjects.sh — create the KEDA auth chain + two TOKEN-AWARE ScaledObjects.
#
#   prefill:  seconds of uncached prefill backlog = sum(inflight_tokens) / V_P  > PREFILL_THRESHOLD s
#   decode:   how full the decode KV caches are    = sum(kv_cache_usage_perc)   > 0.8
#
# Both triggers are the ones published in TOKEN-AWARE-AUTOSCALING-SUMMARY.md section 4.
# The summary's own framing: prefill divides queued tokens by a measured token rate to get
# SECONDS of backlog and compares that to a share of the TTFT budget; decode reads a
# value that is already a fraction of capacity, so 0.8 is compared to it directly and means
# "act at 80% full".
#
# THE PREFILL THRESHOLD IS WORKLOAD-DEPENDENT — 1.5s is the interactive/short-context default,
# not a universal constant. It is the QUEUE-WAIT budget left after the irreducible idle floor:
#
#     threshold  ~=  TTFT_SLO  -  (ISL_uncached / V_P)
#                    └ product ┘   └──── idle floor ────┘
#
# ISL/V_P ALREADY IS the end-to-end idle floor -- do NOT add a transfer term. calibrate-peak-
# prefill.sh defines V_P = CHUNK_SIZE / median(TTFT) measured THROUGH THE ROUTER on an idle
# stack; since TTFT is time-to-first-token, it already includes the NIXL KV transfer + first
# decode step. Adding "+ transfer" double-counts what V_P absorbed. Autoscaling only removes
# queueing, never the floor. If TTFT_SLO <= floor, no threshold meets it (raise V_P or shrink
# uncached ISL). Per reference workload at V_P~=2696 (see summary section 2 table):
#     prefill-heavy  ISL 8192 -> floor 3.0s  -> SLO 8s  => --prefill-threshold 5.0
#     symmetrical    ISL 2048 -> floor 0.76s -> SLO 3s  => --prefill-threshold 2.2
#     decode-heavy   ISL 256  -> TTFT trivially met     -> prefill moot; decode KV (0.8) governs
# For a p90/p99 SLO, trim to ~60-70% of the formula (the metric is a per-replica AVERAGE). The
# calibrated floor is a median under controlled load; if your SLO is against real client TTFT,
# subtract your own measured low-load TTFT instead (this cluster saw ~4.2s mean vs 3.0s floor --
# serving/measurement overhead, NOT transfer, which is already in V_P).
# NOTE: this is NOT the router's maxTTFTPenaltyMs (default 18s) -- that is the total TTFT
# degradation ceiling for breaking prefix-cache stickiness (routing), a different budget.
#
# This is MUTUALLY EXCLUSIVE with any other autoscaler on the same Deployments (e.g. the
# shipped pool-saturation triggers): two ScaledObjects on one Deployment create two
# competing HPAs. This script therefore DELETES any conflicting ScaledObjects it finds in
# the namespace before applying — anything not labelled app.kubernetes.io/part-of=epp-tokenaware.
#
# Three details that are load-bearing, all verified against llm-d-router source and this
# cluster:
#
#   1. The prefill query intersects inflight_tokens with per_endpoint_queue_size on a
#      derived `target_pod` label (LIVENESS GATE). inflight_tokens is a GaugeVec whose
#      series are never deleted when an endpoint goes away, so a scaled-down pod leaves a
#      stranded non-zero series that would permanently inflate the query. The queue-size
#      collector is rebuilt from live endpoints every scrape, so the intersection admits
#      only live pods. The two metrics label the same value differently
#      (endpoint_name vs model_server_endpoint), hence label_replace onto a common name.
#
#   2. Both queries use sum(), not max(). With metricType: AverageValue the HPA computes
#      ceil(total / threshold), so the numerator must GROW with the pool. max() of a
#      per-pod value cannot grow, which would structurally cap the role's replica count
#      regardless of maxReplicaCount.
#
#   3. `or vector(0)` only collapses to one series if the left side has an EMPTY label set,
#      which sum() guarantees. Without sum() the result keeps its pod labels, `or` appends a
#      second {} series, and KEDA fails with FailedGetExternalMetric. (Observed here.)
#
# CAVEAT ON THE DECODE TRIGGER, recorded because this script previously used a different one.
# kv_cache_usage_perc is an OCCUPANCY LEVEL, and a full-but-flowing cache is healthy: in the
# ISL-8192 arm of the earlier experiments it read 0.72-0.98 while decode was merely waiting on
# prefill's NIXL handoffs, so it bought decode replicas that were not the bottleneck. The
# alternative signal is vllm:num_requests_waiting_by_reason{reason="capacity"} -- requests vLLM
# ACTUALLY REFUSED to admit for want of KV blocks (work denied, not memory used), smoothed over
# 2m. Pass --decode-signal waiters to render that variant instead; the summary's occupancy
# trigger is the default because it is the published design. The two are not interchangeable
# by threshold: occupancy is a fraction of KV capacity (0.8), waiters is a count of refused
# requests (1), so --decode-signal also moves the default threshold.
#
# Usage:
#   ./launch-scaledobjects.sh                      # discover, apply, verify
#   ./launch-scaledobjects.sh --dry-run            # render only
#   ./launch-scaledobjects.sh --delete             # remove
#   ./launch-scaledobjects.sh --vp 2665 --max 4   # 2665 measured on the reference stack
#   ./launch-scaledobjects.sh --prefill-threshold 5.0   # long-context (ISL 8192, ~8s TTFT SLO)
#   ./launch-scaledobjects.sh --decode-signal waiters   # the refused-admission variant
#
set -uo pipefail
cd "$(dirname "$0")"

NAMESPACE="${NAMESPACE:-pd-test}"
MIN_REPLICAS="${MIN_REPLICAS:-1}"
MAX_REPLICAS="${MAX_REPLICAS:-10}"        # summary section 4: min 1 / max 10
# tokens/sec. NOT defaulted to a constant: the value is hardware-, model-, TP- and
# fabric-specific, and a wrong denominator silently changes when prefill scales (the same
# load asked for 8 replicas at 2665 and 3 at 15928 on the reference stack). Resolution
# order: --vp / $V_P  ->  the live EPP's own peakPrefillThroughput (what
# calibrate-peak-prefill.sh --apply wrote there)  ->  refuse and tell the operator to
# measure it.
V_P="${V_P:-}"
PREFILL_THRESHOLD="${PREFILL_THRESHOLD:-1.5}"   # seconds of queue backlog per replica; WORKLOAD-DEPENDENT
                                                # (= TTFT_SLO - ISL/V_P; the ISL/V_P floor already includes
                                                # NIXL transfer + first token via calibrate). 1.5 = interactive/
                                                # short-ISL default; raise for long context (see header table).
DECODE_THRESHOLD="${DECODE_THRESHOLD:-0.8}"     # pods\x27 worth of full KV cache (summary section 3)
POLLING_INTERVAL="${POLLING_INTERVAL:-15}"
THANOS="${THANOS:-https://thanos-querier.openshift-monitoring.svc.cluster.local:9091}"
OUT="${OUT:-rendered.yaml}"
DECODE_SIGNAL="${DECODE_SIGNAL:-occupancy}"     # occupancy (summary) | waiters (variant)
DECODE_THRESHOLD_SET=false                      # tracks an explicit --decode-threshold
DRY_RUN=false; DELETE=false
FAILED=0                                        # verification failures; nonzero -> nonzero exit

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=true; shift ;;
    --delete)  DELETE=true; shift ;;
    --min)     MIN_REPLICAS="${2:?}"; shift 2 ;;
    --max)     MAX_REPLICAS="${2:?}"; shift 2 ;;
    --vp)      V_P="${2:?}"; shift 2 ;;
    --prefill-threshold) PREFILL_THRESHOLD="${2:?}"; shift 2 ;;
    --decode-threshold)  DECODE_THRESHOLD="${2:?}"; DECODE_THRESHOLD_SET=true; shift 2 ;;
    --decode-signal)     DECODE_SIGNAL="${2:?}"; shift 2 ;;
    -p|--namespace) NAMESPACE="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,/^set -uo/p' "$0" | sed 's/^#\{0,1\} \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$DECODE_SIGNAL" in
  occupancy|waiters) ;;
  *) echo "--decode-signal must be 'occupancy' or 'waiters', got '${DECODE_SIGNAL}'" >&2; exit 1 ;;
esac
# The two decode signals are measured in different units, so they cannot share a
# threshold: occupancy is a fraction of KV capacity (0.8 = act at 80% full), waiters
# is a count of requests refused admission (1 = one refused request per replica).
if [[ "$DECODE_SIGNAL" == waiters && "$DECODE_THRESHOLD_SET" == false ]]; then
  DECODE_THRESHOLD=1
fi

g=$'\033[32m'; r=$'\033[31m'; y=$'\033[33m'; b=$'\033[1m'; o=$'\033[0m'
hdr(){ printf '\n%s%s%s\n' "$b" "$*" "$o"; }
ok(){   printf '  %sok%s %s\n'    "$g" "$o" "$*"; }
warn(){ printf '  %swarn%s %s\n'  "$y" "$o" "$*" >&2; }
die(){  printf '  %serror%s %s\n' "$r" "$o" "$*" >&2; exit 1; }

command -v oc >/dev/null || die "oc not found"
oc whoami >/dev/null 2>&1 || die "not logged in (oc login)"
oc get ns "$NAMESPACE" >/dev/null 2>&1 || die "namespace ${NAMESPACE} not found"

if [[ "$DELETE" == true ]]; then
  hdr "Removing token-aware ScaledObjects from ${NAMESPACE}"
  oc delete scaledobject -n "$NAMESPACE" -l app.kubernetes.io/part-of=epp-tokenaware --ignore-not-found=true
  oc delete triggerauthentication prometheus-auth -n "$NAMESPACE" --ignore-not-found=true
  oc delete secret keda-epp-metrics-reader-token -n "$NAMESPACE" --ignore-not-found=true
  oc delete sa keda-epp-metrics-reader -n "$NAMESPACE" --ignore-not-found=true
  oc delete clusterrolebinding "keda-epp-metrics-reader-monitoring-view-${NAMESPACE}" --ignore-not-found=true
  ok "removed"; exit 0
fi

# ───────────────────────────────────────────────────────── discover
hdr "1. Discovering names in ${NAMESPACE}"
POOL=$(oc get inferencepool -n "$NAMESPACE" --no-headers 2>/dev/null | awk '{print $1}' | head -1)
PREFILL=$(oc get deploy -n "$NAMESPACE" -o name 2>/dev/null | sed 's|deployment.apps/||' | grep -- '-prefill$' | head -1)
DECODE=$(oc get deploy  -n "$NAMESPACE" -o name 2>/dev/null | sed 's|deployment.apps/||' | grep -- '-decode$'  | head -1)
[[ -n "$POOL"    ]] || die "no InferencePool found — is the stack deployed?"
[[ -n "$PREFILL" ]] || die "no *-prefill Deployment found"
[[ -n "$DECODE"  ]] || die "no *-decode Deployment found"
ok "pool     ${POOL}"
ok "prefill  ${PREFILL}"
ok "decode   ${DECODE}"
if [[ -z "$V_P" ]]; then
  V_P=$(oc get cm "${POOL}-epp" -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
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
    ok "V_P      ${V_P} tok/s  (read from the live EPP ConfigMap)"
  else
    die "peakPrefillThroughput is unknown and must not be guessed.
         Measure it on THIS hardware:   ./calibrate-peak-prefill.sh --apply
         then re-run, or pass it explicitly:   --vp <tokens/sec>
         (the upstream configuration matrix lists 15928 for Qwen3-32B / H100 / TP=2 on the
          AGGREGATED path; through a P/D path with a slow KV fabric it can be ~6x lower.)"
  fi
fi
ok "V_P      ${V_P} tok/s  =>  prefill fires above ${PREFILL_THRESHOLD}s backlog per replica"

# Refuse to coexist with the saturation ScaledObjects: same Deployments, two HPAs, they fight.
CONFLICT=$(oc get scaledobject -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit()
for it in d.get("items",[]):
    lbl=it["metadata"].get("labels",{}) or {}
    if lbl.get("app.kubernetes.io/part-of")!="epp-tokenaware":
        print(it["metadata"]["name"])' 2>/dev/null)
if [[ -n "$CONFLICT" ]]; then
  if [[ "$DRY_RUN" == true ]]; then
    warn "conflicting ScaledObjects present; a real run would DELETE these (competing HPAs):"
    for c in $CONFLICT; do printf '       - %s\n' "$c" >&2; done
  else
    warn "deleting conflicting ScaledObjects (would create competing HPAs on the same Deployments):"
    for c in $CONFLICT; do printf '       - %s\n' "$c" >&2; oc delete scaledobject "$c" -n "$NAMESPACE" >/dev/null 2>&1; done
    ok "conflicts removed"
  fi
fi

# ───────────────────────────────────────────────────────── render
hdr "2. Rendering (min=${MIN_REPLICAS} max=${MAX_REPLICAS} decode-signal=${DECODE_SIGNAL})"

# The decode signal is chosen here, once, so the heredoc below stays a single template.
if [[ "$DECODE_SIGNAL" == occupancy ]]; then
  # SUMMARY section 4: how full the decode KV caches are, summed across pods. Already a
  # fraction of capacity, so the threshold is compared to it directly.
  DECODE_TRIGGER_NAME="decode-kv-utilization"
  DECODE_QUERY=$(cat <<QEOF
          sum(vllm:kv_cache_usage_perc{namespace="${NAMESPACE}", pod=~".*decode.*"}) or vector(0)
QEOF
)
else
  # VARIANT: requests vLLM actually REFUSED to admit for want of KV blocks — work denied,
  # not memory used. Two guards: 2m smoothing so a burst that drains in seconds does not
  # buy a replica that takes ~200s to load weights, and a strict-majority test so a
  # replica is only added when the shortage is fleet-wide rather than a routing imbalance.
  # At N=1 the gate passes (2 > 1) so boot is not blocked; at N=2 with one pod short it
  # fails (2 > 2 is false), which is why the test is `>` and not `>=`.
  DECODE_TRIGGER_NAME="decode-capacity-waiters"
  DECODE_QUERY=$(cat <<QEOF
          (
            avg_over_time(
              sum(vllm:num_requests_waiting_by_reason{namespace="${NAMESPACE}",
                                                      pod=~".*decode.*",
                                                      reason="capacity"})[2m:15s]
            )
            and on ()
            (
                count(vllm:num_requests_waiting_by_reason{namespace="${NAMESPACE}",
                                                          pod=~".*decode.*",
                                                          reason="capacity"} > 0) * 2
              > count(vllm:kv_cache_usage_perc{namespace="${NAMESPACE}", pod=~".*decode.*"})
            )
          ) or vector(0)
QEOF
)
fi
{
cat <<EOF
# GENERATED by epp-tokenaware/launch-scaledobjects.sh — re-run the script, do not edit.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: keda-epp-metrics-reader
  namespace: ${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware}
---
apiVersion: v1
kind: Secret
metadata:
  name: keda-epp-metrics-reader-token
  namespace: ${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware}
  annotations:
    kubernetes.io/service-account.name: keda-epp-metrics-reader
type: kubernetes.io/service-account-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: keda-epp-metrics-reader-monitoring-view-${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-monitoring-view}
subjects:
  - {kind: ServiceAccount, name: keda-epp-metrics-reader, namespace: ${NAMESPACE}}
---
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: prometheus-auth
  namespace: ${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware}
spec:
  secretTargetRef:
    - {parameter: bearerToken, name: keda-epp-metrics-reader-token, key: token}
    - {parameter: ca,          name: keda-epp-metrics-reader-token, key: service-ca.crt}
---
# PREFILL — token-aware. Seconds of uncached prefill backlog, gated on live endpoints.
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: prefill-tokenaware
  namespace: ${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware, llm-d.ai/role: prefill}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: ${PREFILL}}
  minReplicaCount: ${MIN_REPLICAS}
  maxReplicaCount: ${MAX_REPLICAS}
  pollingInterval: ${POLLING_INTERVAL}
  triggers:
    - type: prometheus
      name: prefill-token-backlog
      metricType: AverageValue
      authenticationRef: {name: prometheus-auth}
      metadata:
        serverAddress: "${THANOS}"
        authModes: bearer
        query: |
          (
            sum(
                label_replace(
                  llm_d_epp_inflight_tokens{namespace="${NAMESPACE}",
                                            producer_name="inflight-load-producer",
                                            endpoint_name=~".*prefill.*"},
                  "target_pod", "\$1", "endpoint_name", "(.+)")
              and on (target_pod)
                label_replace(
                  llm_d_epp_per_endpoint_queue_size{name="${POOL}",
                                                    model_server_endpoint=~".*prefill.*"},
                  "target_pod", "\$1", "model_server_endpoint", "(.+)")
            )
          )
        threshold: "${PREFILL_THRESHOLD}"
        activationThreshold: "0"
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:   {stabilizationWindowSeconds: 0,   policies: [{type: Pods, value: 1, periodSeconds: 180}]}
        scaleDown: {stabilizationWindowSeconds: 300, policies: [{type: Pods, value: 1, periodSeconds: 300}]}
---
# DECODE — signal: ${DECODE_SIGNAL} (see the header for what each one means and why).
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: decode-tokenaware
  namespace: ${NAMESPACE}
  labels: {app.kubernetes.io/part-of: epp-tokenaware, llm-d.ai/role: decode}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: ${DECODE}}
  minReplicaCount: ${MIN_REPLICAS}
  maxReplicaCount: ${MAX_REPLICAS}
  pollingInterval: ${POLLING_INTERVAL}
  triggers:
    - type: prometheus
      name: ${DECODE_TRIGGER_NAME}
      metricType: AverageValue
      authenticationRef: {name: prometheus-auth}
      metadata:
        serverAddress: "${THANOS}"
        authModes: bearer
        query: |
${DECODE_QUERY}
        threshold: "${DECODE_THRESHOLD}"
        activationThreshold: "0"
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:   {stabilizationWindowSeconds: 0,   policies: [{type: Pods, value: 1, periodSeconds: 180}]}
        scaleDown: {stabilizationWindowSeconds: 300, policies: [{type: Pods, value: 1, periodSeconds: 300}]}
EOF
} > "$OUT"
# The prefill threshold is per-replica seconds; V_P divides the token sum. KEDA has no
# arithmetic on the result, so the division must live inside the PromQL.
python3 - "$OUT" "$V_P" <<'PY'
import re,sys
p,vp=sys.argv[1],sys.argv[2]
s=open(p).read()
# close the outer paren group with the division, immediately before the threshold line
s=s.replace("            )\n          )\n        threshold:",
            "            )\n            / %s\n          ) or vector(0)\n        threshold:" % vp)
open(p,"w").write(s)
PY
grep -q "/ ${V_P}" "$OUT" || die "V_P division was not injected into the prefill query"
# KEDA accepts exactly ONE series from a trigger query. `or vector(0)` only collapses if the
# left side already has an EMPTY label set, which is what sum() guarantees -- without it the
# gated result keeps its pod labels, `or` appends a second {} series, and KEDA fails with
# FailedGetExternalMetric. (Observed on this stack.)
grep -q "sum(" "$OUT" || die "prefill query lost its sum() -- would return 2 series and KEDA would reject it"
# Same one-series rule for decode, plus the two guards that make the signal usable.
# Decode: assert the shape of whichever signal was selected, so a rendering bug cannot
# ship a query that merely looks plausible.
if [[ "$DECODE_SIGNAL" == occupancy ]]; then
  grep -q 'vllm:kv_cache_usage_perc' "$OUT" || die "decode query lost vllm:kv_cache_usage_perc"
  grep -q 'pod=~".*decode.*"'        "$OUT" || die "decode query lost its decode-pod filter -- it would sum PREFILL KV too"
  grep -q 'sum(vllm:kv_cache_usage_perc.*) or vector(0)' "$OUT" \
    || die "decode query must be sum(...) or vector(0) -- KEDA accepts exactly one series"
  grep -q 'num_requests_waiting_by_reason' "$OUT" \
    && die "decode-signal=occupancy but the waiters query rendered -- template bug"
else
  grep -q 'reason="capacity"' "$OUT" || die "decode query lost reason=\"capacity\" -- it would count deferred waiters too"
  grep -q '\[2m:15s\]'        "$OUT" || die "decode query lost its 2m smoothing window"
  grep -q '} > 0) \* 2'       "$OUT" || die "decode query lost the strict-majority fleet-wide guard"
  grep -q 'kv_cache_usage_perc.*}) or vector(0)' "$OUT" \
    && die "decode-signal=waiters but the occupancy query rendered -- template bug"
fi
ok "decode signal rendered: ${DECODE_SIGNAL} (threshold ${DECODE_THRESHOLD})"
ok "rendered $(grep -c '^kind:' "$OUT") objects -> ${OUT}"

if [[ "$DRY_RUN" == true ]]; then hdr "Dry run — nothing applied"; cat "$OUT"; exit 0; fi

# ───────────────────────────────────────────────────────── apply
hdr "3. Applying"
oc apply -f "$OUT" || die "apply failed"

# ───────────────────────────────────────────────────────── verify
hdr "4. Verifying KEDA can read the metrics"
for i in $(seq 1 12); do
  KEYS=$(oc get secret keda-epp-metrics-reader-token -n "$NAMESPACE" -o json 2>/dev/null \
    | python3 -c 'import json,sys;print(" ".join((json.load(sys.stdin).get("data") or {}).keys()))' 2>/dev/null)
  [[ "$KEYS" == *token* && "$KEYS" == *service-ca.crt* ]] && break
  python3 -c "import time; time.sleep(5)"
done
[[ "$KEYS" == *token*          ]] && ok "token secret populated"  || { warn "token missing — KEDA auth will fail"; FAILED=$((FAILED+1)); }
[[ "$KEYS" == *service-ca.crt* ]] && ok "service-ca.crt injected" || { warn "service-ca.crt missing"; FAILED=$((FAILED+1)); }

GOOD=0
for i in $(seq 1 24); do
  GOOD=$(oc get hpa -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json,sys
d=json.load(sys.stdin); n=0
for it in d.get("items",[]):
    for c in it.get("status",{}).get("conditions",[]):
        if c.get("type")=="ScalingActive" and c.get("reason")=="ValidMetricFound": n+=1
print(n)' 2>/dev/null || echo 0)
  (( GOOD >= 2 )) && break
  python3 -c "import time; time.sleep(10)"
done
if (( GOOD >= 2 )); then
  ok "both HPAs report ScalingActive=True ValidMetricFound"
else
  warn "only ${GOOD}/2 HPAs can read metrics — run ./test-metric-flow.sh for the reason"
  FAILED=$((FAILED+1))
fi

hdr "State"
oc get scaledobject -n "$NAMESPACE" --no-headers 2>/dev/null | awk '{printf "  %-24s min=%s max=%s ready=%s active=%s\n",$1,$4,$5,$6,$7}'
oc get hpa          -n "$NAMESPACE" --no-headers 2>/dev/null | awk '{printf "  %-40s targets=%s replicas=%s\n",$1,$3,$7}'
printf '\n  remove:  ./launch-scaledobjects.sh --delete -p %s\n' "$NAMESPACE"
(( FAILED > 0 )) && die "${FAILED} verification check(s) failed — ScaledObjects were applied but KEDA cannot read the metrics yet"
exit 0
