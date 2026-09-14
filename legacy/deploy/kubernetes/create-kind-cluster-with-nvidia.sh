#!/usr/bin/env bash

set -e
set -o pipefail

GPU_OPERATOR_NS=gpu-operator

echo "> Creating Kind cluster"
kind create cluster --config - <<EOF
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
nodes:
- role: control-plane
  image: kindest/node:v1.32.0
  # required for GPU workaround
  extraMounts:
    - hostPath: /dev/null
      containerPath: /var/run/nvidia-container-devices/all
EOF

echo "> Deploying cert manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.15.3/cert-manager.yaml

echo "> Creating symlink in the control-plane container"
docker exec -ti kind-control-plane ln -s /sbin/ldconfig /sbin/ldconfig.real

echo "> Unmounting the nvidia devices in the control-plane container"
docker exec -ti kind-control-plane umount -R /proc/driver/nvidia

# According to https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html
echo "> Adding/updateding the NVIDIA Helm repository"
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update

echo "> Installing the GPU Operator Helm chart"
helm upgrade --install --wait gpu-operator -n ${GPU_OPERATOR_NS} --create-namespace nvidia/gpu-operator \
	--set mig.strategy=mixed \
	--set cdi.enabled=true \
	--set migManager.enabled=false \
	--set migManager.config.default=""

echo "> Waiting for container toolkit daemonset to be created"
timeout 60s bash -c "until kubectl get daemonset nvidia-container-toolkit-daemonset -o name -n ${GPU_OPERATOR_NS}; do sleep 10; done"

echo "> Waiting for container toolkit daemonset to become ready"
kubectl rollout status daemonset nvidia-container-toolkit-daemonset -n ${GPU_OPERATOR_NS}

echo "> Waiting for device plugin daemonset to be created"
timeout 60s bash -c "until kubectl get daemonset nvidia-device-plugin-daemonset -o name -n ${GPU_OPERATOR_NS}; do sleep 10; done"

echo "> Waiting for device plugin daemonset to become ready"
kubectl rollout status daemonset nvidia-device-plugin-daemonset -n ${GPU_OPERATOR_NS}
