#!/usr/bin/env bash

set -eou pipefail

KIND=${KIND:-kind}
KIND_NAME=${KIND_NAME:-"kind-wva-gpu-cluster"}

_kind() {
    "${KIND}" "$@"
}

echo "Checking if kind cluster '${KIND_NAME}' is running..."

if _kind get kubeconfig --name "${KIND_NAME}" &>/dev/null; then
    echo "Cluster '${KIND_NAME}' is running. Deleting..."
    _kind delete cluster --name "${KIND_NAME}"
    echo "🧹 Cluster '${KIND_NAME}' deleted successfully."
else
    echo "Cluster '${KIND_NAME}' does not exist or is not running."
fi