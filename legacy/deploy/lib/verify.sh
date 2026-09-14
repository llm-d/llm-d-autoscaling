#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately via llm-d project guides.
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    sleep "$DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS"

    # --- WVA
    if [ "${DEPLOY_WVA:-true}" = "true" ]; then
        log_info "Checking WVA controller pods..."
        if kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            log_success "WVA controller is running"
        else
            log_warning "WVA controller may still be starting"
            all_good=false
        fi
    else
        log_info "Skipping WVA controller verification (DEPLOY_WVA=false)"
    fi

    # --- Monitoring
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_info "Checking Prometheus..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l app.kubernetes.io/name=prometheus 2>/dev/null | grep -q Running; then
            log_success "Prometheus is running"
        else
            log_warning "Prometheus may still be starting"
        fi
    fi

    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        log_info "Checking Grafana..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l app.kubernetes.io/name=grafana 2>/dev/null | grep -q Running; then
            log_success "Grafana is running"
        else
            log_warning "Grafana may still be starting"
        fi
    fi

    # --- Scaler backend
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        if kubectl get pods -n "$KEDA_NAMESPACE" -l "$KEDA_OPERATOR_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            log_success "KEDA is running"
        else
            log_warning "KEDA may still be starting"
        fi
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Scaler backend skipped (SCALER_BACKEND=none) — assuming external metrics API is pre-installed"
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
    else
        log_warning "Some components may still be starting. Check the logs above."
    fi
}

print_summary() {
    echo ""
    echo "=========================================="
    echo " Deployment Summary"
    echo "=========================================="
    echo ""
    echo "Deployment Environment: $ENVIRONMENT"
    echo "WVA Namespace:           $WVA_NS"
    echo "LLMD Namespace:          $LLMD_NS"
    echo "Monitoring Namespace:    $MONITORING_NAMESPACE"
    echo "WVA Image:              $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo ""
    echo "Deployed Components:"
    echo "===================="
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        echo "✓ kube-prometheus-stack (Prometheus)"
    fi
    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        echo "✓ kube-prometheus-stack-grafana (Grafana)"
    fi
    if [ "$DEPLOY_WVA" = "true" ]; then
        echo "✓ WVA Controller (via Kustomize)"
    fi
    if [ "$SCALER_BACKEND" = "keda" ]; then
        echo "✓ KEDA (scaler backend, external metrics API)"
    elif [ "$SCALER_BACKEND" = "none" ]; then
        echo "- Scaler backend: skipped (SCALER_BACKEND=none, pre-installed on cluster)"
    fi
    echo ""
    echo "Next Steps:"
    echo "==========="
    echo ""
    echo "1. Deploy llm-d (EPP, gateway, ModelService) when needed:"
    echo "     See llm-d project guides at https://github.com/llm-d/llm-d"
    echo ""
    echo "2. Create an annotated HPA or KEDA ScaledObject (llm-d.ai/managed=true) so WVA discovers the variant."
    echo ""
    echo "3. View WVA logs:"
    echo "   kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo ""
    echo "4. Check external metrics API (KEDA):"
    echo "   kubectl get --raw \"/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/wva_desired_replicas\" | jq"
    echo ""
    echo "5. Port-forward Prometheus to view metrics:"
    echo "   kubectl port-forward -n $MONITORING_NAMESPACE svc/${PROMETHEUS_SVC_NAME} ${PROMETHEUS_PORT}:${PROMETHEUS_PORT}"
    echo ""
    echo "=========================================="
}
