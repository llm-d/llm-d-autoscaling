#!/usr/bin/env bash
#
# Shared WVA-specific deployment helpers.
# Requires vars: WVA_NS, LLMD_NS, MONITORING_NAMESPACE, WVA_PROJECT,
# chart/image values, env mode lists.
# Requires funcs: log_info/log_warning/log_success/log_error, containsElement().
#

set_tls_verification() {
    log_info "Setting TLS verification..."

    # Auto-detect TLS verification setting if not specified
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            SKIP_TLS_VERIFY="true"
            log_info "Emulated environment detected - enabling TLS skip verification for self-signed certificates"
    else
        case "$ENVIRONMENT" in
            "kubernetes")
                # TODO: change to false when Kubernetes support for TLS verification is enabled
                SKIP_TLS_VERIFY="true"
                log_info "Kubernetes cluster - enabling TLS skip verification for self-signed certificates"
                ;;
            "openshift")
                # For OpenShift, we can use proper TLS verification since we have the Service CA
                # However, defaulting to true for now to match current behavior
                # TODO: Set to false once Service CA certificate extraction is fully validated
                SKIP_TLS_VERIFY="true"
                log_info "OpenShift cluster - TLS verification setting: $SKIP_TLS_VERIFY"
                ;;
            *)
                SKIP_TLS_VERIFY="true"
                log_warning "Unknown environment - enabling TLS skip verification for self-signed certificates"
                ;;
        esac
    fi

    export SKIP_TLS_VERIFY

    log_success "Successfully set TLS verification to: $SKIP_TLS_VERIFY"
}

set_wva_logging_level() {
    log_info "Setting WVA logging level..."

    # Set logging level based on environment
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi

    export WVA_LOG_LEVEL
    log_success "WVA logging level set to: $WVA_LOG_LEVEL"
    echo ""
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"

    # Select the Kustomize overlay by install scope. OpenShift installs use the
    # namespace-scoped overlay; Kubernetes installs use the cluster-scoped overlay.
    local kustomize_overlay
    if [ "$ENVIRONMENT" = "openshift" ]; then
        kustomize_overlay="$(cd "$WVA_PROJECT/config/overlays/namespace-scoped/openshift" && pwd)"
    else
        kustomize_overlay="$(cd "$WVA_PROJECT/config/overlays/cluster-scoped/kubernetes" && pwd)"
    fi

    # Build a throw-away overlay that pins the image without modifying tracked files.
    # Symlink the base overlay so kustomization.yaml can reference it with a relative
    # path — Kustomize rejects absolute paths in resources.
    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    trap 'rm -rf "$tmp_overlay"' EXIT

    ln -s "$kustomize_overlay" "$tmp_overlay/base"

    # Symlink prometheus-alerts component if needed
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        ln -s "$WVA_PROJECT/config/components/prometheus-alerts" "$tmp_overlay/prometheus-alerts"
    fi

    # config/base/manager/kustomization.yaml transforms the base image name "controller"
    # to the published release image. The overlay must match the POST-transform name.
    local base_image
    base_image=$(grep 'newName:' "$WVA_PROJECT/config/base/manager/kustomization.yaml" | awk '{print $2}' | head -1)
    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
images:
- name: $base_image
  newName: $WVA_IMAGE_REPO
  newTag: "$WVA_IMAGE_TAG"
EOF

    # Include Prometheus alerting rules component if requested
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        log_info "Including Prometheus alerting rules component..."
        printf 'components:\n' >> "$tmp_overlay/kustomization.yaml"
        printf -- '- ./prometheus-alerts\n' >> "$tmp_overlay/kustomization.yaml"
    fi

    # On OpenShift shared clusters, all ClusterRoleBindings share the same fixed
    # names. Concurrent deployments overwrite each other's subject namespace.
    # Append name-rename patches so each namespace gets its own uniquely named CRBs.
    if [ "$ENVIRONMENT" = "openshift" ]; then
        local ns_hash
        ns_hash="$(printf '%s' "${WVA_NS}" | sha256sum | cut -c1-8)"
        printf 'patches:\n' >> "$tmp_overlay/kustomization.yaml"
        for crb in \
            wva-epp-metrics-reader-role-binding \
            wva-manager-cluster-monitoring-view \
            wva-manager-rolebinding \
            wva-metrics-auth-rolebinding \
            wva-metrics-reader-rolebinding \
            wva-prometheus-cluster-monitoring-view; do
            cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${crb}-${ns_hash}
  target:
    kind: ClusterRoleBinding
    name: ${crb}
EOF
        done
    fi


    log_info "Applying Kustomize overlay: $kustomize_overlay"
    kubectl apply -k "$tmp_overlay"

    # Clean up a previously-deployed PrometheusRule when alerting rules are disabled.
    # The `get` guard keeps the common path quiet (only logs/acts when a rule actually
    # exists) and tolerates the PrometheusRule CRD being absent — `--ignore-not-found`
    # only covers a missing object, not a missing resource type.
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "false" ] && \
        kubectl get prometheusrule controller-manager-alerts -n "$WVA_NS" &> /dev/null; then
        log_info "DEPLOY_ALERTING_RULES=false - removing existing PrometheusRule controller-manager-alerts"
        kubectl delete prometheusrule controller-manager-alerts -n "$WVA_NS" --ignore-not-found=true
    fi

    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "Enabling scale-to-zero in WVA ConfigMap (ENABLE_SCALE_TO_ZERO=true)..."
        # Flip WVA_SCALE_TO_ZERO inside data["config.yaml"] — the file the
        # controller actually reads (mounted at /etc/wva/config.yaml).
        # Patch only that single field so we don't echo back server-managed
        # metadata (resourceVersion, managedFields, etc.). Use yq so the edit
        # is idempotent and tolerates quoting/whitespace variance — sed would
        # silently fail to match e.g. unquoted or single-quoted values.
        local current_config
        current_config=$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
            -o jsonpath='{.data.config\.yaml}')
        if [ -z "$current_config" ]; then
            log_error "ConfigMap wva-manager-config has no data['config.yaml'] key"
        fi

        local updated_config
        updated_config=$(echo "$current_config" | yq '.WVA_SCALE_TO_ZERO = "true"')

        kubectl patch configmap wva-manager-config -n "$WVA_NS" --type=merge \
            -p "$(jq -n --arg cfg "$updated_config" '{data:{"config.yaml":$cfg}}')"
    fi

    # Wait for WVA to be ready
    log_info "Waiting for WVA controller to be ready..."
    if kubectl wait --for=condition=Ready pod -l "$WVA_CONTROLLER_LABEL_SELECTOR" -n "$WVA_NS" --timeout=60s; then
        :
    else
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    fi

    log_success "WVA deployment complete"
}

# Shared namespace creation loop for deploy/*/install.sh environment plugins.
# Platform adapter provides materialize_namespace(ns), then calls this helper.
create_namespaces_shared_loop() {
    log_info "Creating namespaces..."

    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        local ns_exists=false
        local ns_terminating=false

        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status
            ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi

        materialize_namespace "$ns"
        log_success "Namespace $ns created"
    done
}

delete_namespaces_kube_like() {
    log_info "Deleting namespaces..."

    for ns in $LLMD_NS $WVA_NS $MONITORING_NAMESPACE; do
        if kubectl get namespace $ns &> /dev/null; then
            if [[ "$ns" == "$WVA_NS" && "$DEPLOY_WVA" == "false" ]] || [[ "$ns" == "$MONITORING_NAMESPACE" && "$DEPLOY_PROMETHEUS" == "false" ]]; then
                log_info "Skipping deletion of namespace $ns as it was not deployed"
            else
                log_info "Deleting namespace $ns..."
                kubectl delete namespace $ns 2>/dev/null || \
                    log_warning "Failed to delete namespace $ns"
            fi
        fi
    done

    log_success "Namespaces deleted"
}

# Shared WVA prerequisites for Kubernetes-like environments.
# Optional:
#   - KUBE_LIKE_VALUES_DEV_IF_PRESENT=true|false (defaults false)
deploy_wva_prerequisites_kube_like() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # InferencePool CRDs must exist before the controller starts or its initial
    # watch fails. install-epp.sh installs them again later (idempotent).
    install_inference_crds

    # Extract Prometheus CA certificate (used by the Prometheus Adapter scaler backend).
    log_info "Extracting Prometheus TLS certificate"
    kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"

    # LeaderWorkerSet (WVA dependency; see upstream chart / #910).
    if [ "${DEPLOY_LWS:-true}" = "true" ]; then
        if kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io &> /dev/null; then
            log_info "LeaderWorkerSet CRD already installed, skipping LWS deployment"
        else
            log_info "Installing LeaderWorkerSet version ${LWS_CHART_VERSION} into ${LWS_NAMESPACE} namespace"
            helm upgrade -i lws oci://registry.k8s.io/lws/charts/lws \
                --version="${LWS_CHART_VERSION}" \
                --namespace "${LWS_NAMESPACE}" \
                --create-namespace \
                --wait --timeout 300s
        fi
    else
        log_info "Skipping LeaderWorkerSet installation (DEPLOY_LWS=false)"
    fi

    log_success "WVA prerequisites complete"
}

# OpenShift-specific CA extraction used by deploy/openshift/install.sh.
extract_openshift_prometheus_ca() {
    # Extract OpenShift Service CA certificate for Thanos verification
    # Note: For OpenShift service certificates, we need the Service CA that signed the server cert,
    # not the server certificate itself. The server cert is in thanos-querier-tls, but we need the CA.
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"

    # Method 1: Extract Service CA from openshift-service-ca.crt ConfigMap (preferred)
    # This is the actual CA certificate that signs OpenShift service certificates
    if kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
        log_info "Extracting Service CA from openshift-service-ca.crt ConfigMap"
        kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-service-ca.crt ConfigMap"
        fi
    fi

    # Method 2: Extract Service CA from openshift-config namespace
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_info "Trying to extract Service CA from openshift-config namespace"
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-config namespace"
        fi
    fi

    # Method 3: Fallback to thanos-querier-tls secret
    # Note: This extracts the server certificate, which may work if the cert chain includes the CA
    # but it's not ideal - we should use the Service CA instead.
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Service CA not found, falling back to server certificate from thanos-querier-tls"
        log_warning "This may cause TLS verification issues - Service CA is preferred"
        if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
            log_info "Extracting certificate from thanos-querier-tls secret"
            kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
            if [ -s "$PROM_CA_CERT_PATH" ]; then
                log_success "Extracted certificate from thanos-querier-tls secret"
            fi
        fi
    fi

    # Verify we have a valid certificate
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "Failed to extract OpenShift Service CA certificate"
        log_error "Tried: openshift-service-ca.crt ConfigMap, openshift-config ConfigMap, and thanos-querier-tls secret"
        exit 1
    fi

    # Verify the certificate is valid PEM format
    if ! openssl x509 -in "$PROM_CA_CERT_PATH" -text -noout &> /dev/null; then
        log_warning "Certificate file may not be in valid PEM format, but continuing..."
        log_warning "If TLS errors occur, verify the certificate format is correct"
    else
        # Log certificate details for debugging
        local cert_subject
        cert_subject=$(openssl x509 -in "$PROM_CA_CERT_PATH" -noout -subject 2>/dev/null | sed 's/subject=//' || echo "unknown")
        log_info "Certificate subject: $cert_subject"
    fi
}
