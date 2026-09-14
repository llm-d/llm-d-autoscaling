#!/usr/bin/env bash
#
# Shared scaler-backend orchestration for deploy/install.sh.
# Requires vars: SCALER_BACKEND.
# Requires funcs: deploy_keda(), log_info(), log_error().
#

deploy_scaler_backend() {
    # Deploy scaler backend: KEDA or none.
    # OpenShift: KEDA is never Helm-installed (platform-managed); see deploy_keda in scaler_runtime.sh.
    # Kubernetes: deploy_keda skips Helm by default (cluster-managed); KEDA_HELM_INSTALL=true enables Helm.
    # kind-emulator: Helm when needed; shared-cluster guard uses ClusterRole keda-operator when Helm is used.
    # Use SCALER_BACKEND=none on clusters that already have an external metrics API
    # (e.g. llmd benchmark clusters with KEDA pre-installed) to avoid conflicts.
    if [ "$SCALER_BACKEND" = "keda" ]; then
        deploy_keda
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Skipping scaler backend deployment (SCALER_BACKEND=none)"
        log_info "Assumes an external metrics API (e.g. KEDA) is already installed on the cluster"
    else
        log_error "Unsupported SCALER_BACKEND: $SCALER_BACKEND (supported: keda, none)"
    fi
}
