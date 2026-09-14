#!/usr/bin/env bash
set -euo pipefail

echo "=== Deployment Status ==="
echo ""
echo "=== llm-d namespace ($LLMD_NAMESPACE) ==="
kubectl get hpa -n "$LLMD_NAMESPACE" || true
kubectl get scaledobject -n "$LLMD_NAMESPACE" 2>/dev/null || true
echo ""
echo "=== WVA Controller ($WVA_NAMESPACE) ==="
kubectl get pods -n "$WVA_NAMESPACE"
