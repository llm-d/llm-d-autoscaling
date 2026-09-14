#!/usr/bin/env bash
set -euo pipefail

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  KIND_ARCH="amd64" ;;
  aarch64) KIND_ARCH="arm64" ;;
  *)       echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac
curl -Lo ./kind "https://kind.sigs.k8s.io/dl/v0.25.0/kind-linux-${KIND_ARCH}"
chmod +x ./kind
sudo mv ./kind /usr/local/bin/kind
kind version
