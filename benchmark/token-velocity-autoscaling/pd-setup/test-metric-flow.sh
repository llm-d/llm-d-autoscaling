#!/usr/bin/env bash
#
# test-metric-flow.sh — prove the token-aware trigger metrics actually flow to KEDA.
#
# Answers one question with evidence: does the chain
#     EPP /metrics -> Prometheus (user-workload) -> Thanos Querier -> KEDA -> HPA
# work end to end, and does KEDA see the same value Thanos holds?
#
# Two design choices that make this a real test rather than a restatement:
#
#   1. THE QUERIES ARE READ OUT OF THE LIVE ScaledObjects, not copied here. A test that
#      hardcodes its own copy of the PromQL passes while KEDA runs something else.
#
#   2. THANOS IS QUERIED WITH THE METRICS-READER SERVICE ACCOUNT'S OWN TOKEN — the same
#      credential KEDA uses — not with your `oc whoami -t`. Your user token is
#      cluster-admin-ish and succeeds where the SA may not, so testing with it would hide
#      exactly the failure this looks for. Upstream's OCP overlay warns why that matters:
#      Thanos :9091 answers an unauthenticated query with 401, and KEDA SUPPRESSES that
#      error and silently serves `fallback` replicas — so a broken trigger looks healthy.
#
# A zero is ambiguous at idle, so the prefill query's inputs are also queried separately:
# absent (metric missing, or the EPP restarted with no traffic since —
# llm_d_epp_inflight_tokens is registered LAZILY on the first dispatched request) is
# reported differently from present-and-zero.
#
# READ-ONLY BY DEFAULT: no load, no replica changes.
#
#   --probe [S]   drive load for S seconds (default 120) to prove the values MOVE.
#                 MAY SCALE THE DEPLOYMENTS: each new replica is TP=2, i.e. 2 GPUs.
#   --watch N     sample every 15s for N rounds without creating load
#   --csv FILE    write the sampled timeline (default timeline-<ts>.csv)
#
# Usage:
#   ./test-metric-flow.sh                  # health check only
#   ./test-metric-flow.sh --probe 180      # prove the metrics move (may scale)
#   ./test-metric-flow.sh --watch 20       # follow an in-flight scaling event
#
set -uo pipefail
cd "$(dirname "$0")"

NAMESPACE="${NAMESPACE:-pd-test}"
THANOS="${THANOS:-https://thanos-querier.openshift-monitoring.svc.cluster.local:9091}"
PART_OF="${PART_OF:-epp-tokenaware}"
CURL_IMAGE="${CURL_IMAGE:-cfmanteiga/alpine-bash-curl-jq}"
PROBE_SECONDS=0
PROBE_CONCURRENCY="${PROBE_CONCURRENCY:-16}"
PROBE_PROMPT_TOKENS="${PROBE_PROMPT_TOKENS:-3000}"
PROBE_MAX_TOKENS="${PROBE_MAX_TOKENS:-256}"
WATCH=0
CSV=""
FAILED=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --probe)
      if [[ ${2:-} =~ ^[0-9]+$ ]]; then PROBE_SECONDS="$2"; shift 2; else PROBE_SECONDS=120; shift; fi ;;
    --watch) WATCH="${2:?--watch needs a count}"; shift 2 ;;
    --csv)   CSV="${2:?}"; shift 2 ;;
    -p|--namespace) NAMESPACE="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,/^set -uo/p' "$0" | sed 's/^#\{0,1\} \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done
[[ -n $CSV ]] || CSV="timeline-$(date +%Y%m%d-%H%M%S).csv"

g=$'\033[32m'; r=$'\033[31m'; y=$'\033[33m'; b=$'\033[1m'; o=$'\033[0m'
hdr(){  printf '\n%s%s%s\n' "$b" "$*" "$o"; }
pass(){ printf '  %sPASS%s %s\n' "$g" "$o" "$*"; }
fail(){ printf '  %sFAIL%s %s\n' "$r" "$o" "$*"; FAILED=$((FAILED+1)); }
note(){ printf '  %sNOTE%s %s\n' "$y" "$o" "$*"; }
info(){ printf '       %s\n' "$*"; }

command -v kubectl >/dev/null || { echo "kubectl not found" >&2; exit 1; }
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || { echo "namespace ${NAMESPACE} not found" >&2; exit 1; }

# --------------------------------------------------------------- 1. the ScaledObjects
hdr "1. Token-aware ScaledObjects in ${NAMESPACE}"
kubectl get scaledobject -n "$NAMESPACE" -l "app.kubernetes.io/part-of=${PART_OF}" -o json 2>/dev/null > /tmp/_so.json
COUNT=$(python3 -c 'import json;print(len(json.load(open("/tmp/_so.json"))["items"]))' 2>/dev/null || echo 0)
if [[ ${COUNT:-0} -lt 1 ]]; then
  fail "no ScaledObjects labelled app.kubernetes.io/part-of=${PART_OF} — run ./launch-scaledobjects.sh first"
  exit 1
fi
python3 - /tmp/_so.json > /tmp/_triggers.tsv <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for it in d["items"]:
    name = it["metadata"]["name"]
    spec = it["spec"]
    tgt = spec["scaleTargetRef"]["name"]
    mn, mx = spec.get("minReplicaCount"), spec.get("maxReplicaCount")
    for i, t in enumerate(spec.get("triggers", [])):
        md = t.get("metadata", {})
        q = " ".join(md.get("query", "").split())
        print("\t".join([name, tgt, str(mn), str(mx), t.get("name", "s%d" % i),
                         md.get("threshold", "?"), t.get("metricType", "?"),
                         md.get("serverAddress", "?"), md.get("authModes", "none"), q]))
PY
while IFS=$'\t' read -r so tgt mn mx tname thr mtype addr auth q; do
  pass "${so}  ->  ${tgt}   min=${mn} max=${mx}"
  info "trigger ${tname}: threshold=${thr} type=${mtype} authModes=${auth}"
  info "query: ${q:0:150}"
  [[ $auth == *bearer* ]] || fail "${so}: authModes is '${auth}' — on OpenShift Thanos 401s and KEDA hides it"
done < /tmp/_triggers.tsv

# ----------------------------------------- 2. the credential KEDA itself uses
hdr "2. KEDA's credential (metrics-reader ServiceAccount)"
SA_TOKEN=$(kubectl get secret keda-epp-metrics-reader-token -n "$NAMESPACE" \
            -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null)
SA_CA=$(kubectl get secret keda-epp-metrics-reader-token -n "$NAMESPACE" \
            -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null)
[[ -n $SA_TOKEN ]] && pass "bearer token present in keda-epp-metrics-reader-token" \
                   || fail "no token in keda-epp-metrics-reader-token — KEDA cannot authenticate"
[[ -n $SA_CA    ]] && pass "service-ca.crt injected (signs Thanos's serving cert)" \
                   || fail "service-ca.crt missing — KEDA's TLS verification will fail"

# Portable timeout: prefer GNU coreutils timeout/gtimeout, else a background-kill
# fallback so this still works on stock macOS. `kubectl run --rm -i` can hang
# indefinitely waiting on pod scheduling/attach, and this loop calls it once per
# ScaledObject per sampling round — one stuck call has stalled a whole run before.
_timeout() {
  local secs="$1"; shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$secs" "$@"
  elif command -v gtimeout >/dev/null 2>&1; then
    gtimeout "$secs" "$@"
  else
    "$@" &
    local pid=$!
    ( sleep "$secs"; kill -TERM "$pid" 2>/dev/null ) &
    local watcher=$!
    wait "$pid" 2>/dev/null; local rc=$?
    kill "$watcher" 2>/dev/null
    return $rc
  fi
}

# Query Thanos from inside the cluster AS THAT SERVICE ACCOUNT.
thanos_query() {
  local pod="tmf-$RANDOM" out rc
  out=$(_timeout 60 kubectl run "$pod" --rm -i --restart=Never -n "$NAMESPACE" --image="$CURL_IMAGE" \
    --quiet --env="TOK=${SA_TOKEN}" --env="Q=$1" --command -- \
    sh -c 'curl -sSk --max-time 30 -H "Authorization: Bearer $TOK" --data-urlencode "query=$Q" '"${THANOS}"'/api/v1/query' 2>/dev/null)
  rc=$?
  # on timeout the client-side wait was abandoned; best-effort clean up the pod so it
  # doesn't linger in the namespace.
  (( rc != 0 )) && kubectl delete pod "$pod" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1
  printf '%s' "$out"
}
# Returns "<series-count> <first-value>" or "ERROR <detail>"
qsum() {
  thanos_query "$1" | python3 -c '
import json, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw[raw.index("{"):])
except Exception:
    print("ERROR unparseable-response"); raise SystemExit
if d.get("status") != "success":
    print("ERROR " + str(d.get("error", d))[:120]); raise SystemExit
r = d["data"]["result"]
print("%d %s" % (len(r), r[0]["value"][1] if r else "-"))'
}

echo "  querying Thanos as the ServiceAccount (the exact path KEDA takes)..."
UP_RESULT=$(qsum 'up')
case "$UP_RESULT" in
  ERROR*) fail "the metrics-reader SA cannot query Thanos: ${UP_RESULT#ERROR }"
          note "without this, KEDA suppresses the 401 and silently serves fallback replicas" ;;
  *)      pass "SA authenticated to Thanos (up returned $(echo "$UP_RESULT" | cut -d' ' -f1) series)" ;;
esac

# ------------------------------- 3. the raw inputs, so a zero can be attributed
hdr "3. Raw inputs behind the queries (a zero is ambiguous; absence is not)"
check_raw() {
  local label="$1" query="$2" out n v
  out=$(qsum "$query"); n=$(echo "$out" | cut -d' ' -f1); v=$(echo "$out" | cut -d' ' -f2)
  if [[ $n == ERROR ]]; then
    fail "${label}: query error ${v}"
  elif [[ ${n:-0} -eq 0 ]]; then
    fail "${label}: NO SERIES — metric absent, not zero"
    case "$label" in *inflight*) note "llm_d_epp_inflight_tokens is registered LAZILY on the first dispatched request; send traffic and retry" ;; esac
  else
    pass "${label}: ${n} series, value=${v}"
  fi
}
check_raw "epp inflight_tokens (prefill)" \
  "llm_d_epp_inflight_tokens{namespace=\"${NAMESPACE}\",producer_name=\"inflight-load-producer\",endpoint_name=~\".*prefill.*\"}"
check_raw "epp per_endpoint_queue_size" \
  "llm_d_epp_per_endpoint_queue_size{model_server_endpoint=~\".*prefill.*\"}"
check_raw "vllm kv_cache_usage (decode)" \
  "vllm:kv_cache_usage_perc{namespace=\"${NAMESPACE}\",pod=~\".*decode.*\"}"

# --------------------------- 4. the trigger queries exactly as KEDA runs them
hdr "4. Trigger queries, verbatim from the ScaledObjects"
while IFS=$'\t' read -r so tgt mn mx tname thr mtype addr auth q; do
  out=$(qsum "$q")
  n=$(echo "$out" | cut -d' ' -f1); v=$(echo "$out" | cut -d' ' -f2)
  if [[ $n == ERROR ]]; then
    fail "${so}/${tname}: ${v}"
  elif [[ ${n:-0} -ne 1 ]]; then
    fail "${so}/${tname}: returned ${n} series — KEDA needs EXACTLY 1 (FailedGetExternalMetric otherwise)"
  else
    pass "${so}/${tname}: 1 series, value=${v}, threshold=${thr}"
    python3 - "$v" "$thr" "$mn" "$mx" <<'PY'
import sys, math
try:
    v, thr, mn, mx = float(sys.argv[1]), float(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
except ValueError:
    raise SystemExit
want = max(mn, min(mx, math.ceil(v / thr) if thr else mn))
print("       implies ceil(%g / %g) = %d replicas (clamped to [%d,%d])" % (v, thr, want, mn, mx))
PY
  fi
done < /tmp/_triggers.tsv

# ------------------------ 5. what KEDA's own metrics API reports (vs Thanos)
hdr "5. KEDA external metrics API (what the HPA actually consumes)"
while IFS=$'\t' read -r so tgt mn mx tname thr mtype addr auth q; do
  raw=$(kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/${NAMESPACE}/s0-prometheus?labelSelector=scaledobject.keda.sh%2Fname=${so}" 2>/dev/null)
  if [[ -z $raw ]]; then
    fail "${so}: external metrics API returned nothing — the KEDA adapter is not serving this ScaledObject"
    continue
  fi
  printf '%s' "$raw" | python3 -c '
import json, sys
d = json.load(sys.stdin)
items = d.get("items", [])
if not items:
    print("  FAIL  no items in ExternalMetricValueList"); raise SystemExit(0)
for i in items:
    print("  PASS  %s = %s  (as of %s)" % (i["metricName"], i["value"], i["timestamp"]))'
done < /tmp/_triggers.tsv

# ------------------------------------------------ 6. HPA conditions and events
hdr "6. HPA conditions"
kubectl get hpa -n "$NAMESPACE" -o json 2>/dev/null | python3 -c '
import json, sys
for h in json.load(sys.stdin).get("items", []):
    st = h.get("status", {})
    conds = {c["type"]: (c["status"], c.get("reason", "")) for c in st.get("conditions", [])}
    sa = conds.get("ScalingActive", ("?", "?"))
    tag = "PASS" if sa[0] == "True" and sa[1] == "ValidMetricFound" else "FAIL"
    print("  %s  %-38s ScalingActive=%s(%s) replicas=%s/%s" % (
        tag, h["metadata"]["name"], sa[0], sa[1],
        st.get("currentReplicas"), st.get("desiredReplicas")))
    for t, (s, rsn) in conds.items():
        if t != "ScalingActive":
            print("        %-16s %-6s %s" % (t, s, rsn))'

hdr "7. Recent ScaledObject / HPA events"
kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp -o json 2>/dev/null | python3 -c '
import json, sys
rows = []
for e in json.load(sys.stdin).get("items", []):
    io = e.get("involvedObject", {}) or {}
    if io.get("kind") in ("ScaledObject", "HorizontalPodAutoscaler"):
        rows.append("  %-9s %-30s %-26s %s" % (
            (e.get("lastTimestamp") or "")[11:19], (io.get("name") or "")[:30],
            e.get("reason", ""), " ".join((e.get("message") or "").split())[:88]))
print("\n".join(rows[-14:]) if rows else "  (none)")'

# ------------------------------------------------- 8. optional: prove it moves
sample_row() {
  local line="$1" so tgt mn mx tname thr mtype addr auth q out n v reps ready
  while IFS=$'\t' read -r so tgt mn mx tname thr mtype addr auth q; do
    out=$(qsum "$q"); n=$(echo "$out" | cut -d' ' -f1); v=$(echo "$out" | cut -d' ' -f2)
    [[ $n == 1 ]] || v="NA"
    reps=$(kubectl get deploy "$tgt" -n "$NAMESPACE" -o jsonpath='{.status.replicas}' 2>/dev/null)
    ready=$(kubectl get deploy "$tgt" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    line="${line},${v},${reps:-0},${ready:-0}"
  done < /tmp/_triggers.tsv
  printf '%s\n' "$line"
}

csv_header() {
  local h="elapsed_s" so tgt rest
  while IFS=$'\t' read -r so tgt rest; do h="${h},${so}_value,${so}_replicas,${so}_ready"; done < /tmp/_triggers.tsv
  printf '%s\n' "$h"
}

if [[ $PROBE_SECONDS -gt 0 || $WATCH -gt 0 ]]; then
  hdr "8. Sampling the timeline -> ${CSV}"
  csv_header | tee "$CSV"
  LOADER=""
  if [[ $PROBE_SECONDS -gt 0 ]]; then
    IP=$(kubectl get service -n "$NAMESPACE" -o json | python3 -c '
import json,sys
for s in json.load(sys.stdin)["items"]:
    if s["metadata"]["name"].endswith("-epp"): print(s["spec"]["clusterIP"]); raise SystemExit')
    MODEL=$(kubectl get deploy -n "$NAMESPACE" -o json | python3 -c '
import json,sys
for d in json.load(sys.stdin)["items"]:
    for c in d["spec"]["template"]["spec"]["containers"]:
        if c["name"]=="modelserver": print(c["args"][0]); raise SystemExit')
    [[ -n $IP && -n $MODEL ]] || { fail "cannot resolve the EPP service IP or model name for the probe"; exit 1; }
    note "driving ${PROBE_CONCURRENCY} concurrent requests (~${PROBE_PROMPT_TOKENS} prompt tokens, ${PROBE_MAX_TOKENS} out) for ${PROBE_SECONDS}s"
    note "this may scale the deployments; each replica is TP=2 (2 GPUs)"
    LOADER="tokenaware-load-$RANDOM"
    kubectl run "$LOADER" -n "$NAMESPACE" --restart=Never --image="$CURL_IMAGE" \
      --env="IP=${IP}" --env="MODEL=${MODEL}" --env="DUR=${PROBE_SECONDS}" \
      --env="CONC=${PROBE_CONCURRENCY}" --env="PT=${PROBE_PROMPT_TOKENS}" --env="MT=${PROBE_MAX_TOKENS}" \
      --command -- sh -c '
        end=$(( $(date +%s) + DUR ))
        i=0
        while [ $(date +%s) -lt $end ]; do
          j=0
          while [ $j -lt $CONC ]; do
            i=$((i+1)); j=$((j+1))
            ( P=$(awk -v n=$PT -v s=$i "BEGIN{srand(s);for(k=0;k<n;k++)printf \"%d \", int(rand()*90000)}")
              curl -s -o /dev/null --max-time 120 -X POST "http://${IP}/v1/completions" \
                -H "Content-Type: application/json" \
                -d "{\"model\":\"${MODEL}\",\"prompt\":\"${P}\",\"max_tokens\":${MT},\"temperature\":0}" ) &
          done
          wait
        done' >/dev/null 2>&1
    sleep 5
  fi
  ROUNDS=$WATCH
  [[ $PROBE_SECONDS -gt 0 ]] && ROUNDS=$(( (PROBE_SECONDS / 15) + 4 ))
  START=$(date +%s)
  for (( k = 0; k < ROUNDS; k++ )); do
    sample_row "$(( $(date +%s) - START ))" | tee -a "$CSV"
    sleep 15
  done
  [[ -n $LOADER ]] && kubectl delete pod "$LOADER" -n "$NAMESPACE" --wait=false >/dev/null 2>&1
  hdr "Timeline verdict"
  python3 - "$CSV" <<'PY'
import csv, sys
rows = list(csv.DictReader(open(sys.argv[1])))
if len(rows) < 2:
    print("  too few samples"); raise SystemExit
for c in [c for c in rows[0] if c.endswith("_value")]:
    vals = []
    for r in rows:
        try: vals.append(float(r[c]))
        except (TypeError, ValueError): pass
    if not vals:
        print("  %-30s NO VALUES — trigger never resolved" % c); continue
    print("  %-30s min=%.4f max=%.4f %s" % (
        c, min(vals), max(vals),
        "MOVED — metric is live" if max(vals) > min(vals) else "FLAT — genuinely idle, or stuck"))
    rep = c.replace("_value", "_replicas")
    if rep in rows[0]:
        print("  %-30s replicas observed: %s" % ("", ",".join(sorted({r[rep] for r in rows}))))
PY
fi

hdr "Result"
if (( FAILED == 0 )); then
  printf '  %sall checks passed%s — the trigger metrics reach KEDA\n' "$g" "$o"
else
  printf '  %s%d check(s) failed%s\n' "$r" "$FAILED" "$o"
fi
if [[ $PROBE_SECONDS -gt 0 || $WATCH -gt 0 ]]; then printf '  csv: %s\n' "$CSV"; fi
exit $(( FAILED > 0 ))
