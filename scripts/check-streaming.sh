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
  matches=$(grep -RnE '\b(io|ioutil)\.ReadAll\b' "$d" \
              --include='*.go' --exclude='*_test.go' || true)
  if [[ -n "$matches" ]]; then
    echo "$matches"
    violations=$((violations + 1))
  fi
done

if (( violations > 0 )); then
  echo ""
  echo "FAIL: io.ReadAll detected in streaming hot path. Use io.Copy or io.Reader chains instead." >&2
  exit 1
fi
echo "OK: no io.ReadAll in streaming hot paths"
