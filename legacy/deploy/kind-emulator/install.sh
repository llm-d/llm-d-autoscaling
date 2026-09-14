#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler KIND Emulator Deployment Script
# Automated deployment of WVA with llm-d infrastructure on Kind cluster with llm-d-inference-sim simulator
#
# Prerequisites:
# - kubectl installed and configured
# - helm installed
# - kind installed (for cluster creation)
# - Docker installed and running
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration (Kind emulator). EPP deploy is via deploy/install-epp.sh — set LLM_D_ROUTER_VERSION / GAIE_VERSION / LLMD_NS there or in Makefile.
WVA_PROJECT=${WVA_PROJECT:-$PWD}
NAMESPACE_SUFFIX="sim"

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-$NAMESPACE_SUFFIX"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}

# Simulator image — must match defaultModelServiceSimulatorImage in test/e2e/fixtures/model_service_conventions.go
SIM_IMAGE=${SIM_IMAGE:-"ghcr.io/llm-d/llm-d-inference-sim:v0.9.0"}

# WVA Configuration
WVA_RECONCILE_INTERVAL=${WVA_RECONCILE_INTERVAL:-"60s"} # WVA controller reconcile interval - tests set 30s interval
SKIP_TLS_VERIFY=true  # Skip TLS verification in emulated environments
WVA_LOG_LEVEL="debug" # WVA log level set to debug for emulated environments
# Prometheus Configuration
PROMETHEUS_SVC_NAME=${PROMETHEUS_SVC_NAME:-"kube-prometheus-stack-prometheus"}
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL="$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"
PROMETHEUS_SECRET_NAME="prometheus-web-tls"

# KIND cluster configuration
CLUSTER_NAME=${CLUSTER_NAME:-"kind-wva-gpu-cluster"}
CLUSTER_NODES=${CLUSTER_NODES:-"3"}
CLUSTER_GPUS=${CLUSTER_GPUS:-"4"}
CLUSTER_GPU_TYPE=${CLUSTER_GPU_TYPE:-"mix"}

# Flags for deployment steps
CREATE_CLUSTER=${CREATE_CLUSTER:-false}

# Undeployment flags
DELETE_CLUSTER=${DELETE_CLUSTER:-false}

# Kind-specific prerequisites
REQUIRED_TOOLS=("kind")

# Function to check Kind emulator-specific prerequisites
# - checks for kind, kubectl and helm
# - creates Kind cluster if CREATE_CLUSTER=true, otherwise tries to use an existing cluster
# - loads WVA image into Kind cluster
check_specific_prerequisites() {
    log_info "Checking Kubernetes-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including Kubernetes-specific ones)
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
    fi
    
    # Create or use existing KIND cluster
    if [ "$CREATE_CLUSTER" = "true" ]; then
        # Check if the specific cluster exists - if so, delete and recreate
        if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_info "KIND cluster '${CLUSTER_NAME}' already exists, tearing it down and recreating..."
            kind delete cluster --name "${CLUSTER_NAME}"
        else 
            log_info "KIND cluster '${CLUSTER_NAME}' not found, creating it..."
        fi
        create_kind_cluster

    else
        log_info "Cluster creation skipped (CREATE_CLUSTER=false)"
        # Verify the Kind cluster exists
        if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_error "KIND cluster '${CLUSTER_NAME}' not found and CREATE_CLUSTER=false"
        fi
        # Set kubectl context to the Kind cluster
        kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    fi
    # Verify kubectl can connect to the cluster
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Failed to connect to KIND cluster '${CLUSTER_NAME}'"
    fi
    log_success "Using KIND cluster '${CLUSTER_NAME}'"

    # Load the WVA image only when the controller is part of this deployment.
    if [ "${DEPLOY_WVA:-true}" = "true" ]; then
        load_image
    else
        log_info "Skipping WVA image load (DEPLOY_WVA=false)"
    fi

    # Pre-load the simulator image so tests don't pull it cold (avoids PodReadyTimeout).
    load_sim_image

    log_success "All Kind emulated deployment prerequisites met"
}

# Creates Kind cluster using `setup.sh` script for GPU emulation
create_kind_cluster() {
    log_info "Creating KIND cluster with GPU emulation..."
    
    # Check if cluster already exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_warning "KIND cluster '${CLUSTER_NAME}' already exists"
        log_info "Deleting existing cluster to create a fresh one..."
        kind delete cluster --name "${CLUSTER_NAME}"
    fi
    
    # Run setup.sh to create the cluster
    local SETUP_SCRIPT="${WVA_PROJECT}/deploy/kind-emulator/setup.sh"
    
    if [ ! -f "$SETUP_SCRIPT" ]; then
        log_error "Setup script not found at: $SETUP_SCRIPT"
        exit 1
    fi
    
    log_info "Running setup script with: cluster=$CLUSTER_NAME, nodes=$CLUSTER_NODES, gpus=$CLUSTER_GPUS, type=$CLUSTER_GPU_TYPE"
    bash "$SETUP_SCRIPT" -c "${CLUSTER_NAME}" -n "$CLUSTER_NODES" -g "$CLUSTER_GPUS" -t "$CLUSTER_GPU_TYPE"
    
    # Ensure kubectl context is set to the new cluster
    kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    
    log_success "KIND cluster '${CLUSTER_NAME}' created successfully"
}

# Loads WVA image into the Kind cluster.
# When pulling from a registry, we pull a single platform (KIND_IMAGE_PLATFORM) to avoid
# "content digest ... not found" errors from kind load (multi-platform manifests reference
# blobs not included in the export stream; see kubernetes-sigs/kind#3795, #3845).
load_image() {
    log_info "Loading WVA image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' into KIND cluster..."
    
    # If WVA_IMAGE_PULL_POLICY is IfNotPresent, skip pulling and use local image only
    if [ "$WVA_IMAGE_PULL_POLICY" = "IfNotPresent" ]; then
        log_info "Using local image only (WVA_IMAGE_PULL_POLICY=IfNotPresent)"
        
        # Check if the image exists locally
        if ! docker image inspect "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" >/dev/null 2>&1; then
            log_error "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' not found locally - Please build the image first (e.g., 'make docker-build IMG=$WVA_IMAGE_REPO:$WVA_IMAGE_TAG')"
        else
            log_success "Found local image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG'"
        fi
    else
        # Pull a single-platform image so kind load does not hit "content digest not found"
        # (multi-platform manifests can reference blobs that are not in the docker save stream).
        local platform="${KIND_IMAGE_PLATFORM:-}"
        if [ -z "$platform" ]; then
            case "$(uname -m)" in
                aarch64|arm64) platform="linux/arm64" ;;
                *) platform="linux/amd64" ;;
            esac
        fi
        log_info "Pulling single-platform image for KIND (platform=$platform) to avoid load errors..."
        if ! docker pull --platform "$platform" "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG"; then
            log_warning "Failed to pull image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' (platform=$platform)"
            log_info "Attempting to use existing local image..."
            if ! docker image inspect "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" >/dev/null 2>&1; then
                log_error "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' not found locally - Please build or pull the image"
                exit 1
            fi
        else
            log_success "Pulled image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' (platform=$platform)"
        fi
    fi
    
    # Load the image into the KIND cluster via the shared helper.
    # Tries `kind load docker-image` first, falls back to crictl-per-node for the
    # containerd image store issue (kubernetes-sigs/kind#3795).
    local full_image="$WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    if ! _load_into_kind "$full_image"; then
        log_error "Failed to load WVA image '$full_image' into KIND cluster '$CLUSTER_NAME'"
        exit 1
    fi
}

# _load_into_kind FULL_IMAGE
# Loads a pre-pulled image into the KIND cluster: tries `kind load docker-image` first,
# then falls back to crictl-per-node for the containerd image store issue
# (kubernetes-sigs/kind#3795). Returns 0 on success, 1 on failure.
_load_into_kind() {
    local full_image="$1"
    local load_stderr
    if load_stderr="$(kind load docker-image "$full_image" --name "$CLUSTER_NAME" 2>&1)"; then
        log_success "Image '$full_image' loaded into KIND cluster '$CLUSTER_NAME'"
        return 0
    fi

    if ! echo "$load_stderr" | grep -qiE "docker save|multi-?platform|manifest|content digest|no such image|not found"; then
        log_warning "'kind load docker-image' failed for '$full_image': $load_stderr"
        return 1
    fi

    log_warning "'kind load docker-image' failed (containerd image store issue) — falling back to pulling directly into KIND nodes"
    local nodes
    nodes="$(kind get nodes --name "$CLUSTER_NAME")" || return 1
    for node in $nodes; do
        if ! docker exec "$node" crictl pull "$full_image" 2>&1; then
            log_warning "Failed to pre-pull '$full_image' on node '$node'"
            return 1
        fi
    done
    log_success "Image '$full_image' pulled directly into KIND cluster '$CLUSTER_NAME' nodes"
    return 0
}

# Pre-loads the llm-d-inference-sim image into the KIND cluster so tests that create
# model service Deployments don't pull it cold and hit PodReadyTimeout.
load_sim_image() {
    log_info "Pre-loading simulator image '$SIM_IMAGE' into KIND cluster..."

    # Digest-pinned images can be imported incorrectly by `kind load docker-image`
    # and left behind as broken containerd import-* references. Pull them directly
    # into each KIND node's containerd store instead.
    if [[ "$SIM_IMAGE" == *@sha256:* ]]; then
        log_info "Simulator image is digest-pinned; pulling directly into KIND node containerd..."

        local nodes
        if ! nodes="$(kind get nodes --name "$CLUSTER_NAME")"; then
            log_warning "Failed to list KIND nodes for simulator image preload"
            return
        fi

        local node
        for node in $nodes; do
            if ! docker exec "$node" crictl pull "$SIM_IMAGE"; then
                log_warning "Failed to pre-pull simulator image '$SIM_IMAGE' on node '$node'"
                return
            fi
        done

        log_success "Simulator image '$SIM_IMAGE' pulled directly into KIND cluster '$CLUSTER_NAME' nodes"
        return
    fi

    local platform="${KIND_IMAGE_PLATFORM:-}"
    if [ -z "$platform" ]; then
        case "$(uname -m)" in
            aarch64|arm64) platform="linux/arm64" ;;
            *) platform="linux/amd64" ;;
        esac
    fi

    if ! docker pull --platform "$platform" "$SIM_IMAGE"; then
        log_warning "Failed to pull simulator image '$SIM_IMAGE' — tests may be slow on first run"
        return
    fi

    _load_into_kind "$SIM_IMAGE" ||
        log_warning "Failed to load simulator image into KIND cluster — tests may be slow on first run"
}

KUBE_LIKE_VALUES_DEV_IF_PRESENT=true

_wva_deploy_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
# shellcheck source=deploy_prometheus_kube_stack.sh
source "${_wva_deploy_lib}/deploy_prometheus_kube_stack.sh"
# shellcheck source=kube_like_adapter.sh
source "${_wva_deploy_lib}/kube_like_adapter.sh"

#### REQUIRED FUNCTION used by deploy/install.sh ####
delete_namespaces() {
    delete_namespaces_kube_like
    if [ "$DELETE_CLUSTER" = true ]; then
        delete_kind_cluster
    fi
}

# Deletes the Kind cluster
# Used when DELETE_CLUSTER=true by delete_namespaces()
delete_kind_cluster() {
    log_info "Deleting KIND cluster '${CLUSTER_NAME}'..."
    
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        kind delete cluster --name "${CLUSTER_NAME}"
        log_success "KIND cluster '${CLUSTER_NAME}' deleted"
    else
        log_warning "KIND cluster '${CLUSTER_NAME}' not found"
    fi
}
