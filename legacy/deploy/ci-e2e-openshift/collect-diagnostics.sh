#!/usr/bin/env bash
set -euo pipefail

mkdir -p /tmp/cluster-diagnostics
for ns in "$WVA_NAMESPACE" "$LLMD_NAMESPACE"; do
  if kubectl get namespace "$ns" &>/dev/null; then
    kubectl get va,hpa,all -n "$ns" 2>/dev/null > /tmp/cluster-diagnostics/${ns}-resources.txt || true
    kubectl get events -n "$ns" --sort-by='.lastTimestamp' 2>/dev/null > /tmp/cluster-diagnostics/${ns}-events.txt || true
    kubectl logs -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler --tail=500 2>/dev/null > /tmp/cluster-diagnostics/${ns}-wva-logs.txt || true
  fi
done
