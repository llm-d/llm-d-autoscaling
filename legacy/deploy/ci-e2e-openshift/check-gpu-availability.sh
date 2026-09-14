#!/usr/bin/env bash
set -euo pipefail

echo "Checking GPU availability for e2e test..."

# Minimum GPUs needed: 1 replica × 1 GPU (TP=1) + 1 for scale-up = 2
# Recommended with scale-up headroom: 3
REQUIRED_GPUS=2
RECOMMENDED_GPUS=3

# Total allocatable GPUs across all nodes
TOTAL_GPUS=$(kubectl get nodes -o json | \
  jq '[.items[].status.allocatable["nvidia.com/gpu"] // "0" | tonumber] | add // 0')

# Currently requested GPUs by all pods
ALLOCATED_GPUS=$(kubectl get pods --all-namespaces -o json | \
  jq '[.items[] | select(.status.phase == "Running" or .status.phase == "Pending") | .spec.containers[]?.resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0')

AVAILABLE_GPUS=$((TOTAL_GPUS - ALLOCATED_GPUS))

# Total allocatable CPU (cores) and memory (Gi) across all nodes
# CPU may be in millicores (e.g. "8000m") or cores (e.g. "8")
TOTAL_CPU=$(kubectl get nodes -o json | \
  jq '[.items[].status.allocatable.cpu // "0" | if endswith("m") then (gsub("m$";"") | tonumber / 1000) else tonumber end] | add | floor')
TOTAL_MEM_KI=$(kubectl get nodes -o json | \
  jq '[.items[].status.allocatable.memory // "0" | gsub("[^0-9]";"") | tonumber] | add')
TOTAL_MEM_GI=$((TOTAL_MEM_KI / 1048576))

NODE_COUNT=$(kubectl get nodes --no-headers | wc -l | tr -d ' ')
GPU_NODE_COUNT=$(kubectl get nodes -o json | \
  jq '[.items[] | select((.status.allocatable["nvidia.com/gpu"] // "0" | tonumber) > 0)] | length')

# Export all values for the PR comment step
echo "total_gpus=$TOTAL_GPUS" >> "$GITHUB_OUTPUT"
echo "allocated_gpus=$ALLOCATED_GPUS" >> "$GITHUB_OUTPUT"
echo "available_gpus=$AVAILABLE_GPUS" >> "$GITHUB_OUTPUT"
echo "total_cpu=$TOTAL_CPU" >> "$GITHUB_OUTPUT"
echo "total_mem_gi=$TOTAL_MEM_GI" >> "$GITHUB_OUTPUT"
echo "node_count=$NODE_COUNT" >> "$GITHUB_OUTPUT"
echo "gpu_node_count=$GPU_NODE_COUNT" >> "$GITHUB_OUTPUT"
echo "required_gpus=$REQUIRED_GPUS" >> "$GITHUB_OUTPUT"
echo "recommended_gpus=$RECOMMENDED_GPUS" >> "$GITHUB_OUTPUT"

echo "## GPU Status" >> "$GITHUB_STEP_SUMMARY"
echo "| Metric | Count |" >> "$GITHUB_STEP_SUMMARY"
echo "|--------|-------|" >> "$GITHUB_STEP_SUMMARY"
echo "| Total cluster GPUs | $TOTAL_GPUS |" >> "$GITHUB_STEP_SUMMARY"
echo "| Currently allocated | $ALLOCATED_GPUS |" >> "$GITHUB_STEP_SUMMARY"
echo "| Available | $AVAILABLE_GPUS |" >> "$GITHUB_STEP_SUMMARY"
echo "| Required (minimum) | $REQUIRED_GPUS |" >> "$GITHUB_STEP_SUMMARY"
echo "| Recommended (with scale-up) | $RECOMMENDED_GPUS |" >> "$GITHUB_STEP_SUMMARY"

if [ "$AVAILABLE_GPUS" -lt "$REQUIRED_GPUS" ]; then
  echo "" >> "$GITHUB_STEP_SUMMARY"
  echo "❌ **Insufficient GPUs** — need $REQUIRED_GPUS but only $AVAILABLE_GPUS available. Re-run when GPUs free up." >> "$GITHUB_STEP_SUMMARY"
  echo "::error::Insufficient GPUs: need $REQUIRED_GPUS, have $AVAILABLE_GPUS available. Try again later."
  echo "gpu_available=false" >> "$GITHUB_OUTPUT"
  exit 1
elif [ "$AVAILABLE_GPUS" -lt "$RECOMMENDED_GPUS" ]; then
  echo "" >> "$GITHUB_STEP_SUMMARY"
  echo "⚠️ **Low GPU headroom** — $AVAILABLE_GPUS available (need $RECOMMENDED_GPUS for scale-up tests). Tests may fail during scale-up." >> "$GITHUB_STEP_SUMMARY"
  echo "::warning::Low GPU headroom: $AVAILABLE_GPUS available, $RECOMMENDED_GPUS recommended for scale-up tests"
  echo "gpu_available=true" >> "$GITHUB_OUTPUT"
else
  echo "" >> "$GITHUB_STEP_SUMMARY"
  echo "✅ **GPUs available** — $AVAILABLE_GPUS GPUs free ($REQUIRED_GPUS required, $RECOMMENDED_GPUS recommended)" >> "$GITHUB_STEP_SUMMARY"
  echo "gpu_available=true" >> "$GITHUB_OUTPUT"
fi
