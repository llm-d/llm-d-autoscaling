#!/usr/bin/env bash
#
# CLI help and argument parsing for deploy/install.sh.
# Requires vars: WVA_IMAGE_REPO, WVA_IMAGE_TAG, COMPATIBLE_ENV_LIST.
# Requires funcs: log_info/log_warning/log_error, containsElement().
#

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Bootstrap WVA (optional), monitoring, and scaler backend on a Kubernetes or OpenShift cluster.
For llm-d (gateway, EPP, ModelService), see the llm-d project's installation guides.

Options:
  -i, --wva-image IMAGE        Container image for WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -u, --undeploy               Undeploy WVA, monitoring, and scaler backend
  -e, --environment            kubernetes | openshift | kind-emulator (default: kubernetes)
  -h, --help                   Show this help and exit

Deprecated (ignored by install.sh; Helm chart removed):
  --release-name NAME          Helm release name (no-op; WVA now installed via Kustomize)
  --accelerator TYPE           Same as ACCELERATOR_TYPE env

Environment Variables:
  IMG                          WVA image as repo:tag (alternative to -i)
  SKIP_CHECKS                  Skip kubectl/helm/git prerequisite check (default: false). Install scripts are non-interactive and fail fast on errors.
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_OPERATIONAL_DASHBOARD Deploy Grafana and operational dashboard (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  SCALER_BACKEND               keda (default) or none
  KEDA_HELM_INSTALL            Install KEDA via Helm on kubernetes when true (default: false)
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)
  LLMD_NS                      Namespace WVA watches for workloads (default: llm-d-optimized-baseline)

Examples:
  $(basename "$0")

  IMG=registry.example.com/wva:dev $(basename "$0") -e kind-emulator

  $(basename "$0") -e openshift
EOF
}

parse_args() {
  if [[ -n "$IMG" ]]; then
    log_info "Detected IMG environment variable: $IMG"
    if [[ "$IMG" == *":"* ]]; then
      IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$IMG"
    else
      log_warning "IMG has wrong format, using default image"
    fi
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -i|--wva-image)
        if [[ "$2" == *":"* ]]; then
          IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$2"
        else
          WVA_IMAGE_REPO="$2"
        fi
        shift 2
        ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
        ;;
      --accelerator)
        export ACCELERATOR_TYPE="$2"
        shift 2
        ;;
      --release-name)
        # Legacy CI/Helm — install.sh no longer installs via Helm; value is ignored.
        shift 2
        ;;
      -h|--help)              print_help; exit 0 ;;
      *)
        echo "Error: Unknown option: $1" >&2
        print_help
        exit 1
        ;;
    esac
  done
}
