#!/usr/bin/env bash
# Teardown the real-cluster e2e environment.
# Default: helm uninstall + namespace delete (clears PVCs since reclaimPolicy=Delete).
# Use IMGSYNC_E2E_KEEP_NS=1 to keep the namespace and just helm uninstall (faster
# iteration when you want to reuse the postgres data between runs).
set -euo pipefail

NAMESPACE="${IMGSYNC_E2E_NAMESPACE:-imgsync-e2e-real}"

if kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  if helm -n "${NAMESPACE}" status imgsync >/dev/null 2>&1; then
    echo "==> helm uninstall imgsync"
    helm -n "${NAMESPACE}" uninstall imgsync --wait --timeout 2m || true
  fi

  if [ "${IMGSYNC_E2E_KEEP_NS:-0}" = "1" ]; then
    echo "==> Keeping namespace ${NAMESPACE} (IMGSYNC_E2E_KEEP_NS=1)"
  else
    echo "==> Deleting namespace ${NAMESPACE}"
    kubectl delete namespace "${NAMESPACE}" --wait --timeout 3m
  fi
else
  echo "==> Namespace ${NAMESPACE} not present; nothing to tear down"
fi
