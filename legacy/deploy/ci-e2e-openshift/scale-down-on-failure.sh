#!/usr/bin/env bash
set -euo pipefail

echo "Test failed - scaling down decode deployments to free GPUs..."
echo "Other resources (VA, HPA, controller logs) are preserved for debugging"
echo ""

for ns in "$LLMD_NAMESPACE"; do
  if kubectl get namespace "$ns" &>/dev/null; then
    echo "=== Scaling down decode deployments in $ns ==="
    kubectl scale deployment -n "$ns" -l llm-d.ai/inferenceServing=true --replicas=0 || true
    # Also try by name pattern in case labels are missing
    kubectl get deployment -n "$ns" -o name 2>/dev/null | grep decode | while read -r deploy; do
      echo "  Scaling down: $deploy"
      kubectl scale "$deploy" -n "$ns" --replicas=0 || true
    done || true
  fi
done
