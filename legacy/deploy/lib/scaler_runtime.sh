#!/usr/bin/env bash
#
# Scaler backend deployment/runtime helpers for deploy/install.sh.
# Requires vars: MONITORING_NAMESPACE, KEDA_NAMESPACE, KEDA_CHART_VERSION.
# Optional vars: KEDA_HELM_VALUES, KEDA_HELM_TIMEOUT.
# Requires funcs: log_info/log_warning/log_success/log_error,
# should_skip_helm_repo_update().
#

deploy_keda() {
    log_info "Deploying KEDA (scaler backend)..."
    # Deploy flow is non-interactive: missing/failed scaler backend is a hard failure.

    # OpenShift: KEDA is cluster-managed (OLM/operator); never Helm-install — avoids
    # ClusterRole/release conflicts with an existing platform KEDA.
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: assuming platform-managed KEDA — skipping Helm install"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        else
            log_error "OpenShift: scaledobjects.keda.sh CRD not found — install cluster KEDA before E2E (SCALER_BACKEND=keda)"
        fi
        return
    fi

    # Kubernetes (e.g. CKS, shared clusters): assume cluster-managed KEDA; never Helm unless opted in.
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: assuming cluster-managed KEDA — skipping Helm (set KEDA_HELM_INSTALL=true to install via Helm)"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        else
            log_error "Kubernetes: scaledobjects.keda.sh CRD not found — install KEDA on the cluster or set KEDA_HELM_INSTALL=true"
        fi
        return
    fi

    # Skip install if KEDA is already fully operational on the cluster.
    # Check CRD + operator pods + external metrics APIService to avoid false positives
    # from stale CRDs left behind after a prior uninstall.
    if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
        if kubectl get pods -A -l "$KEDA_OPERATOR_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            if kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" >/dev/null 2>&1; then
                log_success "KEDA CRD, operator, and metrics APIService detected — skipping helm install"
                return
            fi
        fi
        # Shared clusters (e.g. CKS) often pre-install KEDA without the exact pod label / APIService
        # shape our probe expects, but ClusterRole keda-operator already exists without Helm metadata.
        # Helm install then fails with ownership errors — skip Helm when that pattern is present.
        if kubectl get clusterrole keda-operator >/dev/null 2>&1; then
            keda_cr_managed_by=$(kubectl get clusterrole keda-operator -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
            if [ "$keda_cr_managed_by" != "Helm" ]; then
                log_info "KEDA CRD present and ClusterRole keda-operator is not Helm-managed — skipping Helm install (pre-installed KEDA)"
                return
            fi
        fi
        log_warning "KEDA ScaledObject CRD found but operator or metrics APIService not detected; proceeding with helm install"
    fi

    kubectl create namespace "$KEDA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
    if [ "$(should_skip_helm_repo_update)" = "true" ]; then
        log_info "Skipping helm repo update for KEDA (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    local helm_args=(
        upgrade -i "$KEDA_RELEASE_NAME" kedacore/keda
        --version "$KEDA_CHART_VERSION"
        -n "$KEDA_NAMESPACE"
    )
    if [ -n "${KEDA_HELM_VALUES:-}" ]; then
        helm_args+=(--values "$KEDA_HELM_VALUES")
    fi
    helm_args+=(
        --set prometheus.metricServer.enabled=true
        --set prometheus.operator.enabled=true
        --wait
        --timeout="${KEDA_HELM_TIMEOUT:-5m}"
    )

    if ! helm "${helm_args[@]}"; then
        log_error "KEDA Helm installation failed (SCALER_BACKEND=keda)"
    else
        log_success "KEDA deployed in $KEDA_NAMESPACE"
    fi
}
