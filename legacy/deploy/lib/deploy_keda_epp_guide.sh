#!/usr/bin/env bash
#
# Deploy the canonical llm-d KEDA+EPP guide contract through the existing WVA
# Kind e2e lifecycle. All llm-d-derived files remain temporary.

set -Eeuo pipefail

readonly WVA_PROJECT="${WVA_PROJECT:?WVA_PROJECT must name the WVA repository}"
readonly CLUSTER_NAME="${CLUSTER_NAME:?CLUSTER_NAME must name the Kind cluster}"
readonly LLMD_REPOSITORY_URL="https://github.com/llm-d/llm-d"
readonly LLMD_NS="${LLMD_NS:-llm-d-optimized-baseline}"
readonly MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-workload-variant-autoscaler-monitoring}"
readonly KEDA_NAMESPACE="${KEDA_NAMESPACE:-keda-system}"
readonly PROMETHEUS_RELEASE_NAME="${PROMETHEUS_RELEASE_NAME:-kube-prometheus-stack}"
readonly PROMETHEUS_SVC_NAME="${PROMETHEUS_SVC_NAME:-${PROMETHEUS_RELEASE_NAME}-prometheus}"
readonly PROMETHEUS_PORT="${PROMETHEUS_PORT:-9090}"
readonly PROMETHEUS_SECRET_NAME="${PROMETHEUS_SECRET_NAME:-prometheus-web-tls}"
readonly GUIDE_SIMULATOR_DEPLOYMENT="optimized-baseline-nvidia-gpu-vllm-decode"
readonly GUIDE_SIMULATOR_MANIFEST="$WVA_PROJECT/test/e2e/fixtures/keda_epp_guide_simulator.yaml"

work_dir=""
runtime_started=false

collect_setup_diagnostics() {
    [ "$runtime_started" = "true" ] || return 0

    echo "=== KEDA+EPP guide setup diagnostics ===" >&2
    kubectl --request-timeout=10s get pods,svc,endpoints,deployments -n "$LLMD_NS" -o wide >&2 || true
    kubectl --request-timeout=10s get servicemonitor -n "$LLMD_NS" -o yaml >&2 || true
    kubectl --request-timeout=10s get scaledobject,hpa -n "$LLMD_NS" -o yaml >&2 || true
    kubectl --request-timeout=10s get pods,deployments -n "$MONITORING_NAMESPACE" -o wide >&2 || true
    kubectl --request-timeout=10s get pods,deployments -n "$KEDA_NAMESPACE" -o wide >&2 || true
    kubectl --request-timeout=10s logs -n "$LLMD_NS" -l app.kubernetes.io/name=optimized-baseline-epp \
        --all-containers --tail=300 >&2 || true
    kubectl --request-timeout=10s logs -n "$KEDA_NAMESPACE" -l app.kubernetes.io/name=keda-operator \
        --all-containers --tail=300 >&2 || true
    for namespace in "$LLMD_NS" "$MONITORING_NAMESPACE" "$KEDA_NAMESPACE"; do
        kubectl --request-timeout=10s get events -n "$namespace" --sort-by=.lastTimestamp >&2 || true
    done
    echo "=== end KEDA+EPP guide setup diagnostics ===" >&2
}

on_error() {
    local status=$?
    trap - ERR
    collect_setup_diagnostics
    return "$status"
}

cleanup() {
    if [ -n "$work_dir" ] && [ "$work_dir" != "/" ] &&
        [[ "$(basename "$work_dir")" == wva-keda-epp-guide.* ]]; then
        rm -rf -- "$work_dir"
    fi
}
trap cleanup EXIT
trap on_error ERR

fail() {
    echo "ERROR: $*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_files_equal() {
    local expected_file="$1"
    local actual_file="$2"
    local description="$3"

    if ! cmp -s "$expected_file" "$actual_file"; then
        diff -u "$expected_file" "$actual_file" >&2 || true
        fail "$description"
    fi
}

normalize_contract_environment() {
    local source_file="$1"
    local destination_file="$2"

    sed \
        -e 's/^  namespace: .*$/  namespace: <environment-namespace>/' \
        -e 's|^      serverAddress: .*$|      serverAddress: <environment-prometheus-endpoint>|' \
        "$source_file" >"$destination_file"
}

for command_name in cmp curl diff git helm kubectl tar; do
    require_command "$command_name"
done

[ "$LLMD_NS" = "llm-d-optimized-baseline" ] ||
    fail "LLMD_NS must remain llm-d-optimized-baseline for this canonical contract"
[ -r "$GUIDE_SIMULATOR_MANIFEST" ] ||
    fail "guide simulator manifest is missing or unreadable: $GUIDE_SIMULATOR_MANIFEST"

selected_llmd_source_sha=""
if [ -n "${LLMD_SOURCE_SHA:-}" ]; then
    [[ "$LLMD_SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]] ||
        fail "LLMD_SOURCE_SHA override must be a 40-character lowercase hexadecimal full commit SHA"
    selected_llmd_source_sha="$LLMD_SOURCE_SHA"
    echo "Selected llm-d source SHA: ${selected_llmd_source_sha} (explicit LLMD_SOURCE_SHA override)"
else
    echo "Resolving official llm-d main once from ${LLMD_REPOSITORY_URL}..."
    official_main_result="$(git ls-remote --exit-code \
        "$LLMD_REPOSITORY_URL" refs/heads/main)" ||
        fail "could not resolve official llm-d main from ${LLMD_REPOSITORY_URL}"
    [[ "$official_main_result" != *$'\n'* ]] ||
        fail "official llm-d main resolution returned more than one revision"
    read -r selected_llmd_source_sha official_main_ref unexpected_field <<<"$official_main_result"
    [ "$official_main_ref" = "refs/heads/main" ] && [ -z "${unexpected_field:-}" ] ||
        fail "official llm-d main resolution returned an unexpected ref"
    [[ "$selected_llmd_source_sha" =~ ^[0-9a-f]{40}$ ]] ||
        fail "official llm-d main did not resolve to a full lowercase commit SHA"
    echo "Selected llm-d source SHA: ${selected_llmd_source_sha} (official main resolved once)"
fi
readonly selected_llmd_source_sha

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/wva-keda-epp-guide.XXXXXX")"

if [ -n "${LLMD_SOURCE:-}" ]; then
    llmd_git_root="$(git -C "$LLMD_SOURCE" rev-parse --show-toplevel)" ||
        fail "LLMD_SOURCE is not a readable git checkout: $LLMD_SOURCE"
    git -C "$llmd_git_root" cat-file -e "${selected_llmd_source_sha}^{commit}" 2>/dev/null ||
        fail "LLMD_SOURCE does not contain selected commit ${selected_llmd_source_sha}; no fallback revision will be used"
    llmd_root="$work_dir/llm-d-source"
    mkdir -p "$llmd_root"
    echo "Extracting selected llm-d source ${selected_llmd_source_sha} from read-only checkout: $llmd_git_root"
    git -C "$llmd_git_root" archive "$selected_llmd_source_sha" | tar -x -C "$llmd_root"
else
    echo "Downloading selected llm-d source at ${selected_llmd_source_sha} into temporary storage..."
    curl --fail --location --silent --show-error \
        --connect-timeout 10 \
        --max-time 120 \
        "https://github.com/llm-d/llm-d/archive/${selected_llmd_source_sha}.tar.gz" \
        --output "$work_dir/llm-d.tar.gz"
    tar -xzf "$work_dir/llm-d.tar.gz" -C "$work_dir"
    llmd_root="$work_dir/llm-d-${selected_llmd_source_sha}"
fi

readonly guide_root="$llmd_root/guides/workload-autoscaling/keda-epp-queue/optimized-baseline"
readonly canonical_kustomization="$guide_root/k8s/kustomization.yaml"
readonly canonical_router_base_values="$llmd_root/guides/recipes/router/base.values.yaml"
readonly canonical_optimized_baseline_values="$llmd_root/guides/optimized-baseline/router/optimized-baseline.values.yaml"
readonly canonical_router_values="$guide_root/router.values.yaml"
readonly canonical_router_monitoring_values="$llmd_root/guides/recipes/router/features/monitoring.values.yaml"
readonly kind_router_values="$WVA_PROJECT/deploy/lib/keda-epp-guide-kind.values.yaml"
readonly kind_monitoring_values="$WVA_PROJECT/deploy/lib/keda-epp-guide-monitoring.values.yaml"
readonly kind_keda_values="$WVA_PROJECT/deploy/lib/keda-epp-guide-keda.values.yaml"

for canonical_asset in \
    "$canonical_kustomization" \
    "$canonical_router_base_values" \
    "$canonical_optimized_baseline_values" \
    "$canonical_router_values" \
    "$canonical_router_monitoring_values" \
    "$kind_router_values" \
    "$kind_monitoring_values" \
    "$kind_keda_values"; do
    [ -r "$canonical_asset" ] ||
        fail "canonical llm-d asset is missing or unreadable: $canonical_asset"
done

# This array is the single source for the runtime install: canonical base ->
# optimized baseline -> monitoring -> queue guide
# -> the narrow WVA Kind override.
epp_helm_values=(
    "$canonical_router_base_values"
    "$canonical_optimized_baseline_values"
    "$canonical_router_monitoring_values"
    "$canonical_router_values"
    "$kind_router_values"
)
printf -v epp_helm_values_list '%s\n' "${epp_helm_values[@]}"

guide_simulator_image="$(awk '
        $1 == "image:" {
            if (NF != 2) {
                exit 1
            }
            image = $2
            count++
        }
        END {
            if (count != 1 || image == "") {
                exit 1
            }
            print image
        }
    ' "$GUIDE_SIMULATOR_MANIFEST")" ||
    fail "guide simulator manifest must contain exactly one container image"

prometheus_fqdn="${PROMETHEUS_SVC_NAME}.${MONITORING_NAMESPACE}.svc.cluster.local"
prometheus_url="https://${prometheus_fqdn}:${PROMETHEUS_PORT}"

overlay_dir="$work_dir/kustomize"
mkdir -p "$overlay_dir"
ln -s "$guide_root/k8s" "$overlay_dir/canonical"
cat >"$overlay_dir/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: llm-d-optimized-baseline
resources:
  - canonical
patches:
  - target:
      group: keda.sh
      version: v1alpha1
      kind: ScaledObject
      name: optimized-baseline-keda-epp
    patch: |-
      - op: replace
        path: /spec/triggers/0/metadata/serverAddress
        value: ${prometheus_url}
      - op: replace
        path: /spec/triggers/1/metadata/serverAddress
        value: ${prometheus_url}
EOF

rendered_contract="$work_dir/keda-epp-guide.yaml"
kubectl kustomize "$overlay_dir" --load-restrictor=LoadRestrictionsNone >"$rendered_contract"

canonical_contract="$work_dir/keda-epp-guide-canonical.yaml"
canonical_normalized="$work_dir/keda-epp-guide-canonical.normalized.yaml"
rendered_normalized="$work_dir/keda-epp-guide.normalized.yaml"
kubectl kustomize "$guide_root/k8s" --load-restrictor=LoadRestrictionsNone >"$canonical_contract"
normalize_contract_environment "$canonical_contract" "$canonical_normalized"
normalize_contract_environment "$rendered_contract" "$rendered_normalized"
require_files_equal \
    "$canonical_normalized" \
    "$rendered_normalized" \
    "temporary overlay may change only resource namespace and Prometheus serverAddress"

for command_name in kind openssl; do
    require_command "$command_name"
done

# Freshness is established by the exact current context and absence of guide
# namespaces and KEDA.
kind get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME" ||
    fail "Kind cluster '$CLUSTER_NAME' does not exist; create it first"
[ "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}" ] ||
    fail "current kubectl context must be kind-${CLUSTER_NAME}"
for namespace in "$LLMD_NS" "$MONITORING_NAMESPACE" "$KEDA_NAMESPACE"; do
    if kubectl get namespace "$namespace" >/dev/null 2>&1; then
        fail "namespace '$namespace' already exists; use a fresh disposable Kind cluster"
    fi
done
if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
    fail "KEDA is already present; use a fresh disposable Kind cluster"
fi
runtime_started=true

openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -keyout "$work_dir/ca.key" -out "$work_dir/ca.crt" \
    -subj "/CN=wva-keda-epp-guide-ca" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes \
    -keyout "$work_dir/prometheus.key" -out "$work_dir/prometheus.csr" \
    -subj "/CN=${PROMETHEUS_SVC_NAME}" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s,DNS:%s.%s.svc\n' \
    "$prometheus_fqdn" "$PROMETHEUS_SVC_NAME" "$MONITORING_NAMESPACE" \
    >"$work_dir/prometheus.ext"
openssl x509 -req -days 365 \
    -in "$work_dir/prometheus.csr" \
    -CA "$work_dir/ca.crt" -CAkey "$work_dir/ca.key" -CAcreateserial \
    -out "$work_dir/prometheus.crt" \
    -extfile "$work_dir/prometheus.ext" >/dev/null 2>&1

for namespace in "$LLMD_NS" "$MONITORING_NAMESPACE" "$KEDA_NAMESPACE"; do
    kubectl create namespace "$namespace"
done

kubectl create secret generic "$PROMETHEUS_SECRET_NAME" \
    -n "$MONITORING_NAMESPACE" \
    --from-file=tls.crt="$work_dir/prometheus.crt" \
    --from-file=tls.key="$work_dir/prometheus.key" \
    --from-file=ca.crt="$work_dir/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic llmd-prometheus-ca \
    -n "$KEDA_NAMESPACE" \
    --from-file=ca.crt="$work_dir/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic keda-prometheus-auth \
    -n "$LLMD_NS" \
    --from-file=ca.crt="$work_dir/ca.crt" \
    --dry-run=client -o yaml | kubectl apply -f -

env \
    IMG="" \
    KUBECONFIG="${KUBECONFIG:-}" \
    ENVIRONMENT=kind-emulator \
    CLUSTER_NAME="$CLUSTER_NAME" \
    LLMD_NS="$LLMD_NS" \
    MONITORING_NAMESPACE="$MONITORING_NAMESPACE" \
    KEDA_NAMESPACE="$KEDA_NAMESPACE" \
    DEPLOY_WVA=false \
    SCALER_BACKEND=keda \
    SCALE_TO_ZERO_ENABLED=false \
    DEPLOY_OPERATIONAL_DASHBOARD=false \
    PROMETHEUS_RELEASE_NAME="$PROMETHEUS_RELEASE_NAME" \
    PROMETHEUS_SVC_NAME="$PROMETHEUS_SVC_NAME" \
    PROMETHEUS_CHART_VERSION=62.7.0 \
    PROMETHEUS_HELM_VALUES="$kind_monitoring_values" \
    PROMETHEUS_TLS_SECRET_PRECREATED=true \
    KEDA_CHART_VERSION=2.20.0 \
    KEDA_HELM_VALUES="$kind_keda_values" \
    KEDA_HELM_TIMEOUT=10m \
    LLM_D_ROUTER_VERSION=v0.9.0 \
    GAIE_VERSION=v1.5.0 \
    EPP_HELM_VALUES_LIST="$epp_helm_values_list" \
    SIM_IMAGE="$guide_simulator_image" \
    WVA_PROJECT="$WVA_PROJECT" \
    "${MAKE:-make}" -C "$WVA_PROJECT" deploy-e2e-infra

echo "Installing the guide-owned deterministic simulator..."
kubectl apply -f "$GUIDE_SIMULATOR_MANIFEST"
kubectl wait \
    --for=jsonpath='{.status.readyReplicas}'=1 \
    deployment/"$GUIDE_SIMULATOR_DEPLOYMENT" \
    -n "$LLMD_NS" --timeout=10m

simulator_spec_replicas="$(kubectl get deployment "$GUIDE_SIMULATOR_DEPLOYMENT" \
    -n "$LLMD_NS" -o jsonpath='{.spec.replicas}')"
simulator_ready_replicas="$(kubectl get deployment "$GUIDE_SIMULATOR_DEPLOYMENT" \
    -n "$LLMD_NS" -o jsonpath='{.status.readyReplicas}')"
[ "$simulator_spec_replicas" = "1" ] && [ "$simulator_ready_replicas" = "1" ] ||
    fail "guide simulator must have exactly one desired and Ready replica"

echo "Applying the canonical TriggerAuthentication and ScaledObject..."
kubectl apply -f "$rendered_contract"

echo "Canonical KEDA+EPP guide contract deployed."
echo "Final cleanup boundary: make destroy-kind-cluster CLUSTER_NAME=${CLUSTER_NAME}"
