#!/usr/bin/env bash
set -euo pipefail

echo "Waiting for WVA controller to be ready..."
kubectl rollout status deployment -l app.kubernetes.io/name=workload-variant-autoscaler -n "$WVA_NAMESPACE" --timeout=300s || true
kubectl get pods -n "$WVA_NAMESPACE"
