#!/usr/bin/env bash
set -euo pipefail

# Generate unique image tag for this PR run (local image, no registry needed)
IMAGE_NAME="llm-d-workload-variant-autoscaler"
IMAGE_TAG="pr-${GITHUB_RUN_ID}-${CHECKOUT_SHA:0:7}"
# Use localhost prefix for local-only image (Kind will load it directly)
FULL_IMAGE="localhost/${IMAGE_NAME}:${IMAGE_TAG}"

echo "Building local image: $FULL_IMAGE"
echo "Image will be loaded into Kind cluster (no push needed)"

# Build image locally (no push needed for Kind)
make docker-build IMG="$FULL_IMAGE"

echo "image=$FULL_IMAGE" >> "$GITHUB_OUTPUT"
echo "image_tag=${IMAGE_TAG}" >> "$GITHUB_OUTPUT"
echo "Image built locally: $FULL_IMAGE"
