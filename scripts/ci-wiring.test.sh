#!/usr/bin/env bash
# Test for .github/workflows/ci.yml: the integration suites, the C5' sniffer
# E2E, and the streaming guard's own meta-test must all be wired into CI.
# See issue #25 — these checks fail (red) until ci.yml wires them in.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CI="$REPO_ROOT/.github/workflows/ci.yml"

fail() { echo "FAIL: $*" >&2; exit 1; }

[ -f "$CI" ] || fail "missing $CI"

# 0) ci.yml must be well-formed YAML.
python3 -c "import sys,yaml; yaml.safe_load(open(sys.argv[1]))" "$CI" \
  || fail "ci.yml is not well-formed YAML"

# 1) The integration-tagged suites (sniffer S0-S3, metrics, migrate-0003 index)
#    must be reachable from CI. Either CI runs go test with -tags integration,
#    OR the build tag was dropped so the default ./... suite covers them.
tagged_files=(
  "$REPO_ROOT/internal/sniffer/integration_test.go"
  "$REPO_ROOT/internal/metrics/integration_test.go"
  "$REPO_ROOT/internal/db/migrate_integration_test.go"
)
ci_runs_integration_tag=0
grep -Eq -- '-tags[ =]integration' "$CI" && ci_runs_integration_tag=1

still_tagged=0
for f in "${tagged_files[@]}"; do
  if grep -q '//go:build integration' "$f"; then
    still_tagged=$((still_tagged + 1))
  fi
done

if [ "$ci_runs_integration_tag" -eq 0 ] && [ "$still_tagged" -gt 0 ]; then
  fail "integration suites unreachable in CI: ci.yml has no '-tags integration' step and $still_tagged suite(s) still carry '//go:build integration' (sniffer/metrics/migrate invariants never run in CI)"
fi

# 2) The C5' sniffer E2E (make e2e-sniffer) must be invoked from CI.
grep -Eq 'make[[:space:]]+e2e-sniffer' "$CI" \
  || fail "C5' sniffer E2E not wired: ci.yml never runs 'make e2e-sniffer'"

# 3) The streaming guard's own meta-test must run in CI.
grep -q 'check-streaming.sh.test.sh' "$CI" \
  || fail "streaming self-test not wired: ci.yml never runs 'scripts/check-streaming.sh.test.sh'"

echo "PASS: CI wires integration suites, e2e-sniffer, and the streaming self-test"
