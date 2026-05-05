#!/usr/bin/env bash
# Bootstrap the real-cluster e2e environment. Idempotent — safe to re-run.
#
# Preconditions:
#   - kubectl is pointed at the target cluster (e.g. admin@talos-homelab)
#   - `nfs-client` storage class exists (or is the default)
#   - Image has been pushed to ghcr.io via scripts/e2e-image-push.sh
#
# Result: namespace imgsync-e2e-real ready with postgres, source-postgres,
# shared-localfs PVC, DSN secrets, and helm release `imgsync` (replicas=2,
# sniffer enabled).
set -euo pipefail

NAMESPACE="${IMGSYNC_E2E_NAMESPACE:-imgsync-e2e-real}"
CHART="deploy/helm/imgsync"
VALUES="e2e/manifests/real/values-real.yaml"
REGISTRY="${IMGSYNC_E2E_REGISTRY:-ghcr.io/nineking424}"
SHA="$(git rev-parse --short HEAD)"
TAG="${IMGSYNC_E2E_TAG:-e2e-${SHA}}"

echo "==> Target context: $(kubectl config current-context)"
echo "==> Namespace:      ${NAMESPACE}"
echo "==> Image:          ${REGISTRY}/imgsync:${TAG}"

# 1. Namespace with baseline PSS so postgres pods (no hardened securityContext)
#    can run on clusters whose default is "restricted". The chart's worker and
#    sniffer pods set their own hardened context and remain compliant.
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${NAMESPACE}" \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/warn=baseline \
  pod-security.kubernetes.io/audit=baseline \
  --overwrite

# 2. Storage and DBs
kubectl apply -f e2e/manifests/real/shared-localfs-pvc.yaml
kubectl apply -f e2e/manifests/real/postgres.yaml
kubectl apply -f e2e/manifests/real/source-postgres.yaml

echo "==> Waiting for postgres ready"
kubectl -n "${NAMESPACE}" rollout status deployment/postgres --timeout=180s

echo "==> Waiting for source-postgres ready"
kubectl -n "${NAMESPACE}" rollout status deployment/source-postgres --timeout=180s

# 3. DSN secrets (DSN values reference in-cluster Service DNS)
DSN_CONTROL="postgres://imgsync:imgsync@postgres.${NAMESPACE}.svc.cluster.local:5432/imgsync?sslmode=disable"
DSN_SOURCE="postgres://source:source@source-postgres.${NAMESPACE}.svc.cluster.local:5432/source?sslmode=disable"

kubectl -n "${NAMESPACE}" create secret generic imgsync-dsn \
  --from-literal=dsn="${DSN_CONTROL}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${NAMESPACE}" create secret generic imgsync-db-dsn \
  --from-literal=SNIFFER_IMGSYNC_DSN="${DSN_CONTROL}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${NAMESPACE}" create secret generic imgsync-source-dsn \
  --from-literal=SNIFFER_SOURCE_DSN="${DSN_SOURCE}" \
  --dry-run=client -o yaml | kubectl apply -f -

# 4. Pre-create ServiceAccount so the pre-install migrate Job hook can reference
#    it. Helm creates the SA AFTER the pre-install hook runs, so a fresh
#    cluster needs this. Apply Helm ownership labels so `helm install` adopts.
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: imgsync
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/managed-by: Helm
  annotations:
    meta.helm.sh/release-name: imgsync
    meta.helm.sh/release-namespace: ${NAMESPACE}
EOF

# 5. Helm install
echo "==> Helm upgrade --install imgsync"
helm upgrade --install imgsync "${CHART}" \
  --namespace "${NAMESPACE}" \
  -f "${VALUES}" \
  --set image.repository="${REGISTRY}/imgsync" \
  --set image.tag="${TAG}" \
  --wait --timeout 5m

echo "==> Real-cluster e2e environment up"
kubectl -n "${NAMESPACE}" get deploy,svc,pvc
