#!/usr/bin/env bash
set -euo pipefail

echo "Cleaning up ALL test infrastructure..."
echo "  LLMD_NAMESPACE: $LLMD_NAMESPACE"
echo "  WVA_NAMESPACE: $WVA_NAMESPACE"

echo "Removing cluster-scoped WVA resources..."
kubectl delete clusterrole,clusterrolebinding -l app.kubernetes.io/name=workload-variant-autoscaler --ignore-not-found || true

echo "Uninstalling llm-d helm releases..."
for release in $(helm list -n "$LLMD_NAMESPACE" -q 2>/dev/null); do
  echo "  Uninstalling release: $release"
  helm uninstall "$release" -n "$LLMD_NAMESPACE" --ignore-not-found --wait --timeout 60s || true
done

echo "Deleting llm-d namespace $LLMD_NAMESPACE..."
kubectl delete namespace "$LLMD_NAMESPACE" --ignore-not-found --timeout=120s || true

echo "Deleting WVA namespace $WVA_NAMESPACE..."
kubectl delete namespace "$WVA_NAMESPACE" --ignore-not-found --timeout=120s || true

echo "Cleanup complete"
