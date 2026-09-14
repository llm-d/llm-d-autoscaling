#!/usr/bin/env bash
#
# EPP infrastructure setup for all environments (kind-emulator, kubernetes, openshift).
# Installs Gateway API CRDs, GAIE CRDs, and the llm-d-router-standalone chart (EPP + InferencePool).
# EPP helm values are vendored in deploy/lib/epp-base.values.yaml and
# deploy/lib/epp-optimized-baseline.values.yaml — no network fetch required.
#
# Usage:
#   LLM_D_ROUTER_VERSION=v0.9.0 GAIE_VERSION=v1.5.0 LLMD_NS=llm-d-sim ./deploy/install-epp.sh
#

set -e
set -o pipefail

WVA_PROJECT=${WVA_PROJECT:-$PWD}
LLM_D_ROUTER_VERSION=${LLM_D_ROUTER_VERSION:-v0.9.0}
GAIE_VERSION=${GAIE_VERSION:-v1.5.0}
LLMD_NS=${LLMD_NS:-llm-d-sim}
ENVIRONMENT=${ENVIRONMENT:-kind-emulator}
UNDEPLOY=${UNDEPLOY:-false}
ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-false}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/infra_epp.sh
source "$DEPLOY_LIB_DIR/infra_epp.sh"

if [ "$UNDEPLOY" = "true" ]; then
    undeploy_epp
else
    deploy_epp
fi
