#!/usr/bin/env bash
#
# EPP and InferencePool setup for all environments (kind-emulator, kubernetes, openshift).
# Installs Gateway API CRDs, GAIE CRDs, and the llm-d-router-standalone chart (EPP + InferencePool).
#
# Required vars: LLM_D_ROUTER_VERSION, GAIE_VERSION, LLMD_NS
#   LLM_D_ROUTER_VERSION  — chart + EPP image tag from llm-d/llm-d-router (e.g. v0.9.0)
#   GAIE_VERSION          — kubernetes-sigs GAIE CRDs ref (e.g. v1.5.0)
# Optional vars:
#   EPP_HELM_VALUES_LIST  — newline-delimited complete ordered values chain.
# When unset, the existing WVA base + optimized-baseline defaults are unchanged.
# Required funcs: log_info, log_success, log_warning, log_error
#

GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-"v1.2.0"}

# Install Gateway API and GAIE CRDs.
# Called from deploy_wva_prerequisites before the WVA controller starts so its
# InferencePool watches succeed on the first attempt. Also called inside deploy_epp
# — idempotent, kubectl apply is a no-op when the CRDs are already present.
install_inference_crds() {
    log_info "Installing Gateway API CRDs (${GATEWAY_API_VERSION})..."
    kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" || \
        log_warning "Gateway API CRD install returned non-zero — may already be present"

    log_info "Installing GAIE CRDs (ref=${GAIE_VERSION})..."
    kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || \
        log_warning "GAIE CRD install returned non-zero — may already be present"
}

deploy_epp() {
    log_info "Deploying EPP infrastructure (llm-d-router-standalone chart ${LLM_D_ROUTER_VERSION}, GAIE CRDs ${GAIE_VERSION})..."

    local _lib_dir
    _lib_dir="$(dirname "${BASH_SOURCE[0]}")"

    # CRD installation — skipped on shared clusters where CRDs are pre-installed
    # (e.g. OpenShift e2e). Set SKIP_CLUSTER_CRDS=true to skip.
    if [ "${SKIP_CLUSTER_CRDS:-false}" != "true" ]; then
        log_info "Installing Gateway API CRDs (${GATEWAY_API_VERSION})..."
        kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" || \
            log_warning "Gateway API CRD install returned non-zero — may already be present"

        log_info "Installing GAIE CRDs (ref=${GAIE_VERSION})..."
        kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || \
            log_warning "GAIE CRD install returned non-zero — may already be present"
    else
        log_info "Skipping CRD installation (SKIP_CLUSTER_CRDS=true — pre-installed on shared cluster)"
    fi

    # llm-d namespace and dummy HF token secret for emulated environments.
    log_info "Creating llm-d namespace ($LLMD_NS)..."
    kubectl create namespace "$LLMD_NS" --dry-run=client -o yaml | kubectl apply -f -
    # Create HF token secret only if it does not already exist (preserves real token on OpenShift CI).
    if ! kubectl get secret llm-d-hf-token -n "$LLMD_NS" &>/dev/null; then
        local hf_token="${HF_TOKEN:-dummy-token}"
        kubectl create secret generic llm-d-hf-token \
            --from-literal="HF_TOKEN=${hf_token}" \
            -n "$LLMD_NS"
        log_info "Created llm-d-hf-token secret in $LLMD_NS"
    else
        log_info "llm-d-hf-token secret already exists in $LLMD_NS — skipping"
    fi

    # llm-d-router-standalone chart — bundles EPP + Envoy proxy; no external
    # gateway controller needed. Chart + EPP image version is LLM_D_ROUTER_VERSION.
    # Override chart's production resource requests (4CPU/8Gi per container)
    # to fit a kind cluster.
    log_info "Installing llm-d-router-standalone chart (release=optimized-baseline, version=${LLM_D_ROUTER_VERSION})..."

    # A caller-provided list is a complete chain. This lets the guide path use
    # its immutable current llm-d inputs without prepending the repository's
    # historical v0.8.1 defaults. Callers that do not provide the list retain
    # the existing WVA behavior exactly.
    local epp_helm_args=()
    local epp_resource_args=()
    if [ -n "${EPP_HELM_VALUES_LIST:-}" ]; then
        local helm_values_file
        while IFS= read -r helm_values_file; do
            [ -n "$helm_values_file" ] || continue
            if [ ! -r "$helm_values_file" ]; then
                log_error "EPP_HELM_VALUES_LIST contains an unreadable file: $helm_values_file"
            fi
            log_info "Layering caller-provided EPP Helm values: $helm_values_file"
            epp_helm_args+=(-f "$helm_values_file")
        done <<< "$EPP_HELM_VALUES_LIST"
    else
        epp_helm_args=(
            -f "$_lib_dir/epp-base.values.yaml"
            -f "$_lib_dir/epp-optimized-baseline.values.yaml"
        )
        epp_resource_args=(
            --set router.epp.resources.requests.cpu=100m
            --set router.epp.resources.requests.memory=256Mi
            --set router.epp.resources.limits.cpu=500m
            --set router.epp.resources.limits.memory=512Mi
            --set router.proxy.resources.requests.cpu=100m
            --set router.proxy.resources.requests.memory=128Mi
            --set router.proxy.resources.limits.cpu=500m
            --set router.proxy.resources.limits.memory=256Mi
        )
    fi

    # When scale-to-zero is enabled, add flowControl feature gate to EPP config so the
    # scale-from-zero engine can read inference_extension_flow_control_queue_size metrics.
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "ENABLE_SCALE_TO_ZERO=true: enabling EPP flowControl feature gate..."
        epp_helm_args+=(-f "$_lib_dir/epp-flow-control.values.yaml")
    fi

    helm upgrade --install optimized-baseline \
        oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
        "${epp_helm_args[@]}" \
        "${epp_resource_args[@]}" \
        -n "$LLMD_NS" --version "$LLM_D_ROUTER_VERSION" --create-namespace

    # Grant EPP SA permission to create tokenreviews/subjectaccessreviews so its
    # metrics endpoint authentication works (otherwise /metrics returns 500).
    log_info "Applying EPP tokenreview RBAC..."
    kubectl apply -f "$_lib_dir/epp-tokenreview-rbac.yaml"
    kubectl create clusterrolebinding optimized-baseline-epp-tokenreview \
        --clusterrole=optimized-baseline-epp-tokenreview \
        --serviceaccount="$LLMD_NS:optimized-baseline-epp" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Wait for EPP to be available.
    log_info "Waiting for EPP deployment (optimized-baseline-epp) in $LLMD_NS..."
    kubectl wait --for=condition=Available deployment/optimized-baseline-epp \
        -n "$LLMD_NS" --timeout=120s || \
        log_warning "EPP not ready yet — check 'kubectl get pods -n $LLMD_NS'"

    log_success "EPP infrastructure deployed (llm-d-router-standalone ${LLM_D_ROUTER_VERSION})"
}

undeploy_epp() {
    log_info "Removing EPP infrastructure..."
    helm uninstall optimized-baseline -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    kubectl delete secret llm-d-hf-token -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrolebinding optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrole optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    log_success "EPP infrastructure removed"
}
