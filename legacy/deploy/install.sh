#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler infrastructure bootstrap: optional WVA controller,
# Prometheus monitoring stack, and scaler backend (KEDA).
#
# For llm-d (gateway, EPP, ModelService), see the llm-d project guides at https://github.com/llm-d/llm-d.
# For EPP setup (all environments), run deploy/install-epp.sh after this script.
#
# Prerequisites:
# - kubectl and helm installed
# - Cluster credentials configured
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
WVA_PROJECT=${WVA_PROJECT:-$PWD}

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-optimized-baseline"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# WVA Configuration (required when DEPLOY_WVA=true)
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/llm-d-workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"latest"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-"false"}
WVA_LOG_LEVEL=${WVA_LOG_LEVEL:-"info"}
# Optional: multi-controller isolation (sets controller_instance on metrics / selectors when non-empty).
CONTROLLER_INSTANCE=${CONTROLLER_INSTANCE:-""}

ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-true}

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
DEPLOY_OPERATIONAL_DASHBOARD=${DEPLOY_OPERATIONAL_DASHBOARD:-true}
DEPLOY_ALERTING_RULES=${DEPLOY_ALERTING_RULES:-false}
DEPLOY_WVA=${DEPLOY_WVA:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}
WVA_METRICS_SECURE=${WVA_METRICS_SECURE:-true}

# Scaler backend: keda | none.
# - keda on kubernetes: expects cluster CRD unless KEDA_HELM_INSTALL=true (then this script installs Helm KEDA).
# - keda on openshift: platform-managed KEDA only (no Helm install from this script).
# - none: skip scaler install (cluster already provides external metrics).
SCALER_BACKEND=${SCALER_BACKEND:-keda}
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}
# Pinned for reproducible Helm installs (used when deploy_keda actually runs helm upgrade).
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-2.19.0}
# On kubernetes: default false (cluster-managed KEDA); kind-emulator flows often set true or use cluster path.
KEDA_HELM_INSTALL=${KEDA_HELM_INSTALL:-false}

# LeaderWorkerSet. Set true when LWS tests run (e.g. full e2e suite). Defaults false so smoke and benchmarks skip it.
DEPLOY_LWS=${DEPLOY_LWS:-false}
LWS_NAMESPACE=${LWS_NAMESPACE:-"lws-system"}
LWS_CHART_VERSION=${LWS_CHART_VERSION:-"0.8.0"}

# Environment-related variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git")
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

PRODUCTION_ENV_LIST=("openshift")

# Shared deploy helpers
# shellcheck source=lib/verify.sh
source "$DEPLOY_LIB_DIR/verify.sh"
# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/constants.sh
source "$DEPLOY_LIB_DIR/constants.sh"
# shellcheck source=lib/wait_helpers.sh
source "$DEPLOY_LIB_DIR/wait_helpers.sh"
# shellcheck source=lib/cli.sh
source "$DEPLOY_LIB_DIR/cli.sh"
# shellcheck source=lib/prereqs.sh
source "$DEPLOY_LIB_DIR/prereqs.sh"
# shellcheck source=lib/infra_scaler_backend.sh
source "$DEPLOY_LIB_DIR/infra_scaler_backend.sh"
# shellcheck source=lib/scaler_runtime.sh
source "$DEPLOY_LIB_DIR/scaler_runtime.sh"
# shellcheck source=lib/infra_wva.sh
source "$DEPLOY_LIB_DIR/infra_wva.sh"
# shellcheck source=lib/infra_epp.sh
source "$DEPLOY_LIB_DIR/infra_epp.sh"
# shellcheck source=lib/infra_monitoring.sh
source "$DEPLOY_LIB_DIR/infra_monitoring.sh"
# shellcheck source=lib/cleanup.sh
source "$DEPLOY_LIB_DIR/cleanup.sh"
# shellcheck source=lib/install_core.sh
source "$DEPLOY_LIB_DIR/install_core.sh"

UNDEPLOY=${UNDEPLOY:-false}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}

# Orchestration lives in deploy/lib/install_core.sh (keeps this entrypoint to variable defaults + sourcing only).
main "$@"
