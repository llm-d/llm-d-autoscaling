#!/bin/bash
set -euo pipefail

# Cleanup script to remove all resources from pd-test namespace

NAMESPACE="pd-test"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if namespace exists
if ! kubectl get namespace "${NAMESPACE}" > /dev/null 2>&1; then
    log_warn "Namespace ${NAMESPACE} does not exist. Nothing to clean up."
    exit 0
fi

log_info "Cleaning up ${NAMESPACE} namespace..."

# Get all resources
log_info "Listing resources to be deleted:"
kubectl get all -n "${NAMESPACE}" || true

# Delete all deployments, daemonsets, statefulsets
log_info "Deleting deployments..."
kubectl delete deployments --all -n "${NAMESPACE}" --ignore-not-found=true

log_info "Deleting daemonsets..."
kubectl delete daemonsets --all -n "${NAMESPACE}" --ignore-not-found=true

log_info "Deleting statefulsets..."
kubectl delete statefulsets --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete jobs
log_info "Deleting jobs..."
kubectl delete jobs --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete services
log_info "Deleting services..."
kubectl delete services --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete PVCs
log_info "Deleting PVCs..."
kubectl delete pvc --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete configmaps
log_info "Deleting configmaps..."
kubectl delete configmaps --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete secrets
log_info "Deleting secrets..."
kubectl delete secrets --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete service accounts
log_info "Deleting service accounts..."
kubectl delete serviceaccounts --all -n "${NAMESPACE}" --ignore-not-found=true

# Delete RBAC resources
log_info "Deleting RBAC resources..."
kubectl delete roles --all -n "${NAMESPACE}" --ignore-not-found=true
kubectl delete rolebindings --all -n "${NAMESPACE}" --ignore-not-found=true

# Wait for pods to terminate
log_info "Waiting for pods to terminate..."
kubectl wait --for=delete pod --all -n "${NAMESPACE}" --timeout=60s || log_warn "Some pods did not terminate gracefully"

# Verify cleanup
REMAINING=$(kubectl get all -n "${NAMESPACE}" 2>/dev/null | wc -l)
if [ "${REMAINING}" -eq 1 ]; then
    log_info "Cleanup complete!"
    log_info "Remaining resources in ${NAMESPACE}:"
    kubectl get all -n "${NAMESPACE}" || true
else
    log_warn "Some resources may still exist in ${NAMESPACE}"
    kubectl get all -n "${NAMESPACE}" || true
fi

log_info "Done!"
