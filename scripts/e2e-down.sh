#!/usr/bin/env bash
set -euo pipefail
kind delete cluster --name imgsync-e2e || true
rm -rf /tmp/imgsync-e2e-localfs || true
