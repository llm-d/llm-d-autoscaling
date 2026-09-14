#!/usr/bin/env bash
set -euo pipefail

 echo "Deploying WVA and llm-d infrastructure..."
echo "  ACCELERATOR_TYPE: $ACCELERATOR_TYPE"
echo "  LLMD_NS: $LLMD_NS"
echo "  WVA_NS: $WVA_NS"
echo "  WVA_IMAGE_TAG: $WVA_IMAGE_TAG"
echo "  CONTROLLER_INSTANCE: $CONTROLLER_INSTANCE"
echo "  KV_SPARE_TRIGGER: ${KV_SPARE_TRIGGER:-<default>}"
echo "  QUEUE_SPARE_TRIGGER: ${QUEUE_SPARE_TRIGGER:-<default>}"
echo "  HF token configuration: ✓"
# Base monitoring + WVA + scaler backend.
./deploy/install.sh --environment openshift

# Create llm-d namespace and HuggingFace token secret.
kubectl create namespace "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic llm-d-hf-token \
  --from-literal="HF_TOKEN=${HF_TOKEN}" \
  -n "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# OpenShift: install GAIE CRDs before standalone chart.
kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || true

# EPP / scheduler via install-epp.sh (handles llm-d-router-standalone chart +
# flowControl feature gate + tokenreview RBAC).
WVA_PROJECT="$GITHUB_WORKSPACE" \
LLMD_NS="$LLMD_NAMESPACE" \
GAIE_VERSION="$GAIE_VERSION" \
LLM_D_ROUTER_VERSION="$LLM_D_ROUTER_VERSION" \
ENVIRONMENT=openshift \
ENABLE_SCALE_TO_ZERO=true \
SKIP_CLUSTER_CRDS=true \
./deploy/install-epp.sh

# Tune saturation thresholds for CI simulator mode.
# NOTE (v0.9.0): this patch overwrites the `default` entry with only the V1-only
# spare-trigger fields, which drops the shipped `analyzers:` section and therefore
# pins this CI run to the legacy V1 (percentage-based) analyzer. This is deliberate
# for now — V1's spare-trigger tuning gives deterministic scale-up in simulator mode.
# TODO(v2): reconfigure this e2e path for the V2 (token/capacity-based) analyzer by
# patching an `analyzers:` list + scaleUpThreshold/scaleDownBoundary instead.
kubectl patch configmap wva-saturation-scaling-config \
  -n "$WVA_NAMESPACE" --type=merge \
  -p "$(printf '{"data":{"default":"kvSpareTrigger: %s\\nqueueSpareTrigger: %s\\n"}}' \
    "${KV_SPARE_TRIGGER}" "${QUEUE_SPARE_TRIGGER}")"
