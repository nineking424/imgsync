#!/usr/bin/env bash
# e2e-up-sniffer.sh — idempotent: bring up cluster (via e2e-up.sh) then upgrade
# helm to enable the sniffer with a fast poll interval.
#
# e2e-up.sh already:
#   - creates the kind cluster
#   - builds + loads the image
#   - applies source-postgres.yaml
#   - creates imgsync-source-dsn + imgsync-db-dsn Secrets
#   - runs the initial helm install with sniffer.enabled=true, intervalSec=5
#
# This wrapper re-runs e2e-up.sh (idempotent on an already-up cluster) then
# issues a helm upgrade that locks in the test-specific sniffer overrides so
# the sniffer uses localfs protocols and a 5-second poll.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="${SCRIPT_DIR}/../deploy/helm/imgsync"
NAMESPACE=imgsync-e2e

echo "==> Running e2e-up.sh (idempotent base cluster + helm install)"
"${SCRIPT_DIR}/e2e-up.sh"

echo "==> Helm upgrade: enable sniffer with localfs protocols + intervalSec=5"
helm upgrade --install imgsync "$CHART" \
  --namespace "$NAMESPACE" \
  --set image.repository=imgsync \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set replicaCount=2 \
  --set sniffer.enabled=true \
  --set sniffer.config.intervalSec=5 \
  --set sniffer.config.shadow=true \
  --set sniffer.config.srcProtocol=localfs \
  --set sniffer.config.dstProtocol=localfs \
  --set "sniffer.config.srcPattern=/srv/imgsync/src/{{.file_path}}.bin" \
  --set "sniffer.config.dstPattern=/srv/imgsync/dst/{{.file_path}}.bin" \
  --wait --timeout 5m

echo "==> e2e sniffer environment up"
