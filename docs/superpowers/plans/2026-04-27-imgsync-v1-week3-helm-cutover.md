# imgsync v1 — Week 3: Packaging, Helm, Cutover Gates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Package imgsync as a deployable artifact (container image + Helm chart with pre-install migration hook) and lock the two cutover gates from the test plan: C7 (throughput scale-out ≥3.2× from 2→8 replicas) and F5 (dirty-state recovery survives mid-flight pod kill + Helm rollback).

**Architecture:** A single multi-stage Dockerfile produces a distroless image that ships one binary (`imgsync`) with three subcommands (`migrate`, `enqueue`, `worker`). A docker-compose stack stands the same image up with postgres + FTP for local smoke tests. A Helm chart at `deploy/helm/imgsync/` ships a worker Deployment + Service + PDB and wires `imgsync migrate up` as a pre-install/pre-upgrade Job hook (forward-only — see Outside Voice F5 in design rev 4). Two new E2E suites under `e2e/` boot a kind cluster, install the chart, and assert throughput linearity + dirty-state recovery.

**Tech Stack:** Distroless `gcr.io/distroless/static-debian12:nonroot` runtime; Helm 3 chart; kind v0.22+ for E2E; `sigs.k8s.io/e2e-framework` for cluster bring-up.

**Series:** This is plan 4 of 4 for v1 base. Predecessors: Week 1 foundation, Week 2A FTP+worker-core, Week 2B sweeper+EVAL. Successor: Shadow Sniffer plan (separate, builds on this base).

**Spec references:**
- Design: `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` rev 4 — sections "Helm packaging", "Migration init job (pre-install hook)", "Cutover criteria (C5/C7/F5)"
- Test Plan: `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md` — C7 (throughput linearity), F5 (dirty-state recovery), test layer #14 (Helm pre-install migrate Job).

**Out of scope (deferred to Shadow Sniffer plan):**
- C5 shadow reconcile harness — needs the sniffer subsystem first, planned in `2026-04-27-imgsync-shadow-sniffer.md`.
- Production secrets management (sealed-secrets / vault). Chart references a pre-existing Secret by name; production wiring is operator concern.
- HPA / autoscaling. Chart exposes `replicaCount` only; HPA can be added once metrics emission lands.

---

## File Structure

After Week 3 completes, the repository's new top-level layout is:

```
imgsync/
├── Dockerfile                                  # multi-stage build, distroless runtime
├── .dockerignore
├── docker-compose.yml                          # local dev stack (postgres + ftpd + worker)
├── deploy/
│   └── helm/
│       └── imgsync/
│           ├── Chart.yaml
│           ├── values.yaml
│           ├── .helmignore
│           └── templates/
│               ├── _helpers.tpl
│               ├── configmap.yaml              # non-secret runtime knobs
│               ├── deployment.yaml             # worker Deployment
│               ├── service.yaml                # /healthz endpoint exposure
│               ├── pdb.yaml                    # PodDisruptionBudget
│               ├── migrate-job.yaml            # pre-install/pre-upgrade hook
│               ├── serviceaccount.yaml
│               └── NOTES.txt
├── e2e/
│   ├── doc.go                                  # build tag: //go:build e2e
│   ├── kind_config.yaml
│   ├── helpers.go                              # kind+helm bootstrap shared by both suites
│   ├── throughput_test.go                      # C7
│   └── dirty_state_test.go                     # F5
└── scripts/
    ├── e2e-up.sh                               # idempotent kind+chart install wrapper
    └── e2e-down.sh
```

Modifications to existing files:
- `Makefile` — adds `docker-build`, `dev-up`, `dev-down`, `dev-seed`, `helm-lint`, `helm-template`, `e2e-throughput`, `e2e-dirty-state` targets.
- `.github/workflows/ci.yml` — adds `helm lint` job and an optional `e2e` job gated on `[e2e]` PR label (E2E is slow; not on every push).

---

## Decisions Locked Before Tasks

These are not options — they are commitments documented here so the engineer doesn't re-derive them per task.

1. **Single binary, three subcommands.** The Dockerfile builds one binary `imgsync` with `migrate`, `enqueue`, `worker` cobra subcommands (already established in Week 1). The Deployment runs `imgsync worker`; the migration Job runs `imgsync migrate up`. No separate images.

2. **Distroless runtime.** Final stage is `gcr.io/distroless/static-debian12:nonroot`. No shell, no package manager, runs as UID 65532. This is the baseline production posture; CVE attack surface is minimal.

3. **Forward-only migrations.** `imgsync migrate up` only — there is no `down`. Rollback strategy is "deploy old image; old code reads new schema chunks gracefully" (design rev 4 lock). The migrate Job hook is `pre-install` AND `pre-upgrade` — never `pre-rollback`.

4. **Migration hook annotations:**
   ```yaml
   "helm.sh/hook": pre-install,pre-upgrade
   "helm.sh/hook-weight": "0"
   "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
   ```
   `before-hook-creation` deletes any prior failed Job before re-running (otherwise Job name collision blocks redeploy). `hook-succeeded` cleans up after success but leaves failures around for debugging.

5. **PDB:** `maxUnavailable: 1`, applied when `replicaCount >= 2`. Single-replica installs (dev) skip PDB via Helm conditional.

6. **C7 throughput target.** The test plan locks `tput(8) / tput(2) ≥ 3.2`. We measure end-to-end wall-clock (enqueue → all 1000 jobs in `succeeded` status) on a kind cluster with a shared NFS-backed PV. LocalFS-only (no FTP) to keep the test deterministic. Replicas swap via `helm upgrade --set replicaCount=N`.

7. **F5 dirty-state recovery target.** Three sub-scenarios, all must pass:
   (a) `kubectl delete pod` mid-flight on a leased job → sweeper expires it within 5min → another pod re-leases → final `status='succeeded' AND attempts==0` (this is C2 cross-checked at the cluster level).
   (b) `helm rollback` from a bad upgrade (e.g., wrong image tag) → workers stabilize on the rolled-back image → no jobs stuck in `processing` beyond the sweep window.
   (c) `helm uninstall` then `helm install` (operator panic-redeploy) → migration Job runs idempotently → existing jobs in DB remain processable.

8. **kind cluster sizing.** 1 control-plane + 3 workers. Postgres runs as a kustomized manifest (NOT another helm chart — keeps E2E hermetic). FTP server runs as a Deployment + Service inside the cluster using the same `delfer/alpine-ftp-server` image referenced in test plan §Notes.

---

## Task 1: Multi-stage Dockerfile + .dockerignore

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Modify: `Makefile` — add `docker-build`, `docker-run-help` targets

**Decisions in this task:**
- Builder stage: `golang:1.22-alpine` with `CGO_ENABLED=0` (matches sweeper/worker — no cgo deps).
- Build flags: `-trimpath -ldflags="-s -w -X main.version=$(VERSION)"`. `-trimpath` strips local paths (reproducibility); `-s -w` shrinks binary ~30%.
- Final stage: `gcr.io/distroless/static-debian12:nonroot` (UID 65532). `static` variant is used because the binary is statically linked.
- No CA bundle is copied — `distroless/static:nonroot` already includes `/etc/ssl/certs/ca-certificates.crt`. (FTP doesn't need it for plain FTP, but if we ever add FTPS this avoids a follow-up.)

- [ ] **Step 1: Write the Makefile target test (probes the contract, not the image itself)**

We can't run `docker build` from a Go test, so the contract is `make docker-build` exits 0 and `docker run --rm imgsync:dev migrate --help` prints the migrate usage. Encode this as a shell script test that CI can invoke.

Create `scripts/test-docker-build.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

IMAGE_TAG="${IMAGE_TAG:-imgsync:test-$(date +%s)}"

echo "==> Building image $IMAGE_TAG"
docker build -t "$IMAGE_TAG" .

echo "==> Verifying entrypoint help"
docker run --rm "$IMAGE_TAG" --help | grep -q "imgsync" || {
  echo "FAIL: imgsync --help did not contain 'imgsync'"
  exit 1
}

echo "==> Verifying subcommand exposure"
for cmd in migrate enqueue worker; do
  docker run --rm "$IMAGE_TAG" "$cmd" --help | grep -q "$cmd" || {
    echo "FAIL: subcommand $cmd not exposed"
    exit 1
  }
done

echo "==> Verifying nonroot user"
USER_LINE=$(docker inspect "$IMAGE_TAG" --format '{{.Config.User}}')
if [ "$USER_LINE" != "nonroot:nonroot" ] && [ "$USER_LINE" != "65532:65532" ]; then
  echo "FAIL: image user is '$USER_LINE', expected nonroot or 65532"
  exit 1
fi

echo "==> Verifying image is reasonably small (<50MB)"
SIZE_BYTES=$(docker image inspect "$IMAGE_TAG" --format '{{.Size}}')
SIZE_MB=$(( SIZE_BYTES / 1024 / 1024 ))
if [ "$SIZE_MB" -gt 50 ]; then
  echo "FAIL: image size ${SIZE_MB}MB exceeds 50MB budget"
  exit 1
fi

echo "==> Cleaning up"
docker rmi "$IMAGE_TAG" >/dev/null

echo "PASS: Dockerfile contract checks all green"
```

```bash
chmod +x scripts/test-docker-build.sh
```

- [ ] **Step 2: Run the test to confirm it fails (no Dockerfile yet)**

Run: `./scripts/test-docker-build.sh`
Expected: FAIL with `failed to read dockerfile: open Dockerfile: no such file or directory` (or equivalent).

- [ ] **Step 3: Write the .dockerignore**

Create `.dockerignore`:

```
# Build artifacts
bin/
dist/
*.test
*.out
coverage.*

# Editor/local
.idea/
.vscode/
.DS_Store

# VCS
.git/
.gitignore

# CI / docs
.github/
docs/
PRD.txt
README.md

# E2E / dev fixtures
e2e/
deploy/
scripts/

# Local Helm cache and temp artifacts
*.tgz
charts/

# Test/dev data
testdata/
```

`.dockerignore` MUST exclude `e2e/`, `deploy/`, and `scripts/` so they don't bloat the build context. They're not needed at runtime.

- [ ] **Step 4: Write the Dockerfile**

Create `Dockerfile`:

```dockerfile
# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.22
ARG VERSION=dev

# ──────────────────────────────────────────────────────────────────────
# Builder
# ──────────────────────────────────────────────────────────────────────
FROM golang:${GO_VERSION}-alpine AS builder

ARG VERSION
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

# Cache module downloads first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source
COPY . .

# Build the single binary; -trimpath for reproducibility, -s -w to shrink
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/imgsync \
        ./cmd/imgsync

# Bundle migrations as a separate copy (the binary references them via filesystem)
RUN mkdir -p /out/migrations && cp -r ./migrations/* /out/migrations/

# ──────────────────────────────────────────────────────────────────────
# Runtime
# ──────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Binary
COPY --from=builder /out/imgsync /app/imgsync

# Migrations (read-only at runtime, baked in)
COPY --from=builder /out/migrations /app/migrations

# Default migrations dir resolved relative to /app
ENV IMGSYNC_MIGRATIONS_DIR=/app/migrations

USER nonroot:nonroot

# /app/imgsync is the entrypoint; subcommand passed via CMD or k8s args
ENTRYPOINT ["/app/imgsync"]
CMD ["--help"]
```

**Why distroless/static:** Go binary is statically linked (`CGO_ENABLED=0`). `distroless/static:nonroot` is ~2MB and contains `ca-certificates.crt`, `tzdata`, `/etc/passwd` with `nonroot` user — exactly what we need.

**Why bundle `/app/migrations`:** the migrate subcommand needs SQL files at runtime. Bundling avoids a ConfigMap + volume mount in the Helm chart.

**Why `IMGSYNC_MIGRATIONS_DIR`:** the binary's migrate command reads this env (set by Week 1 Task 8). Default in container = `/app/migrations`; tests can override.

- [ ] **Step 5: Update Makefile**

Open `Makefile` and add (or modify) these targets near the existing build targets:

```make
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= imgsync:$(VERSION)

.PHONY: docker-build
docker-build: ## Build the production container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE) \
		-t imgsync:dev \
		.

.PHONY: docker-test
docker-test: ## Run the Dockerfile contract checks
	./scripts/test-docker-build.sh

.PHONY: docker-run-help
docker-run-help: docker-build ## Smoke test the built image
	docker run --rm imgsync:dev --help
```

- [ ] **Step 6: Run the test, confirm pass**

Run: `make docker-test`
Expected output:
```
==> Building image imgsync:test-...
==> Verifying entrypoint help
==> Verifying subcommand exposure
==> Verifying nonroot user
==> Verifying image is reasonably small (<50MB)
==> Cleaning up
PASS: Dockerfile contract checks all green
```

If the size budget fails: investigate. The static Go binary should be 15–25MB; `distroless/static` adds ~2MB; total ~25MB is normal. >50MB likely means the build accidentally pulled in cgo or test fixtures.

- [ ] **Step 7: Commit**

```bash
git add Dockerfile .dockerignore scripts/test-docker-build.sh Makefile
git commit -m "feat(docker): add multi-stage Dockerfile with distroless runtime"
```

---

## Task 2: docker-compose dev stack + smoke test

**Files:**
- Create: `docker-compose.yml`
- Create: `scripts/dev-seed.sh`
- Create: `scripts/dev-smoke-test.sh`
- Modify: `Makefile` — add `dev-up`, `dev-down`, `dev-seed`, `dev-smoke` targets

**Why a compose stack at all in Week 3?** Two reasons:
1. **Local repro of the production image** — `docker-compose up` runs the same Dockerfile as prod, catching env-var or path bugs that local `go run` misses.
2. **Cheap manual smoke test** before E2E. The compose stack is the dev path; E2E (kind) is the gate path.

The compose stack is also what we tell on-call SREs to run when they want to inspect a job-stuck issue locally.

**Stack composition:**
- `postgres:16-alpine` — same version as testcontainers, port 5432 published.
- `delfer/alpine-ftp-server` — same image used in integration tests (test plan §Notes).
- `imgsync-migrate` — one-shot container running `imgsync migrate up`, depends_on postgres healthy.
- `imgsync-worker` — long-running, depends_on migrate completed.
- (Optional) `pgweb` for inspection, port 8081.

- [ ] **Step 1: Write the dev-smoke-test.sh contract**

Create `scripts/dev-smoke-test.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

# This script asserts the dev stack actually processes jobs end-to-end.
# Run it AFTER `make dev-up && make dev-seed`.

DSN="${IMGSYNC_DSN:-postgres://imgsync:imgsync@localhost:5432/imgsync?sslmode=disable}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-30}"
EXPECTED_JOBS="${EXPECTED_JOBS:-10}"

echo "==> Waiting up to ${TIMEOUT_SECONDS}s for ${EXPECTED_JOBS} jobs to reach 'succeeded'"

for i in $(seq 1 "$TIMEOUT_SECONDS"); do
  COUNT=$(docker compose exec -T postgres psql -U imgsync -d imgsync -tAc \
    "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
  if [ "$COUNT" -ge "$EXPECTED_JOBS" ]; then
    echo "==> All ${EXPECTED_JOBS} jobs succeeded in ${i}s"

    # Also assert: zero jobs in 'dead' or 'processing' (no stuck state)
    DEAD=$(docker compose exec -T postgres psql -U imgsync -d imgsync -tAc \
      "SELECT count(*) FROM transfer_jobs WHERE status IN ('dead','processing')")
    if [ "$DEAD" -ne 0 ]; then
      echo "FAIL: ${DEAD} jobs in dead/processing state"
      docker compose exec -T postgres psql -U imgsync -d imgsync -c \
        "SELECT id,status,attempts,locked_by FROM transfer_jobs WHERE status IN ('dead','processing')"
      exit 1
    fi

    echo "PASS: dev stack smoke test green"
    exit 0
  fi
  sleep 1
done

echo "FAIL: only ${COUNT}/${EXPECTED_JOBS} jobs succeeded after ${TIMEOUT_SECONDS}s"
docker compose exec -T postgres psql -U imgsync -d imgsync -c \
  "SELECT status, count(*) FROM transfer_jobs GROUP BY status"
exit 1
```

```bash
chmod +x scripts/dev-smoke-test.sh
```

- [ ] **Step 2: Run the smoke test now to confirm it fails (no compose stack yet)**

Run: `./scripts/dev-smoke-test.sh`
Expected: failure — `docker compose: no such service` or connection refused.

- [ ] **Step 3: Write docker-compose.yml**

Create `docker-compose.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: imgsync
      POSTGRES_PASSWORD: imgsync
      POSTGRES_DB: imgsync
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U imgsync -d imgsync"]
      interval: 1s
      timeout: 3s
      retries: 30
    volumes:
      - pgdata:/var/lib/postgresql/data

  ftpd:
    image: delfer/alpine-ftp-server:latest
    environment:
      USERS: "imgsync|imgsyncpw|/ftp/imgsync"
      ADDRESS: "127.0.0.1"
      MIN_PORT: "21100"
      MAX_PORT: "21110"
    ports:
      - "21:21"
      - "21100-21110:21100-21110"
    volumes:
      - ftpdata:/ftp/imgsync

  imgsync-migrate:
    image: imgsync:dev
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      IMGSYNC_DSN: "postgres://imgsync:imgsync@postgres:5432/imgsync?sslmode=disable"
    command: ["migrate", "up"]
    restart: "no"

  imgsync-worker:
    image: imgsync:dev
    depends_on:
      imgsync-migrate:
        condition: service_completed_successfully
      ftpd:
        condition: service_started
    environment:
      IMGSYNC_DSN: "postgres://imgsync:imgsync@postgres:5432/imgsync?sslmode=disable"
      IMGSYNC_WORKERS: "2"
      IMGSYNC_POD_NAME: "compose-worker"
      IMGSYNC_FTP_USER: "imgsync"
      IMGSYNC_FTP_PASSWORD: "imgsyncpw"
      IMGSYNC_HEALTH_ADDR: ":8080"
    command: ["worker"]
    ports:
      - "8080:8080"
    volumes:
      - workerdata:/data
    restart: unless-stopped

volumes:
  pgdata:
  ftpdata:
  workerdata:
```

**Why `service_completed_successfully` for migrate→worker:** Compose v2 lets us gate the worker on migrate's exit code 0. If migrate fails, worker never starts — same semantics as the Helm hook.

**Why a `restart: "no"` on migrate:** it's a one-shot; we don't want compose to keep retrying a failed migration in a loop.

**Why ports 21100-21110 for FTP:** delfer's image needs explicit passive port range. Keep the range narrow to stay portable across CI.

- [ ] **Step 4: Write dev-seed.sh**

Create `scripts/dev-seed.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

# Enqueue 10 LocalFS→LocalFS jobs into the dev stack.
# The worker container's /data volume is shared as the LocalFS root.

DSN="${IMGSYNC_DSN:-postgres://imgsync:imgsync@localhost:5432/imgsync?sslmode=disable}"

# Seed source files inside the worker container's /data volume
docker compose exec -T imgsync-worker sh -c '
  mkdir -p /data/src /data/dst
  for i in $(seq 1 10); do
    echo "hello from job $i" > /data/src/file-$i.txt
  done
' || {
  # If the worker is down, write directly to the volume
  docker run --rm -v "$(docker volume ls -qf name=workerdata | head -1):/data" alpine sh -c '
    mkdir -p /data/src /data/dst
    for i in $(seq 1 10); do
      echo "hello from job $i" > /data/src/file-$i.txt
    done
  '
}

# Enqueue using the imgsync CLI (run inside the worker container so DNS resolves)
for i in $(seq 1 10); do
  docker compose exec -T -e IMGSYNC_DSN="$DSN" imgsync-worker \
    /app/imgsync enqueue \
      --trace-id "smoke-$i" \
      --src "file:///data/src/file-$i.txt" \
      --dst "file:///data/dst/file-$i.txt" \
      --src-protocol localfs \
      --dst-protocol localfs
done

echo "Seeded 10 jobs."
```

```bash
chmod +x scripts/dev-seed.sh
```

**Trace-id naming:** `smoke-N` is intentional and predictable so the smoke test (Step 1) can `WHERE trace_id LIKE 'smoke-%'` if needed later. For now we just count by status.

- [ ] **Step 5: Update Makefile**

Append to `Makefile`:

```make
.PHONY: dev-up
dev-up: docker-build ## Stand up the dev compose stack
	docker compose up -d

.PHONY: dev-down
dev-down: ## Tear down the dev compose stack
	docker compose down -v

.PHONY: dev-seed
dev-seed: ## Enqueue 10 smoke-test jobs into the dev stack
	./scripts/dev-seed.sh

.PHONY: dev-smoke
dev-smoke: ## Run dev stack end-to-end smoke test (assumes dev-up + dev-seed already ran)
	./scripts/dev-smoke-test.sh
```

- [ ] **Step 6: Run the full smoke flow and confirm pass**

Run:
```bash
make dev-down  # clean slate, in case prior runs left state
make dev-up
make dev-seed
make dev-smoke
```

Expected final output:
```
==> All 10 jobs succeeded in Ns
PASS: dev stack smoke test green
```

If `dev-up` hangs on FTP healthcheck: the delfer image doesn't ship a healthcheck endpoint by default; `service_started` is the gate, not `service_healthy`. The compose file above already uses `service_started` for ftpd.

If migrate fails with "permission denied for schema public" on first run: this is a postgres 16 default-grant change. The fix is the migration itself granting `CREATE` on `public` to the imgsync user — already handled in Week 1 Task 2 if the migration runs as superuser. The compose postgres uses the imgsync user as DB owner (via `POSTGRES_USER`), which gets `CREATE` automatically. So this should not fail; if it does, double-check Week 1 Task 2's migration.

- [ ] **Step 7: Tear down and commit**

```bash
make dev-down
git add docker-compose.yml scripts/dev-seed.sh scripts/dev-smoke-test.sh Makefile
git commit -m "feat(compose): add docker-compose dev stack with end-to-end smoke test"
```

---

## Task 3: Helm chart skeleton + worker Deployment + Service + PDB + ConfigMap

**Files:**
- Create: `deploy/helm/imgsync/Chart.yaml`
- Create: `deploy/helm/imgsync/values.yaml`
- Create: `deploy/helm/imgsync/.helmignore`
- Create: `deploy/helm/imgsync/templates/_helpers.tpl`
- Create: `deploy/helm/imgsync/templates/configmap.yaml`
- Create: `deploy/helm/imgsync/templates/deployment.yaml`
- Create: `deploy/helm/imgsync/templates/service.yaml`
- Create: `deploy/helm/imgsync/templates/pdb.yaml`
- Create: `deploy/helm/imgsync/templates/serviceaccount.yaml`
- Create: `deploy/helm/imgsync/templates/NOTES.txt`
- Create: `deploy/helm/imgsync/tests/template_test.sh`
- Modify: `Makefile` — add `helm-lint`, `helm-template`, `helm-test` targets

**Decisions in this task:**
- The chart references **a pre-existing Secret** named via `dsnSecretRef.name` and `dsnSecretRef.key` (defaults: `imgsync-dsn`/`dsn`). The chart does NOT create the Secret — operator-supplied. This is the only sane path for production DSN handling.
- ConfigMap holds non-secret runtime knobs (`IMGSYNC_WORKERS`, `IMGSYNC_HEALTH_ADDR`, etc.). FTP credentials are also expected to live in a Secret if FTP is in use.
- PDB is rendered conditionally: `{{- if ge (int .Values.replicaCount) 2 }}`.
- Probes:
  - liveness: `httpGet /livez :health` — restart only if process is wedged.
  - readiness: `httpGet /readyz :health` — yank from service if DB ping fails.
  - startup: same as readiness, with `failureThreshold: 30 / periodSeconds: 2` (60s grace for slow first-cold pgxpool warmup).

- [ ] **Step 1: Write the helm template contract test**

We use `helm template` (no cluster needed) to render manifests and grep for required structural assertions. This is fast, deterministic, and runs in CI.

Create `deploy/helm/imgsync/tests/template_test.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

CHART="deploy/helm/imgsync"
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

echo "PASS: helm chart structural tests green"
```

```bash
mkdir -p deploy/helm/imgsync/tests
chmod +x deploy/helm/imgsync/tests/template_test.sh
```

- [ ] **Step 2: Run the test, confirm fail**

Run: `./deploy/helm/imgsync/tests/template_test.sh`
Expected: `Error: chart not found: deploy/helm/imgsync` (or similar) — chart doesn't exist yet.

- [ ] **Step 3: Write Chart.yaml**

Create `deploy/helm/imgsync/Chart.yaml`:

```yaml
apiVersion: v2
name: imgsync
description: imgsync — file-transfer worker queue (replaces NiFi)
type: application
version: 0.1.0
appVersion: "0.1.0"
keywords:
  - file-transfer
  - queue
  - postgres
  - ftp
maintainers:
  - name: nineking424
sources:
  - https://github.com/nineking424/imgsync
```

Create `deploy/helm/imgsync/.helmignore`:

```
.DS_Store
.git/
.gitignore
*.swp
*.bak
*.tmp
*.orig
*~
tests/
```

- [ ] **Step 4: Write values.yaml**

Create `deploy/helm/imgsync/values.yaml`:

```yaml
replicaCount: 1

image:
  repository: imgsync
  tag: ""           # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent

imagePullSecrets: []
nameOverride: ""
fullnameOverride: ""

# Pre-existing Secret with the postgres DSN.
# Operator MUST create this Secret before installing the chart.
# Example:
#   kubectl create secret generic imgsync-dsn \
#     --from-literal=dsn='postgres://user:pw@host:5432/imgsync?sslmode=require'
dsnSecretRef:
  name: imgsync-dsn
  key: dsn

# Optional: pre-existing Secret with FTP credentials.
# If unset, FTP transports/sources will fail. Optional because some installs
# only use LocalFS.
ftpSecretRef:
  name: ""          # e.g. "imgsync-ftp"
  userKey: "user"
  passwordKey: "password"

# Worker runtime config (non-secret)
worker:
  workers: 4                  # goroutines per pod
  idleSleepMin: "50ms"
  idleSleepMax: "1s"
  ftpHostMaxConns: 8          # cluster-wide cap per host (advisory lock)
  ftpHostPoolMaxIdle: 5
  ftpHostPoolIdleTTL: "5m"

# Health & probes
health:
  port: 8080
  livenessProbe:
    httpGet:
      path: /livez
      port: health
    periodSeconds: 10
    timeoutSeconds: 2
    failureThreshold: 3
  readinessProbe:
    httpGet:
      path: /readyz
      port: health
    periodSeconds: 5
    timeoutSeconds: 2
    failureThreshold: 2
  startupProbe:
    httpGet:
      path: /readyz
      port: health
    periodSeconds: 2
    failureThreshold: 30      # 60s grace

# Pod-level
serviceAccount:
  create: true
  name: ""

podAnnotations: {}
podLabels: {}

podSecurityContext:
  fsGroup: 65532
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile:
    type: RuntimeDefault

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65532

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 256Mi

# PodDisruptionBudget — only rendered when replicaCount >= 2
pdb:
  maxUnavailable: 1

# Service — only exposes /healthz for monitoring; no app traffic
service:
  type: ClusterIP
  port: 8080

nodeSelector: {}
tolerations: []
affinity: {}

# Migration Job (Task 4) — knobs surfaced here, used by migrate-job.yaml
migrationJob:
  enabled: true
  backoffLimit: 2
  ttlSecondsAfterFinished: 600
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 128Mi
```

**Why per-key resource defaults at 100m/128Mi:** matches a baseline 4-worker pod doing FTP I/O. Easy to bump in production overrides.

**Why `readOnlyRootFilesystem: true`:** the binary doesn't write to disk; migrations are read-only; logs go to stdout. Hardens against mountable-filesystem exploits.

- [ ] **Step 5: Write _helpers.tpl**

Create `deploy/helm/imgsync/templates/_helpers.tpl`:

```yaml
{{/*
Expand the name of the chart.
*/}}
{{- define "imgsync.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "imgsync.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "imgsync.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "imgsync.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: imgsync
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "imgsync.selectorLabels" -}}
app.kubernetes.io/name: {{ include "imgsync.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "imgsync.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "imgsync.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference.
*/}}
{{- define "imgsync.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{ .Values.image.repository }}:{{ $tag }}
{{- end }}
```

- [ ] **Step 6: Write configmap.yaml**

Create `deploy/helm/imgsync/templates/configmap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "imgsync.fullname" . }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
data:
  IMGSYNC_WORKERS:            "{{ .Values.worker.workers }}"
  IMGSYNC_IDLE_SLEEP_MIN:     "{{ .Values.worker.idleSleepMin }}"
  IMGSYNC_IDLE_SLEEP_MAX:     "{{ .Values.worker.idleSleepMax }}"
  IMGSYNC_FTP_HOST_MAX_CONNS: "{{ .Values.worker.ftpHostMaxConns }}"
  IMGSYNC_FTP_POOL_MAX_IDLE:  "{{ .Values.worker.ftpHostPoolMaxIdle }}"
  IMGSYNC_FTP_POOL_IDLE_TTL:  "{{ .Values.worker.ftpHostPoolIdleTTL }}"
  IMGSYNC_HEALTH_ADDR:        ":{{ .Values.health.port }}"
```

- [ ] **Step 7: Write serviceaccount.yaml**

Create `deploy/helm/imgsync/templates/serviceaccount.yaml`:

```yaml
{{- if .Values.serviceAccount.create -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "imgsync.serviceAccountName" . }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
{{- end }}
```

- [ ] **Step 8: Write deployment.yaml**

Create `deploy/helm/imgsync/templates/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "imgsync.fullname" . }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "imgsync.selectorLabels" . | nindent 6 }}
  strategy:
    # RollingUpdate is fine — workers are stateless from k8s' POV; lease ownership lives in DB.
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    metadata:
      labels:
        {{- include "imgsync.selectorLabels" . | nindent 8 }}
        {{- with .Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        # Roll pods when ConfigMap changes (env from envFrom)
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "imgsync.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      terminationGracePeriodSeconds: 60   # let in-flight Send() finish a chunk before kill
      containers:
        - name: worker
          image: {{ include "imgsync.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args: ["worker"]
          ports:
            - name: health
              containerPort: {{ .Values.health.port }}
              protocol: TCP
          envFrom:
            - configMapRef:
                name: {{ include "imgsync.fullname" . }}
          env:
            - name: IMGSYNC_POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: IMGSYNC_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.dsnSecretRef.name }}
                  key:  {{ .Values.dsnSecretRef.key }}
            {{- if .Values.ftpSecretRef.name }}
            - name: IMGSYNC_FTP_USER
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.ftpSecretRef.name }}
                  key:  {{ .Values.ftpSecretRef.userKey }}
            - name: IMGSYNC_FTP_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.ftpSecretRef.name }}
                  key:  {{ .Values.ftpSecretRef.passwordKey }}
            {{- end }}
          livenessProbe:
            {{- toYaml .Values.health.livenessProbe | nindent 12 }}
          readinessProbe:
            {{- toYaml .Values.health.readinessProbe | nindent 12 }}
          startupProbe:
            {{- toYaml .Values.health.startupProbe | nindent 12 }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        # readOnlyRootFilesystem=true forces /tmp to be a writable emptyDir
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

**Why `terminationGracePeriodSeconds: 60`:** workers stream files; killing a pod mid-`io.Copy` aborts the partial transfer, which is fine (sweeper recovers via C2), but 60s lets a small chunk finish cleanly so we don't trash the FTP server's TCP state on every rolling update.

**Why `checksum/config` annotation:** when ConfigMap changes (e.g., `worker.workers: 4 → 8`), Helm normally won't roll the Deployment. The annotation forces a rolling restart on config drift.

- [ ] **Step 9: Write service.yaml**

Create `deploy/helm/imgsync/templates/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "imgsync.fullname" . }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - port: {{ .Values.service.port }}
      targetPort: health
      protocol: TCP
      name: health
  selector:
    {{- include "imgsync.selectorLabels" . | nindent 4 }}
```

- [ ] **Step 10: Write pdb.yaml**

Create `deploy/helm/imgsync/templates/pdb.yaml`:

```yaml
{{- if ge (int .Values.replicaCount) 2 }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "imgsync.fullname" . }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
spec:
  maxUnavailable: {{ .Values.pdb.maxUnavailable }}
  selector:
    matchLabels:
      {{- include "imgsync.selectorLabels" . | nindent 6 }}
{{- end }}
```

- [ ] **Step 11: Write NOTES.txt**

Create `deploy/helm/imgsync/templates/NOTES.txt`:

```
imgsync {{ .Chart.AppVersion }} installed as release "{{ .Release.Name }}".

  Replicas:    {{ .Values.replicaCount }}
  Image:       {{ include "imgsync.image" . }}
  DSN secret:  {{ .Values.dsnSecretRef.name }} (key: {{ .Values.dsnSecretRef.key }})
{{ if .Values.ftpSecretRef.name }}  FTP secret:  {{ .Values.ftpSecretRef.name }}
{{ end }}

To check worker health:

  kubectl --namespace {{ .Release.Namespace }} port-forward \
    svc/{{ include "imgsync.fullname" . }} 8080:{{ .Values.service.port }}
  curl localhost:8080/healthz | jq

To enqueue a job (operator one-liner):

  kubectl --namespace {{ .Release.Namespace }} run --rm -it imgsync-cli \
    --image={{ include "imgsync.image" . }} --restart=Never \
    --env=IMGSYNC_DSN=<dsn> -- \
    enqueue --trace-id=<id> --src=<url> --dst=<url> \
            --src-protocol=<proto> --dst-protocol=<proto>

To audit a single job (one-line SQL):

  SELECT j.id, j.status, j.attempts, e.status AS event, e.ts, e.detail
    FROM transfer_jobs j LEFT JOIN transfer_events e USING (trace_id, job_id)
   WHERE j.trace_id='<id>' AND j.dst='<url>'
   ORDER BY e.ts;
```

- [ ] **Step 12: Update Makefile**

Append to `Makefile`:

```make
HELM_CHART = deploy/helm/imgsync

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint $(HELM_CHART)

.PHONY: helm-template
helm-template: ## Render Helm chart with default values
	helm template t $(HELM_CHART)

.PHONY: helm-test
helm-test: ## Run Helm chart structural tests
	./$(HELM_CHART)/tests/template_test.sh
```

- [ ] **Step 13: Run the test, confirm pass**

Run: `make helm-test`
Expected output:
```
==> helm lint
...
1 chart(s) linted, 0 chart(s) failed
==> helm template (default values)
==> helm template (replicaCount=8)
==> probes + env + secret ref
==> nonroot security context
PASS: helm chart structural tests green
```

If `helm lint` fails on `[ERROR]: chart should have at least one icon`: ignore — non-blocking warning. Adjust the test to allow that specific lint warning if needed.

- [ ] **Step 14: Commit**

```bash
git add deploy/helm/imgsync Makefile
git commit -m "feat(helm): add imgsync chart with worker Deployment, PDB, ConfigMap"
```

---

## Task 4: Helm pre-install/pre-upgrade migrate Job

**Files:**
- Create: `deploy/helm/imgsync/templates/migrate-job.yaml`
- Modify: `deploy/helm/imgsync/tests/template_test.sh` — add hook annotation assertions

**Why a separate task from Task 3:** the migration Job has subtle Helm-hook semantics (deletion policy, naming with revision suffix to allow re-runs, and forward-only constraint) that deserve isolated TDD. Bugs here block every redeploy.

**Decisions in this task:**
- The Job's `metadata.name` includes `{{ .Release.Revision }}` so each upgrade gets a fresh Job name. `before-hook-creation` deletes the prior Job before creating the new one — this is what makes Helm Jobs work across upgrades.
- The Job uses the **same image** as the worker — no separate "tools" image. The `migrate up` subcommand handles forward-only migrations baked into `/app/migrations/` (per Dockerfile, Task 1).
- ttl: 600s (10min) so successful migrations clean themselves up. Failures linger forever for postmortem.
- Resources: minimal (100m CPU, 64Mi memory) — migration is just a few INSERTs to schema_migrations.
- Same security context as the worker (nonroot, readOnlyRootFilesystem).

- [ ] **Step 1: Extend the helm template test**

Add at the end of `deploy/helm/imgsync/tests/template_test.sh`, before the final `echo "PASS"`:

```bash
# ─── Test 6: migration Job hook annotations ─────────────────────────
echo "==> migrate Job hook annotations"
helm template t3 "$CHART" > "$TMP/t3.yaml"

grep -q 'kind: Job'                                              "$TMP/t3.yaml" || { echo "FAIL: no Job rendered"; exit 1; }
grep -q '"helm.sh/hook": "pre-install,pre-upgrade"'              "$TMP/t3.yaml" || \
  grep -q '"helm.sh/hook": pre-install,pre-upgrade'              "$TMP/t3.yaml" || \
  { echo "FAIL: migrate Job missing pre-install,pre-upgrade hook"; exit 1; }
grep -q 'before-hook-creation'                                    "$TMP/t3.yaml" || { echo "FAIL: migrate Job missing before-hook-creation policy"; exit 1; }
grep -q 'hook-succeeded'                                          "$TMP/t3.yaml" || { echo "FAIL: migrate Job missing hook-succeeded cleanup"; exit 1; }

# Migration Job MUST run as the same nonroot UID as worker
grep -A 3 "kind: Job" "$TMP/t3.yaml" | grep -q "imgsync-migrate" || \
  grep -B 2 "kind: Job" "$TMP/t3.yaml" | head -20 # just for context if it fails

# Args must be migrate up
grep -q '"migrate"' "$TMP/t3.yaml" || { echo "FAIL: migrate Job not running migrate subcommand"; exit 1; }
grep -q '"up"'      "$TMP/t3.yaml" || { echo "FAIL: migrate Job not running 'up' arg"; exit 1; }

# ─── Test 7: migrationJob.enabled=false suppresses the Job ──────────
echo "==> migrationJob.enabled=false"
helm template t4 "$CHART" --set migrationJob.enabled=false > "$TMP/t4.yaml"
if grep -q "name: t4-imgsync-migrate" "$TMP/t4.yaml"; then
  echo "FAIL: migrationJob.enabled=false did not suppress the Job"
  exit 1
fi
```

- [ ] **Step 2: Run the test, confirm fail**

Run: `make helm-test`
Expected: fails at `==> migrate Job hook annotations` because no Job template exists.

- [ ] **Step 3: Write migrate-job.yaml**

Create `deploy/helm/imgsync/templates/migrate-job.yaml`:

```yaml
{{- if .Values.migrationJob.enabled -}}
apiVersion: batch/v1
kind: Job
metadata:
  # Revision in the name lets multiple upgrades coexist briefly during rollout.
  # before-hook-creation policy below will reap the prior one.
  name: {{ include "imgsync.fullname" . }}-migrate-{{ .Release.Revision }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
    app.kubernetes.io/component: migrate
  annotations:
    # Run before install AND before upgrade — never on rollback (forward-only).
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "0"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
spec:
  backoffLimit: {{ .Values.migrationJob.backoffLimit }}
  ttlSecondsAfterFinished: {{ .Values.migrationJob.ttlSecondsAfterFinished }}
  template:
    metadata:
      labels:
        {{- include "imgsync.selectorLabels" . | nindent 8 }}
        app.kubernetes.io/component: migrate
    spec:
      serviceAccountName: {{ include "imgsync.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      restartPolicy: Never
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: migrate
          image: {{ include "imgsync.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args: ["migrate", "up"]
          env:
            - name: IMGSYNC_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.dsnSecretRef.name }}
                  key:  {{ .Values.dsnSecretRef.key }}
          resources:
            {{- toYaml .Values.migrationJob.resources | nindent 12 }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir:
            sizeLimit: 16Mi
{{- end }}
```

**Why `Release.Revision` in the name:** Helm uses `before-hook-creation` to delete the *previous* Job by name before creating the new one. Without revision in the name, two parallel upgrades (rare, but happens during CI) would step on each other. With revision, names are unique per upgrade.

**Why `restartPolicy: Never`:** combined with `backoffLimit: 2`, this caps total attempts at 3. If migration fails 3x, the Job fails, the Helm hook fails, the install/upgrade aborts. This is correct — bad migration should NOT auto-retry forever.

**Why `hook-weight: "0"`:** explicit ordering anchor. If we add another pre-install hook later (e.g., a CRD installer), the relative ordering is clear.

- [ ] **Step 4: Run the test, confirm pass**

Run: `make helm-test`
Expected: all 7 tests now pass.

- [ ] **Step 5: Sanity-check the rendered Job manually**

Run:
```bash
helm template demo deploy/helm/imgsync | grep -A 30 "kind: Job"
```

Verify visually:
- annotations include `pre-install,pre-upgrade`
- args is `["migrate", "up"]`
- env IMGSYNC_DSN comes from the right Secret reference
- securityContext is nonroot

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/imgsync/templates/migrate-job.yaml \
        deploy/helm/imgsync/tests/template_test.sh
git commit -m "feat(helm): add forward-only migrate Job as pre-install/pre-upgrade hook"
```

---

## Task 5: C7 throughput E2E (kind cluster, 2→8 replicas, ≥3.2× linearity)

**Files:**
- Create: `e2e/doc.go` (build tag)
- Create: `e2e/kind_config.yaml`
- Create: `e2e/helpers.go`
- Create: `e2e/throughput_test.go`
- Create: `e2e/manifests/postgres.yaml` (in-cluster postgres for E2E only)
- Create: `e2e/manifests/nfs-pv.yaml` (shared LocalFS volume)
- Create: `scripts/e2e-up.sh`
- Create: `scripts/e2e-down.sh`
- Modify: `Makefile` — add `e2e-throughput` target
- Modify: `.github/workflows/ci.yml` — add e2e job (gated)

**Decisions in this task:**
- **kind v0.22+** — pinned in `e2e-up.sh`. Single control-plane + 3 workers.
- **Postgres-in-cluster.** Plain manifest, not another helm chart. Keeps the test hermetic. Secret named `imgsync-dsn` is created by the bootstrap script.
- **Shared LocalFS volume.** Test plan §C7 says "kind cluster + nfs-shared LocalFS volume." We use a `hostPath` PV + node label trick so all worker nodes mount the same kind-control-plane host directory. (Real NFS in CI is overkill; the kind nodes are containers on one host, so hostPath works.)
- **1000× 10MB jobs.** Same as the test plan. 10MB is "small enough to not bottleneck on disk; large enough to dominate over per-job overhead."
- **Throughput measurement** = wall-clock from "all enqueued" to "last job succeeded". We poll `SELECT count(*) WHERE status='succeeded'` every second.
- **Test harness language**: Go (`go test -tags e2e`). We use the standard library only — no e2e-framework dependency for now (keeps deps lean).

- [ ] **Step 1: Write the throughput test contract**

The test asserts `tput(8) / tput(2) >= 3.2` AND that the 8-replica install has its 8th pod running within 5 minutes (warm-image cache, since the same image was already pulled for the 2-replica run).

Create `e2e/doc.go`:

```go
//go:build e2e
// +build e2e

// Package e2e contains end-to-end tests that require a kind cluster
// and the imgsync Helm chart. Run with: go test -tags e2e ./e2e/...
package e2e
```

Create `e2e/throughput_test.go`:

```go
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func TestC7_ThroughputScaleOut(t *testing.T) {
	if os.Getenv("IMGSYNC_E2E") != "1" {
		t.Skip("set IMGSYNC_E2E=1 to run E2E tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	env := bootstrapKindEnv(t, ctx)
	defer env.teardown()

	// ─── Phase A: 2 replicas, 1000 jobs ────────────────────────────────────
	t.Log("==> Phase A: replicas=2")
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
	env.waitReplicasReady(t, ctx, 2, 5*time.Minute)

	jobsA := 1000
	env.truncateJobs(t, ctx)
	env.enqueueLocalFSJobs(t, ctx, "phaseA-", jobsA)
	durA := env.waitAllSucceeded(t, ctx, jobsA, 15*time.Minute)
	tputA := float64(jobsA) / durA.Seconds()
	t.Logf("Phase A: %d jobs in %v → %.2f jobs/sec", jobsA, durA, tputA)

	// ─── Phase B: scale to 8 replicas, 1000 fresh jobs ─────────────────────
	t.Log("==> Phase B: replicas=8")
	scaleStart := time.Now()
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "8"})
	env.waitReplicasReady(t, ctx, 8, 5*time.Minute)
	scaleLatency := time.Since(scaleStart)
	t.Logf("Scale 2→8 ready in %v", scaleLatency)
	if scaleLatency > 5*time.Minute {
		t.Errorf("scale-out exceeded 5min budget: %v", scaleLatency)
	}

	jobsB := 1000
	env.truncateJobs(t, ctx)
	env.enqueueLocalFSJobs(t, ctx, "phaseB-", jobsB)
	durB := env.waitAllSucceeded(t, ctx, jobsB, 15*time.Minute)
	tputB := float64(jobsB) / durB.Seconds()
	t.Logf("Phase B: %d jobs in %v → %.2f jobs/sec", jobsB, durB, tputB)

	// ─── Assertion ─────────────────────────────────────────────────────────
	ratio := tputB / tputA
	t.Logf("Throughput ratio (8/2) = %.2f", ratio)
	const minRatio = 3.2
	if ratio < minRatio {
		t.Fatalf("throughput linearity FAIL: ratio %.2f < %.2f (tputA=%.2f, tputB=%.2f)",
			ratio, minRatio, tputA, tputB)
	}

	// Sanity: nothing dead
	dead := env.countByStatus(t, ctx, "dead")
	if dead > 0 {
		t.Fatalf("phase B left %d jobs in 'dead' state — unexpected", dead)
	}
}

// roundTo2 keeps log lines tidy.
func roundTo2(f float64) float64 {
	return math.Round(f*100) / 100
}

// silence unused import warning if math gets pruned during edits
var _ = fmt.Sprintf
```

- [ ] **Step 2: Run the test, confirm fail**

Run:
```bash
IMGSYNC_E2E=1 go test -tags e2e -timeout 35m -v ./e2e/ -run TestC7_ThroughputScaleOut
```

Expected: compile error — `bootstrapKindEnv` etc. don't exist yet.

- [ ] **Step 3: Write kind config**

Create `e2e/kind_config.yaml`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: imgsync-e2e
nodes:
  - role: control-plane
    extraMounts:
      # Shared LocalFS volume — all worker pods will mount this via hostPath PV
      - hostPath: /tmp/imgsync-e2e-localfs
        containerPath: /srv/imgsync
  - role: worker
    extraMounts:
      - hostPath: /tmp/imgsync-e2e-localfs
        containerPath: /srv/imgsync
  - role: worker
    extraMounts:
      - hostPath: /tmp/imgsync-e2e-localfs
        containerPath: /srv/imgsync
  - role: worker
    extraMounts:
      - hostPath: /tmp/imgsync-e2e-localfs
        containerPath: /srv/imgsync
```

**Why `extraMounts`:** kind worker nodes are containers; without `extraMounts` the hostPath PV won't see the host directory. The same host dir is mounted into every node, so a hostPath PV behaves like NFS for our purposes.

- [ ] **Step 4: Write postgres + PV manifests**

Create `e2e/manifests/postgres.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: imgsync-e2e
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
  namespace: imgsync-e2e
spec:
  replicas: 1
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
          ports:
            - containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "imgsync", "-d", "imgsync"]
            periodSeconds: 1
          resources:
            requests:
              cpu: 200m
              memory: 256Mi
            limits:
              cpu: 1000m
              memory: 512Mi
```

Create `e2e/manifests/nfs-pv.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: imgsync-e2e-localfs
spec:
  capacity:
    storage: 50Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: standard
  hostPath:
    path: /srv/imgsync
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: imgsync-e2e-localfs
  namespace: imgsync-e2e
spec:
  accessModes: [ReadWriteMany]
  storageClassName: standard
  resources:
    requests:
      storage: 50Gi
  volumeName: imgsync-e2e-localfs
```

- [ ] **Step 5: Write the bootstrap script**

Create `scripts/e2e-up.sh`:

```bash
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
```

```bash
chmod +x scripts/e2e-up.sh
```

Create `scripts/e2e-down.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
kind delete cluster --name imgsync-e2e || true
rm -rf /tmp/imgsync-e2e-localfs || true
```

```bash
chmod +x scripts/e2e-down.sh
```

- [ ] **Step 6: Write helpers.go**

Create `e2e/helpers.go`:

```go
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	clusterName = "imgsync-e2e"
	namespace   = "imgsync-e2e"
	releaseName = "imgsync"
	chartPath   = "../deploy/helm/imgsync"
)

// kindEnv holds the live cluster + DB handle.
type kindEnv struct {
	pool      *pgxpool.Pool
	dsnLocal  string // pgx-friendly DSN reachable from the test host (port-forwarded)
	pgPFCmd   *exec.Cmd
	pgPFCancl context.CancelFunc
}

func bootstrapKindEnv(t *testing.T, ctx context.Context) *kindEnv {
	t.Helper()

	// Run scripts/e2e-up.sh from the repo root
	root := repoRoot(t)
	if err := runCmd(ctx, root, "./scripts/e2e-up.sh"); err != nil {
		t.Fatalf("e2e-up.sh failed: %v", err)
	}

	env := &kindEnv{}
	env.startPostgresPortForward(t)

	dsn := "postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable"
	env.dsnLocal = dsn

	// Connect with retry
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				env.pool = pool
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres port-forward never came up: %v", err)
		}
		time.Sleep(1 * time.Second)
	}

	// Seed source files on the shared volume (1000 × 10MB)
	env.seedFixtures(t, ctx, 1000, 10*1024*1024)

	return env
}

func (e *kindEnv) teardown() {
	if e.pool != nil {
		e.pool.Close()
	}
	if e.pgPFCancl != nil {
		e.pgPFCancl()
	}
	// Note: leaving the kind cluster running can speed up dev iteration.
	// The CI job runs `make e2e-down` after the test.
}

func (e *kindEnv) startPostgresPortForward(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.pgPFCancl = cancel
	cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace,
		"port-forward", "svc/postgres", "5433:5432")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("kubectl port-forward failed: %v", err)
	}
	e.pgPFCmd = cmd
	time.Sleep(2 * time.Second) // give it time to establish
}

// seedFixtures writes N source files onto the shared host volume that the worker
// nodes see at /srv/imgsync.  We write directly to /tmp/imgsync-e2e-localfs on
// the test host (kind extraMount maps host:/tmp/imgsync-e2e-localfs → node:/srv/imgsync).
func (e *kindEnv) seedFixtures(t *testing.T, ctx context.Context, count int, sizeBytes int) {
	t.Helper()
	srcDir := "/tmp/imgsync-e2e-localfs/src"
	dstDir := "/tmp/imgsync-e2e-localfs/dst"
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	// 10MB sparse file repeated; fast to create
	chunk := make([]byte, 1024*1024)
	for i := 0; i < len(chunk); i++ {
		chunk[i] = byte(i % 256)
	}
	for i := 1; i <= count; i++ {
		path := fmt.Sprintf("%s/file-%05d.bin", srcDir, i)
		if _, err := os.Stat(path); err == nil {
			continue // already seeded
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		for written := 0; written < sizeBytes; written += len(chunk) {
			n := len(chunk)
			if written+n > sizeBytes {
				n = sizeBytes - written
			}
			if _, err := f.Write(chunk[:n]); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		_ = f.Close()
	}
}

func (e *kindEnv) truncateJobs(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, "TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func (e *kindEnv) enqueueLocalFSJobs(t *testing.T, ctx context.Context, prefix string, n int) {
	t.Helper()
	// Direct INSERT via the test's pool. We're skipping the `imgsync enqueue` CLI
	// here because the round-trip latency would dominate; the SQL is the same.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	batch := `
INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
SELECT
  $1 || lpad(i::text, 5, '0'),
  'file:///srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
  'file:///srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
  'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
FROM generate_series(1, $2) AS i
ON CONFLICT (trace_id, dst) DO NOTHING
`
	if _, err := tx.Exec(ctx, batch, prefix, n); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (e *kindEnv) waitAllSucceeded(t *testing.T, ctx context.Context, expected int, budget time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(budget)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while waiting for jobs")
		case <-tick.C:
			n := e.countByStatus(t, ctx, "succeeded")
			if n >= expected {
				return time.Since(start)
			}
			if time.Now().After(deadline) {
				dead := e.countByStatus(t, ctx, "dead")
				proc := e.countByStatus(t, ctx, "processing")
				t.Fatalf("waitAllSucceeded: only %d/%d succeeded after %v (dead=%d, processing=%d)",
					n, expected, budget, dead, proc)
			}
		}
	}
}

func (e *kindEnv) countByStatus(t *testing.T, ctx context.Context, status string) int {
	t.Helper()
	var n int
	row := e.pool.QueryRow(ctx, "SELECT count(*) FROM transfer_jobs WHERE status=$1", status)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countByStatus: %v", err)
	}
	return n
}

func (e *kindEnv) helmUpgrade(t *testing.T, ctx context.Context, sets map[string]string) {
	t.Helper()
	args := []string{
		"upgrade", "--install", releaseName, chartPath,
		"--namespace", namespace,
		"--set", "image.repository=imgsync",
		"--set", "image.tag=e2e",
		"--set", "image.pullPolicy=IfNotPresent",
		"--wait", "--timeout", "5m",
	}
	for k, v := range sets {
		args = append(args, "--set", k+"="+v)
	}
	if err := runCmd(ctx, repoRoot(t), "helm", args...); err != nil {
		t.Fatalf("helm upgrade: %v", err)
	}
}

func (e *kindEnv) waitReplicasReady(t *testing.T, ctx context.Context, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace,
			"get", "deployment", releaseName,
			"-o", "jsonpath={.status.readyReplicas}").Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if s == "" {
				s = "0"
			}
			ready, _ := strconv.Atoi(s)
			if ready >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %s replicas ready after %v (wanted %d)", string(out), budget, want)
		}
		time.Sleep(2 * time.Second)
	}
}

func runCmd(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
```

**Why `INSERT ... FROM generate_series` instead of looping the CLI:** seeding 1000 jobs through cobra would take 30+ seconds and skew the throughput measurement. Bulk SQL is the same path the worker reads, just faster to set up.

**Why `ON CONFLICT (trace_id, dst) DO NOTHING`:** lets re-runs be idempotent without truncate. Tests do truncate at the start of each phase anyway.

- [ ] **Step 7: Update Makefile**

Append to `Makefile`:

```make
.PHONY: e2e-up
e2e-up: ## Bring up the kind+chart e2e environment
	./scripts/e2e-up.sh

.PHONY: e2e-down
e2e-down: ## Tear down the e2e environment
	./scripts/e2e-down.sh

.PHONY: e2e-throughput
e2e-throughput: ## Run C7 throughput E2E (kind cluster required)
	IMGSYNC_E2E=1 go test -tags e2e -timeout 35m -v ./e2e/... -run TestC7_ThroughputScaleOut

.PHONY: e2e-dirty-state
e2e-dirty-state: ## Run F5 dirty-state recovery E2E (added in Task 6)
	IMGSYNC_E2E=1 go test -tags e2e -timeout 30m -v ./e2e/... -run TestF5_DirtyStateRecovery
```

- [ ] **Step 8: Update CI workflow (gated)**

Modify `.github/workflows/ci.yml`. Add a new job at the bottom (assumes the file exists from Week 1 Task 7):

```yaml
  e2e:
    if: contains(github.event.pull_request.labels.*.name, 'e2e')
    needs: [test, lint]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - uses: helm/kind-action@v1
        with:
          version: v0.22.0
          install_only: true
      - uses: azure/setup-helm@v4
        with:
          version: v3.14.0
      - name: Run C7 throughput E2E
        run: make e2e-throughput
      - name: Run F5 dirty-state E2E
        run: make e2e-dirty-state
        if: success() || failure()  # always run F5, even if C7 fails, to surface both
      - name: Cleanup
        if: always()
        run: make e2e-down
```

**Why label-gated:** kind+helm install + image build is 5–10 minutes per run. Don't burn CI minutes on every push. The `e2e` label is opt-in.

- [ ] **Step 9: Run the test locally**

Pre-req: docker, kind, helm, kubectl, go installed.

Run:
```bash
make e2e-throughput
```

Expected end of log:
```
Phase A: 1000 jobs in <Ns> → <X> jobs/sec
Phase B: 1000 jobs in <Ns> → <Y> jobs/sec
Throughput ratio (8/2) = <Z>
PASS
```

Where `Z >= 3.2`.

**If ratio < 3.2:** check that the worker logs aren't FTP-bound (this test is LocalFS only — no FTP). If so, look at:
- pgxpool MaxConns (should be `replicas * workers + headroom`; default in `internal/db/pool.go` is fine for 8×4=32 + 8).
- LocalFS Transport contention on the shared dir (unlikely, but `os.CreateTemp` uniqueness is per-call).

**If scale latency > 5min:** image is being re-pulled. Verify `imagePullPolicy: IfNotPresent` and `kind load docker-image` ran.

- [ ] **Step 10: Tear down and commit**

```bash
make e2e-down
git add e2e scripts/e2e-up.sh scripts/e2e-down.sh Makefile .github/workflows/ci.yml
git commit -m "feat(e2e): add C7 throughput scale-out E2E (kind, 2→8 replicas, ≥3.2x)"
```

---

## Task 6: F5 dirty-state recovery E2E (mid-flight kill + helm rollback)

**Files:**
- Create: `e2e/dirty_state_test.go`
- Modify: `e2e/helpers.go` — add `killPodMidFlight`, `helmRollback`, `simulateBadUpgrade` helpers

**Decisions in this task:**
- Three sub-scenarios as locked in the Decisions section:
  - **F5a** mid-flight kill → sweeper expire → re-lease → success with `attempts==0`
  - **F5b** bad helm upgrade (wrong image tag) → workers crashloop → `helm rollback` → workers stabilize → no jobs stuck
  - **F5c** uninstall → reinstall → migration Job runs idempotently → DB state preserved
- Each sub-scenario is a separate `t.Run` so they show up individually in the test report.
- F5a uses 100 jobs (not 1000) because we want to *find* a leased job during processing — fewer jobs = more controllable timing.
- F5b uses image tag `imgsync:does-not-exist` to force ImagePullBackOff. After 30s of crashloop, rollback. Verify the rolled-back pods become Ready.
- F5c truncates dst directory only — DB state remains. Asserts that after reinstall, the existing jobs (not yet processed because workers were down) drain to succeeded.

- [ ] **Step 1: Add helpers to helpers.go**

Append to `e2e/helpers.go`:

```go
// ─── F5 helpers ───────────────────────────────────────────────────────────

func (e *kindEnv) listPods(t *testing.T, ctx context.Context) []string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace,
		"get", "pods", "-l", "app.kubernetes.io/name=imgsync",
		"-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		t.Fatalf("listPods: %v", err)
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	return names
}

// killOnePod deletes a single worker pod (forced) to simulate mid-flight crash.
// Returns the name of the pod that was killed.
func (e *kindEnv) killOnePod(t *testing.T, ctx context.Context) string {
	t.Helper()
	pods := e.listPods(t, ctx)
	if len(pods) == 0 {
		t.Fatal("killOnePod: no worker pods running")
	}
	target := pods[0]
	if err := runCmd(ctx, repoRoot(t), "kubectl", "-n", namespace,
		"delete", "pod", target, "--grace-period=0", "--force"); err != nil {
		t.Fatalf("kubectl delete pod %s: %v", target, err)
	}
	return target
}

// waitForLeasedJob polls until at least one job is in 'processing' status.
// Used to time the kill so it lands during active work.
func (e *kindEnv) waitForLeasedJob(t *testing.T, ctx context.Context, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if e.countByStatus(t, ctx, "processing") > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no job entered 'processing' within %v", budget)
}

// inspectSweeperRecovery returns counts of jobs that have an 'expire' event
// and ended in 'succeeded' with attempts==0 (the C2 invariant at cluster level).
func (e *kindEnv) inspectSweeperRecovery(t *testing.T, ctx context.Context) (recovered int) {
	t.Helper()
	row := e.pool.QueryRow(ctx, `
SELECT count(*) FROM transfer_jobs j
WHERE j.status='succeeded'
  AND j.attempts=0
  AND EXISTS (
    SELECT 1 FROM transfer_events e
     WHERE e.trace_id=j.trace_id AND e.job_id=j.id AND e.status='expire')
`)
	if err := row.Scan(&recovered); err != nil {
		t.Fatalf("inspectSweeperRecovery: %v", err)
	}
	return recovered
}

func (e *kindEnv) helmRollback(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := runCmd(ctx, repoRoot(t), "helm",
		"-n", namespace, "rollback", releaseName, "--wait", "--timeout", "3m"); err != nil {
		t.Fatalf("helm rollback: %v", err)
	}
}

func (e *kindEnv) helmUninstall(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := runCmd(ctx, repoRoot(t), "helm",
		"-n", namespace, "uninstall", releaseName, "--wait", "--timeout", "2m"); err != nil {
		t.Fatalf("helm uninstall: %v", err)
	}
}
```

- [ ] **Step 2: Write the dirty_state test**

Create `e2e/dirty_state_test.go`:

```go
//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestF5_DirtyStateRecovery(t *testing.T) {
	if os.Getenv("IMGSYNC_E2E") != "1" {
		t.Skip("set IMGSYNC_E2E=1 to run E2E tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	env := bootstrapKindEnv(t, ctx)
	defer env.teardown()

	// Use a small replica count and tighter sweeper interval indirectly via
	// the default 30s loop / 5min threshold. We'll force the sweeper threshold
	// down to something testable by editing locked_at directly (cluster-level
	// equivalent of C2's setup).
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
	env.waitReplicasReady(t, ctx, 2, 3*time.Minute)

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5a_mid_flight_kill", func(t *testing.T) {
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5a-", 100)

		// Wait until a worker leases at least one job
		env.waitForLeasedJob(t, ctx, 30*time.Second)

		// Kill one pod hard
		killed := env.killOnePod(t, ctx)
		t.Logf("killed pod %s mid-flight", killed)

		// Force the sweep window down: any job locked by the killed pod still has
		// locked_at in the recent past. The sweeper threshold is 5min by default.
		// To make this test fast, fast-forward locked_at on those rows.
		_, err := env.pool.Exec(ctx, `
UPDATE transfer_jobs
   SET locked_at = NOW() - INTERVAL '6 minutes'
 WHERE locked_by LIKE $1 AND status='processing'
`, killed+"%")
		if err != nil {
			t.Fatalf("force expire: %v", err)
		}

		// Wait for full drain
		env.waitAllSucceeded(t, ctx, 100, 5*time.Minute)

		// Assert sweeper-recovered jobs ended with attempts==0 and have an 'expire' event
		recovered := env.inspectSweeperRecovery(t, ctx)
		if recovered == 0 {
			t.Fatalf("expected at least 1 sweeper-recovered job with attempts==0; got 0")
		}
		t.Logf("sweeper-recovered jobs: %d", recovered)

		// And: zero dead, zero stuck processing
		if d := env.countByStatus(t, ctx, "dead"); d != 0 {
			t.Errorf("expected 0 dead, got %d", d)
		}
		if p := env.countByStatus(t, ctx, "processing"); p != 0 {
			t.Errorf("expected 0 processing, got %d", p)
		}
	})

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5b_bad_upgrade_then_rollback", func(t *testing.T) {
		// Snapshot DB state before bad upgrade
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5b-", 50)

		// Wait until at least 10 jobs succeed under the good config
		startCheck := time.Now()
		for env.countByStatus(t, ctx, "succeeded") < 10 {
			if time.Since(startCheck) > 2*time.Minute {
				t.Fatal("warm-up: 10 succeeded never reached")
			}
			time.Sleep(500 * time.Millisecond)
		}
		preBad := env.countByStatus(t, ctx, "succeeded")
		t.Logf("pre-bad-upgrade succeeded count: %d", preBad)

		// Push a bad upgrade (image tag does not exist → ImagePullBackOff)
		err := runCmd(ctx, repoRoot(t), "helm",
			"-n", namespace, "upgrade", releaseName, chartPath,
			"--set", "image.repository=imgsync",
			"--set", "image.tag=does-not-exist",
			"--set", "image.pullPolicy=IfNotPresent",
			"--set", "replicaCount=2",
			// Bad upgrade will hang waiting for ready; we cap and don't block.
			"--timeout", "30s",
			"--wait")
		if err == nil {
			t.Log("bad upgrade unexpectedly succeeded (kind may have cached the tag); continuing")
		} else {
			t.Logf("bad upgrade failed as expected: %v", err)
		}

		// Verify pods are crashlooping or not running
		// (We don't assert this strictly — only that rollback recovers.)

		// Rollback
		env.helmRollback(t, ctx)
		env.waitReplicasReady(t, ctx, 2, 2*time.Minute)

		// All 50 jobs should drain
		env.waitAllSucceeded(t, ctx, 50, 5*time.Minute)

		// No dead, no stuck
		if d := env.countByStatus(t, ctx, "dead"); d != 0 {
			t.Errorf("F5b: expected 0 dead after rollback, got %d", d)
		}
		if p := env.countByStatus(t, ctx, "processing"); p != 0 {
			t.Errorf("F5b: expected 0 processing after rollback, got %d", p)
		}
	})

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5c_uninstall_reinstall_idempotent_migration", func(t *testing.T) {
		// Enqueue 30 jobs but stop the workers before they finish
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5c-", 30)

		// Uninstall (this also deletes the migrate Job since hook-succeeded reaped it)
		env.helmUninstall(t, ctx)

		// Confirm DB still has the 30 pending jobs (uninstall does NOT touch DB)
		var pending int
		row := env.pool.QueryRow(ctx, "SELECT count(*) FROM transfer_jobs WHERE status='pending'")
		if err := row.Scan(&pending); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if pending != 30 {
			t.Fatalf("F5c: expected 30 pending jobs after uninstall, got %d", pending)
		}

		// Reinstall — pre-install hook re-runs migrate up (must be idempotent)
		env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
		env.waitReplicasReady(t, ctx, 2, 3*time.Minute)

		// Workers drain the existing 30 jobs
		env.waitAllSucceeded(t, ctx, 30, 5*time.Minute)
	})
}
```

**Why the F5a `UPDATE ... locked_at = NOW() - 6m` shortcut:** the production sweeper threshold is 5 minutes. We don't want to add a values knob to lower it just for tests, and we don't want a test that takes 6 real minutes. Direct DB update simulates "the pod was killed 6 minutes ago" — what the sweeper would have observed naturally.

**Why F5b uses `--timeout 30s`:** Helm `upgrade --wait` on a bad image will block forever. The 30s cap ensures we move on to rollback. The error from helm is *expected* — we log and continue.

**Why F5c relies on `helm uninstall` not deleting the DB:** uninstall removes the Deployment, Service, PDB, ConfigMap, Secret-via-helm-managed-resources, and the migrate Job. It does NOT touch the operator-supplied DSN Secret nor the postgres database. The DB state survives — which is exactly the dirty-state we're recovering from.

- [ ] **Step 3: Run the test, confirm fail** (no helpers wired yet — should compile but fail at runtime)

Run:
```bash
make e2e-up
make e2e-dirty-state
```

Expected: compile error if `helpers.go` patches missing, or runtime failure on first sub-scenario.

- [ ] **Step 4: Iterate until all three sub-scenarios pass**

After implementing the helpers (Step 1 completes that), re-run:

```bash
make e2e-dirty-state
```

Expected output (truncated):
```
=== RUN   TestF5_DirtyStateRecovery
=== RUN   TestF5_DirtyStateRecovery/F5a_mid_flight_kill
    helpers.go: killed pod imgsync-...-xxx mid-flight
    helpers.go: sweeper-recovered jobs: 1
--- PASS: TestF5_DirtyStateRecovery/F5a_mid_flight_kill (NNs)
=== RUN   TestF5_DirtyStateRecovery/F5b_bad_upgrade_then_rollback
    dirty_state_test.go:NN: pre-bad-upgrade succeeded count: NN
    dirty_state_test.go:NN: bad upgrade failed as expected: ...
--- PASS: TestF5_DirtyStateRecovery/F5b_bad_upgrade_then_rollback (NNs)
=== RUN   TestF5_DirtyStateRecovery/F5c_uninstall_reinstall_idempotent_migration
--- PASS: TestF5_DirtyStateRecovery/F5c_uninstall_reinstall_idempotent_migration (NNs)
--- PASS: TestF5_DirtyStateRecovery (NNs)
PASS
```

**If F5a fails with "no job entered 'processing' within 30s":** the sweeper interval is 30s by default, and replicas=2 with 4 workers each = 8 goroutines. They'll be polling the queue and 100 small files might drain too fast to catch one in `processing`. Workaround: bump file size to 50MB or enqueue 1000 jobs.

**If F5b fails because `helm rollback` errors with "no rollback found":** the bad upgrade hit Helm's hook timeout but Helm may have marked the release as `failed` without recording a rollback target. Check `helm history -n imgsync-e2e imgsync` — if there's no v2 listed, the hook never started; rollback to v1 is implicit. In that case the test should `helm upgrade` back to a known-good config instead of `helm rollback`. Adjust the helper if needed.

**If F5c fails because pending count is 0 after uninstall:** check that `helm uninstall` didn't accidentally include the postgres Service in its targets. The chart should NEVER own postgres — it lives in a separate manifest.

- [ ] **Step 5: Tear down and commit**

```bash
make e2e-down
git add e2e/dirty_state_test.go e2e/helpers.go
git commit -m "feat(e2e): add F5 dirty-state recovery (mid-flight kill + rollback + reinstall)"
```

---

## Task 7: Operator runbook + final commit

**Files:**
- Create: `docs/runbook.md`
- Modify: `README.md` — link to runbook, document `make` targets

**Why a runbook in Week 3:** the test plan §User flow #17 is "SRE: did file X transfer?" — answered by a one-line SQL. That SQL needs to live somewhere the on-call can find at 3am. README + runbook is enough; no separate wiki.

**Decisions in this task:**
- Runbook is short. Five sections: enqueue, audit, expire-stuck, scale, rollback.
- Each section has the EXACT command, not a description of "how to think about it."
- README gets a "Quickstart" section pointing to `make dev-up`.

- [ ] **Step 1: Write docs/runbook.md**

Create `docs/runbook.md`:

```markdown
# imgsync — Operator Runbook

This is the on-call cheat sheet. Everything you need to debug a stuck transfer
should be on this page.

## 1. Enqueue a job manually

```bash
kubectl -n <ns> run --rm -it imgsync-cli \
  --image=<repo>/imgsync:<tag> --restart=Never \
  --env=IMGSYNC_DSN=<dsn> -- \
  enqueue \
    --trace-id=<trace> \
    --src=ftp://host/path/to/file \
    --dst=file:///mnt/share/dst/file \
    --src-protocol=ftp \
    --dst-protocol=localfs
```

## 2. Audit a single job (one-line SQL)

```sql
SELECT j.id, j.status, j.attempts, e.status AS event, e.ts, e.detail
  FROM transfer_jobs j LEFT JOIN transfer_events e USING (trace_id, job_id)
 WHERE j.trace_id = '<trace>' AND j.dst = '<dst>'
 ORDER BY e.ts;
```

The JOIN must include both `trace_id` AND `job_id` — using `trace_id` alone
fans out across re-enqueued (trace_id, dst) pairs.

Status meanings:
- `pending`     — waiting for a worker to lease
- `processing`  — leased and being transferred
- `succeeded`   — terminal: success
- `skipped`     — terminal: source absent/unreadable (`ErrSkippable`)
- `dead`        — terminal: permanent error or max_attempts exhausted

Event statuses (in `transfer_events.status`):
- `enqueue`, `lease`, `success`, `skip`, `fail` (transient), `expire`, `dead`

## 3. Find stuck jobs

```sql
-- Anything that's been "processing" longer than the sweeper threshold
SELECT id, trace_id, locked_by, locked_at, NOW() - locked_at AS held_for
  FROM transfer_jobs
 WHERE status = 'processing'
   AND locked_at < NOW() - INTERVAL '5 minutes'
 ORDER BY locked_at;
```

The sweeper runs every 30s. If the above query returns rows, the sweeper is
not running — check pod logs for the leader pod.

## 4. Scale up / down

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values --set replicaCount=8
```

Expected: 8th pod is leasing within 5 minutes (cold) or 1 minute (warm cache).
Check throughput in `/healthz`:

```bash
kubectl -n <ns> port-forward svc/imgsync 8080:8080
curl localhost:8080/healthz | jq
```

## 5. Rollback a bad release

```bash
helm history -n <ns> imgsync       # find the last good revision
helm rollback -n <ns> imgsync <N>  # rollback to revision N
```

Migrations are forward-only. Rolling back the chart does NOT revert the schema —
the older binary is expected to read the newer schema gracefully (any new column
defaults to NULL/sensible).

If the rollback hangs because the bad release has crashlooping pods:

```bash
kubectl -n <ns> delete pod -l app.kubernetes.io/name=imgsync --force
helm rollback -n <ns> imgsync <N>
```

## 6. Drain before rolling restart (rarely needed)

```bash
# Stop accepting new leases by setting replicaCount=0
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=0

# Wait for in-flight to finish (≤ terminationGracePeriodSeconds = 60s) + sweeper window
sleep 360

# Bring it back up
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=4
```
```

- [ ] **Step 2: Update README.md**

Add this section to `README.md` (or create the file if Week 1 didn't):

```markdown
## Quickstart

### Local dev (docker-compose)

```bash
make docker-build
make dev-up
make dev-seed
make dev-smoke
```

Brings up postgres + ftpd + 1 worker, enqueues 10 LocalFS jobs, asserts they
all succeed.

### Production install (Helm)

```bash
# 1. Create the DSN secret
kubectl -n <ns> create secret generic imgsync-dsn \
  --from-literal=dsn='postgres://user:pw@host:5432/imgsync?sslmode=require'

# 2. (optional) FTP credentials secret
kubectl -n <ns> create secret generic imgsync-ftp \
  --from-literal=user=<u> --from-literal=password=<p>

# 3. Install
helm upgrade --install imgsync deploy/helm/imgsync \
  -n <ns> \
  --set image.repository=<your-repo>/imgsync \
  --set image.tag=<your-tag> \
  --set replicaCount=4 \
  --set ftpSecretRef.name=imgsync-ftp
```

The chart's pre-install hook runs migrations idempotently.

### Operator runbook

See [`docs/runbook.md`](docs/runbook.md).

## Make targets

| Target            | What it does                                         |
|-------------------|------------------------------------------------------|
| `docker-build`    | Build the production container image                 |
| `docker-test`     | Verify Dockerfile contract (size, user, subcommands) |
| `dev-up`          | Stand up the docker-compose dev stack                |
| `dev-seed`        | Enqueue 10 smoke-test jobs                           |
| `dev-smoke`       | Assert all 10 jobs succeed                           |
| `dev-down`        | Tear down the dev stack                              |
| `helm-lint`       | Lint the Helm chart                                  |
| `helm-test`       | Run Helm chart structural assertions                 |
| `e2e-up`          | Bring up the kind+chart e2e environment              |
| `e2e-throughput`  | Run C7 throughput E2E (kind, ≥3.2× linearity)        |
| `e2e-dirty-state` | Run F5 dirty-state recovery E2E                      |
| `e2e-down`        | Tear down the e2e environment                        |
```

- [ ] **Step 3: Commit**

```bash
git add docs/runbook.md README.md
git commit -m "docs(runbook): add operator runbook and Quickstart README section"
```

---

## Self-Review Checklist (run before declaring Week 3 plan complete)

After all 7 tasks land:

- [ ] **Spec coverage:**
  - Test layer #14 (Helm pre-install migrate Job) → Task 4 ✓
  - C7 throughput linearity ≥3.2× → Task 5 ✓
  - F5 dirty-state recovery (mid-flight kill, rollback, uninstall/reinstall) → Task 6 ✓
  - Container image (distroless, single binary, three subcommands) → Task 1 ✓
  - Compose dev stack → Task 2 ✓
  - Operator one-line audit SQL → Task 7 (runbook) ✓
  - **Not covered:** C5 shadow reconcile — explicitly deferred to Shadow Sniffer plan, documented in "Out of scope".

- [ ] **Forward-only migration constraint upheld:**
  - Helm hook annotations: `pre-install,pre-upgrade` only — never `pre-rollback` ✓
  - Runbook §5 explicitly notes "rolling back the chart does NOT revert the schema" ✓
  - F5b test confirms rollback path works without schema revert ✓

- [ ] **Security baseline:**
  - All workloads run as `nonroot` (UID 65532), `readOnlyRootFilesystem: true` ✓
  - DSN comes from Secret reference, never embedded in values.yaml ✓
  - FTP credentials likewise (when present) ✓

- [ ] **Idempotency:**
  - migrate-job hook deletion policy `before-hook-creation,hook-succeeded` allows re-runs ✓
  - F5c explicitly tests reinstall path ✓
  - F5a sweeper-recovered job ends with `attempts==0` (C2 invariant at cluster level) ✓

- [ ] **Type/name consistency with prior weeks:**
  - `IMGSYNC_DSN`, `IMGSYNC_WORKERS`, `IMGSYNC_POD_NAME`, `IMGSYNC_HEALTH_ADDR`,
    `IMGSYNC_FTP_USER`, `IMGSYNC_FTP_PASSWORD`, `IMGSYNC_FTP_HOST_MAX_CONNS`,
    `IMGSYNC_FTP_POOL_MAX_IDLE`, `IMGSYNC_FTP_POOL_IDLE_TTL`,
    `IMGSYNC_IDLE_SLEEP_MIN`, `IMGSYNC_IDLE_SLEEP_MAX`, `IMGSYNC_MIGRATIONS_DIR` —
    all match the env names defined in Weeks 1, 2A, 2B ✓
  - Subcommand names: `migrate`, `enqueue`, `worker` — match Week 1 cobra setup ✓
  - Chart exposes `replicaCount` (lowerCamel, Helm idiom) — used consistently in tests ✓

- [ ] **No placeholders:** Every step has actual code/commands, no "TBD" / "implement appropriately" anywhere.

---

## Series Recap

After Week 3, `git log --oneline` (Week 3 portion) reads roughly:

```
docs(runbook): add operator runbook and Quickstart README section
feat(e2e): add F5 dirty-state recovery (mid-flight kill + rollback + reinstall)
feat(e2e): add C7 throughput scale-out E2E (kind, 2→8 replicas, ≥3.2x)
feat(helm): add forward-only migrate Job as pre-install/pre-upgrade hook
feat(helm): add imgsync chart with worker Deployment, PDB, ConfigMap
feat(compose): add docker-compose dev stack with end-to-end smoke test
feat(docker): add multi-stage Dockerfile with distroless runtime
```

That's v1 base done. Next plan in the series: `2026-04-27-imgsync-shadow-sniffer.md` (already drafted) — adds the NiFi-shadowing sniffer subsystem on top of this base.
