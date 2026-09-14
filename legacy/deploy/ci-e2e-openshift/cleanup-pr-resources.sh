#!/usr/bin/env bash
set -euo pipefail

echo "Cleaning up WVA resources for this PR's namespaces only..."
echo "  LLMD_NAMESPACE: $LLMD_NAMESPACE"
echo "  WVA_NAMESPACE: $WVA_NAMESPACE"

# Only clean up the namespaces associated with THIS PR
# Do NOT touch namespaces from other PRs to avoid race conditions
for ns in "$LLMD_NAMESPACE" "$WVA_NAMESPACE"; do
  if kubectl get namespace "$ns" &>/dev/null; then
    echo ""
    echo "=== Cleaning up namespace: $ns ==="
    # Delete WVA resources in this namespace
    echo "  Removing HPAs and ScaledObjects..."
    kubectl delete hpa -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler --ignore-not-found || true
    kubectl delete scaledobject -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler --ignore-not-found 2>/dev/null || true
    # Uninstall all helm releases in the namespace
    for release in $(helm list -n "$ns" -q 2>/dev/null); do
      echo "  Uninstalling helm release: $release"
      helm uninstall "$release" -n "$ns" --ignore-not-found --wait --timeout 60s || true
    done
    echo "  Deleting namespace: $ns"
    kubectl delete namespace "$ns" --ignore-not-found --timeout=60s || true
  else
    echo "Namespace $ns does not exist, skipping cleanup"
  fi
done

# Clean up legacy namespaces if they exist (these are not PR-specific)
for legacy_ns in llm-d-optimized-baseline workload-variant-autoscaler-system; do
  if kubectl get namespace "$legacy_ns" &>/dev/null; then
    echo ""
    echo "=== Cleaning up legacy namespace: $legacy_ns ==="
    # Uninstall all helm releases in the namespace first
    for release in $(helm list -n "$legacy_ns" -q 2>/dev/null); do
      echo "  Uninstalling helm release: $release"
      helm uninstall "$release" -n "$legacy_ns" --ignore-not-found --wait --timeout 60s || true
    done
    echo "  Deleting namespace: $legacy_ns"
    kubectl delete namespace "$legacy_ns" --ignore-not-found --timeout=60s || true
  fi
done

echo ""
echo "Cleanup complete for this PR's namespaces"
