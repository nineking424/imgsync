#!/usr/bin/env bash
# CI guard: forbid io.ReadAll / ioutil.ReadAll inside streaming hot paths.
# Runs from repo root.
set -euo pipefail

DIRS=(
  "internal/sources"
  "internal/transports"
  "internal/transfer"
)

violations=0
for d in "${DIRS[@]}"; do
  if [[ ! -d "$d" ]]; then
    continue
  fi
  matches=$(grep -RnE '\b(io|ioutil)\.ReadAll\b|bytes\.NewBuffer\b.*\bbody\b' "$d" \
              --include='*.go' --exclude='*_test.go' \
              | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' \
              || true)
  if [[ -n "$matches" ]]; then
    echo "$matches"
    violations=$((violations + 1))
  fi
done

if (( violations > 0 )); then
  echo ""
  echo "FAIL: streaming hot path violation (io.ReadAll or bytes.NewBuffer(...body...))." >&2
  exit 1
fi
echo "OK: no io.ReadAll or bytes.NewBuffer(body) buffering in streaming hot paths"
