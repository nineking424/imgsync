#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=imgsync-e2e
CHART=deploy/helm/imgsync
NAMESPACE=imgsync-e2e
IMAGE_TAG="${IMAGE_TAG:-imgsync:e2e}"

# 1. Create the kind cluster (idempotent)
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  echo "==> Creating kind cluster"
  mkdir -p /tmp/imgsync-e2e-localfs
  kind create cluster --name "$CLUSTER_NAME" --config e2e/kind_config.yaml
fi

# 2. Build + load the image into kind
echo "==> Building image $IMAGE_TAG"
docker build -t "$IMAGE_TAG" .

echo "==> Loading image into kind"
kind load docker-image "$IMAGE_TAG" --name "$CLUSTER_NAME"

# 3. Namespace + PV/PVC + postgres
echo "==> Applying namespace and infra"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f e2e/manifests/nfs-pv.yaml
kubectl apply -f e2e/manifests/postgres.yaml

echo "==> Waiting for postgres ready"
kubectl -n "$NAMESPACE" rollout status deployment/postgres --timeout=120s

# 4. Create DSN Secret
DSN="postgres://imgsync:imgsync@postgres.${NAMESPACE}.svc.cluster.local:5432/imgsync?sslmode=disable"
kubectl -n "$NAMESPACE" create secret generic imgsync-dsn \
  --from-literal=dsn="$DSN" \
  --dry-run=client -o yaml | kubectl apply -f -

# 5. Helm install (initial replicas=2; tests will helm upgrade --set replicaCount=8)
echo "==> Helm install"
helm upgrade --install imgsync "$CHART" \
  --namespace "$NAMESPACE" \
  --set image.repository=imgsync \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set replicaCount=2 \
  --wait --timeout 5m

echo "==> e2e environment up"
