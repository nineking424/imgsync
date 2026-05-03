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

echo "==> Building ${IMAGE}"
DOCKER_BUILDKIT=1 docker build \
  --build-arg VERSION="${SHA}" \
  -t "${IMAGE}" \
  .

echo "==> Pushing ${IMAGE}"
docker push "${IMAGE}"

echo
echo "Pushed: ${IMAGE}"
echo "Use this in helm: --set image.repository=${REGISTRY}/imgsync --set image.tag=${TAG}"
