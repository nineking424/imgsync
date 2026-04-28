#!/usr/bin/env bash
set -euo pipefail

# Enqueue 10 LocalFS→LocalFS jobs into the dev stack.
# The worker container's /data volume is shared as the LocalFS root.
# Note: imgsync-worker uses a distroless image (no shell), so we use a
# separate alpine container to seed the volume, then exec the binary directly.

# DSN for enqueue: use postgres hostname (resolves inside the compose network)
ENQUEUE_DSN="postgres://imgsync:imgsync@postgres:5432/imgsync?sslmode=disable"

# Resolve the worker's /data volume by inspecting the running container.
# This avoids guessing the compose project name (which defaults to the
# directory basename and varies across worktrees / clones).
WORKER_CID="$(docker compose ps -q imgsync-worker)"
if [ -z "$WORKER_CID" ]; then
  echo "FAIL: imgsync-worker container is not running. Run 'make dev-up' first." >&2
  exit 1
fi
WORKER_VOLUME="$(docker inspect "$WORKER_CID" \
  --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')"
if [ -z "$WORKER_VOLUME" ]; then
  echo "FAIL: could not resolve /data volume for imgsync-worker." >&2
  exit 1
fi

echo "==> Seeding source files into volume: ${WORKER_VOLUME}"
docker run --rm -v "${WORKER_VOLUME}:/data" alpine sh -c '
  mkdir -p /data/src /data/dst
  chmod 777 /data/src /data/dst
  for i in $(seq 1 10); do
    echo "hello from job $i" > /data/src/file-$i.txt
  done
  chmod 644 /data/src/file-*.txt
  echo "Files created:"
  ls /data/src/
'

echo "==> Enqueuing 10 jobs via imgsync-worker container"
for i in $(seq 1 10); do
  docker compose exec -T -e IMGSYNC_DSN="${ENQUEUE_DSN}" imgsync-worker \
    /app/imgsync enqueue \
      --trace-id "smoke-$i" \
      --src "/data/src/file-$i.txt" \
      --dst "/data/dst/file-$i.txt" \
      --src-protocol localfs \
      --dst-protocol localfs
done

echo "Seeded 10 jobs."
