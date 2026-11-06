#!/usr/bin/env bash
# Run the bounded, CPU-only #1525 DSRS mechanics experiment.
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly REPO_ROOT
readonly FIXTURE_DIR="${REPO_ROOT}/benchmark/config/fixtures/wide-ep-dsrs-mechanics"
readonly DS_MANIFEST="${FIXTURE_DIR}/disaggregatedset.yaml"
readonly SCALEDOBJECT_MANIFEST="${FIXTURE_DIR}/scaledobject.yaml"
readonly NAMESPACE="llmd-1525-dsrs" DS_NAME="wide-ep-dsrs" DSRS_NAME="wide-ep-dsrs-decode"
readonly SCALEDOBJECT_NAME="wide-ep-dsrs-decode" TRIGGER_CONFIGMAP="wide-ep-dsrs-trigger"
readonly POLL_INTERVAL="${POLL_INTERVAL:-5}" INITIAL_TIMEOUT="${INITIAL_TIMEOUT:-300}"
readonly TRANSITION_TIMEOUT="${TRANSITION_TIMEOUT:-300}" CLEANUP_TIMEOUT="${CLEANUP_TIMEOUT:-180}"
readonly LWS_VERSION="${LWS_VERSION:-v0.11.0}" KEDA_VERSION="${KEDA_VERSION:-2.20.0}"
readonly PROMETHEUS_PORT="${PROMETHEUS_PORT:-19090}"
readonly PROMETHEUS_LOCAL_URL="http://127.0.0.1:${PROMETHEUS_PORT}"
readonly PROMETHEUS_URL="${PROMETHEUS_URL:-${PROMETHEUS_LOCAL_URL}}"
readonly PROMETHEUS_CONNECT_TIMEOUT_SECONDS="${PROMETHEUS_CONNECT_TIMEOUT_SECONDS:-1}"
readonly PROMETHEUS_REQUEST_TIMEOUT_SECONDS="${PROMETHEUS_REQUEST_TIMEOUT_SECONDS:-3}" KUBECTL_REQUEST_TIMEOUT_SECONDS="${KUBECTL_REQUEST_TIMEOUT_SECONDS:-30}"
readonly PROM_QUERY='max(kube_configmap_info{namespace="llmd-1525-dsrs",configmap="wide-ep-dsrs-trigger"}) or vector(0)'
readonly PROM_SOURCE_QUERY='kube_configmap_info{namespace="llmd-1525-dsrs",configmap="wide-ep-dsrs-trigger"}'
readonly TEST_LABEL='llm-d.ai/test=1525-dsrs-mechanics'
RUN_DIR="${TMPDIR:-/tmp}/llmd-1525-dsrs-$(date +%s)-$$"
readonly RUN_DIR
readonly EVIDENCE_FILE="${RUN_DIR}/transition-record.txt"
PHASE="preflight" PHASE_STARTED_SECONDS=0 PHASE_DEADLINE_SECONDS=0 PHASE_BUDGET_SECONDS=0
PROMETHEUS_FORWARD_PID="" PROMETHEUS_FORWARD_LOG="" NAMESPACE_CREATED="false" HPA_NAME=""
PREFILL_LWS_NAME="" DECODE_LWS_NAME="" ALLOW_EXPIRED_KUBECTL="false" ALLOW_EXPIRED_PROMETHEUS="false"
KUBECTL_BIN="$(type -P kubectl || true)"
kubectl() {
  local timeout="${KUBECTL_REQUEST_TIMEOUT_SECONDS}" remaining
  if [[ "${ALLOW_EXPIRED_KUBECTL}" != true ]] && (( PHASE_BUDGET_SECONDS > 0 )); then
    remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
    if (( remaining <= 0 )); then
      phase_timeout "Kubernetes request" || true
      return 124
    fi
    (( timeout > remaining )) && timeout="${remaining}"
  fi
  "${KUBECTL_BIN}" "--request-timeout=${timeout}s" "$@"
}
die() {
  printf 'ERROR [%s] %s\n' "${PHASE}" "$*" >&2
  return 1
}
require_command() {
  if [[ "$1" == kubectl ]]; then [[ -n "${KUBECTL_BIN}" ]] || die "required command is missing: kubectl"; else
    command -v "$1" >/dev/null 2>&1 || die "required command is missing: $1"; fi
}
begin_phase() {
  PHASE="$1"; PHASE_BUDGET_SECONDS="$2"; PHASE_STARTED_SECONDS="${SECONDS}"
  PHASE_DEADLINE_SECONDS=$((PHASE_STARTED_SECONDS + PHASE_BUDGET_SECONDS))
}
phase_remaining_seconds() {
  local remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
  (( remaining > 0 )) || return 1
  printf '%s\n' "${remaining}"
}
phase_timeout() {
  local condition="$1" remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
  (( remaining < 0 )) && remaining=0
  die "phase=${PHASE}; condition=${condition}; elapsed=$((SECONDS - PHASE_STARTED_SECONDS))s; remaining=${remaining}s; phase_timeout=${PHASE_BUDGET_SECONDS}s"
}
phase_completion_guard() {
  (( PHASE_DEADLINE_SECONDS > SECONDS )) && return 0
  phase_timeout "$1"; return 1
}
wait_for() {
  local description="$1" remaining sleep_for; shift
  while :; do
    remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
    if (( remaining <= 0 )); then phase_timeout "${description}"; return 1; fi
    if "$@" >/dev/null 2>&1; then
      phase_completion_guard "${description}" || return 1
      return 0
    fi
    remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
    (( remaining > 0 )) || { phase_timeout "${description}"; return 1; }
    sleep_for="${POLL_INTERVAL}"; (( sleep_for > remaining )) && sleep_for="${remaining}"
    sleep "${sleep_for}"
  done
}
wait_external() {
  local description="$1" timeout="$2" started="${SECONDS}"; shift 2
  local deadline=$((SECONDS + timeout)) remaining sleep_for
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1 && (( SECONDS < deadline )); then return 0; fi
    remaining=$((deadline - SECONDS)); (( remaining > 0 )) || break; sleep_for="${POLL_INTERVAL}"
    (( sleep_for > remaining )) && sleep_for="${remaining}"
    sleep "${sleep_for}"
  done
  die "phase=${PHASE}; condition=${description}; elapsed=$((SECONDS - started))s; remaining=0s; timeout=${timeout}s"
}
validate_limits() {
  local name value maximum
  for name in INITIAL_TIMEOUT TRANSITION_TIMEOUT CLEANUP_TIMEOUT; do
    value="${!name}"
    if ! [[ "${value}" =~ ^[0-9]+$ ]] || (( value <= 0 )); then die "${name} must be a positive integer"; fi
    maximum=300
    [[ "${name}" == CLEANUP_TIMEOUT ]] && maximum=180
    (( value <= maximum )) || die "${name}=${value}s exceeds approved maximum ${maximum}s"
  done
  if ! [[ "${POLL_INTERVAL}" =~ ^[0-9]+$ ]] || (( POLL_INTERVAL <= 0 )); then die "POLL_INTERVAL must be positive"; fi
  for name in PROMETHEUS_CONNECT_TIMEOUT_SECONDS PROMETHEUS_REQUEST_TIMEOUT_SECONDS KUBECTL_REQUEST_TIMEOUT_SECONDS; do
    value="${!name}"
    if ! [[ "${value}" =~ ^[0-9]+$ ]] || (( value <= 0 )); then die "${name} must be positive"; fi
  done
}
prometheus_query_json() {
  local request_timeout="${PROMETHEUS_REQUEST_TIMEOUT_SECONDS}" connect_timeout="${PROMETHEUS_CONNECT_TIMEOUT_SECONDS}" remaining
  if [[ "${ALLOW_EXPIRED_PROMETHEUS}" != true ]] && (( PHASE_BUDGET_SECONDS > 0 )); then
    remaining=$((PHASE_DEADLINE_SECONDS - SECONDS))
    if (( remaining <= 0 )); then phase_timeout "Prometheus query"; return 1; fi
    (( request_timeout > remaining )) && request_timeout="${remaining}"
    (( connect_timeout > remaining )) && connect_timeout="${remaining}"
  fi
  (( request_timeout < 1 )) && request_timeout=1
  (( connect_timeout < 1 )) && connect_timeout=1
  curl --fail --silent --show-error --connect-timeout "${connect_timeout}" \
    --max-time "${request_timeout}" --get --data-urlencode "query=${1}" \
    "${PROMETHEUS_URL}/api/v1/query"
}
metric_is() {
  local value
  value="$(prometheus_query_json "${PROM_QUERY}" | jq -er \
    'if (.data.result | length) != 1 then error("expected exactly one Prometheus sample") else .data.result[0].value[1] end')" || return 1
  [[ "${value}" == "$1" ]]
}
source_is_absent() {
  prometheus_query_json "${PROM_SOURCE_QUERY}" | jq -e '.data.result | length == 0' >/dev/null
}
source_is_one() {
  prometheus_query_json "${PROM_SOURCE_QUERY}" | jq -e '.data.result | length == 1' >/dev/null
}
record_evidence() {
  printf '%s\n' "$*" >>"${EVIDENCE_FILE}"
}
check_kind_context() {
  local context cluster
  context="$(kubectl config current-context)" || die "unable to read active kubectl context"
  [[ "${context}" == kind-* ]] || die "active kubectl context ${context} is not a Kind context"
  cluster="${context#kind-}"
  kind get clusters | rg -Fqx -- "${cluster}" || die "Kind cluster ${cluster} is absent"
  printf 'Kind: context=%s; cluster=%s\n' "${context}" "${cluster}"; record_evidence "kind.context=${context}"; record_evidence "kind.cluster=${cluster}"
}
check_kubernetes() {
  local version major minor
  version="$(kubectl version -o json | jq -er '.serverVersion.gitVersion')"
  major="${version#v}"; major="${major%%.*}"
  minor="${version#v*.}"; minor="${minor%%.*}"
  (( major == 1 && minor >= 34 )) || die "Kubernetes ${version} requires v1.34 or newer"
  record_evidence "kubernetes.version=${version}"
}
check_keda() {
  local image
  kubectl wait -n keda deployment/keda-operator --for=condition=available --timeout=60s >/dev/null
  image="$(kubectl -n keda get deployment/keda-operator -o json | jq -er \
    '.spec.template.spec.containers[] | select(.name == "keda-operator") | .image')"
  [[ "${image}" == *":${KEDA_VERSION}" ]] || die "KEDA image ${image} is not pinned to ${KEDA_VERSION}"
  record_evidence "keda.version=${KEDA_VERSION};keda.operator_image=${image}"
}
check_lws() {
  local release image dsrs_crd webhook_count
  kubectl wait -n lws-system deployment/lws-controller-manager --for=condition=available --timeout=120s >/dev/null
  release="$(helm -n lws-system list -o json | jq -er \
    '.[] | select(.name == "lws") | [.chart, .status] | @tsv')"
  [[ "${release}" == "lws-${LWS_VERSION}"$'\t'deployed ]] || die "LWS Helm release is not ${LWS_VERSION}: ${release}"
  image="$(kubectl -n lws-system get deployment/lws-controller-manager -o json | jq -er \
    '.spec.template.spec.containers[0].image')"
  [[ "${image}" == *":${LWS_VERSION}" ]] || die "LWS image ${image} is not pinned to ${LWS_VERSION}"
  kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io disaggregatedsets.disaggregatedset.x-k8s.io disaggregatedsetrolescalers.disaggregatedset.x-k8s.io >/dev/null
  dsrs_crd="$(kubectl get crd disaggregatedsetrolescalers.disaggregatedset.x-k8s.io -o json)"
  jq -e 'any(.spec.versions[]?; .served and
    .subresources.scale.specReplicasPath == ".spec.replicas" and
    .subresources.scale.statusReplicasPath == ".status.replicas" and
    .subresources.scale.labelSelectorPath == ".status.selector")' \
    <<<"${dsrs_crd}" >/dev/null || die "DSRS /scale is not exposed as expected"
  webhook_count="$(kubectl get validatingwebhookconfiguration -o json | jq -er '
    [.items[] | select(.metadata.name | test("lws|disaggregated"; "i")) |
      .webhooks[]? | select(.failurePolicy == "Fail") |
      select(any(.rules[]?.resources[]?; . == "disaggregatedsets"))] | length')"
  (( webhook_count > 0 )) || die "DisaggregatedSet validating webhook is absent"
  record_evidence "lws.version=${LWS_VERSION};lws.controller_image=${image};lws.helm_release=${release}"
}
check_prometheus() {
  kubectl -n monitoring get service prometheus-operated >/dev/null
  kubectl -n monitoring get deployment -l app.kubernetes.io/name=kube-state-metrics \
    -o json | jq -e '.items | length > 0' >/dev/null || die "kube-state-metrics is absent"
  kubectl -n monitoring get prometheus -o json | jq -e '.items | length > 0' >/dev/null || \
    die "Prometheus custom resource is absent"
  start_prometheus_forward; prometheus_query_json 'up' >/dev/null || die "Prometheus API is not ready"
  record_evidence "prometheus.url=${PROMETHEUS_URL}"
}
check_clean_namespace() {
  local kind count
  kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1 && die "namespace ${NAMESPACE} already exists"
  for kind in scaledobject hpa; do
    count="$(kubectl get "${kind}" -A -l "scaledobject.keda.sh/name=${SCALEDOBJECT_NAME}" \
      -o json | jq -er '.items | length')"
    (( count == 0 )) || die "a competing ${kind} targets ${SCALEDOBJECT_NAME}"
  done
}
start_prometheus_forward() {
  kubectl -n monitoring port-forward svc/prometheus-operated "${PROMETHEUS_PORT}:9090" \
    >"${RUN_DIR}/prometheus-port-forward.log" 2>&1 &
  PROMETHEUS_FORWARD_PID=$!
  PROMETHEUS_FORWARD_LOG="${RUN_DIR}/prometheus-port-forward.log"
  wait_external "Prometheus port-forward" 60 prometheus_forward_ready
  record_evidence "prometheus.forward_pid=${PROMETHEUS_FORWARD_PID}"
  record_evidence "prometheus.forward_log=${PROMETHEUS_FORWARD_LOG}"
}
prometheus_forward_ready() {
  [[ -n "${PROMETHEUS_FORWARD_PID}" ]] || return 1
  kill -0 "${PROMETHEUS_FORWARD_PID}" >/dev/null 2>&1 || return 1
  [[ -s "${PROMETHEUS_FORWARD_LOG}" ]] || return 1
  rg -q -- "Forwarding from .*:${PROMETHEUS_PORT} -> 9090" "${PROMETHEUS_FORWARD_LOG}" || return 1
  prometheus_query_json 'up' >/dev/null
}
stop_prometheus_forward() {
  [[ -n "${PROMETHEUS_FORWARD_PID}" ]] || return 0
  kill "${PROMETHEUS_FORWARD_PID}" >/dev/null 2>&1 || true
  local remaining=0
  remaining="$(phase_remaining_seconds 2>/dev/null || printf '0')"
  (( remaining > 10 )) && remaining=10
  local deadline=$((SECONDS + remaining))
  while kill -0 "${PROMETHEUS_FORWARD_PID}" >/dev/null 2>&1 && (( SECONDS < deadline )); do
    sleep 1
  done
  kill -KILL "${PROMETHEUS_FORWARD_PID}" >/dev/null 2>&1 || true
  wait "${PROMETHEUS_FORWARD_PID}" >/dev/null 2>&1 || true
  PROMETHEUS_FORWARD_PID=""
}
child_lws_json() {
  kubectl -n "${NAMESPACE}" get leaderworkerset \
    -l "disaggregatedset.x-k8s.io/name=${DS_NAME},disaggregatedset.x-k8s.io/role=${1},disaggregatedset.x-k8s.io/slice=0" \
    -o json
}
child_lws_name() {
  child_lws_json "$1" | jq -er \
    'if (.items | length) != 1 then error("expected one child LWS") else .items[0].metadata.name end'
}
ds_available() {
  local decode_replicas="${1:-1}"
  kubectl -n "${NAMESPACE}" get disaggregatedset "${DS_NAME}" -o json | jq -e \
    --arg name "${DS_NAME}" --argjson decode "${decode_replicas}" '(.metadata.name == $name) and (.spec.slices == 1) and
      (.status.observedGeneration == .metadata.generation) and
      any(.status.conditions[]?; .type == "Available" and .status == "True") and
      ((.status.roleStatuses | map(select(.name == "prefill" and .replicas == 1 and .readyReplicas == 1)) | length) == 1) and
      ((.status.roleStatuses | map(select(.name == "decode" and .replicas == $decode and .readyReplicas == $decode)) | length) == 1)' >/dev/null
}
dsrs_state() {
  local expected="$1"
  kubectl -n "${NAMESPACE}" get dsrs "${DSRS_NAME}" -o json | jq -e \
    --argjson expected "${expected}" '(.spec.replicas == $expected) and
      ((.status.replicas // 0) == $expected) and
      (.status.selector | contains("leaderworkerset.sigs.k8s.io/worker-index=0"))' >/dev/null
}
dsrs_scale_readable() {
  kubectl get --raw \
    "/apis/disaggregatedset.x-k8s.io/v1/namespaces/${NAMESPACE}/disaggregatedsetrolescalers/${DSRS_NAME}/scale" |
    jq -e --arg name "${DSRS_NAME}" \
      '.kind == "Scale" and .metadata.name == $name and (.spec.replicas | type == "number") and
       (.status.replicas | type == "number")' >/dev/null
}
child_lws_state() {
  local role="$1" target="$2" lws lws_name pods ready selector leaders
  lws="$(child_lws_json "${role}")" || return 1
  jq -e --argjson target "${target}" \
    '(.items | length == 1) and (.items[0].spec.replicas == $target) and
     ((.items[0].status.readyReplicas // 0) == $target)' <<<"${lws}" >/dev/null || return 1
  lws_name="$(jq -er '.items[0].metadata.name' <<<"${lws}")"
  pods="$(kubectl -n "${NAMESPACE}" get pod -l "leaderworkerset.sigs.k8s.io/name=${lws_name}" -o json)" || return 1
  ready="$(jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length' <<<"${pods}")"
  (( ready == target * 2 )) || return 1
  if [[ "${role}" == decode ]]; then
    selector="$(kubectl -n "${NAMESPACE}" get dsrs "${DSRS_NAME}" -o json | jq -er '.status.selector')" || return 1
    leaders="$(kubectl -n "${NAMESPACE}" get pod -l "${selector}" -o json | jq '.items | length')" || return 1
    (( leaders == target )) || return 1
  fi
}
prefill_unchanged() {
  child_lws_state prefill 1
}
assert_fixture_ownership() {
  local ds dsrs lws role prefill_dsrs
  ds="$(kubectl -n "${NAMESPACE}" get disaggregatedset "${DS_NAME}" -o json)"
  jq -e '[.metadata.ownerReferences[]? | select(.kind == "DisaggregatedSet")] | length == 0' <<<"${ds}" >/dev/null
  dsrs="$(kubectl -n "${NAMESPACE}" get dsrs "${DSRS_NAME}" -o json)"
  jq -e --arg name "${DS_NAME}" \
    '[.metadata.ownerReferences[]? | select(.kind == "DisaggregatedSet" and .name == $name)] | length == 1' <<<"${dsrs}" >/dev/null
  kubectl -n "${NAMESPACE}" get dsrs -o json | jq -e --arg name "${DSRS_NAME}" \
    '.items | length == 1 and .[0].metadata.name == $name' >/dev/null
  for role in prefill decode; do
    lws="$(child_lws_json "${role}")"
    jq -e --arg name "${DS_NAME}" --arg role "${role}" \
      '[.items[0].metadata.ownerReferences[]? | select(.kind == "DisaggregatedSet" and .name == $name)] | length == 1' <<<"${lws}" >/dev/null
    jq -e --arg role "${role}" '.items[0].metadata.labels["disaggregatedset.x-k8s.io/role"] == $role' <<<"${lws}" >/dev/null
  done
  if ! prefill_dsrs="$(kubectl -n "${NAMESPACE}" get dsrs "${DS_NAME}-prefill" --ignore-not-found -o name)"; then
    die "unable to verify Static prefill DSRS absence"
    return 1
  fi
  if [[ -n "${prefill_dsrs}" ]]; then
    die "Static prefill unexpectedly received a DSRS: ${prefill_dsrs}"
    return 1
  fi
  return 0
}
hpa_ready() {
  kubectl -n "${NAMESPACE}" get hpa -l "scaledobject.keda.sh/name=${SCALEDOBJECT_NAME}" -o json |
    jq -e '.items | length == 1' >/dev/null
}
hpa_state() {
  local expected="$1"
  kubectl -n "${NAMESPACE}" get hpa -l "scaledobject.keda.sh/name=${SCALEDOBJECT_NAME}" -o json |
    jq -e --argjson expected "${expected}" '.items | length == 1 and
      .[0].status.desiredReplicas == $expected and .[0].status.currentReplicas == $expected' >/dev/null
}
scaledobject_absent() {
  ! kubectl -n "${NAMESPACE}" get scaledobject "${SCALEDOBJECT_NAME}" >/dev/null 2>&1
}
capture_diagnostics() {
  local reason="${1:-failure}" old_kubectl="${ALLOW_EXPIRED_KUBECTL}" old_prom="${ALLOW_EXPIRED_PROMETHEUS}"
  ALLOW_EXPIRED_KUBECTL=true
  ALLOW_EXPIRED_PROMETHEUS=true
  mkdir -p "${RUN_DIR}/diagnostics" >/dev/null 2>&1 || true
  printf '%s\n' "${PROM_QUERY}" >"${RUN_DIR}/diagnostics/promql.txt" 2>/dev/null || true; printf '%s\n' "${PROM_SOURCE_QUERY}" >"${RUN_DIR}/diagnostics/promql-source.txt" 2>/dev/null || true
  prometheus_query_json "${PROM_QUERY}" >"${RUN_DIR}/diagnostics/promql-result.json" 2>&1 || true; prometheus_query_json "${PROM_SOURCE_QUERY}" >"${RUN_DIR}/diagnostics/promql-source-result.json" 2>&1 || true
  kubectl get --raw "/apis/disaggregatedset.x-k8s.io/v1/namespaces/${NAMESPACE}/disaggregatedsetrolescalers/${DSRS_NAME}/scale" >"${RUN_DIR}/diagnostics/dsrs-scale.json" 2>&1 || true
  for object in \
    "disaggregatedset:${DS_NAME}:ds.yaml" "dsrs:${DSRS_NAME}:dsrs.yaml" \
    "leaderworkerset::child-lws.yaml" "hpa::hpa.yaml" "scaledobject:${SCALEDOBJECT_NAME}:scaledobject.yaml" \
    "pod::pods.yaml"; do
    IFS=: read -r kind name file <<<"${object}"; if [[ -n "${name}" ]]; then kubectl -n "${NAMESPACE}" get "${kind}" "${name}" -o yaml >"${RUN_DIR}/diagnostics/${file}" 2>&1 || true; else kubectl -n "${NAMESPACE}" get "${kind}" -o yaml >"${RUN_DIR}/diagnostics/${file}" 2>&1 || true; fi
  done
  kubectl -n "${NAMESPACE}" get events --sort-by=.lastTimestamp >"${RUN_DIR}/diagnostics/events.txt" 2>&1 || true; kubectl -n keda logs deployment/keda-operator --since=5m --tail=200 >"${RUN_DIR}/diagnostics/keda.log" 2>&1 || true; kubectl -n lws-system logs deployment/lws-controller-manager --since=5m --tail=200 >"${RUN_DIR}/diagnostics/lws.log" 2>&1 || true
  [[ -f "${PROMETHEUS_FORWARD_LOG}" ]] && cp "${PROMETHEUS_FORWARD_LOG}" "${RUN_DIR}/diagnostics/prometheus-port-forward.log" || true
  printf 'failure=%s\nphase=%s\n' "${reason}" "${PHASE}" >"${RUN_DIR}/diagnostics/context.txt" 2>/dev/null || true
  ALLOW_EXPIRED_KUBECTL="${old_kubectl}"
  ALLOW_EXPIRED_PROMETHEUS="${old_prom}"
  printf 'Diagnostics retained at %s\n' "${RUN_DIR}/diagnostics" >&2
}
namespace_absent() {
  local result
  phase_remaining_seconds >/dev/null 2>&1 || return 1
  result="$(kubectl get namespace "${NAMESPACE}" --ignore-not-found -o name 2>&1)" || { printf 'ERROR [cleanup] namespace check failed: %s\n' "${result}" >&2; return 1; }
  [[ -z "${result}" ]]
}
cleanup() {
  local failed=0 remaining kind name
  begin_phase cleanup "${CLEANUP_TIMEOUT}"
  stop_prometheus_forward
  if [[ "${NAMESPACE_CREATED}" == true ]]; then
    for resource in "scaledobject:${SCALEDOBJECT_NAME}" "configmap:${TRIGGER_CONFIGMAP}"; do
      IFS=: read -r kind name <<<"${resource}"
      if ! remaining="$(phase_remaining_seconds 2>/dev/null)" || ! kubectl -n "${NAMESPACE}" delete "${kind}" "${name}" --ignore-not-found --wait=false >/dev/null 2>&1; then
        printf 'ERROR [cleanup] failed to delete %s/%s\n' "${kind}" "${name}" >&2
        failed=1
      fi
    done
    if ! remaining="$(phase_remaining_seconds 2>/dev/null)" || ! kubectl -n "${NAMESPACE}" delete disaggregatedset "${DS_NAME}" --ignore-not-found --wait=true --timeout="${remaining}s" >/dev/null 2>&1; then
      printf 'ERROR [cleanup] failed to delete DisaggregatedSet %s\n' "${DS_NAME}" >&2
      failed=1
    fi
    if ! remaining="$(phase_remaining_seconds 2>/dev/null)" || ! kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout="${remaining}s" >/dev/null 2>&1; then
      printf 'ERROR [cleanup] failed to delete namespace %s\n' "${NAMESPACE}" >&2
      failed=1
    fi
    namespace_absent || failed=1
  fi
  if (( failed != 0 )); then
    record_evidence "cleanup.result=FAIL"; printf 'ERROR [cleanup] cleanup verification failed.\n' >&2; return 1
  fi
  if [[ "${NAMESPACE_CREATED}" == true ]]; then record_evidence "cleanup.namespace_absent=true"; else record_evidence "cleanup.namespace_absent=SKIPPED (namespace not created)"; fi
  if ! phase_completion_guard "cleanup final verification"; then
    record_evidence "cleanup.result=FAIL"; printf 'ERROR [cleanup] cleanup verification failed.\n' >&2; return 1
  fi
  record_evidence "cleanup.result=PASS"
  printf 'Cleanup verified: owned namespace absent when created.\n'
  return 0
}
on_exit() {
  local experiment_status=$? cleanup_status=0
  if (( experiment_status != 0 )); then capture_diagnostics "exit-${experiment_status}" || true; fi
  cleanup || cleanup_status=$?
  if (( cleanup_status != 0 )); then capture_diagnostics "cleanup-${cleanup_status}" || true; fi
  if (( experiment_status == 0 && cleanup_status != 0 )); then experiment_status="${cleanup_status}"; fi
  if (( experiment_status == 0 )); then
    printf 'PASS: metric 0 -> manual DSRS 1->2->1 -> metric 1 -> HPA/DSRS 2/2; decode had two Ready groups and prefill remained at 1.\nSuccess evidence retained at %s\n' "${EVIDENCE_FILE}"
  fi
  exit "${experiment_status}"
}
trap on_exit EXIT
main() {
  mkdir -p "${RUN_DIR}"; : >"${EVIDENCE_FILE}"; record_evidence "experiment=wide-ep-dsrs-mechanics"; record_evidence "run_dir=${RUN_DIR}"
  require_command kubectl; require_command jq; require_command curl; require_command rg; require_command kind; require_command helm
  validate_limits; [[ "${PROMETHEUS_URL}" == "${PROMETHEUS_LOCAL_URL}" ]] || die "PROMETHEUS_URL must use the owned local forward"
  check_kind_context; check_kubernetes; check_keda; check_lws; check_clean_namespace; check_prometheus
  metric_is 0 || die "baseline approved PromQL result is not exactly 0"; record_evidence "baseline.metric=0"
  begin_phase "initial fixture" "${INITIAL_TIMEOUT}"
  kubectl create namespace "${NAMESPACE}" >/dev/null; NAMESPACE_CREATED=true; kubectl label namespace "${NAMESPACE}" "${TEST_LABEL}" --overwrite >/dev/null
  kubectl apply --server-side --dry-run=server -n "${NAMESPACE}" -f "${DS_MANIFEST}" >/dev/null; kubectl apply --server-side -n "${NAMESPACE}" -f "${DS_MANIFEST}" >/dev/null
  wait_for "DS Available=True with both roles Ready at 1" ds_available 1
  wait_for "DSRS ${DSRS_NAME} at 1/1" dsrs_state 1
  wait_for "DSRS /scale readability" dsrs_scale_readable
  wait_for "prefill child LWS one complete Ready group" child_lws_state prefill 1
  wait_for "decode child LWS one complete Ready group" child_lws_state decode 1
  assert_fixture_ownership; PREFILL_LWS_NAME="$(child_lws_name prefill)"; DECODE_LWS_NAME="$(child_lws_name decode)"
  record_evidence "initial.ds.available=true"
  record_evidence "initial.prefill.child_lws=${PREFILL_LWS_NAME};ready_groups=1;ready_pods=2"; record_evidence "initial.decode.child_lws=${DECODE_LWS_NAME};ready_groups=1;ready_pods=2"
  phase_completion_guard "initial fixture setup"
  begin_phase "manual DSRS 1 to 2 to 1" "${TRANSITION_TIMEOUT}"
  scaledobject_absent || die "ScaledObject exists before manual phase"
  kubectl -n "${NAMESPACE}" scale dsrs "${DSRS_NAME}" --replicas=2 >/dev/null
  wait_for "manual DSRS 1 to 2" dsrs_state 2
  wait_for "manual decode two complete Ready groups" child_lws_state decode 2
  wait_for "manual prefill remains at one group" prefill_unchanged
  kubectl -n "${NAMESPACE}" scale dsrs "${DSRS_NAME}" --replicas=1 >/dev/null
  wait_for "manual DSRS 2 to 1" dsrs_state 1
  wait_for "manual decode baseline group" child_lws_state decode 1
  wait_for "manual prefill remains unchanged" prefill_unchanged
  record_evidence "manual.dsrs=1->2->1;manual.decode.ready_groups=2->1;manual.prefill.ready_groups=1"
  phase_completion_guard "manual DSRS 1 to 2 to 1"
  begin_phase "KEDA/HPA transition" "${TRANSITION_TIMEOUT}"
  kubectl apply --server-side --dry-run=server -n "${NAMESPACE}" -f "${SCALEDOBJECT_MANIFEST}" >/dev/null; kubectl apply --server-side -n "${NAMESPACE}" -f "${SCALEDOBJECT_MANIFEST}" >/dev/null
  wait_for "KEDA generated HPA" hpa_ready
  HPA_NAME="$(kubectl -n "${NAMESPACE}" get hpa -l "scaledobject.keda.sh/name=${SCALEDOBJECT_NAME}" -o json | jq -er '.items[0].metadata.name')"
  kubectl -n "${NAMESPACE}" get hpa "${HPA_NAME}" -o json | jq -e --arg name "${DSRS_NAME}" \
    '.spec.scaleTargetRef.apiVersion == "disaggregatedset.x-k8s.io/v1" and
     .spec.scaleTargetRef.kind == "DisaggregatedSetRoleScaler" and .spec.scaleTargetRef.name == $name' >/dev/null ||
    die "HPA does not target the DSRS"
  wait_for "baseline approved PromQL result 0" metric_is 0
  wait_for "baseline marker source absent" source_is_absent
  wait_for "baseline DSRS 1/1" dsrs_state 1
  wait_for "baseline HPA 1/1" hpa_state 1
  wait_for "baseline decode group" child_lws_state decode 1
  wait_for "baseline prefill group" prefill_unchanged
  record_evidence "hpa.target=disaggregatedset.x-k8s.io/v1/DisaggregatedSetRoleScaler/${DSRS_NAME};baseline.promql=${PROM_QUERY};result=0;source_series=0;baseline.hpa=1/1;baseline.dsrs=1/1"
  kubectl -n "${NAMESPACE}" create configmap "${TRIGGER_CONFIGMAP}" \
    --from-literal=purpose="test-only; metric=1" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "${NAMESPACE}" label configmap "${TRIGGER_CONFIGMAP}" "${TEST_LABEL}" --overwrite >/dev/null
  wait_for "exact approved PromQL result 1" metric_is 1
  wait_for "one fresh Prometheus source series" source_is_one
  wait_for "KEDA HPA 2/2" hpa_state 2
  wait_for "KEDA DSRS 2/2" dsrs_state 2
  wait_for "decode two complete Ready groups and four Ready pods" child_lws_state decode 2
  wait_for "KEDA prefill remains at one group" prefill_unchanged
  wait_for "DS remains Available=True with decode at 2" ds_available 2
  record_evidence "trigger.promql=${PROM_QUERY};result=1;source_series=1;trigger.hpa=2/2;trigger.dsrs=2/2;decode.ready_groups=2;decode.ready_pods=4;prefill.ready_groups=1"
  phase_completion_guard "KEDA/HPA transition"
  printf 'Transitions complete; cleanup verification pending.\n'
}
main "$@"
