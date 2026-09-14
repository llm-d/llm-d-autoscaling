#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler OpenShift Environment-Specific Configuration
# This script provides OpenShift-specific functions and variable overrides
# It is sourced by the main install.sh script
# Note: it is NOT meant to be executed directly
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

#
# OpenShift-specific Prometheus Configuration
# Note: overriding defaults from common script
#
PROMETHEUS_SVC_NAME="thanos-querier"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.openshift-monitoring.svc.cluster.local"
PROMETHEUS_PORT="9091"
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
MONITORING_NAMESPACE="openshift-user-workload-monitoring"
PROMETHEUS_SECRET_NAME="thanos-querier-tls"
PROMETHEUS_SECRET_NS="openshift-monitoring"
DEPLOY_PROMETHEUS=false  # OpenShift uses built-in monitoring stack

# OpenShift-specific prerequisites
REQUIRED_TOOLS=("oc")

# TLS verification enabled by default on OpenShift
SKIP_TLS_VERIFY=false

#### REQUIRED FUNCTION used by deploy/install.sh ####
check_specific_prerequisites() {
    log_info "Checking OpenShift-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including OpenShift-specific ones)
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Please install the missing tools and try again"
        exit 1
    fi
    
    # Check OpenShift connection
    if ! oc whoami &> /dev/null; then
        log_error "Not logged into OpenShift cluster. Please run 'oc login' first"
        exit 1
    fi
    
    log_success "All OpenShift prerequisites met"
    log_info "Connected to OpenShift as: $(oc whoami)"
    log_info "Current project: $(oc project -q)"
}

materialize_namespace() {
    local ns=$1
    if [ "$ns" = "$WVA_NS" ]; then
        kubectl create namespace "$ns" --dry-run=client -o yaml | \
            kubectl label --local -f - openshift.io/user-monitoring=true -o yaml | \
            kubectl apply -f -
    else
        kubectl create namespace "$ns"
    fi
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
create_namespaces() {
    create_namespaces_shared_loop
}

find_thanos_url() {
    log_info "Finding Thanos querier URL..."

    local thanos_svc
    thanos_svc=$(kubectl get svc -n "$PROMETHEUS_SECRET_NS" "$PROMETHEUS_SVC_NAME" -o jsonpath='{.metadata.name}' 2>/dev/null || echo "")

    # Set PROMETHEUS_URL if Thanos service is found
    if [ -n "$thanos_svc" ]; then
        # Extract the actual service name and port from the service
        local svc_name
        local svc_port
        svc_name=$(kubectl get svc -n "$PROMETHEUS_SECRET_NS" "$PROMETHEUS_SVC_NAME" -o jsonpath='{.metadata.name}' 2>/dev/null)
        svc_port=$(kubectl get svc -n "$PROMETHEUS_SECRET_NS" "$PROMETHEUS_SVC_NAME" -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null)
        
        # Fallback to default port if not found or try first port
        if [ -z "$svc_port" ]; then
            svc_port=$(kubectl get svc -n "$PROMETHEUS_SECRET_NS" "$PROMETHEUS_SVC_NAME" -o jsonpath='{.spec.ports[0].port}' 2>/dev/null)
        fi

        if [ -z "$svc_port" ]; then
            svc_port="9091"
            log_warning "Could not extract port from service, using default: $svc_port"
        fi
        
        # Construct the full URL
        PROMETHEUS_URL="https://${svc_name}.${PROMETHEUS_SECRET_NS}.svc.cluster.local:${svc_port}"
        log_success "Found Thanos querier: $PROMETHEUS_URL (port: $svc_port)"
    else
        log_error "Thanos querier service not found in openshift-monitoring namespace - using default URL: $PROMETHEUS_URL"
    fi
    
    export PROMETHEUS_URL
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# OpenShift uses existing monitoring stack (Thanos/Prometheus)
deploy_prometheus_stack() {
    log_info "Using OpenShift built-in monitoring (Thanos)..."
    find_thanos_url
    log_success "OpenShift monitoring stack is available"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
deploy_wva_prerequisites() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    extract_openshift_prometheus_ca

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

    log_success "WVA prerequisites deployed"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# OpenShift uses built-in monitoring, nothing to undeploy
undeploy_prometheus_stack() {
    log_info "OpenShift uses built-in monitoring stack (no cleanup needed)"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# Namespaces are not deleted on OpenShift to avoid removing user projects
delete_namespaces() {
    log_info "Not deleting namespaces on OpenShift to avoid removing user projects"
}

# Environment-specific functions are now sourced by the main install.sh script
# Do not call functions directly when sourced

