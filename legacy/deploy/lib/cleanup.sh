#!/usr/bin/env bash
#
# Undeploy and cleanup helpers for deploy/install.sh.
# Requires vars: KEDA_NAMESPACE, MONITORING_NAMESPACE, LLMD_NS, WVA_NS, WVA_PROJECT.
# Requires funcs: containsElement(),
# undeploy_prometheus_stack(), delete_namespaces(), undeploy_epp(), log_*().
#

undeploy_keda() {
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: skipping KEDA uninstall (platform-managed)"
        return
    fi
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: skipping KEDA uninstall (cluster-managed; set KEDA_HELM_INSTALL=true if this script installed KEDA)"
        return
    fi
    log_info "Uninstalling KEDA..."
    helm uninstall "$KEDA_RELEASE_NAME" -n "$KEDA_NAMESPACE" 2>/dev/null || \
        log_warning "KEDA not found or already uninstalled"
    kubectl delete namespace "$KEDA_NAMESPACE" --ignore-not-found --timeout=120s 2>/dev/null || true
    log_success "KEDA uninstalled"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler..."

    local kustomize_overlay
    if [ "$ENVIRONMENT" = "openshift" ]; then
        kustomize_overlay="$(cd "$WVA_PROJECT/config/overlays/namespace-scoped/openshift" && pwd)"
    else
        kustomize_overlay="$(cd "$WVA_PROJECT/config/overlays/cluster-scoped/kubernetes" && pwd)"
    fi

    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    ln -s "$kustomize_overlay" "$tmp_overlay/base"
    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
EOF

    kubectl delete -k "$tmp_overlay" --ignore-not-found 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler resources not found or already removed"
    rm -rf "$tmp_overlay"

    # Remove the per-deployment ClusterRoleBindings created for shared-cluster isolation.
    kubectl delete clusterrolebinding "workload-variant-autoscaler-manager-${WVA_NS}" \
        --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrolebinding "workload-variant-autoscaler-cluster-monitoring-view-${WVA_NS}" \
        --ignore-not-found 2>/dev/null || true

    rm -f "$PROM_CA_CERT_PATH"

    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Undeploy environment-specific components (Prometheus, etc.)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        undeploy_prometheus_stack
    fi

    # Undeploy scaler backend (KEDA or none). Mirror deploy_scaler_backend()'s
    # supported-value check so an unknown SCALER_BACKEND is surfaced rather than
    # silently leaving a previously-installed backend orphaned. Warn (not error)
    # so the rest of the teardown still runs.
    if [ "$SCALER_BACKEND" = "keda" ]; then
        undeploy_keda
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Skipping scaler backend undeployment (SCALER_BACKEND=none)"
    else
        log_warning "Unsupported SCALER_BACKEND: $SCALER_BACKEND (supported: keda, none); skipping scaler backend undeployment — any installed backend may be left behind"
    fi

    # EPP (llm-d-router-standalone chart) is torn down via undeploy_epp() from infra_epp.sh.
    undeploy_epp

    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi

    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ] || [ "$DELETE_CLUSTER" = "true" ]; then
        delete_namespaces
    else
        log_info "Keeping namespaces (use --delete-namespaces or set DELETE_NAMESPACES=true to remove)"
    fi

    echo ""
    log_success "Undeployment complete!"
    echo ""
    echo "=========================================="
    echo " Undeployment Summary for $ENVIRONMENT"
    echo "=========================================="
    echo ""
    echo "Removed components:"
    [ "$SCALER_BACKEND" = "keda" ] && echo "✓ KEDA"
    [ "$DEPLOY_WVA" = "true" ] && echo "✓ WVA Controller"
    [ "$DEPLOY_PROMETHEUS" = "true" ] && echo "✓ Prometheus Stack"

    if [ "$DELETE_NAMESPACES" = "true" ]; then
        echo "✓ Namespaces"
    else
        echo ""
        echo "Namespaces preserved:"
        echo "  - $LLMD_NS"
        echo "  - $WVA_NS"
        echo "  - $MONITORING_NAMESPACE"
        [ "$SCALER_BACKEND" = "keda" ] && echo "  - $KEDA_NAMESPACE"
    fi
    echo ""
    echo "=========================================="
}
