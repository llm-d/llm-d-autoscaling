#!/usr/bin/env bash
set -euo pipefail

sudo apt-get update && sudo apt-get install -y make jq
# mikefarah/yq — required for patching llm-d Kustomize manifests
YQ_VERSION="v4.45.1"
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  YQ_ARCH="amd64" ;;
  aarch64) YQ_ARCH="arm64" ;;
  *) echo "::error::Unsupported arch for yq: $ARCH"; exit 1 ;;
esac
curl -fsSL --retry 3 --retry-delay 5 -o yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${YQ_ARCH}"
chmod +x yq
sudo mv yq /usr/local/bin/
yq --version
# Install kubectl - use pinned version for reproducible CI builds
# Pinned 2025-12: v1.31.0 tested compatible with OpenShift 4.16+
# Update this version when upgrading target cluster or during regular dependency reviews
KUBECTL_VERSION="v1.31.0"
echo "Installing kubectl version: $KUBECTL_VERSION"
curl -fsSL --retry 3 --retry-delay 5 -o kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
curl -fsSL --retry 3 --retry-delay 5 -o kubectl.sha256 "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl.sha256"
echo "$(cat kubectl.sha256)  kubectl" | sha256sum --check
chmod +x kubectl
sudo mv kubectl /usr/local/bin/
rm -f kubectl.sha256
# Install oc (OpenShift CLI)
curl -fsSL --retry 3 --retry-delay 5 -O "https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/openshift-client-linux.tar.gz"
tar -xzf openshift-client-linux.tar.gz
sudo mv oc /usr/local/bin/
rm -f openshift-client-linux.tar.gz kubectl README.md
# Install helm
curl -fsSL --retry 3 --retry-delay 5 https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
