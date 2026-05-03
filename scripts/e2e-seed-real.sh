#!/usr/bin/env bash
# Run the seeder Job in the real e2e namespace, overriding COUNT and SIZE_BYTES
# from CLI args. Waits until the Job completes.
#
# Usage:
#   ./scripts/e2e-seed-real.sh                  # defaults: 1000 × 1024 bytes
#   ./scripts/e2e-seed-real.sh 1000 1048576     # 1000 × 1MB (C7)
#   ./scripts/e2e-seed-real.sh 100  1024        # 100 × 1KB (F5* warm-up)
set -euo pipefail

NAMESPACE="${IMGSYNC_E2E_NAMESPACE:-imgsync-e2e-real}"
COUNT="${1:-1000}"
SIZE_BYTES="${2:-1024}"

echo "==> Seeding ${COUNT} files of ${SIZE_BYTES} bytes into PVC imgsync-localfs"

# Delete any prior Job (Jobs are immutable on retry; we always start fresh).
kubectl -n "${NAMESPACE}" delete job imgsync-seeder --ignore-not-found --wait=true

# Substitute COUNT/SIZE_BYTES into the manifest before apply. kubectl set env
# is rejected on Jobs because spec.template is immutable post-create, so we
# rewrite the env values upstream. The sed idiom anchors on the env name and
# rewrites the next line's value, surviving any change to the manifest defaults.
sed \
  -e "/name: COUNT$/{n;s|value: .*|value: \"${COUNT}\"|;}" \
  -e "/name: SIZE_BYTES$/{n;s|value: \"[0-9]*\".*|value: \"${SIZE_BYTES}\"|;}" \
  e2e/manifests/real/seeder-job.yaml | kubectl apply -f -

echo "==> Waiting for Job completion (timeout 10m)"
kubectl -n "${NAMESPACE}" wait --for=condition=complete \
  job/imgsync-seeder --timeout=600s

echo "==> Seeder logs (tail):"
kubectl -n "${NAMESPACE}" logs job/imgsync-seeder --tail=10
