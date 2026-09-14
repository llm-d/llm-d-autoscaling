#!/usr/bin/env bash
set -euo pipefail

# Build image with git ref tag for this PR
# Use first 8 chars of the git ref (POSIX-compliant)
IMAGE_TAG="ref-$(printf '%s' "$GIT_REF" | cut -c1-8)"
FULL_IMAGE="${REGISTRY}/${IMAGE_NAME}:${IMAGE_TAG}"
echo "Building image: $FULL_IMAGE"
echo "Git ref: $GIT_REF"

# Build and push using make targets
make docker-build IMG="$FULL_IMAGE"
make docker-push IMG="$FULL_IMAGE"

echo "image_tag=${IMAGE_TAG}" >> "$GITHUB_OUTPUT"
echo "Image built and pushed: $FULL_IMAGE"
