#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler Kubernetes Environment-Specific Configuration
# This script provides Kubernetes-specific functions and variable overrides
# It is sourced by the main install.sh script
# Note: it is NOT meant to be executed directly
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

#
# Kubernetes-specific Prometheus Configuration
# Note: overriding defaults from common script
#
PROMETHEUS_SVC_NAME="kube-prometheus-stack-prometheus"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-"true"}
DEPLOY_OPERATIONAL_DASHBOARD=${DEPLOY_OPERATIONAL_DASHBOARD:-"true"}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-"true"}

check_specific_prerequisites() {
    log_info "No Kubernetes-specific prerequisites needed other than common prerequisites"
}

# Deploy WVA prerequisites for Kubernetes
KUBE_LIKE_VALUES_DEV_IF_PRESENT=false

_wva_deploy_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
# shellcheck source=deploy_prometheus_kube_stack.sh
source "${_wva_deploy_lib}/deploy_prometheus_kube_stack.sh"
# shellcheck source=kube_like_adapter.sh
source "${_wva_deploy_lib}/kube_like_adapter.sh"

delete_namespaces() {
    delete_namespaces_kube_like
}

# Environment-specific functions are now sourced by the main install.sh script
# Do not call functions directly when sourced

