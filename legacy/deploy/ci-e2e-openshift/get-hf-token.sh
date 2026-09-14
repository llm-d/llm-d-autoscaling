#!/usr/bin/env bash
set -euo pipefail

echo "Reading HF token from cluster secret llm-d-hf-token in default namespace..."
# The llm-d-hf-token secret exists in the default namespace on the cluster
# Check secret existence separately from key retrieval for better error messages
if ! kubectl get secret llm-d-hf-token -n default &>/dev/null; then
  echo "::error::Secret 'llm-d-hf-token' not found in default namespace"
  echo "::error::Please ensure the HF token secret exists on the cluster"
  exit 1
fi
# Read the token and mask it in logs
HF_TOKEN=$(kubectl get secret llm-d-hf-token -n default -o jsonpath='{.data.HF_TOKEN}' | base64 -d)
if [ -z "$HF_TOKEN" ]; then
  echo "::error::Secret 'llm-d-hf-token' exists but 'HF_TOKEN' key is empty or missing"
  exit 1
fi
# Mask the token in workflow logs
echo "::add-mask::$HF_TOKEN"
# Export for subsequent steps
echo "HF_TOKEN=$HF_TOKEN" >> "$GITHUB_ENV"
echo "HF token retrieved successfully from cluster secret"
