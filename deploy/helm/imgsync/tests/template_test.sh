#!/usr/bin/env bash
set -euo pipefail

# Resolve the chart directory relative to this script so the test runs from
# any cwd (CI containers, ad-hoc shells), not just the repo root.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="${SCRIPT_DIR}/.."
TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

# ─── Test 1: helm lint passes ───────────────────────────────────────
echo "==> helm lint"
helm lint "$CHART"

# ─── Test 2: default render produces a single-replica Deployment ────
echo "==> helm template (default values)"
helm template t1 "$CHART" > "$TMP/t1.yaml"

grep -q "kind: Deployment" "$TMP/t1.yaml" || { echo "FAIL: no Deployment in default render"; exit 1; }
grep -q "replicas: 1"      "$TMP/t1.yaml" || { echo "FAIL: default replicas != 1"; exit 1; }

# Default render should NOT have a PDB (single replica)
if grep -q "kind: PodDisruptionBudget" "$TMP/t1.yaml"; then
  echo "FAIL: PDB rendered for single-replica install"
  exit 1
fi

# ─── Test 3: replicaCount=8 produces PDB ────────────────────────────
echo "==> helm template (replicaCount=8)"
helm template t2 "$CHART" --set replicaCount=8 > "$TMP/t2.yaml"

grep -q "replicas: 8"                  "$TMP/t2.yaml" || { echo "FAIL: replicas != 8"; exit 1; }
grep -q "kind: PodDisruptionBudget"    "$TMP/t2.yaml" || { echo "FAIL: PDB missing for 8 replicas"; exit 1; }
grep -q "maxUnavailable: 1"            "$TMP/t2.yaml" || { echo "FAIL: PDB maxUnavailable != 1"; exit 1; }

# ─── Test 4: probes, env, secret ref are wired ──────────────────────
echo "==> probes + env + secret ref"
grep -q "path: /livez"                            "$TMP/t1.yaml" || { echo "FAIL: liveness /livez missing"; exit 1; }
grep -q "path: /readyz"                           "$TMP/t1.yaml" || { echo "FAIL: readiness /readyz missing"; exit 1; }
grep -q "name: IMGSYNC_DSN"                       "$TMP/t1.yaml" || { echo "FAIL: IMGSYNC_DSN env missing"; exit 1; }
grep -q "secretKeyRef"                            "$TMP/t1.yaml" || { echo "FAIL: DSN should come from secretKeyRef"; exit 1; }
grep -q "name: imgsync-dsn"                       "$TMP/t1.yaml" || { echo "FAIL: default Secret name not 'imgsync-dsn'"; exit 1; }

# ─── Test 5: nonroot security context ───────────────────────────────
grep -q "runAsNonRoot: true"   "$TMP/t1.yaml" || { echo "FAIL: runAsNonRoot not set"; exit 1; }
grep -q "runAsUser: 65532"     "$TMP/t1.yaml" || { echo "FAIL: runAsUser not 65532"; exit 1; }
grep -q "readOnlyRootFilesystem: true" "$TMP/t1.yaml" || { echo "FAIL: readOnlyRootFilesystem missing"; exit 1; }

# ─── Test 6: migration Job hook annotations ─────────────────────────
echo "==> migrate Job hook annotations"
helm template t3 "$CHART" > "$TMP/t3.yaml"

grep -q 'kind: Job'                                              "$TMP/t3.yaml" || { echo "FAIL: no Job rendered"; exit 1; }
grep -q '"helm.sh/hook": "pre-install,pre-upgrade"'              "$TMP/t3.yaml" || \
  grep -q '"helm.sh/hook": pre-install,pre-upgrade'              "$TMP/t3.yaml" || \
  { echo "FAIL: migrate Job missing pre-install,pre-upgrade hook"; exit 1; }
grep -q 'before-hook-creation'                                    "$TMP/t3.yaml" || { echo "FAIL: migrate Job missing before-hook-creation policy"; exit 1; }
grep -q 'hook-succeeded'                                          "$TMP/t3.yaml" || { echo "FAIL: migrate Job missing hook-succeeded cleanup"; exit 1; }

# Migration Job MUST run as the same nonroot UID as the worker. Extract the
# migrate-job manifest in isolation so a regression that flips this Job to
# root can't slip past on Test 5's full-render grep.
awk '/^# Source: imgsync\/templates\/migrate-job\.yaml/{p=1} p; p && /^---$/{exit}' \
  "$TMP/t3.yaml" > "$TMP/t3-migrate-job.yaml"
[ -s "$TMP/t3-migrate-job.yaml" ] || { echo "FAIL: could not isolate migrate-job manifest"; exit 1; }
grep -q "runAsNonRoot: true"           "$TMP/t3-migrate-job.yaml" || { echo "FAIL: migrate Job runAsNonRoot not true"; exit 1; }
grep -q "runAsUser: 65532"             "$TMP/t3-migrate-job.yaml" || { echo "FAIL: migrate Job runAsUser not 65532"; exit 1; }
grep -q "readOnlyRootFilesystem: true" "$TMP/t3-migrate-job.yaml" || { echo "FAIL: migrate Job readOnlyRootFilesystem missing"; exit 1; }

# Args must be migrate up — assert exact array form to avoid false positives
# from quoted strings elsewhere in the render.
grep -q 'args: \["migrate", "up"\]' "$TMP/t3-migrate-job.yaml" || { echo "FAIL: migrate Job args not [\"migrate\", \"up\"]"; exit 1; }

# ─── Test 7: migrationJob.enabled=false suppresses the Job ──────────
echo "==> migrationJob.enabled=false"
helm template t4 "$CHART" --set migrationJob.enabled=false > "$TMP/t4.yaml"
if grep -q '^kind: Job$' "$TMP/t4.yaml"; then
  echo "FAIL: migrationJob.enabled=false did not suppress the Job"
  exit 1
fi

echo "PASS: helm chart structural tests green"
