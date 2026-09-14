#!/usr/bin/env bash

set -euo pipefail

# --------------------------------------------------------------------
# Defaults
# --------------------------------------------------------------------
DEFAULT_CLUSTER_NAME="kind-wva-gpu-cluster"
DEFAULT_NODES=3
DEFAULT_GPUS_PER_NODE=2
DEFAULT_GPU_TYPE="mix"
DEFAULT_GPU_MODEL="NVIDIA-A100-PCIE-80GB"
DEFAULT_GPU_MEMORY=81920
DEFAULT_K8S_VERSION="v1.32.0"

# Initialize variables
cluster_name="$DEFAULT_CLUSTER_NAME"
nodes="$DEFAULT_NODES"
gpus_per_node="$DEFAULT_GPUS_PER_NODE"
gpu_type="$DEFAULT_GPU_TYPE"
gpu_model="$DEFAULT_GPU_MODEL"
gpu_memory="$DEFAULT_GPU_MEMORY"
k8s_version="${K8S_VERSION:-$DEFAULT_K8S_VERSION}"
# Enable HPAScaleToZero feature gate (alpha feature for scale-to-zero HPA support)
enable_scale_to_zero="${ENABLE_SCALE_TO_ZERO:-true}"

# --------------------------------------------------------------------
# Cleanup on exit
# --------------------------------------------------------------------
cleanup() {
    [[ -f "kind-config.yaml" ]] && rm -f "kind-config.yaml" || true
    return 0
}
trap cleanup EXIT

# --------------------------------------------------------------------
# Usage
# --------------------------------------------------------------------
usage() {
    cat << EOF
Usage: $0 [OPTIONS]

Options:
    -c CLUSTER_NAME    Cluster name (default: $DEFAULT_CLUSTER_NAME)
    -n NODES           Number of nodes (default: $DEFAULT_NODES)
    -g GPUS            GPUs per node (default: $DEFAULT_GPUS_PER_NODE)
    -t TYPE            GPU type: nvidia, amd, intel-gaudi, intel-i915, intel-xe, mix, nvidia-mix, amd-mix (default: $DEFAULT_GPU_TYPE)
                       - nvidia-mix: H100, A100, MI300X heterogeneous (for limiter tests)
                       - amd-mix: MI300X, MI250, A100 heterogeneous (for limiter tests)
    -d MODEL           GPU model (default: $DEFAULT_GPU_MODEL)
    -m MEMORY          GPU memory in MB (default: $DEFAULT_GPU_MEMORY)
    -h                 Show this help message

Environment Variables:
    K8S_VERSION           Kubernetes version to use (default: $DEFAULT_K8S_VERSION)
    ENABLE_SCALE_TO_ZERO  Enable HPAScaleToZero feature gate (default: true)
EOF
}

validate_gpu_type() {
    case "$1" in
        nvidia|amd|intel-gaudi|intel-i915|intel-xe|mix|nvidia-mix|amd-mix) return 0 ;;
        *)
            echo "Error: Invalid GPU type '$1'. Valid: nvidia, amd, intel-gaudi, intel-i915, intel-xe, mix, nvidia-mix, amd-mix"
            exit 1
            ;;
    esac
}

# --------------------------------------------------------------------
# Parse Args
# --------------------------------------------------------------------
while getopts "c:n:g:t:d:m:h" opt; do
    case $opt in
        c) cluster_name="$OPTARG" ;;
        n) nodes="$OPTARG" ;;
        g) gpus_per_node="$OPTARG" ;;
        t) gpu_type="$OPTARG"; validate_gpu_type "$gpu_type" ;;
        d) gpu_model="$OPTARG" ;;
        m) gpu_memory="$OPTARG" ;;
        h) usage; exit 0 ;;
        *) usage; exit 1 ;;
    esac
done

# --------------------------------------------------------------------
# Create Kind Cluster
# --------------------------------------------------------------------
echo "[1/6] Creating Kind cluster: ${cluster_name} with ${nodes} nodes and ${gpus_per_node} GPUS each..."

# Build feature gates string
feature_gates=""
if [ "$enable_scale_to_zero" = "true" ]; then
    feature_gates="HPAScaleToZero=true"
    echo "  HPAScaleToZero feature gate: enabled"
fi

cat <<EOF > kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: kindest/node:${k8s_version}
EOF

# Add kubeadmConfigPatches for feature gates if any are enabled
if [ -n "$feature_gates" ]; then
    cat <<EOF >> kind-config.yaml
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
        feature-gates: ${feature_gates}
    controllerManager:
      extraArgs:
        feature-gates: ${feature_gates}
EOF
fi

for ((i=1; i<nodes; i++)); do
    echo "- role: worker" >> kind-config.yaml
    echo "  image: kindest/node:${k8s_version}" >> kind-config.yaml
done

kind create cluster --name "${cluster_name}" --config kind-config.yaml

control_plane_node="${cluster_name}-control-plane"
echo "[2/6] Waiting for all nodes to be ready..."
# Wait for all nodes to be Ready using kubectl wait (idiomatic approach)
# This ensures CNI is fully initialized on all nodes before proceeding
if ! kubectl wait --for=condition=Ready node --all --timeout=120s 2>/dev/null; then
    echo "Warning: Timed out waiting for nodes to be Ready after 120s"
    kubectl get nodes
fi
echo "All nodes are Ready"

echo "[2.1/6] Removing control-plane node taint to allow scheduling..."

kubectl taint nodes "${control_plane_node}" node-role.kubernetes.io/control-plane- || true

# --------------------------------------------------------------------
# Patch Node Labels
# --------------------------------------------------------------------
patch_node_gpu() {
    local node_name="$1"
    local gpu_type="$2"
    local gpu_count="$3"
    local gpu_product="$4"
    local gpu_memory="$5"
    # NVIDIA defaults
    local domain="${gpu_type}.com"
    local count_label="gpu.count"
    local memory_label="gpu.memory"
    local product_label="gpu.product"

    case "$gpu_type" in
        "amd")
            product_label="gpu.product-name" ;;
        "intel-gaudi")
            domain="habana.ai"; count_label="device.count"; memory_label="device.memory"; product_label="product.name" ;;
        "intel-i915"|"intel-xe")
            domain="gpu.intel.com"; count_label="device.count"; memory_label="memory"; product_label="product" ;;
        "nvidia"|*)
            ;;
    esac

    kubectl patch node "${node_name}" --type merge --patch "
metadata:
  labels:
    ${domain}/${count_label}: \"${gpu_count}\"
    ${domain}/${memory_label}: \"${gpu_memory}\"
    ${domain}/${product_label}: \"${gpu_product}\"
"
}

# Patch node with custom label for GPU configuration targeting
patch_node_custom_label() {
    local node_name="$1"
    local label_key="$2"
    local label_value="$3"
    kubectl label node "${node_name}" "${label_key}=${label_value}" --overwrite
}

nodes_list=$(kubectl get nodes --no-headers -o custom-columns=":metadata.name")
# K8s node names do not contain spaces
# shellcheck disable=SC2206
node_array=($nodes_list)

for i in "${!node_array[@]}"; do
    node_name="${node_array[$i]}"
    custom_label=""

    case "$gpu_type" in
        "nvidia-mix")
            # Heterogeneous NVIDIA-focused: H100, A100, AMD MI300X
            case $((i % 3)) in
                0) current_type="nvidia"; current_model="NVIDIA-H100-SXM5-80GB"; current_memory=81920
                   custom_label="4H100" ;;
                1) current_type="nvidia"; current_model="NVIDIA-A100-PCIE-80GB"; current_memory=81920
                   custom_label="4A100" ;;
                2) current_type="amd";    current_model="AMD-MI300X-192G";       current_memory=196608
                   custom_label="4MI300X" ;;
            esac
            ;;
        "amd-mix")
            # Heterogeneous AMD-focused: MI300X, MI250, NVIDIA A100
            case $((i % 3)) in
                0) current_type="amd";    current_model="AMD-MI300X-192G";       current_memory=196608
                   custom_label="4MI300X" ;;
                1) current_type="amd";    current_model="AMD-MI250-128G";        current_memory=131072
                   custom_label="4MI250" ;;
                2) current_type="nvidia"; current_model="NVIDIA-A100-PCIE-80GB"; current_memory=81920
                   custom_label="4A100" ;;
            esac
            ;;
        "mix")
            # Mix of everything supported
            case $((i % 5)) in
                0) current_type="nvidia"; current_model="NVIDIA-A100-PCIE-80GB";   current_memory=81920 ;;
                1) current_type="amd";    current_model="AMD-MI300X-192G";         current_memory=196608 ;;
                2) current_type="intel-gaudi"; current_model="Intel-Gaudi-2-96GB"; current_memory=98304 ;;
                3) current_type="intel-i915";  current_model="Max_1100";           current_memory=49152 ;;
                4) current_type="intel-xe";    current_model="Pro-B60-Graphics";   current_memory=24576 ;;
            esac
            ;;
        *)
            # Single vendor type
            current_type="$gpu_type"
            current_model="$gpu_model"
            current_memory="$gpu_memory"
            ;;
    esac

    patch_node_gpu "$node_name" "$current_type" "$gpus_per_node" "$current_model" "$current_memory"

    # Apply custom label if set (for heterogeneous GPU targeting)
    if [[ -n "${custom_label}" ]]; then
        patch_node_custom_label "$node_name" "gpu-config" "$custom_label"
    fi
done

# --------------------------------------------------------------------
# Patch Node Capacities
# --------------------------------------------------------------------
echo "[3/6] Patching node capacities..."
for i in "${!node_array[@]}"; do
    node_name="${node_array[$i]}"

    case "$gpu_type" in
        "nvidia-mix")
            case $((i % 3)) in
                0|1) current_type="nvidia" ;;
                2)   current_type="amd" ;;
            esac
            ;;
        "amd-mix")
            case $((i % 3)) in
                0|1) current_type="amd" ;;
                2)   current_type="nvidia" ;;
            esac
            ;;
        "mix")
            case $((i % 5)) in
                0) current_type="nvidia" ;;
                1) current_type="amd" ;;
                2) current_type="intel-gaudi" ;;
                3) current_type="intel-i915" ;;
                4) current_type="intel-xe" ;;
            esac
            ;;
        *)
            current_type="$gpu_type"
            ;;
    esac

    case "$current_type" in
        "intel-gaudi")
            resource_name="habana.ai~1gaudi" ;;
        "intel-i915")
            resource_name="gpu.intel.com~1i915" ;;
        "intel-xe")
            resource_name="gpu.intel.com~1xe" ;;
        *)
            resource_name="${current_type}.com~1gpu" ;;
    esac

    # Use kubectl patch with --subresource=status to directly update node status
    # This avoids the need for kubectl proxy and raw curl requests
    kubectl patch node "${node_name}" --subresource=status --type=json -p '[
        {"op":"add","path":"/status/capacity/'"${resource_name}"'","value":"'"${gpus_per_node}"'"},
        {"op":"add","path":"/status/allocatable/'"${resource_name}"'","value":"'"${gpus_per_node}"'"}
    ]'
done

# --------------------------------------------------------------------
# Summary
# --------------------------------------------------------------------
echo "[4/6] Summary of GPU resources in cluster '${cluster_name}':"
echo "-------------------------------------------------------------------------------------------------------------------------------"
printf "%-40s %-20s %-10s %-10s %-30s %-10s\n" "Node" "Resource" "Capacity" "Allocatable" "GPU Product" "Memory (MB)"
echo "-------------------------------------------------------------------------------------------------------------------------------"

for node in "${node_array[@]}"; do
  node_json=$(kubectl get node "$node" -o json)
  for resource in "nvidia.com/gpu" "amd.com/gpu" "habana.ai/gaudi" "gpu.intel.com/i915" "gpu.intel.com/xe"; do
    cap=$(echo "$node_json" | jq -r ".status.capacity[\"$resource\"] // empty")
    alloc=$(echo "$node_json" | jq -r ".status.allocatable[\"$resource\"] // empty")
    if [[ -n "$cap" || -n "$alloc" ]]; then
      case ${resource} in
        "nvidia.com/gpu")   prod_label="nvidia.com/gpu.product";   mem_label="nvidia.com/gpu.memory" ;;
        "amd.com/gpu")      prod_label="amd.com/gpu.product-name"; mem_label="amd.com/gpu.memory" ;;
        "habana.ai/gaudi")  prod_label="habana.ai/product.name";   mem_label="habana.ai/device.memory" ;;
        "gpu.intel.com/i915"|"gpu.intel.com/xe")
                            prod_label="gpu.intel.com/product";    mem_label="gpu.intel.com/memory" ;;
        *)                  echo "ERROR: resource '$resource' mismatch!"; exit 1 ;;
      esac
      product=$(echo "$node_json" | jq -r ".metadata.labels[\"${prod_label}\"] // \"-\"")
      memory=$(echo "$node_json" | jq -r ".metadata.labels[\"${mem_label}\"] // \"-\"")
      printf "%-40s %-20s %-10s %-10s %-30s %-10s\n" "$node" "$resource" "$cap" "$alloc" "$product" "$memory"
    fi
  done
done
echo "-------------------------------------------------------------------------------------------------------------------------------"



echo "[6/6] Done!"
