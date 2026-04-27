#!/usr/bin/env bash
set -euo pipefail
export DOCKER_BUILDKIT=1

IMAGE_TAG="${IMAGE_TAG:-imgsync:test-$(date +%s)}"

echo "==> Building image $IMAGE_TAG"
docker build -t "$IMAGE_TAG" .

echo "==> Verifying entrypoint help"
HELP_OUT=$(docker run --rm "$IMAGE_TAG" --help 2>&1 || true)
echo "$HELP_OUT" | grep -q "imgsync" || {
  echo "FAIL: imgsync --help did not contain 'imgsync'"
  exit 1
}

echo "==> Verifying subcommand exposure"
for cmd in migrate enqueue worker; do
  CMD_OUT=$(docker run --rm "$IMAGE_TAG" "$cmd" --help 2>&1 || true)
  echo "$CMD_OUT" | grep -q "$cmd" || {
    echo "FAIL: subcommand $cmd not exposed"
    exit 1
  }
done

echo "==> Verifying nonroot user"
USER_LINE=$(docker inspect "$IMAGE_TAG" --format '{{.Config.User}}')
if [ "$USER_LINE" != "nonroot:nonroot" ] && [ "$USER_LINE" != "65532:65532" ]; then
  echo "FAIL: image user is '$USER_LINE', expected nonroot or 65532"
  exit 1
fi

echo "==> Verifying image is reasonably small (<50MB)"
SIZE_BYTES=$(docker image inspect "$IMAGE_TAG" --format '{{.Size}}')
SIZE_MB=$(( SIZE_BYTES / 1024 / 1024 ))
if [ "$SIZE_MB" -gt 50 ]; then
  echo "FAIL: image size ${SIZE_MB}MB exceeds 50MB budget"
  exit 1
fi

echo "==> Cleaning up"
docker rmi "$IMAGE_TAG" >/dev/null

echo "PASS: Dockerfile contract checks all green"
