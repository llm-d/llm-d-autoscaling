#!/usr/bin/env bash
set -euo pipefail

echo "Verifying cluster access..."
kubectl cluster-info
kubectl get nodes
