# imgsync E2E on Real Kubernetes (Talos Homelab) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reproduce the existing 5 e2e scenarios (C7, F5a, F5b, F5c, C5') against the operator's real Talos Kubernetes cluster (`admin@talos-homelab`) instead of kind, and ship a reusable bootstrap path + manual verification guide.

**Architecture:**
- Reuse the production helm chart unchanged. Layer "real cluster" specifics (image source, storage class, RWX PVCs, image pull secret) via a values overlay file at `e2e/manifests/real/values-real.yaml` and a parallel set of bootstrap scripts (`scripts/e2e-{up,down,seed}-real.sh`).
- Replace kind hostPath with NFS-backed RWX PVCs from the cluster's default `nfs-client` storage class. Postgres, source-postgres, and the shared `localfs` volume (where workers read/write fixtures) all bind to NFS PVCs.
- Replace test-host filesystem writes (kind extraMount trick) with a one-shot **seeder Job** that mounts the shared PVC and writes N fixture files into it from inside the cluster.
- Replace `kind load docker-image` with `docker push ghcr.io/nineking424/imgsync:e2e-<sha>`. The image is pulled from a public ghcr.io repository — no imagePullSecret required.
- The Go e2e harness (`go test -tags e2e ./e2e/...`) is **not** modified. It still targets kind. The real-cluster path is a parallel manual verification track using `kubectl`/`helm`/`psql` exactly as documented in the new guide.

**Tech stack:**
- Existing: Go 1.25, postgres 16, helm 3, kubectl, the production helm chart at `deploy/helm/imgsync`
- Cluster: Talos Linux v1.13, Kubernetes v1.36, 3 control planes + 2 workers, NFS subdir provisioner (`nfs-client` default), MetalLB, ingress-nginx
- New: ghcr.io as image registry (public package), NFS-backed RWX PVCs, in-cluster seeder Job

**Out of scope (explicit):**
- Refactoring `e2e/helpers.go` or any `*_test.go` to support a backend switch — doing that is a separate, larger plan. This plan keeps the Go suite kind-only and adds a manual verification path for real k8s.
- C7 throughput linearity assertion (`ratio ≥ 3.2`). On NFS-backed I/O, linearity is bandwidth-bound, not CPU-bound. C7 here is reduced to a smoke run that asserts `dead = 0` and all jobs `succeeded`. Linearity stays a kind-only assertion until we have local-disk PVs in the homelab.

---

## File Structure

| File | Responsibility | Status |
|---|---|---|
| `e2e/manifests/real/shared-localfs-pvc.yaml` | RWX 50Gi PVC backing `/srv/imgsync` for worker pods | Create |
| `e2e/manifests/real/postgres.yaml` | Control DB Deployment + RWO PVC + Service | Create |
| `e2e/manifests/real/source-postgres.yaml` | Sniffer source DB Deployment + RWO PVC + Service | Create |
| `e2e/manifests/real/seeder-job.yaml` | One-shot Job: mount PVC, write N fixture files | Create |
| `e2e/manifests/real/values-real.yaml` | Helm values overlay: image ref + sniffer config + localfs volume | Create |
| `scripts/e2e-up-real.sh` | Bootstrap on existing kubectl context (no kind) | Create |
| `scripts/e2e-down-real.sh` | Teardown helm release + namespace | Create |
| `scripts/e2e-seed-real.sh` | Run the seeder Job, wait for completion | Create |
| `scripts/e2e-image-push.sh` | Build local image and push to ghcr.io | Create |
| `Makefile` | New targets: `e2e-up-real`, `e2e-down-real`, `e2e-seed-real`, `e2e-push-real` | Modify |
| `docs/e2e-real-cluster-guide.md` | Manual verification guide tailored to real cluster | Create |
| `docs/e2e-manual-guide.md` | Cross-link to new guide in §0 | Modify |

---

## Decision Log

These choices are baked into the tasks below. If you find a reason to change them while implementing, stop and discuss before coding.

1. **Image registry: ghcr.io public package.** Cluster pulls from `ghcr.io/nineking424/imgsync`. No imagePullSecret. No insecure-registry containerd patches needed on Talos nodes. One-time setup: `gh auth login` + `gh api -X PATCH /user/packages/container/imgsync/visibility -f visibility=public` after the first push.
2. **Storage: `nfs-client` storage class for everything.** Postgres on NFS is sub-optimal for production but adequate for the 5–15 minute test scenarios in this plan. The shared localfs PVC is RWX (NFS supports RWX trivially via subdir provisioner).
3. **Namespace: `imgsync-e2e-real`.** Distinct from kind's `imgsync-e2e` so both can coexist.
4. **Image tag: `e2e-<git-short-sha>`.** Avoids the cache poisoning that floating tags like `e2e` cause on a multi-node cluster (`pullPolicy: IfNotPresent` would skip new content under the same tag).
5. **Seeder fixture sizes:** 1KB × 1000 for C5'/F5a/F5b/F5c (small, fast), 1MB × 1000 = 1GB for C7 (large enough to surface streaming bugs but fits comfortably in the NFS PVC).
6. **Sniffer interval:** 5s for C5' (fast feedback), default 60s otherwise.

---

### Task 1: Image push pipeline (build local + push to ghcr.io)

**Files:**
- Create: `scripts/e2e-image-push.sh`
- Modify: `Makefile` (add `e2e-push-real` target)

- [ ] **Step 1: Create `scripts/e2e-image-push.sh`**

```bash
#!/usr/bin/env bash
# Build the imgsync image locally and push it to ghcr.io for the real-cluster
# e2e flow. The image is tagged `e2e-<short-sha>` so a multi-node cluster with
# `pullPolicy: IfNotPresent` always sees fresh content (a floating `e2e` tag
# would let nodes hold a stale layer).
set -euo pipefail

REGISTRY="${IMGSYNC_E2E_REGISTRY:-ghcr.io/nineking424}"
SHA="$(git rev-parse --short HEAD)"
TAG="${IMGSYNC_E2E_TAG:-e2e-${SHA}}"
IMAGE="${REGISTRY}/imgsync:${TAG}"

echo "==> Building ${IMAGE}"
DOCKER_BUILDKIT=1 docker build \
  --build-arg VERSION="${SHA}" \
  -t "${IMAGE}" \
  .

echo "==> Pushing ${IMAGE}"
docker push "${IMAGE}"

echo
echo "Pushed: ${IMAGE}"
echo "Use this in helm: --set image.repository=${REGISTRY}/imgsync --set image.tag=${TAG}"
```

- [ ] **Step 2: Make script executable**

Run: `chmod +x scripts/e2e-image-push.sh`

- [ ] **Step 3: Add `Makefile` target**

Append to `/Users/nineking/workspace/app/imgsync/Makefile`:

```makefile
.PHONY: e2e-push-real
e2e-push-real: ## Build and push imgsync image to ghcr.io for real-cluster e2e
	./scripts/e2e-image-push.sh
```

- [ ] **Step 4: Verify ghcr.io login works**

Run:
```bash
echo "$GHCR_PAT" | docker login ghcr.io -u nineking424 --password-stdin
```

Expected: `Login Succeeded`. If you don't have a PAT yet:
```bash
gh auth login --scopes write:packages
gh auth token | docker login ghcr.io -u nineking424 --password-stdin
```

- [ ] **Step 5: First push + make package public**

Run:
```bash
make e2e-push-real
gh api -X PATCH "/user/packages/container/imgsync/visibility" \
  --raw-field visibility=public
```

Expected: `make` finishes with a `Pushed: ghcr.io/nineking424/imgsync:e2e-<sha>` line, and the `gh api` call returns a JSON body with `"visibility": "public"`.

Verify the image is publicly pullable from a node-network perspective:
```bash
kubectl run --rm -i --image=ghcr.io/nineking424/imgsync:e2e-$(git rev-parse --short HEAD) \
  --restart=Never --image-pull-policy=Always test-pull -- /imgsync --help
```
Expected: imgsync CLI help output. The pod is auto-deleted by `--rm`.

- [ ] **Step 6: Commit**

```bash
git add scripts/e2e-image-push.sh Makefile
git commit -m "feat(e2e): script to build+push imgsync image to ghcr.io for real-cluster e2e"
```

---

### Task 2: Shared localfs RWX PVC manifest

**Files:**
- Create: `e2e/manifests/real/shared-localfs-pvc.yaml`

- [ ] **Step 1: Write the PVC manifest**

Create `/Users/nineking/workspace/app/imgsync/e2e/manifests/real/shared-localfs-pvc.yaml`:

```yaml
# Shared volume that both worker pods (rwx) and the seeder Job mount.
# Worker pods see this at /srv/imgsync (matching kind's hostPath mount); the
# seeder Job uses the same path. NFS subdir provisioner gives us RWX trivially
# (each PVC is a subdir of the NFS export), so multiple replicas mounting
# concurrently is fine.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: imgsync-localfs
  namespace: imgsync-e2e-real
  labels:
    app.kubernetes.io/part-of: imgsync-e2e
spec:
  accessModes: [ReadWriteMany]
  storageClassName: nfs-client
  resources:
    requests:
      storage: 20Gi   # C7 needs ~1GB (1MB × 1000); 20Gi gives headroom for re-runs
```

- [ ] **Step 2: Apply and verify**

Run:
```bash
kubectl create namespace imgsync-e2e-real --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f e2e/manifests/real/shared-localfs-pvc.yaml
kubectl -n imgsync-e2e-real get pvc imgsync-localfs
```

Expected: `STATUS=Bound` within ~5 seconds. If it stays `Pending`, run `kubectl -n imgsync-e2e-real describe pvc imgsync-localfs` to inspect the provisioner event.

- [ ] **Step 3: Commit**

```bash
git add e2e/manifests/real/shared-localfs-pvc.yaml
git commit -m "feat(e2e): RWX shared localfs PVC for real-cluster worker pods"
```

---

### Task 3: In-cluster postgres + source-postgres on NFS PVCs

**Files:**
- Create: `e2e/manifests/real/postgres.yaml`
- Create: `e2e/manifests/real/source-postgres.yaml`

- [ ] **Step 1: Write `postgres.yaml`**

Create `/Users/nineking/workspace/app/imgsync/e2e/manifests/real/postgres.yaml`:

```yaml
# Control DB for imgsync. Stateful enough that a Pod restart shouldn't lose
# transfer_jobs rows mid-test, so we mount an NFS PVC. Postgres on NFS is
# sub-optimal for prod but adequate for the 5-15 min test scenarios here.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  namespace: imgsync-e2e-real
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: nfs-client
  resources:
    requests:
      storage: 5Gi
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: imgsync-e2e-real
spec:
  selector:
    app: postgres
  ports:
    - port: 5432
      targetPort: 5432
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: imgsync-e2e-real
spec:
  replicas: 1
  strategy:
    type: Recreate   # NFS RWO PVC: never schedule two pods on the same volume
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - name: POSTGRES_USER
              value: imgsync
            - name: POSTGRES_PASSWORD
              value: imgsync
            - name: POSTGRES_DB
              value: imgsync
            # Postgres on NFS: writes its lockfile and pid into the data dir.
            # Use a sub-path so the PV root stays clean and re-mountable.
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
          ports:
            - containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "imgsync", "-d", "imgsync"]
            periodSeconds: 2
            timeoutSeconds: 2
          resources:
            requests: { cpu: 200m, memory: 256Mi }
            limits:   { cpu: 1000m, memory: 1Gi }
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: postgres-data
```

- [ ] **Step 2: Write `source-postgres.yaml`**

Create `/Users/nineking/workspace/app/imgsync/e2e/manifests/real/source-postgres.yaml`:

```yaml
# Source DB the sniffer reads from (C5' scenario). Same shape as postgres.yaml
# with different DB/user/password and PVC name.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: source-postgres-data
  namespace: imgsync-e2e-real
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: nfs-client
  resources:
    requests:
      storage: 2Gi
---
apiVersion: v1
kind: Service
metadata:
  name: source-postgres
  namespace: imgsync-e2e-real
spec:
  selector:
    app: source-postgres
  ports:
    - port: 5432
      targetPort: 5432
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-postgres
  namespace: imgsync-e2e-real
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: source-postgres
  template:
    metadata:
      labels:
        app: source-postgres
    spec:
      containers:
        - name: source-postgres
          image: postgres:16-alpine
          env:
            - name: POSTGRES_USER
              value: source
            - name: POSTGRES_PASSWORD
              value: source
            - name: POSTGRES_DB
              value: source
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
          ports:
            - containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "source", "-d", "source"]
            periodSeconds: 2
            timeoutSeconds: 2
          resources:
            requests: { cpu: 200m, memory: 256Mi }
            limits:   { cpu: 1000m, memory: 512Mi }
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: source-postgres-data
```

- [ ] **Step 3: Apply and verify**

Run:
```bash
kubectl apply -f e2e/manifests/real/postgres.yaml
kubectl apply -f e2e/manifests/real/source-postgres.yaml
kubectl -n imgsync-e2e-real rollout status deployment/postgres --timeout=180s
kubectl -n imgsync-e2e-real rollout status deployment/source-postgres --timeout=180s
kubectl -n imgsync-e2e-real get pods,svc,pvc
```

Expected: both Deployments READY 1/1, both PVCs Bound, both Services have ClusterIPs.

- [ ] **Step 4: Smoke-test connectivity**

Run:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never --image=postgres:16-alpine \
  pgcheck -- psql "postgres://imgsync:imgsync@postgres:5432/imgsync?sslmode=disable" -c '\l'
```

Expected: postgres lists databases including `imgsync`. Pod auto-deletes.

- [ ] **Step 5: Commit**

```bash
git add e2e/manifests/real/postgres.yaml e2e/manifests/real/source-postgres.yaml
git commit -m "feat(e2e): postgres + source-postgres on NFS PVCs for real cluster"
```

---

### Task 4: Seeder Job manifest

**Files:**
- Create: `e2e/manifests/real/seeder-job.yaml`

- [ ] **Step 1: Write the Job manifest**

Create `/Users/nineking/workspace/app/imgsync/e2e/manifests/real/seeder-job.yaml`. The Job is parameterized via env vars (`COUNT`, `SIZE_BYTES`) so it serves both small (C5'/F5*) and large (C7) fixture sets.

```yaml
# One-shot Job that seeds N fixture files into the shared imgsync-localfs PVC,
# then exits. Replaces kind's host-mounted /tmp/imgsync-e2e-localfs trick.
#
# Behavior matches e2e/helpers.go:seedFixtures():
#   - Files are /srv/imgsync/src/file-NNNNN.bin
#   - Filled with a repeating 1MB chunk of bytes (i % 256), truncated at SIZE_BYTES
#   - Idempotent: skips files that already exist (so re-runs are cheap)
apiVersion: batch/v1
kind: Job
metadata:
  name: imgsync-seeder
  namespace: imgsync-e2e-real
spec:
  ttlSecondsAfterFinished: 300
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: imgsync-seeder
    spec:
      restartPolicy: Never
      containers:
        - name: seeder
          image: alpine:3.20
          env:
            - name: COUNT
              value: "1000"
            - name: SIZE_BYTES
              value: "1024"   # 1 KB default; override to 1048576 (1 MB) for C7
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -eu
              SRC=/srv/imgsync/src
              DST=/srv/imgsync/dst
              mkdir -p "$SRC" "$DST"
              echo "==> Seeding ${COUNT} files of ${SIZE_BYTES} bytes into ${SRC}"
              i=1
              while [ "$i" -le "${COUNT}" ]; do
                f="$(printf '%s/file-%05d.bin' "$SRC" "$i")"
                if [ ! -f "$f" ]; then
                  # /dev/urandom would dirty page cache; /dev/zero is fine for tests.
                  # bs is bytes; round SIZE_BYTES up to a 1KB block, dd handles padding.
                  blocks=$(( SIZE_BYTES / 1024 ))
                  if [ "$blocks" -eq 0 ]; then blocks=1; fi
                  dd if=/dev/zero of="$f" bs=1024 count="$blocks" status=none
                fi
                if [ $((i % 100)) -eq 0 ]; then
                  echo "  seeded $i / ${COUNT}"
                fi
                i=$((i + 1))
              done
              echo "==> Seeding complete: $(ls "$SRC" | wc -l) files in $SRC"
          volumeMounts:
            - name: localfs
              mountPath: /srv/imgsync
          resources:
            requests: { cpu: 100m, memory: 64Mi }
            limits:   { cpu: 1000m, memory: 256Mi }
      volumes:
        - name: localfs
          persistentVolumeClaim:
            claimName: imgsync-localfs
```

- [ ] **Step 2: Smoke-run with default 1KB × 1000**

Run:
```bash
kubectl delete job -n imgsync-e2e-real imgsync-seeder --ignore-not-found
kubectl apply -f e2e/manifests/real/seeder-job.yaml
kubectl -n imgsync-e2e-real wait --for=condition=complete job/imgsync-seeder --timeout=180s
kubectl -n imgsync-e2e-real logs job/imgsync-seeder --tail=20
```

Expected: log ends with `==> Seeding complete: 1000 files in /srv/imgsync/src`.

- [ ] **Step 3: Verify file count from a side-pod**

Run:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 inspect \
  --overrides='{"spec":{"containers":[{"name":"inspect","image":"alpine:3.20","stdin":true,"tty":false,"command":["sh","-c","ls /srv/imgsync/src | wc -l && ls -la /srv/imgsync/src/file-00001.bin"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  -- sh -c 'ls /srv/imgsync/src | wc -l'
```

Expected: `1000` (followed by an `ls -la` line confirming `file-00001.bin` is 1024 bytes).

- [ ] **Step 4: Commit**

```bash
git add e2e/manifests/real/seeder-job.yaml
git commit -m "feat(e2e): seeder Job to populate shared localfs PVC inside cluster"
```

---

### Task 5: Helm values overlay for real cluster

**Files:**
- Create: `e2e/manifests/real/values-real.yaml`

- [ ] **Step 1: Inspect chart deployment template for volume mount expectations**

Run:
```bash
grep -nE "(/srv/imgsync|localfs|volumeMounts|volumes)" \
  deploy/helm/imgsync/templates/deployment.yaml \
  deploy/helm/imgsync/templates/sniffer-deployment.yaml || true
```

This is informational — the chart in the repo today does **not** template a localfs volume mount. The kind-based e2e relies on the kind extraMount to surface `/srv/imgsync` on every node. On the real cluster we cannot do that, so the values overlay must inject a `volumes`/`volumeMounts` patch via `extraVolumes`/`extraVolumeMounts` keys (which we add in Step 2 if absent — see check below).

- [ ] **Step 2: Confirm the chart supports `extraVolumes` / `extraVolumeMounts`**

Run:
```bash
grep -nE "extraVolumes|extraVolumeMounts" deploy/helm/imgsync/templates/*.yaml \
  deploy/helm/imgsync/values.yaml || echo "MISSING"
```

If the output is `MISSING`, this plan has a precondition gap: the chart must be patched to surface `extraVolumes` / `extraVolumeMounts` on the worker Deployment and the sniffer Deployment. Stop the plan and address that gap before continuing — it should be a 5-line edit per template:

In `deploy/helm/imgsync/templates/deployment.yaml`, inside the worker container spec, add:

```yaml
          {{- with .Values.extraVolumeMounts }}
          volumeMounts:
            {{- toYaml . | nindent 12 }}
          {{- end }}
```

…and at the pod-spec level:

```yaml
      {{- with .Values.extraVolumes }}
      volumes:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

Apply the same two snippets to `templates/sniffer-deployment.yaml`. Then add `extraVolumes: []` and `extraVolumeMounts: []` defaults to `values.yaml`. Commit that as a separate "chart: surface extraVolumes/extraVolumeMounts" commit, then resume this task.

- [ ] **Step 3: Write `values-real.yaml`**

Create `/Users/nineking/workspace/app/imgsync/e2e/manifests/real/values-real.yaml`. The image repository/tag are placeholders here only because they vary per push — the bootstrap script (Task 6) substitutes them at install time via `--set`.

```yaml
# Helm values overlay for the real-cluster e2e flow. Pair with bootstrap script
# scripts/e2e-up-real.sh, which substitutes image.repository/tag from the
# current git short-sha.
#
# Usage:
#   helm upgrade --install imgsync deploy/helm/imgsync \
#     --namespace imgsync-e2e-real \
#     -f e2e/manifests/real/values-real.yaml \
#     --set image.repository=ghcr.io/nineking424/imgsync \
#     --set image.tag=e2e-<sha> \
#     --wait --timeout 5m

replicaCount: 2

image:
  repository: ghcr.io/nineking424/imgsync   # bootstrap overrides via --set
  tag: e2e-PLACEHOLDER                       # bootstrap overrides via --set
  pullPolicy: IfNotPresent

# Mount the shared NFS PVC at /srv/imgsync for both worker and sniffer pods.
# This is the cluster-side replacement for kind's hostPath extraMount.
extraVolumes:
  - name: localfs
    persistentVolumeClaim:
      claimName: imgsync-localfs
extraVolumeMounts:
  - name: localfs
    mountPath: /srv/imgsync

dsnSecretRef:
  name: imgsync-dsn
  key: dsn

sniffer:
  enabled: true
  replicas: 1
  config:
    sourceID: "main-source-db.images"
    table: "images"
    pkColumn: "id"
    tsColumn: "updated_at"
    extraColumns: "file_path"
    srcPattern: "/srv/imgsync/src/{{.file_path}}.bin"
    dstPattern: "/srv/imgsync/dst/{{.file_path}}.bin"
    srcProtocol: "localfs"
    dstProtocol: "localfs"
    shadow: true
    batchSize: "500"
    biasSec: "5"
    intervalSec: "5"          # tight loop for C5' scenario
  secrets:
    sourceDSNSecretRef: imgsync-source-dsn
    imgsyncDSNSecretRef: imgsync-db-dsn

worker:
  workers: 4
  ftpHostMaxConns: 8
  ftpHostPoolMaxIdle: 5
  ftpHostPoolIdleTTL: "5m"
```

- [ ] **Step 4: `helm template` smoke-render**

Run:
```bash
helm template imgsync deploy/helm/imgsync \
  --namespace imgsync-e2e-real \
  -f e2e/manifests/real/values-real.yaml \
  --set image.repository=ghcr.io/nineking424/imgsync \
  --set image.tag=e2e-test \
  | grep -A2 -B1 -E "(localfs|claimName|/srv/imgsync)" | head -40
```

Expected: at minimum, the worker Deployment and sniffer Deployment both contain a `volumeMounts: - name: localfs, mountPath: /srv/imgsync` block and a `volumes: - persistentVolumeClaim: claimName: imgsync-localfs` block.

If grep finds neither, the chart didn't render the overlay — go back to Step 2 and patch the chart templates.

- [ ] **Step 5: Commit**

```bash
git add e2e/manifests/real/values-real.yaml
git commit -m "feat(e2e): helm values overlay for real-cluster (NFS-backed localfs)"
```

---

### Task 6: Bootstrap script `e2e-up-real.sh`

**Files:**
- Create: `scripts/e2e-up-real.sh`

- [ ] **Step 1: Write the script**

Create `/Users/nineking/workspace/app/imgsync/scripts/e2e-up-real.sh`:

```bash
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

# 1. Namespace
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

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
```

- [ ] **Step 2: Make executable**

Run: `chmod +x scripts/e2e-up-real.sh`

- [ ] **Step 3: Run the bootstrap end-to-end**

Run:
```bash
./scripts/e2e-up-real.sh
```

Expected:
- All `kubectl apply` calls succeed.
- `postgres` and `source-postgres` rollouts finish READY 1/1 within 180s each.
- `helm upgrade --install` finishes within 5m.
- Final `get deploy,svc,pvc` shows: `postgres 1/1`, `source-postgres 1/1`, `imgsync 2/2`, plus 3 services and 3 PVCs.

If the migrate-job hook fails with `ServiceAccount "imgsync" not found`, the Step 4 SA pre-create didn't apply. Run:
```bash
kubectl -n imgsync-e2e-real get sa imgsync -o yaml
```
…and re-run the bootstrap.

If imgsync pods are stuck in `ImagePullBackOff`, verify the image is publicly readable:
```bash
docker pull ghcr.io/nineking424/imgsync:e2e-$(git rev-parse --short HEAD)
```

- [ ] **Step 4: Verify worker logs are alive**

Run:
```bash
kubectl -n imgsync-e2e-real logs -l app.kubernetes.io/name=imgsync --tail=30
```

Expected: lines like `lease loop started`, `no jobs to lease`. (Empty output means probes fired before logs flushed — re-run after 10s.)

- [ ] **Step 5: Commit**

```bash
git add scripts/e2e-up-real.sh
git commit -m "feat(e2e): bootstrap script for real-cluster (no kind, NFS PVCs, ghcr image)"
```

---

### Task 7: Teardown script `e2e-down-real.sh`

**Files:**
- Create: `scripts/e2e-down-real.sh`

- [ ] **Step 1: Write the script**

Create `/Users/nineking/workspace/app/imgsync/scripts/e2e-down-real.sh`:

```bash
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
```

- [ ] **Step 2: Make executable + smoke-test**

Run:
```bash
chmod +x scripts/e2e-down-real.sh
IMGSYNC_E2E_KEEP_NS=1 ./scripts/e2e-down-real.sh
```

Expected: helm uninstall succeeds, namespace remains.

Verify:
```bash
helm -n imgsync-e2e-real status imgsync 2>&1 | head -1   # expect: Error: release: not found
kubectl get ns imgsync-e2e-real -o jsonpath='{.status.phase}'   # expect: Active
```

- [ ] **Step 3: Re-bootstrap and full-teardown smoke**

Run:
```bash
./scripts/e2e-up-real.sh
./scripts/e2e-down-real.sh
```

Expected: bootstrap succeeds, then teardown deletes the namespace within 3 minutes. Confirm:
```bash
kubectl get ns imgsync-e2e-real 2>&1 | head -1
# expect: Error from server (NotFound)
```

- [ ] **Step 4: Commit**

```bash
git add scripts/e2e-down-real.sh
git commit -m "feat(e2e): teardown script for real-cluster e2e"
```

---

### Task 8: Seeder helper script `e2e-seed-real.sh`

**Files:**
- Create: `scripts/e2e-seed-real.sh`

- [ ] **Step 1: Write the script**

Create `/Users/nineking/workspace/app/imgsync/scripts/e2e-seed-real.sh`. Re-running it overwrites previous seed config and produces a fresh Job — safe and idempotent.

```bash
#!/usr/bin/env bash
# Run the seeder Job in the real e2e namespace, overriding COUNT and SIZE_BYTES
# from CLI args. Waits until the Job completes.
#
# Usage:
#   ./scripts/e2e-seed-real.sh                  # defaults: 1000 × 1024 bytes
#   ./scripts/e2e-seed-real.sh 1000 1048576     # 1000 × 1MB (C7)
#   ./scripts/e2e-seed-real.sh 100  1024        # 100 × 1KB (F5* warm-up)
set -euo pipefail

NAMESPACE="${IMGSYNC_E2E_NAMESPACE:-imgsync-e2e-real}"
COUNT="${1:-1000}"
SIZE_BYTES="${2:-1024}"

echo "==> Seeding ${COUNT} files of ${SIZE_BYTES} bytes into PVC imgsync-localfs"

# Delete any prior Job (Jobs are immutable on retry; we always start fresh).
kubectl -n "${NAMESPACE}" delete job imgsync-seeder --ignore-not-found --wait=true

# Apply the manifest, then patch env vars for this run.
kubectl apply -f e2e/manifests/real/seeder-job.yaml

# kubectl set env on a Job patches its template; since the Pod hasn't been
# created yet (we just applied a fresh Job), the patch lands before scheduling.
kubectl -n "${NAMESPACE}" set env job/imgsync-seeder \
  COUNT="${COUNT}" SIZE_BYTES="${SIZE_BYTES}"

echo "==> Waiting for Job completion (timeout 10m)"
kubectl -n "${NAMESPACE}" wait --for=condition=complete \
  job/imgsync-seeder --timeout=600s

echo "==> Seeder logs (tail):"
kubectl -n "${NAMESPACE}" logs job/imgsync-seeder --tail=10
```

- [ ] **Step 2: Make executable + smoke-run**

Run:
```bash
chmod +x scripts/e2e-seed-real.sh
./scripts/e2e-up-real.sh
./scripts/e2e-seed-real.sh 50 1024
```

Expected: Job completes, last log line is `==> Seeding complete: 50 files in /srv/imgsync/src` (or higher if leftovers exist from a prior run).

Verify file count from a side-pod:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 ls-localfs \
  --overrides='{"spec":{"containers":[{"name":"l","image":"alpine:3.20","command":["sh","-c","ls /srv/imgsync/src | wc -l"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
```

Expected: `50` (or higher if prior seed wasn't cleared).

- [ ] **Step 3: Reset between runs (verify clean-state helper)**

When you need a clean fixture set (different sizes), wipe and reseed:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 wipe-localfs \
  --overrides='{"spec":{"containers":[{"name":"w","image":"alpine:3.20","command":["sh","-c","rm -rf /srv/imgsync/src /srv/imgsync/dst && mkdir -p /srv/imgsync/src /srv/imgsync/dst"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
./scripts/e2e-seed-real.sh 100 1024
```

Expected: file count after the reset is exactly 100, not 100+previous.

- [ ] **Step 4: Commit**

```bash
git add scripts/e2e-seed-real.sh
git commit -m "feat(e2e): seeder helper script for real-cluster localfs PVC"
```

---

### Task 9: Makefile targets

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Append targets to Makefile**

Append to `/Users/nineking/workspace/app/imgsync/Makefile` (just before the existing `test-integration-sniffer` target, after `e2e-sniffer`):

```makefile
.PHONY: e2e-up-real
e2e-up-real: ## Bring up the real-cluster e2e environment (requires kubectl context set)
	./scripts/e2e-up-real.sh

.PHONY: e2e-down-real
e2e-down-real: ## Tear down the real-cluster e2e environment
	./scripts/e2e-down-real.sh

.PHONY: e2e-seed-real
e2e-seed-real: ## Seed fixture files into the real-cluster localfs PVC (defaults: 1000 × 1KB)
	./scripts/e2e-seed-real.sh
```

- [ ] **Step 2: Smoke-test all three targets**

Run:
```bash
make e2e-up-real
make e2e-seed-real
make e2e-down-real
```

Expected: each target succeeds. Final `e2e-down-real` deletes the namespace.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "feat(e2e): Makefile targets for real-cluster e2e (up/down/seed)"
```

---

### Task 10: Manual verification guide — bootstrap section

**Files:**
- Create: `docs/e2e-real-cluster-guide.md`
- Modify: `docs/e2e-manual-guide.md` (add cross-link in §0)

This task creates the guide skeleton and bootstrap section. Subsequent tasks (11–14) extend it with one scenario per task.

- [ ] **Step 1: Create the guide skeleton**

Create `/Users/nineking/workspace/app/imgsync/docs/e2e-real-cluster-guide.md`:

```markdown
# imgsync — Real-Cluster E2E 매뉴얼 검증 가이드

`docs/e2e-manual-guide.md` 의 자매 문서. kind 가 아닌 운영자(homelab)에 이미
띄워진 실제 Kubernetes 클러스터(`admin@talos-homelab`)에 직접 imgsync 를
설치해서 동일한 5개 시나리오 — C7, F5a, F5b, F5c, C5' — 의 invariant 를
손으로 확인한다.

## 0. 사전 준비

### 0.1 클러스터 / kubectl

```bash
kubectl config current-context   # 기대: admin@talos-homelab (또는 동등 클러스터)
kubectl get nodes                # 기대: 모두 Ready
kubectl get sc                   # 기대: nfs-client (default) 가 존재
```

### 0.2 도구 버전

| 도구    | 권장 버전 | 확인                                |
|---------|-----------|-------------------------------------|
| Go      | ≥ 1.25    | `go version`                        |
| Docker  | ≥ 24      | `docker version --format '{{.Server.Version}}'` |
| kubectl | ≥ 1.30    | `kubectl version --client=true`     |
| helm    | ≥ 3.14    | `helm version --short`              |
| psql    | ≥ 14      | `psql --version`                    |
| gh      | ≥ 2.40    | `gh version`                        |

### 0.3 ghcr.io 로그인 (1회만)

이미지를 ghcr.io 에 push 해서 클러스터가 그걸 pull 하는 구조다. 다른 사람이
같은 클러스터에 이미 띄워둔 게 있다면 step 1 (이미지 push) 를 건너뛰고
바로 step 2 부터 시작해도 된다.

```bash
gh auth login --scopes write:packages   # PAT 가 이미 있으면 건너뛰기
gh auth token | docker login ghcr.io -u nineking424 --password-stdin
```

`Login Succeeded` 가 나오면 OK.

### 0.4 작업 디렉토리

이 문서의 모든 명령은 imgsync 리포지토리 루트에서 실행한다고 가정한다.

```bash
cd /path/to/imgsync
git status   # working tree clean 권장
```

## 1. 클러스터 부트스트랩

### 1.1 이미지 빌드 + push

```bash
make e2e-push-real
```

내부 동작:
1. `docker build -t ghcr.io/nineking424/imgsync:e2e-<sha> .`
2. `docker push ghcr.io/nineking424/imgsync:e2e-<sha>`

이 step 은 imgsync 코드가 바뀐 직후에만 다시 돌리면 된다.

### 1.2 환경 부트스트랩

```bash
make e2e-up-real
```

내부 동작:
1. namespace `imgsync-e2e-real` 생성
2. shared-localfs PVC + postgres + source-postgres apply, READY 대기
3. DSN secret 3개 생성 (control / sniffer-imgsync / sniffer-source)
4. ServiceAccount `imgsync` 사전 생성 (pre-install hook 의 SA reference 충족)
5. `helm upgrade --install imgsync deploy/helm/imgsync` (replicas=2, sniffer 활성)

### 1.3 부트스트랩 검증

```bash
kubectl -n imgsync-e2e-real get deploy
# 기대: postgres 1/1, source-postgres 1/1, imgsync 2/2
```

```bash
kubectl -n imgsync-e2e-real get pvc
# 기대: imgsync-localfs (RWX), postgres-data (RWO), source-postgres-data (RWO) 모두 Bound
```

```bash
kubectl -n imgsync-e2e-real logs -l app.kubernetes.io/name=imgsync --tail=20
# 기대: "lease loop started" / "no jobs to lease"
```

### 1.4 DB 핸들 (시나리오 공통 준비)

이후 시나리오는 control DB 와 source DB 두 곳을 본다. 별도 터미널 두 개에서
포트포워드를 띄워둔다.

```bash
# 터미널 A — imgsync control DB
kubectl -n imgsync-e2e-real port-forward svc/postgres 5433:5432
```

```bash
# 터미널 B — sniffer 가 보는 source DB (C5' 시나리오에서만 필요)
kubectl -n imgsync-e2e-real port-forward svc/source-postgres 5434:5432
```

연결 확인:
```bash
psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c '\dt'
# 기대: transfer_jobs, transfer_events, schema_migrations, sniffer_state ...
```

### 1.5 시드 fixture (시나리오 공통 준비)

worker pod 가 읽을 source 파일을 NFS PVC 에 깔아둔다.

```bash
# C5' / F5a / F5b / F5c 용 — 1KB × 1000 = 2MB
make e2e-seed-real

# C7 throughput 용 — 1MB × 1000 = 1GB (별도)
./scripts/e2e-seed-real.sh 1000 1048576
```

검증:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 ls-fixtures \
  --overrides='{"spec":{"containers":[{"name":"l","image":"alpine:3.20","command":["sh","-c","ls /srv/imgsync/src | wc -l"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
# 기대: 1000
```

---

## 2. 시나리오 별 절차

각 시나리오는 별도 섹션으로 분리되어 있다 — 이 가이드의 §3~§7 참고.

본 가이드는 §3 부터 §7 까지 채워나가는 살아 있는 문서다. 새 시나리오를
추가할 때는 같은 형식 (목적 / 절차 / 검증 체크리스트) 을 유지한다.

(섹션 §3~§7 은 이 plan 의 Task 11~14 가 채운다.)

---

## 8. 사후 정리

```bash
make e2e-down-real
```

PVC 까지 (NFS 데이터 포함) 모두 회수한다 (`reclaimPolicy=Delete`). 부분 정리만
하고 싶으면:

```bash
helm -n imgsync-e2e-real uninstall imgsync
# (namespace 와 PVC 는 유지)
```

다음 번 시나리오 사이에 깨끗한 출발만 원하면:

```bash
psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
  'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'

kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 wipe-dst \
  --overrides='{"spec":{"containers":[{"name":"w","image":"alpine:3.20","command":["sh","-c","rm -rf /srv/imgsync/dst && mkdir -p /srv/imgsync/dst"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
```

---

## 9. 트러블슈팅

| 증상 | 의심 | 조치 |
|------|------|------|
| pod ImagePullBackOff | ghcr.io 패키지가 private | `gh api -X PATCH /user/packages/container/imgsync/visibility -f visibility=public` |
| pre-install Job 멈춤 | SA `imgsync` 누락 | `e2e-up-real.sh` Step 4 의 SA YAML 다시 apply |
| 모든 잡 dead | `srcProtocol`/`dstProtocol` 가 `fs` | values-real.yaml 의 protocol 값을 `localfs` 로 |
| sniffer enqueue 안 함 | `sniffer_state.last_updated_at` 미래 | `TRUNCATE sniffer_state` 후 sniffer pod 재기동 |
| port-forward 끊김 | 포드 재기동 | port-forward 재실행 |
| C7 ratio 낮음 | NFS 대역폭 한계 | 본 cluster 에서 C7 는 smoke 만 (3.2x 미달성 정상) — `dead = 0` 만 확인 |

---

## 10. 참고

- 자동 e2e (kind) 의 정확한 SQL/타이밍은 `e2e/helpers.go`, `e2e/{sniffer,dirty_state,throughput}_test.go` 가 진실의 소스
- 운영자 일상은 `docs/runbook.md`
- 이 가이드의 자매 문서 (kind+helm 시나리오): `docs/e2e-manual-guide.md`
```

- [ ] **Step 2: Cross-link from kind guide**

Edit `/Users/nineking/workspace/app/imgsync/docs/e2e-manual-guide.md`. Replace the line:

```
환경 (CI 토큰 만료, 격리망, 인프라 실험) 또는 새 시나리오를 시운전할 때 쓴다.
```

with:

```
환경 (CI 토큰 만료, 격리망, 인프라 실험) 또는 새 시나리오를 시운전할 때 쓴다.

> **실제 K8s 클러스터에서 검증하려면 [`docs/e2e-real-cluster-guide.md`](e2e-real-cluster-guide.md)**
> 를 참고할 것. NFS PVC + ghcr.io 기반의 자매 가이드다.
```

- [ ] **Step 3: Smoke-render guide**

Run:
```bash
ls -la docs/e2e-real-cluster-guide.md docs/e2e-manual-guide.md
grep -c "## " docs/e2e-real-cluster-guide.md
```

Expected: file exists, has at least 8 top-level sections (`## `).

- [ ] **Step 4: Commit**

```bash
git add docs/e2e-real-cluster-guide.md docs/e2e-manual-guide.md
git commit -m "docs(e2e): real-cluster manual guide skeleton + cross-link from kind guide"
```

---

### Task 11: C5' sniffer scenario — extend guide + smoke run

**Files:**
- Modify: `docs/e2e-real-cluster-guide.md` (add §3)

- [ ] **Step 1: Append §3 to the guide**

Insert this section after the line `(섹션 §3~§7 은 이 plan 의 Task 11~14 가 채운다.)` in `docs/e2e-real-cluster-guide.md` (replacing that line):

```markdown
## 3. 시나리오 C5' — Sniffer 자가 감사

자동 테스트 (kind): `e2e/sniffer_test.go::TestC5Prime_SnifferSelfAudit`

### 3.1 목적

source DB 에 1000 행을 넣으면 sniffer 가 정확히 1000건을 `transfer_jobs` 로
enqueue 하고, `trace_id` 가 모두 distinct 하며, 워커가 shadow path 로 모두
복사하여 `dead = 0` 이 되는지 확인.

### 3.2 절차

1. 부트스트랩 끝낸 상태 가정 (§1.2). 1KB fixture 시드:
   ```bash
   make e2e-seed-real
   ```

2. control DB / source DB 포트포워드 (§1.4).

3. 깨끗한 출발 — control DB 와 sniffer watermark 초기화:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE sniffer_state'

   # dst 디렉토리 비움 (이전 run 의 결과 파일 잔재 제거)
   kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
     --image=alpine:3.20 wipe-dst \
     --overrides='{"spec":{"containers":[{"name":"w","image":"alpine:3.20","command":["sh","-c","rm -rf /srv/imgsync/dst && mkdir -p /srv/imgsync/dst"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
     --

   kubectl -n imgsync-e2e-real rollout restart deploy/imgsync deploy/imgsync-sniffer
   kubectl -n imgsync-e2e-real rollout status deploy/imgsync
   kubectl -n imgsync-e2e-real rollout status deploy/imgsync-sniffer
   ```

4. source DB 에 schema + 1000 행 (`updated_at` 은 sniffer 의 `biasSec=5` 보다 큰 10초 전):
   ```bash
   psql 'postgres://source:source@127.0.0.1:5434/source?sslmode=disable' <<'SQL'
   CREATE TABLE IF NOT EXISTS images (
     id         BIGSERIAL PRIMARY KEY,
     updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
     file_path  TEXT        NOT NULL
   );
   TRUNCATE images RESTART IDENTITY;
   INSERT INTO images (updated_at, file_path)
   SELECT NOW() - INTERVAL '10 seconds',
          'file-' || lpad(i::text, 5, '0')
     FROM generate_series(1, 1000) AS i;
   SQL
   ```

5. drain 폴링 — sniffer interval=5s, 워커가 비울 때까지:
   ```bash
   while true; do
     read ENQ PEN DEAD <<<$(psql -At -F' ' \
       'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
       SELECT count(*),
              count(*) FILTER (WHERE status='pending'),
              count(*) FILTER (WHERE status='dead')
         FROM transfer_jobs")
     echo "$(date +%T) enqueued=$ENQ pending=$PEN dead=$DEAD"
     [ "$ENQ" -ge 1000 ] && [ "$PEN" -eq 0 ] && break
     sleep 3
   done
   ```

### 3.3 검증 체크리스트

- [ ] `enqueued = 1000`
  ```sql
  SELECT count(*) FROM transfer_jobs;
  ```
- [ ] `count(distinct trace_id) = 1000`
  ```sql
  SELECT count(DISTINCT trace_id) FROM transfer_jobs;
  ```
- [ ] `dead = 0`
  ```sql
  SELECT count(*) FROM transfer_jobs WHERE status='dead';
  ```
- [ ] dst 가 shadow suffix 와 함께 실제 존재:
  ```bash
  kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
    --image=alpine:3.20 ls-shadow \
    --overrides='{"spec":{"containers":[{"name":"l","image":"alpine:3.20","command":["sh","-c","ls /srv/imgsync/dst/file-00001.bin.imgsync_shadow_v1 2>&1 || echo MISSING"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
    --
  # 기대: 파일 한 줄 (size 1024 근처)
  ```

### 3.4 멱등성 확인 (선택)

같은 source 데이터로 60초 더 기다린 뒤:

```sql
-- 새 잡이 안 생겼는가
SELECT count(*) FROM transfer_jobs;   -- 여전히 1000
-- 동일 trace_id 의 enqueue 이벤트가 1회뿐인가
SELECT trace_id, count(*) FROM transfer_events
 WHERE status='enqueue' GROUP BY trace_id HAVING count(*) > 1;
-- 0 rows
```
```

- [ ] **Step 2: Run the scenario end-to-end**

Follow §3.2 in the guide you just wrote. The whole run should take 2–4 minutes.

- [ ] **Step 3: Verify the checklist**

Run all four assertions in §3.3. Record actual values:
- `enqueued`: must be 1000
- `distinct trace_id`: must be 1000
- `dead`: must be 0
- shadow file: must exist, ~1024 bytes

If any assertion fails, **stop the plan and investigate** — do not paper over with retries. Common cause on first run: pre-install migrate Job not idempotent (re-applies failed); see `kubectl -n imgsync-e2e-real get jobs`.

- [ ] **Step 4: Commit guide + record run**

```bash
git add docs/e2e-real-cluster-guide.md
git commit -m "docs(e2e): C5' sniffer scenario in real-cluster guide (verified PASS)"
```

---

### Task 12: F5a mid-flight kill scenario — extend guide + smoke run

**Files:**
- Modify: `docs/e2e-real-cluster-guide.md` (add §4)

- [ ] **Step 1: Append §4**

Append this section to `docs/e2e-real-cluster-guide.md` after §3:

```markdown
## 4. 시나리오 F5a — Mid-flight 워커 강제 종료 후 sweeper 회복

자동 테스트 (kind): `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5a_mid_flight_kill`

### 4.1 목적

워커 한 대를 SIGKILL 로 떨어뜨려도 sweeper 가 leased→pending 으로 회복시켜
모든 잡이 결국 `succeeded` 로 끝나는지 확인. 사라진 잡이 0, 좀비 leased 도 0.

### 4.2 절차

1. 깨끗한 출발 + replicas=2 (§3.2 step 3 + helm upgrade replicaCount=2 보장):
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'

   helm -n imgsync-e2e-real upgrade --install imgsync deploy/helm/imgsync \
     -f e2e/manifests/real/values-real.yaml \
     --set image.repository=ghcr.io/nineking424/imgsync \
     --set image.tag=e2e-$(git rev-parse --short HEAD) \
     --set replicaCount=2 \
     --wait --timeout 5m
   kubectl -n imgsync-e2e-real rollout status deploy/imgsync
   ```

2. fixture 100건이 깔려 있는지 확인 (1000 이상이면 OK — F5a 는 100만 enqueue):
   ```bash
   make e2e-seed-real
   ```

3. 100건 enqueue:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'f5a-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 100) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL
   ```

4. ≥1건이 leased 가 될 때까지 기다리기:
   ```bash
   while true; do
     L=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='leased'")
     [ "$L" -gt 0 ] && break
     sleep 0.2
   done
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     "SELECT id, locked_by FROM transfer_jobs WHERE status='leased'"
   ```

5. 워커 한 대 강제 종료:
   ```bash
   POD=$(kubectl -n imgsync-e2e-real get pods -l app.kubernetes.io/name=imgsync \
         -o jsonpath='{.items[0].metadata.name}')
   kubectl -n imgsync-e2e-real delete pod "$POD" --grace-period=0 --force
   ```

6. 시간 단축 트릭 — leased 의 `locked_at` 을 6분 전으로 점프:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     UPDATE transfer_jobs
        SET locked_at = NOW() - INTERVAL '6 minutes'
      WHERE status='leased'"
   ```

7. 5분 budget 으로 100건 모두 succeeded 폴링:
   ```bash
   START=$(date +%s)
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "$(date +%T) succeeded=$N"
     [ "$N" -ge 100 ] && break
     [ $(($(date +%s) - START)) -gt 300 ] && { echo "TIMEOUT"; break; }
     sleep 2
   done
   ```

### 4.3 검증 체크리스트

- [ ] 5분 내 100건 모두 succeeded
- [ ] dead = 0, leased = 0
  ```sql
  SELECT status, count(*) FROM transfer_jobs GROUP BY status;
  ```
- [ ] sweeper 가 회수한 잡 ≥ 1건 + 그 잡들의 attempts = 0:
  ```sql
  SELECT count(*) FROM transfer_jobs j
   WHERE j.status='succeeded' AND j.attempts=0
     AND EXISTS (
       SELECT 1 FROM transfer_events e
        WHERE e.trace_id=j.trace_id AND e.job_id=j.id AND e.status='expire');
  -- 기대: ≥ 1
  ```
```

- [ ] **Step 2: Run + verify checklist** (§4.2, §4.3)

- [ ] **Step 3: Commit**

```bash
git add docs/e2e-real-cluster-guide.md
git commit -m "docs(e2e): F5a mid-flight kill scenario in real-cluster guide (verified PASS)"
```

---

### Task 13: F5b helm rollback + F5c uninstall/reinstall scenarios

**Files:**
- Modify: `docs/e2e-real-cluster-guide.md` (add §5, §6)

- [ ] **Step 1: Append §5 (F5b)**

Append to `docs/e2e-real-cluster-guide.md`:

```markdown
## 5. 시나리오 F5b — 잘못된 helm upgrade → rollback 회복

자동 테스트 (kind): `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5b_bad_upgrade_then_rollback`

### 5.1 목적

존재하지 않는 이미지 태그로 helm upgrade 했을 때, `helm rollback` 만으로
이전 정상 상태로 돌아오고 in-flight job 이 잃지 않고 완료되는지 확인.

### 5.2 절차

1. 깨끗한 출발 + replicas=2 (good 빌드로 redeploy):
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'

   helm -n imgsync-e2e-real upgrade --install imgsync deploy/helm/imgsync \
     -f e2e/manifests/real/values-real.yaml \
     --set image.repository=ghcr.io/nineking424/imgsync \
     --set image.tag=e2e-$(git rev-parse --short HEAD) \
     --set replicaCount=2 \
     --wait --timeout 5m
   ```

2. 50건 enqueue (`f5b-` prefix, §4.2 step 3 의 INSERT 와 동일하되 prefix 만 변경, 1..50):

   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'f5b-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 50) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL
   ```

3. 10건 이상 succeeded 까지 warm-up 대기:
   ```bash
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "warm-up succeeded=$N"
     [ "$N" -ge 10 ] && break
     sleep 0.5
   done
   ```

4. 망가진 upgrade — 존재하지 않는 태그:
   ```bash
   helm -n imgsync-e2e-real upgrade --install imgsync deploy/helm/imgsync \
     -f e2e/manifests/real/values-real.yaml \
     --set image.repository=ghcr.io/nineking424/imgsync \
     --set image.tag=does-not-exist \
     --set replicaCount=2 \
     --wait --timeout 30s || true
   ```
   상태 점검 (디버깅용):
   ```bash
   kubectl -n imgsync-e2e-real get pods -l app.kubernetes.io/name=imgsync
   # 기대: 신규 pod 가 ImagePullBackOff / ErrImagePull
   ```

5. rollback:
   ```bash
   helm -n imgsync-e2e-real rollback imgsync --wait --timeout 3m
   kubectl -n imgsync-e2e-real rollout status deploy/imgsync
   ```

6. 5분 budget 으로 50건 모두 succeeded 폴링 (§4.2 step 7 동일 형태).

### 5.3 검증 체크리스트

- [ ] `helm history imgsync -n imgsync-e2e-real` 에서 bad revision 이 failed/superseded
- [ ] 5분 내 50건 모두 succeeded
- [ ] dead = 0, leased = 0
```

- [ ] **Step 2: Append §6 (F5c)**

Append to `docs/e2e-real-cluster-guide.md`:

```markdown
## 6. 시나리오 F5c — uninstall → reinstall 멱등 마이그레이션

자동 테스트 (kind): `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5c_uninstall_reinstall_idempotent_migration`

### 6.1 목적

`helm uninstall` 은 DB 를 건드리지 않는다. 잡 30건을 enqueue 한 뒤 워커가
일을 다 끝내기 전에 uninstall 했다가, 다시 install 하면 pre-install hook 이
멱등하게 migrate 를 다시 돌리고 잔여 잡을 워커가 마저 처리하는지 확인.

### 6.2 절차

1. 깨끗한 출발 + replicas=2 (§5.2 step 1 동일).

2. 30건 enqueue (`f5c-` prefix, §5.2 step 2 의 INSERT 에서 50→30, prefix 변경).

3. 빠른 uninstall (잡이 끝나기 전에):
   ```bash
   helm -n imgsync-e2e-real uninstall imgsync --wait --timeout 2m
   ```

4. uninstall 직후 DB 상태 캡처:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     SELECT status, count(*) FROM transfer_jobs GROUP BY status"
   # 기대: pending+leased+succeeded == 30 (분포는 워커 속도에 따라 다름)
   ```

5. orphan leased 빠른 회복용 시간 점프:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     UPDATE transfer_jobs SET locked_at = NOW() - INTERVAL '6 minutes'
      WHERE status='leased'"
   ```

6. reinstall — pre-install hook 이 migrate 를 재실행 (멱등이어야 함):
   ```bash
   # uninstall 이 ServiceAccount 도 지우므로 다시 만들어준다
   kubectl apply -f - <<'EOF'
   apiVersion: v1
   kind: ServiceAccount
   metadata:
     name: imgsync
     namespace: imgsync-e2e-real
     labels:
       app.kubernetes.io/managed-by: Helm
     annotations:
       meta.helm.sh/release-name: imgsync
       meta.helm.sh/release-namespace: imgsync-e2e-real
   EOF

   helm -n imgsync-e2e-real upgrade --install imgsync deploy/helm/imgsync \
     -f e2e/manifests/real/values-real.yaml \
     --set image.repository=ghcr.io/nineking424/imgsync \
     --set image.tag=e2e-$(git rev-parse --short HEAD) \
     --set replicaCount=2 \
     --wait --timeout 5m
   ```

7. 5분 내 30건 모두 succeeded 폴링.

### 6.3 검증 체크리스트

- [ ] uninstall 직후: `pending+leased+succeeded == 30`, dead = 0
- [ ] reinstall 시 pre-install Job 이 Completed:
  ```bash
  kubectl -n imgsync-e2e-real get jobs
  ```
- [ ] 5분 내 30건 succeeded
- [ ] dead = 0, leased = 0
```

- [ ] **Step 3: Run F5b end-to-end (§5.2)** + verify checklist (§5.3)

- [ ] **Step 4: Run F5c end-to-end (§6.2)** + verify checklist (§6.3)

- [ ] **Step 5: Commit**

```bash
git add docs/e2e-real-cluster-guide.md
git commit -m "docs(e2e): F5b rollback + F5c uninstall/reinstall scenarios (verified PASS)"
```

---

### Task 14: C7 throughput smoke scenario

**Files:**
- Modify: `docs/e2e-real-cluster-guide.md` (add §7)

This is **smoke-only**. We do not assert linearity ratio 3.2x because NFS bandwidth, not pod CPU, is the bottleneck on this homelab. The valuable invariants are: (a) 8 replicas come up, (b) 1000 jobs all complete, (c) `dead = 0`.

- [ ] **Step 1: Append §7**

Append to `docs/e2e-real-cluster-guide.md`:

```markdown
## 7. 시나리오 C7 — Throughput Scale-Out (Smoke Only)

자동 테스트 (kind): `e2e/throughput_test.go::TestC7_ThroughputScaleOut`

### 7.1 목적 — 그리고 한계

kind 환경에서는 `tputB / tputA ≥ 3.2` (선형성 80%) 를 강제하지만, 본 가이드의
실제 클러스터(homelab Talos + NFS) 에서는 **bandwidth-bound** 가 되므로
linearity 강제는 의미 없음. C7 의 가치 있는 invariant 는:

1. replicas=8 로 helm upgrade 가 5분 안에 완료
2. 1000 잡이 15분 안에 모두 succeeded
3. dead = 0

선형성 측정은 데이터로 *기록* 만 한다 (PASS/FAIL 판정 X).

### 7.2 절차

1. 1MB × 1000 fixture 시드 (NFS 1GB):
   ```bash
   ./scripts/e2e-seed-real.sh 1000 1048576
   ```

2. replicas=2, 깨끗한 출발 (§4.2 step 1 동일).

3. Phase A — 1000 jobs:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'phaseA-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 1000) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL

   START_A=$(date +%s)
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "$(date +%T) phaseA succeeded=$N"
     [ "$N" -ge 1000 ] && break
     [ $(($(date +%s) - START_A)) -gt 900 ] && { echo "TIMEOUT"; break; }
     sleep 2
   done
   END_A=$(date +%s)
   DUR_A=$(( END_A - START_A ))
   TPUT_A=$(awk "BEGIN{printf \"%.2f\", 1000 / $DUR_A}")
   echo "Phase A: ${DUR_A}s → ${TPUT_A} jobs/sec"
   ```

4. Phase B — replicas=8, 1000 fresh jobs:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'

   # dst wipe
   kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
     --image=alpine:3.20 wipe-dst-c7 \
     --overrides='{"spec":{"containers":[{"name":"w","image":"alpine:3.20","command":["sh","-c","rm -rf /srv/imgsync/dst && mkdir -p /srv/imgsync/dst"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
     --

   SCALE_START=$(date +%s)
   helm -n imgsync-e2e-real upgrade --install imgsync deploy/helm/imgsync \
     -f e2e/manifests/real/values-real.yaml \
     --set image.repository=ghcr.io/nineking424/imgsync \
     --set image.tag=e2e-$(git rev-parse --short HEAD) \
     --set replicaCount=8 \
     --wait --timeout 5m
   kubectl -n imgsync-e2e-real rollout status deploy/imgsync
   SCALE_END=$(date +%s)
   echo "Scale 2→8 ready in $((SCALE_END - SCALE_START))s"   # 기대: ≤ 300

   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'phaseB-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 1000) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL

   START_B=$(date +%s)
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "$(date +%T) phaseB succeeded=$N"
     [ "$N" -ge 1000 ] && break
     [ $(($(date +%s) - START_B)) -gt 900 ] && { echo "TIMEOUT"; break; }
     sleep 2
   done
   END_B=$(date +%s)
   DUR_B=$(( END_B - START_B ))
   TPUT_B=$(awk "BEGIN{printf \"%.2f\", 1000 / $DUR_B}")
   RATIO=$(awk "BEGIN{printf \"%.2f\", $TPUT_B / $TPUT_A}")
   echo "Phase B: ${DUR_B}s → ${TPUT_B} jobs/sec, ratio=${RATIO}"
   ```

### 7.3 검증 체크리스트 (smoke)

- [ ] Phase A: 15분 내 1000건 succeeded
- [ ] Phase B: 15분 내 1000건 succeeded
- [ ] Scale 2→8 ready ≤ 5분
- [ ] 양 phase 종료 시점 dead = 0
- [ ] (정보용) ratio 기록: __________ (kind 에서 ≥ 3.2; NFS 에서는 typically 1.5–2.5)
```

- [ ] **Step 2: Run §7.2 end-to-end** + record `Phase A / Phase B / ratio` numbers

- [ ] **Step 3: Verify checklist** §7.3 — only the four hard requirements (succeeded counts, scale time, dead=0). The ratio is informational.

- [ ] **Step 4: Replace the placeholder ratio in the doc with the actual measured value**

Edit `docs/e2e-real-cluster-guide.md` to fill in the actual ratio you observed in §7.3 last line:

```markdown
- [ ] (정보용) ratio 기록: __________ (kind 에서 ≥ 3.2; NFS 에서는 typically 1.5–2.5)
```
…replacing `__________` with whatever your run produced (e.g., `1.78`).

- [ ] **Step 5: Commit**

```bash
git add docs/e2e-real-cluster-guide.md
git commit -m "docs(e2e): C7 throughput smoke scenario in real-cluster guide (verified PASS)"
```

---

### Task 15: Test report

**Files:**
- Create: `docs/test-reports/2026-05-03-imgsync-real-cluster-<sha>.md`

- [ ] **Step 1: Compute the report path**

Run:
```bash
SHA=$(git rev-parse --short HEAD)
echo "docs/test-reports/2026-05-03-imgsync-real-cluster-${SHA}.md"
```

- [ ] **Step 2: Write the report**

Create the file with the following template, filling in actual numbers from Tasks 11–14:

```markdown
# imgsync Real-Cluster E2E — 2026-05-03

**Cluster:** admin@talos-homelab (3 cp + 2 worker, Talos v1.13, k8s v1.36)
**Image:** ghcr.io/nineking424/imgsync:e2e-<sha-here>
**Repo SHA:** <sha-here>

| Scenario | Spec | Result | Duration | Notes |
|----------|------|--------|----------|-------|
| C5' Sniffer self-audit | enqueued=1000, distinct trace_id=1000, dead=0, shadow file present | __FILL__ (PASS/FAIL) | __FILL__ | __FILL__ |
| F5a Mid-flight kill | 100 succeeded ≤5m, dead=0, leased=0, ≥1 sweeper-recovered job | __FILL__ | __FILL__ | __FILL__ |
| F5b Bad upgrade + rollback | 50 succeeded ≤5m, dead=0, leased=0 | __FILL__ | __FILL__ | __FILL__ |
| F5c Uninstall + reinstall | 30 succeeded ≤5m, dead=0, leased=0 | __FILL__ | __FILL__ | __FILL__ |
| C7 Smoke | 1000 + 1000 succeeded ≤15m each, scale 2→8 ≤5m, dead=0 | __FILL__ | __FILL__ | tputA=__/s tputB=__/s ratio=__ (informational) |

## Anomalies

(빈 칸 — 검증 중 예상 밖 동작 발견 시 여기에 기록)

## Environment notes

- Storage class: nfs-client (default), NFS subdir provisioner
- Postgres on NFS PVC — adequate for 5-15min tests, not representative of prod
- C7 ratio is bandwidth-bound (NFS), not CPU-bound (kind). Ratio ≥ 3.2 strict
  assertion stays kind-only.
```

Replace every `__FILL__` with the actual observed value. Replace `<sha-here>` with `git rev-parse --short HEAD`.

- [ ] **Step 3: Cross-link from guide**

Append to the bottom of `docs/e2e-real-cluster-guide.md` §10 (참고):

```markdown
- 가장 최근 검증 결과: [`docs/test-reports/2026-05-03-imgsync-real-cluster-<sha>.md`](test-reports/2026-05-03-imgsync-real-cluster-<sha>.md)
```

(replacing `<sha>` with the actual SHA used in Step 1)

- [ ] **Step 4: Final teardown**

Run:
```bash
make e2e-down-real
```

Expected: namespace deleted within 3 minutes.

- [ ] **Step 5: Commit**

```bash
git add docs/test-reports/2026-05-03-imgsync-real-cluster-*.md docs/e2e-real-cluster-guide.md
git commit -m "docs(e2e): test report for real-cluster smoke run + guide cross-link"
```

---

## Self-Review Notes

**Spec coverage:** all 5 e2e scenarios from `docs/e2e-manual-guide.md` are mirrored — C5' (Task 11), F5a (Task 12), F5b/F5c (Task 13), C7 (Task 14). Bootstrap parity (image + storage + DBs + helm install) is in Tasks 1–9. Documentation parity is in Tasks 10–14. Test artifact in Task 15.

**Image pull strategy:** ghcr.io public package — chosen over in-cluster registry because Talos requires per-registry containerd patches for HTTP registries, which is invasive. ghcr.io needs no node-side changes.

**Storage:** all PVCs use the cluster's default `nfs-client` SC. Postgres on NFS is sub-optimal for prod but adequate for tests. C7 throughput linearity is downgraded to smoke-only because NFS is bandwidth-bound — explicitly called out in Task 14 §7.1.

**Chart precondition:** Task 5 Step 2 contains a chart-side precondition check for `extraVolumes` / `extraVolumeMounts`. If the chart doesn't already surface them, the plan stops and prompts a 5-line chart patch as a separate commit before resuming. This is a known fork-in-the-road, marked clearly.

**Identifier consistency:** namespace `imgsync-e2e-real`, PVC `imgsync-localfs`, helm release `imgsync`, image repo `ghcr.io/nineking424/imgsync`, tag `e2e-<sha>` — used identically across every task.

**Type/name coherency:** `e2e/manifests/real/...` (not `external/`) — standardized. `scripts/e2e-up-real.sh` (not `e2e-real-up.sh`) — standardized.

---
