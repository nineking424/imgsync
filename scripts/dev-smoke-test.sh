#!/usr/bin/env bash
set -euo pipefail

# This script asserts the dev stack actually processes jobs end-to-end.
# Run it AFTER `make dev-up && make dev-seed`.

DSN="${IMGSYNC_DSN:-postgres://imgsync:imgsync@localhost:5432/imgsync?sslmode=disable}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-30}"
EXPECTED_JOBS="${EXPECTED_JOBS:-10}"

echo "==> Waiting up to ${TIMEOUT_SECONDS}s for ${EXPECTED_JOBS} jobs to reach 'succeeded'"

COUNT=0
for i in $(seq 1 "$TIMEOUT_SECONDS"); do
  COUNT=$(docker compose exec -T postgres psql -U imgsync -d imgsync -tAc \
    "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
  if [ "$COUNT" -ge "$EXPECTED_JOBS" ]; then
    echo "==> All ${EXPECTED_JOBS} jobs succeeded in ${i}s"

    # Also assert: zero jobs in 'dead' or 'leased' (no stuck state)
    DEAD=$(docker compose exec -T postgres psql -U imgsync -d imgsync -tAc \
      "SELECT count(*) FROM transfer_jobs WHERE status IN ('dead','leased')")
    if [ "$DEAD" -ne 0 ]; then
      echo "FAIL: ${DEAD} jobs in dead/leased state"
      docker compose exec -T postgres psql -U imgsync -d imgsync -c \
        "SELECT id,status,attempts,locked_by FROM transfer_jobs WHERE status IN ('dead','leased')"
      exit 1
    fi

    echo "PASS: dev stack smoke test green"
    exit 0
  fi
  sleep 1
done

# Re-query so the FAIL line reflects state at the moment of timeout, not the
# stale read from the last loop iteration.
COUNT=$(docker compose exec -T postgres psql -U imgsync -d imgsync -tAc \
  "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
echo "FAIL: only ${COUNT}/${EXPECTED_JOBS} jobs succeeded after ${TIMEOUT_SECONDS}s"
docker compose exec -T postgres psql -U imgsync -d imgsync -c \
  "SELECT status, count(*) FROM transfer_jobs GROUP BY status"
exit 1
