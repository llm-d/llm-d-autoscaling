#!/usr/bin/env bash
set -euo pipefail

# PR E2E tests must run on the vllm-d cluster, not pok-prod-sa.
# pok-prod-sa is reserved for nightly E2E runs only.
# Runners with the 'pok-prod' label connect to pok-prod-sa;
# runners without it connect to vllm-d.
CLUSTER_API=$(kubectl cluster-info 2>/dev/null | head -1 | grep -oE 'https://[^ ]+')
echo "Cluster API: $CLUSTER_API"
if echo "$CLUSTER_API" | grep -q "pokprod"; then
  echo "::error::This runner is connected to pok-prod-sa, but PR E2E tests must run on vllm-d."
  echo "::error::The runner likely has the 'pok-prod' label. PR CI should only use vllm-d runners."
  exit 1
fi
echo "Cluster verified: running on vllm-d"
