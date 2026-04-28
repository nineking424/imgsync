# imgsync

Go + PostgreSQL file-transfer queue. Replaces an in-house NiFi pipeline.

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
| `build`           | Build the `imgsync` binary                           |
| `test`            | Run the unit test suite (`go test ./... -race`)      |
| `lint`            | Run `golangci-lint`                                  |
| `ci`              | `lint` + streaming guard + `test` (the CI gate)      |
| `docker-build`    | Build the production container image                 |
| `docker-test`     | Verify Dockerfile contract (size, user, subcommands) |
| `dev-up`          | Stand up the docker-compose dev stack                |
| `dev-seed`        | Enqueue 10 smoke-test jobs                           |
| `dev-smoke`       | Assert all 10 jobs succeed                           |
| `dev-down`        | Tear down the dev stack                              |
| `helm-lint`       | Lint the Helm chart                                  |
| `helm-template`   | Render the Helm chart with default values            |
| `helm-test`       | Run Helm chart structural assertions                 |
| `e2e-up`          | Bring up the kind+chart e2e environment              |
| `e2e-throughput`  | Run C7 throughput E2E (kind, ≥3.2× linearity)        |
| `e2e-dirty-state` | Run F5 dirty-state recovery E2E                      |
| `e2e-down`        | Tear down the e2e environment                        |
