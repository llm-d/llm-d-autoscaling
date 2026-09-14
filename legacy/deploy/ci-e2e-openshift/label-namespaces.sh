#!/usr/bin/env bash
set -euo pipefail

echo "Adding openshift.io/user-monitoring label to namespaces for Prometheus scraping..."
kubectl label namespace "$LLMD_NAMESPACE" openshift.io/user-monitoring=true --overwrite
kubectl label namespace "$WVA_NAMESPACE" openshift.io/user-monitoring=true --overwrite
echo "Namespace labels applied"
