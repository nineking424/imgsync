#!/usr/bin/env bash
# Build the imgsync image locally and push it to ghcr.io for the real-cluster
# e2e flow. The image is tagged `e2e-<short-sha>` so a multi-node cluster with
# `pullPolicy: IfNotPresent` always sees fresh content (a floating `e2e` tag
# would let nodes hold a stale layer).
set -euo pipefail

REGISTRY="${IMGSYNC_E2E_REGISTRY:-ghcr.io/nineking424}"
SHA="$(git rev-parse --short HEAD)"
TAG="${IMGSYNC_E2E_TAG:-e2e-${SHA}}"
IMAGE="${REGISTRY}/imgsync:${TAG}"

PLATFORMS="${IMGSYNC_E2E_PLATFORMS:-linux/amd64,linux/arm64}"

echo "==> Building & pushing ${IMAGE} (${PLATFORMS})"
docker buildx build \
  --platform "${PLATFORMS}" \
  --build-arg VERSION="${SHA}" \
  -t "${IMAGE}" \
  --push \
  .

echo
echo "Pushed: ${IMAGE}"
echo "Use this in helm: --set image.repository=${REGISTRY}/imgsync --set image.tag=${TAG}"
