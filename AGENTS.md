# AGENTS.md — imgsync codebase guide for AI agents

> **What this is.** A single ground-truth map of the imgsync codebase, written for AI coding agents. Every concrete claim (paths, symbols, SQL columns, env vars, metric names) was extracted from source and adversarially fact-checked against the code. Treat it as authoritative — but re-read the specific lines before an edit, since code drifts.
>
> **How to use it.** Read *TL;DR* + *Architecture* first, then jump to the relevant deep section via the [table of contents](#table-of-contents). The **"Agent notes"** bullets in each section are the load-bearing gotchas — read them before editing that area. Paths are repo-relative to `/Users/nineking/workspace/app/imgsync`.

**imgsync** is a **Go 1.25 + PostgreSQL file-transfer work queue** that replaces an in-house NiFi pipeline. It moves large volumes of files server-to-server with per-file traceability, FTP session reuse, and horizontal worker scale-out. **The Postgres database _is_ the queue** — there is no broker, no Redis, no in-memory job state. One static binary exposes four cobra subcommands; behavior is selected by `args`.

## Quick facts

| | |
|---|---|
| **Module** | `github.com/nineking424/imgsync` (Go 1.25, single static binary) |
| **Subcommands** | `migrate` · `enqueue` · `worker` · `sniffer` (cobra; `cmd/imgsync/`) |
| **Queue backend** | PostgreSQL, 2 tables (`transfer_jobs` + `transfer_events`), `FOR UPDATE SKIP LOCKED` lease |
| **Protocols** | `localfs`, `ftp` implemented · `s3` + TUI client + backend server are PRD roadmap, **not in code** |
| **Key deps** | `jackc/pgx/v5`, `spf13/cobra`, `jlaffaye/ftp`, `fclairamb/ftpserverlib` (test), `prometheus/client_golang`, `stretchr/testify`, `testcontainers-go` |
| **Observability** | `/livez /readyz /healthz /metrics` on `:8080`; `imgsync_*` Prometheus metrics |
| **Packaging** | distroless image (`gcr.io/distroless/static-debian12:nonroot`, <50 MB, nonroot 65532) |
| **Deploy** | Helm chart `deploy/helm/imgsync` (worker + sniffer Deployments + pre-install migrate Job) |
| **Dev / E2E** | docker-compose (postgres + ftpd + worker) · kind cluster for E2E |
| **CI gate** | `make ci` = `golangci-lint` + streaming guard + `go test ./... -race -count=1` |

## TL;DR for agents — must-know invariants

1. **The queue is two Postgres tables.** `transfer_jobs` (mutable current state, one row/job) + `transfer_events` (append-only audit, one row/transition, `ON DELETE CASCADE`). Workers `UPDATE` jobs and only ever `INSERT` events — never update events. See [Database Schema](#database-schema--migrations).
2. **Two distinct status vocabularies — do not cross them.** `transfer_jobs.status` is the `job_status` **ENUM** = `pending, leased, succeeded, skipped, dead`. `transfer_events.status` is a **TEXT CHECK** of verbs = `enqueue, lease, success, skip, fail, expire, dead`. There is **no `processing`/`running`/`failed`** anywhere.
3. **Leasing is `FOR UPDATE SKIP LOCKED`.** `LeaseJob` claims the oldest due `pending` row (`WHERE status='pending' AND next_run_at <= NOW() ORDER BY next_run_at, id ... LIMIT 1`), flips it to `leased`. Partial index `transfer_jobs_pending_idx` matches this exactly. Changing the ORDER BY/WHERE without updating the index silently causes seq scans.
4. **Idempotency = `(trace_id, dst)` UNIQUE.** `internal/jobs/enqueue.go:Enqueue` (`ON CONFLICT (trace_id, dst) DO NOTHING`) is the **only** sanctioned insert path into `transfer_jobs`. Never hand-roll inserts.
5. **Error class is the control plane.** Status routing is `errors.Is` against `transfer.ErrSkippable` (→ `skipped`, attempts unchanged) and `transfer.ErrPermanent` (→ `dead`, no retry). Anything else → retry with jittered exponential backoff (~`1<<(attempts+1)` s ± jitter). **Wrap with `%w`, never compare error strings.** FTP/localfs transports classify permanent failures (FTP 550/552/553, missing dst dir) as `ErrPermanent` by reply code — but a 550 carrying *permission/access-denied* is carved out so it surfaces, not silently skips.
6. **Streaming is sacred (hard CI gate).** `Source`/`Transport` impls must stream — never `io.ReadAll`/`ioutil.ReadAll` or `bytes.NewBuffer(...body...)` in `internal/{sources,transports,transfer}`. `scripts/check-streaming.sh` greps for these and fails the build. Use `io.Copy`/`io.TeeReader`/`io.MultiWriter`.
7. **Protocol registration is hard-coded in `cmd/imgsync/worker.go`** (the `SourceFor`/`TransportFor` switch closures, `"localfs"`/`"ftp"`). Adding a protocol = edit *both* closures there; `internal/worker` only knows `ErrUnknownProtocol`. FTP transport must stay wrapped by `hostcap.Wrap`.
8. **Sniffer is single-pod by contract.** One `Sniffer` per source maintains a `(ts, pk)` high-watermark in `sniffer_state` (no advisory lock). The watermark advances **only after a whole batch enqueues cleanly** — that's the crash-safety guarantee. Helm hard-`fail`s if `sniffer.replicas > 1`.
9. **`hostcap` is NOT host-capacity measurement.** Despite the name it doesn't use gopsutil (only a transitive `// indirect` dep, never imported). It's a **cluster-wide per-FTP-host concurrency cap** via Postgres advisory locks, and it runs on its **own dedicated DB pool** (`MaxConns = Cap+2`), not the worker pool — so an in-flight transfer can't starve lease/commit/scrape. Worker concurrency is the static `IMGSYNC_WORKERS` env (default 4).
10. **Migrations are forward-only, self-registering, lexically ordered.** Each `NNNN_*.up.sql` does its own `INSERT INTO schema_migrations`; the runner only reads applied versions. `0001` has no `.down.sql`. Helm pre-install/pre-upgrade hook runs `migrate up` (must stay idempotent).

## Never do (hard rules)

- **Never `git push`, `--force`, `git reset --hard`, or `git commit`** without explicit user request (global CLAUDE.md). Never bypass hooks with `--no-verify`.
- **Never reintroduce a `processing`/`running`/`failed` job status** — the enum is fixed; E2E helpers carry explicit comments warning against it.
- **Never buffer a transfer body in memory** in `internal/{sources,transports,transfer}` — it trips the streaming guard and blows the C1 250 MiB RSS contract.
- **Never insert into `transfer_jobs` outside `jobs.Enqueue`** (or the sniffer's `Enqueuer`) — you lose idempotency + the paired `enqueue` event.
- **Never change Helm `selectorLabels`/`component` labels** — they're immutable; `helm upgrade` will fail ("field is immutable").
- **Never "improve" adjacent/dead code, comments, or formatting** outside the requested change (global CLAUDE.md §3 — surgical changes only).

## Repository layout (codemap)

```
cmd/imgsync/              # single binary + 4 cobra subcommands — WIRING ONLY (env → pools → objects → internal/*)
internal/
  transfer/              # domain boundary: Source/Transport interfaces + ErrSkippable/ErrPermanent sentinels + ctxreader (ctx-abortable copy)
  jobs/                  # idempotent Enqueue — the ONLY transfer_jobs insert path
  env/                   # shared typed env accessors (env.Int/env.Bool); replaced the per-file envInt/envBool helpers
  worker/                # lease→process→complete lifecycle (Runner, ProcessJob→(result,err), per-iteration panic recover, drainLease, single-CTE terminal writes)
  sources/{localfs,ftp}/      # input adapters (Source impls)
  transports/{localfs,ftp}/   # output adapters (Transport impls); transports/ftp/pool.go = per-host session pool
  sniffer/               # poll external source DB → incremental enqueue (high-watermark in sniffer_state)
  sweeper/               # reclaim dead/stale leases (pg advisory xact lock)
  retention/             # OPT-IN batched DELETE of old terminal rows (mirrors sweeper; own advisory key; disabled by default)
  db/                    # pgx pool builder + migration runner
  sourcedb/              # separate pool for sniffer's source DB
  health/                # /livez /readyz /healthz /metrics HTTP server
  hostcap/               # per-FTP-host concurrency cap via advisory locks (NOT gopsutil); runs on its OWN dedicated db pool
  backoff/               # shared jittered idle backoff for empty-queue workers
  metrics/               # Prometheus collectors + private registry (imgsync_*)
  cli/                   # sniffer env parsing + poll-loop wiring (logic NOT in cmd/)
  ftpserver/             # in-process FTP server for tests (afero-backed)
  eval/                  # executable correctness spec: C0–C6 invariant/contract tests
migrations/              # 0001–0004 forward-only SQL pairs — the data model IS the queue (0004 drops the redundant trace_id index)
deploy/helm/imgsync/     # production Helm chart (templates/, values.yaml, tests/template_test.sh)
e2e/                     # kind-cluster E2E (C7 throughput / F5 dirty-state / C5' sniffer) + manifests/
scripts/                 # dev-*, e2e-*, check-streaming.sh (CI guard), test-docker-build.sh
docs/                    # MkDocs site (Korean) + superpowers/{plans,specs} + test-reports
PRD.txt  README.md  Makefile  Dockerfile  docker-compose.yml  mkdocs.yml  go.mod
```

## Architecture & data flow

```
 external source DB ──poll(watermark)──▶ sniffer ─┐                    ┌─ destination (localfs / ftp)
 manual CLI ──────────────────────────── enqueue ─┤                    │        ▲
                                                   ▼  ON CONFLICT       │   Transport.Send
                              ┌──────────── transfer_jobs (PG queue) ───┴───┐  (stream + sha256 + size verify)
                              │  pending → leased → succeeded / skipped / dead│        ▲
                              │  + transfer_events (append-only audit log)    │   Source.Open (stream)
                              └────────────────▲──────────────┬──────────────┘        ▲
                                               │              │ LeaseJob              │
                              sweeper ─reclaim─┘     FOR UPDATE SKIP LOCKED ──▶ worker pool (N goroutines)
                              stale leases                                     each: lease → dispatch by protocol
                              (advisory lock,                                  → Source→Transport → write status
                               leased→pending,                                   + event in ONE tx
                               'expire' event)
```

- **Ingest**: `sniffer` polls an external Postgres on an interval and enqueues incrementally (deterministic `trace_id = "<table>-<pk>"`); `enqueue` CLI is the manual path. Both insert via `ON CONFLICT (trace_id, dst) DO NOTHING`.
- **Process**: `worker` runs N goroutines, each leasing a row (`SKIP LOCKED` → disjoint claims), opening a streaming `Source` (ctx-abortable), piping bytes through a byte-counter into `Transport.Send` (computes sha256 + verifies size), then writing terminal status + audit event as **one lease-guarded writable-CTE statement**. Panics are recovered per-iteration so a poison job caps to `dead` instead of killing the worker.
- **Recover**: `sweeper` (a goroutine inside `worker`) resets leases older than 5 min back to `pending` and emits an `expire` event — guarded by a single-writer advisory lock so multiple pods cooperate.
- **Drain / retain**: on SIGTERM the worker resets its own still-`leased` rows to `pending` (immediate requeue, not a 5-min wait); an **opt-in** `retention` goroutine batch-deletes old terminal rows when `IMGSYNC_RETENTION_DAYS>0` (events cascade), disabled by default.
- **Observe**: every daemon serves `/healthz` + `/metrics` on `:8080` (`/livez` 503s a wedged lease loop); scrape-time collectors run `GROUP BY status` / lease-age / oldest-pending-age SQL per Prometheus scrape.

## Build, test & common commands

| Command | What it does |
|---|---|
| `make ci` | **The CI gate.** `lint` + `streaming-check` + `test`. Run before every PR — if it's red, CI is red. |
| `make test` | `go test ./... -race -count=1` (unit; race always on, cache off) |
| `make lint` | `golangci-lint run` (gofmt, goimports, revive `exported`/`var-naming`/`error-return`/`error-strings`, bodyclose, misspell) |
| `make streaming-check` | `scripts/check-streaming.sh` — forbids in-memory body buffering in sources/transports/transfer |
| `make build` | `go build -o bin/imgsync ./cmd/imgsync` |
| `make dev-up && make dev-seed && make dev-smoke && make dev-down` | Local docker-compose smoke: 10 LocalFS jobs end-to-end |
| `make docker-build && make docker-test` | Build prod image + verify Dockerfile contract (size/user/subcommands) |
| `make helm-lint && make helm-template && make helm-test` | Helm chart lint / render / structural assertions |
| `make test-integration-sniffer` | Sniffer S0–S3 integration (`-tags integration`, needs Docker) |
| `make e2e-up && make e2e-throughput && make e2e-dirty-state && make e2e-sniffer && make e2e-down` | kind E2E (C7 ~35 m, F5 ~30 m, C5' ~20 m; `IMGSYNC_E2E=1`) |

Detail on every target, the Helm chart, and CI lives in [Build, Packaging & Deployment](#build-packaging--deployment-docker-helm-makefile-ci); the full test pyramid + build tags in [Testing Strategy](#testing-strategy-build-tags--e2e).

## Platform notes (macOS / BSD dev host)

- This dev host is **macOS/zsh with BSD coreutils**. `find`/`sed`/`date`/`grep` differ from GNU. Prefer `rg` (ripgrep), `gdate`/`gsed`/`gfind` (coreutils) — `sed -i` needs `sed -i ''`; BSD `date` lacks `+%s%N`.
- `gopls` (Serena's symbol backend) is at `~/go/bin/gopls`; `$HOME/go/bin` may not be on `PATH` permanently.
- For semantic code navigation/edits prefer **Serena** symbol tools over raw grep/read where available.

## Table of contents

1. [Database Schema & Migrations](#database-schema--migrations) — the data model = the queue
2. [Domain Interfaces (`internal/transfer`, `internal/jobs`)](#domain-interfaces-internaltransfer-internaljobs) — Source/Transport contract + sentinels + ctxreader + Enqueue
3. [Worker Pipeline (`internal/worker`)](#worker-pipeline-internalworker) — lease→process→complete lifecycle
4. [Sources & Transports](#sources--transports) — localfs/ftp I/O adapters + FTP session pool
5. [Sniffer / DB Connector (`internal/sniffer`)](#sniffer--db-connector-internalsniffer) — incremental ingest + watermark
6. [Sweeper, Eval & Retention](#sweeper-eval--retention) — lease recovery + invariants + opt-in retention
7. [Infra Adapters](#infra-adapters-db-sourcedb-health-hostcap-backoff-metrics-env-ftpserver) — pools, health, hostcap, metrics, env
8. [CLI & Entry Points (`cmd/imgsync`)](#cli--entry-points-cmdimgsync) — subcommand wiring + signals
9. [Configuration & Environment Variables](#configuration--environment-variables) — every env var + flag
10. [Build, Packaging & Deployment](#build-packaging--deployment-docker-helm-makefile-ci) — Docker / Helm / Makefile / CI
11. [Testing Strategy, Build Tags & E2E](#testing-strategy-build-tags--e2e) — unit / integration / E2E + guards
12. [Repository & Docs Map](#repository--docs-map-where-to-find-more) — where to find authoritative detail

---

## Database Schema & Migrations

The queue *is* the database: two Postgres tables (`transfer_jobs`, `transfer_events`) plus a per-source watermark table (`sniffer_state`) hold all state — there is no broker. Migrations are plain `NNNN_name.up.sql`/`.down.sql` files applied lexically by `db.ApplyMigrations`, which self-bootstraps `schema_migrations` and serializes pods with a session advisory lock. The current schema is at version `0004`; **`transfer_jobs_trace_id_idx` no longer exists** — migration 0004 dropped it (issue #34) as a redundant leading-column prefix of the `UNIQUE(trace_id, dst)` index. All terminal/retry writes are single writable-CTE statements (the #19 lease guard), and an opt-in retention job may DELETE old terminal rows (events cascade).

### Key files & symbols
- `migrations/0001_initial.up.sql` — creates `job_status` ENUM, `transfer_jobs`, `transfer_events`, `schema_migrations`; defines three indexes on `transfer_jobs` (`pending_idx`, `leased_idx`, `trace_id_idx`) plus the `UNIQUE(trace_id, dst)` constraint and two `transfer_events` indexes.
- `migrations/0002_sniffer_state.{up,down}.sql` — `sniffer_state` watermark table (one row per `source_id`: `last_run_ts`, `last_run_pk`, `updated_at`).
- `migrations/0003_jobs_status_index.{up,down}.sql` — adds `transfer_jobs_status_idx ON (status)` for the `GROUP BY status` queue-depth metric scrape.
- `migrations/0004_drop_trace_id_index.{up,down}.sql` — **DROPs `transfer_jobs_trace_id_idx`** (issue #34); down-migration recreates it.
- `internal/db/migrate.go:ApplyMigrations(ctx, dsn, dir)` — runs unapplied `*.up.sql` in lexical order (`sort.Strings`) under `pg_advisory_lock(migrationAdvisoryLockID)` (`0x494d4753594e43` = "IMGSYNC"); session-scoped lock, single `pgx.Conn`.
- `internal/worker/job.go:LeaseJob(ctx, pool, lockedBy) (*Job, error)` — atomic lease via `FOR UPDATE SKIP LOCKED` + `UPDATE...RETURNING`; returns `(nil, nil)` on empty queue.
- `internal/worker/process.go:ProcessJob(ctx, Deps, *Job) (string, error)` — drives one job to terminal; first return is the metric result label (`succeeded`/`skipped`/`dead`/`fail`). Terminal writes: `writeSuccess` / `writeTerminal` / `writeTerminalWithAttempts` / `writeRetryOrDead`.
- `internal/sweeper/sweeper.go:Sweep` — recovers stale `leased` rows back to `pending`, emits `expire` events.
- `internal/retention/retention.go:Sweep` — opt-in batched DELETE of old terminal rows.
- `internal/jobs/enqueue.go:Enqueue` — idempotent insert via `ON CONFLICT (trace_id, dst) DO NOTHING`.
- `internal/sources/ftp/source.go:isNotFound` — classifies an FTP 550 reply as missing-source (the 550 permission carve-out, below).
- `internal/hostcap/hostcap.go:Wrap`/`CapTransport.Send` — per-host concurrency cap via `pg_advisory_lock(hashtext(slotKey(host,slot)))` on a connection pinned from the shared pool for the whole transfer.

### How it works / flow
**Schema.** `job_status` ENUM = `pending | leased | succeeded | skipped | dead`. `transfer_jobs` carries `attempts`/`max_attempts` (default 5), `next_run_at` (default `NOW()`), lease columns `locked_at`/`locked_by`, `payload JSONB`, and `UNIQUE(trace_id, dst)` (constraint index `transfer_jobs_trace_id_dst_key`). `transfer_events` is an append-only audit log keyed by `job_id BIGINT NOT NULL REFERENCES transfer_jobs(id) ON DELETE CASCADE`, with `CHECK (status IN ('enqueue','lease','success','skip','fail','expire','dead'))` (`transfer_events_status_check`).

**Current index set** (after 0004 — four on `transfer_jobs`, two on `transfer_events`):
- `transfer_jobs` PK on `id` (`BIGSERIAL`).
- `transfer_jobs_pending_idx ON (next_run_at, id) WHERE status='pending'` — the lease hot path.
- `transfer_jobs_leased_idx ON (locked_at) WHERE status='leased'` — sweeper scan.
- `transfer_jobs_status_idx ON (status)` — metric `GROUP BY status` (index-only scan + HashAggregate).
- `transfer_jobs_trace_id_dst_key` — UNIQUE(trace_id, dst); also serves `WHERE trace_id=$1 AND dst=$2` lookups.
- `transfer_events_job_id_idx ON (job_id)`, `transfer_events_trace_id_ts_idx ON (trace_id, ts)`.
- **No `transfer_jobs_trace_id_idx`** — dropped in 0004.

**State machine.** `pending → leased` (LeaseJob: `SKIP LOCKED`, `next_run_at <= NOW()`, ordered by `(next_run_at, id)`). From `leased`: → `succeeded` / `skipped` / `dead`, or back to `pending` on retry (`next_run_at = NOW() + backoff`). Backoff (`worker.retryBackoff`) is nominal `1<<attempts` seconds (so the first retry is ~2s, then 4/8/16/32…) with ±25% uniform jitter. Retry continues until `nextAttempts >= max_attempts` forces a terminal `dead`. The sweeper resets `leased` rows whose `locked_at` is older than `Threshold` (default 5m) back to `pending` and logs an `expire` event (`detail = {"reason":"lease_expired"}`).

**Error classification → event/status.** `transfer.ErrSkippable` → job `skipped` / event `skip`; `transfer.ErrPermanent` → job `dead` / event `dead` (immediate, no retry); any other error → retry (`fail` event) or, when exhausted, `dead`. The **FTP 550 permission carve-out** lives in `internal/sources/ftp/source.go:isNotFound`: a `550 StatusFileUnavailable` reply (matched on the `*textproto.Error` reply *code*, not message language) maps to `ErrSkippable` (missing source → `skipped`) **only when the message does NOT contain `"permission"` or `"access denied"`** — those are operator misconfigurations that must surface, so they fall through to the default retry→dead path. (LocalFS source mirrors this: `os.ErrNotExist` → `ErrSkippable`, a directory → `ErrPermanent`; FTP/localfs transports map 550/552/553 / missing-parent-dir → `ErrPermanent`.)

**Writable-CTE terminal writes (#19 lease guard).** Every terminal/retry transition is one `pool.Exec` statement: `WITH u AS (UPDATE transfer_jobs SET ... WHERE id=$1 AND status='leased' AND locked_by=$N RETURNING trace_id) INSERT INTO transfer_events ... SELECT ... FROM u`. The `status='leased' AND locked_by=$N` predicate is the lease guard: if the lease was lost (swept + re-leased), the UPDATE matches 0 rows, the CTE is empty, and **no event is inserted** — a silent no-op. `writeRetryOrDead` additionally checks `ct.RowsAffected()==0` (the INSERT count) to skip the `OnRetry` callback in that case.

**Retention (opt-in, default OFF).** `retention.Sweep` batched-DELETEs `transfer_jobs WHERE status IN ('succeeded','skipped','dead') AND updated_at < NOW() - Window` (via `id IN (SELECT ... LIMIT $BatchSize)`), looping in `BatchSize` chunks (default 1000) inside one tx guarded by `pg_try_advisory_xact_lock(hashtext('imgsync_retention'))`. Their `transfer_events` cascade-delete via the FK. Disabled when `Window <= 0` (`retention.Sweep` returns immediately); never touches `pending`/`leased`. Configured programmatically via `retention.Config{Window, BatchSize, Interval}` — there is no env-var wiring in-tree.

### Agent notes (gotchas, conventions, constraints)
- **Do NOT re-add `transfer_jobs_trace_id_idx`.** 0004 dropped it deliberately (#34); the production lookup is fully covered by `UNIQUE(trace_id, dst)` (e.g. the enqueue conflict-path `SELECT ... WHERE trace_id=$1 AND dst=$2`). Re-adding it only adds write cost.
- **New migrations are append-only**: next file is `0005_*.up.sql`/`.down.sql`. `ApplyMigrations` discovers files by lexical `sort.Strings` on `*.up.sql` names and skips versions in `schema_migrations`. Each up-migration MUST `INSERT INTO schema_migrations (version) VALUES ('NNNN_name')` itself (version = filename minus `.up.sql`) and should wrap DDL in `BEGIN;`/`COMMIT;`. Down-migrations `DELETE FROM schema_migrations WHERE version=...`. Note migrations run via a single `conn.Exec(string(body))`, so multi-statement files must be valid as one batch.
- **Advisory-lock keys share one global namespace** and must not collide: migration = constant int64 `0x494d4753594e43` (`pg_advisory_lock`, session); sweeper = `hashtext('imgsync_sweeper')` (`sweeperLockKey`, `pg_try_advisory_xact_lock`); retention = `hashtext('imgsync_retention')` (`retentionLockKey`, xact); hostcap = `hashtext(slotKey(host, slot))` (`pg_advisory_lock`, session, on a pinned pool connection). Adding a new advisory lock requires a new unique key.
- **Lease guard is load-bearing.** Any terminal write to `transfer_jobs` from the worker MUST keep `AND status='leased' AND locked_by=$lockedBy` and the writable-CTE shape so a lost lease is a no-op. Don't split the UPDATE and event INSERT into two statements.
- **Enqueue idempotency** rides on `ON CONFLICT (trace_id, dst) DO NOTHING` inside a tx; the insert path also emits the `enqueue` event, and the conflict path falls back to a `SELECT id ... WHERE trace_id=$1 AND dst=$2` lookup. That's why the UNIQUE index can't be dropped — changing the dedupe key means changing this constraint.
- **`transfer_events` allowed `status` values** are constrained by `transfer_events_status_check` — adding a new event status requires a migration to alter that CHECK, not just a code change.
- **`transfer_events.job_id` has `ON DELETE CASCADE`**; retention relies on it. Removing the cascade would block/orphan deletes.
- **The metric scrape** `SELECT status::text, COUNT(*)::bigint FROM transfer_jobs GROUP BY status` (`internal/metrics/queue_depth.go`, emitted as `imgsync_jobs_in_status{status}`) depends on `transfer_jobs_status_idx`; keep it if you touch index DDL.

Relevant paths: `/Users/nineking/workspace/app/imgsync/migrations/`, `/Users/nineking/workspace/app/imgsync/internal/db/migrate.go`, `/Users/nineking/workspace/app/imgsync/internal/worker/{job.go,process.go,errdetail.go}`, `/Users/nineking/workspace/app/imgsync/internal/sweeper/sweeper.go`, `/Users/nineking/workspace/app/imgsync/internal/retention/retention.go`, `/Users/nineking/workspace/app/imgsync/internal/jobs/enqueue.go`, `/Users/nineking/workspace/app/imgsync/internal/sources/ftp/source.go`, `/Users/nineking/workspace/app/imgsync/internal/hostcap/hostcap.go`, `/Users/nineking/workspace/app/imgsync/internal/metrics/queue_depth.go`. Verified against HEAD `5948ea8`.

## Domain Interfaces (`internal/transfer`, `internal/jobs`)

The `transfer` package defines the two core streaming contracts — `Source` (open a reader for a src) and `Transport` (stream a body to a dst) — plus the two sentinel errors (`ErrSkippable`, `ErrPermanent`) the worker uses to classify terminal outcomes, and `ctxreader.go`, a cancellation-aware reader wrapper. The `jobs` package holds `Enqueue`, the idempotent single-row insert keyed on `(trace_id, dst)`. The interface and error contracts are unchanged by the #51–#55 batch; `ctxreader.go` (#22) wraps the body in both transports so in-flight copies abort on cancel. Separately, migration `0004` dropped the now-redundant single-column index `transfer_jobs_trace_id_idx` (the only production lookup `WHERE trace_id=$1 AND dst=$2` is fully covered by the `UNIQUE(trace_id, dst)` index `transfer_jobs_trace_id_dst_key`).

### Key files & symbols
- `internal/transfer/interfaces.go:Source` — `Open(ctx, src) (body io.ReadCloser, size int64, err error)`; caller MUST `Close`; impl MUST NOT buffer body; `size = -1` if unknown.
- `internal/transfer/interfaces.go:Transport` — `Send(ctx, dst, body io.Reader, expectedSize int64) (writtenBytes int64, sha256Hex string, err error)`; impl MUST count written bytes + compute sha256 over the stream (the interface comment calls this the worker's "D6 size verification"; the actual verify block in `process.go:56` is commented "F4"); MUST NOT buffer.
- `internal/transfer/errors.go:ErrSkippable` — sentinel → worker marks job `skipped` (terminal, audit-only). Production uses: **source not found** — `localfs` Source on `os.Stat` `ErrNotExist` (`sources/localfs/source.go:28`), `ftp` Source on `RETR` not-found (`sources/ftp/source.go:60`). (The doc comment also names "D6 size mismatch / dst-already-exists with identical sha256", but no production path wraps those as `ErrSkippable` — see ErrPermanent below.)
- `internal/transfer/errors.go:ErrPermanent` — sentinel → worker marks job `dead` immediately, bypassing the retry budget. Production uses: malformed/empty src URI, scheme, host, or path (`sources/ftp/source.go:31/34/38/42`); `localfs` src is a directory (`sources/localfs/source.go:33`); `localfs` missing parent dir on tmp-create (`transports/localfs/transport.go:40`, `os.ErrNotExist` only); FTP permanent reply codes 550/552/553 on STOR/Rename (`transports/ftp/transport.go:100`); and **size-verify mismatch** (`process.go:59/65`).
- `internal/transfer/ctxreader.go:NewCtxReader(ctx, r) io.Reader` — wraps `r` so `Read` returns `ctx.Err()` on cancel even if the underlying `Read` blocks indefinitely.
- `internal/transfer/ctxreader.go:ctxReader` — unexported impl; fields `ctx context.Context`, `r io.Reader`, `scratch []byte`, `pending chan readResult`. Helper type `readResult{n int; err error}`.
- `internal/jobs/enqueue.go:EnqueueArgs` — input struct: `TraceID, Src, Dst, SrcProtocol, DstProtocol string; Payload []byte; MaxAttempts int`.
- `internal/jobs/enqueue.go:Enqueue(ctx, pool *pgxpool.Pool, a EnqueueArgs) (int64, bool, error)` — idempotent insert; returns `(id, inserted, err)`.

### How it works / flow

**`NewCtxReader` (exact behavior):** Returns an `io.Reader` that honors `ctx` cancellation per-`Read`. On each `Read(p)`: first returns `ctx.Err()` immediately if ctx is already cancelled (a nil/already-cancelled ctx short-circuits before any underlying read). Otherwise, if no read is in flight (`pending == nil`), it grows a single owned `scratch` buffer only when `len(scratch) < len(p)` (it does not shrink), launches one goroutine running `c.r.Read(c.scratch[:len(p)])`, and delivers the `readResult{n, err}` to a buffered (cap-1) `pending` channel. It then `select`s on `pending` vs `ctx.Done()`. On result: clears `pending`, `copy`s `scratch[:res.n]` into `p`, returns `(copied, res.err)`. On cancel: returns `(0, ctx.Err())` **leaving `pending` in place** — the abandoned goroutine still solely owns `scratch`, and a subsequent `Read` reuses that same `pending` channel rather than starting a concurrent underlying read (avoids racing two `Read`s on `c.r`). Not safe for concurrent use; never shares `scratch` with the caller's `p` (always copies out). It does NOT buffer the full body — one bounded underlying `Read` at a time.

**Transport usage:** `localfs` does `io.Copy(mw, transfer.NewCtxReader(ctx, body))` (`mw` = MultiWriter over tmp file + sha256 hasher), then fsync/close/rename (`transports/localfs/transport.go:50`). `ftp` does `io.TeeReader(transfer.NewCtxReader(ctx, body), cw)` where `cw` is a `countingHashWriter` over the sha256 hasher (`transports/ftp/transport.go:65`). So a cancelled job's `io.Copy`/STOR aborts promptly instead of hanging on a stalled socket. The wrapped ctx error (`context.Canceled`/`DeadlineExceeded`) surfaces as a copy/STOR error — it is NOT one of the `transfer` sentinels, so `classifyAndWrite` falls to its `default` case and the worker treats it as retryable. (Note: `ProcessJob` itself wraps `body` in a separate `counter{r: body}` (`process.go:50`, field `cw.n`) to count read bytes for the unknown-size verify path — that counter is distinct from the transports' `NewCtxReader`/`countingHashWriter`.)

**Sentinel classification (`internal/worker/process.go:classifyAndWrite`):** `errors.Is(jobErr, transfer.ErrSkippable)` → `skipped`; `errors.Is(jobErr, transfer.ErrPermanent)` → `dead`; everything else falls to `writeRetryOrDead` (retry, or `dead` once attempts are exhausted). The size-verify block (`process.go:57–68`) wraps both the known-size (`written != srcSize`) and unknown-size (`cw.n != written`) mismatches with `%w … ErrPermanent`, so a mismatch marks the job **dead**, not skipped. Transports also wrap sentinels via `%w` (e.g. `transports/localfs/transport.go:40` wraps `ErrPermanent` for missing-parent-dir tmp-create; `transports/ftp/transport.go:100` wraps it for FTP 550/552/553). Sentinels are matched by `errors.Is`, so always wrap with `%w`, never replace. The terminal/retry writes themselves are single writable-CTE statements (`WITH u AS (UPDATE … WHERE status='leased' AND locked_by=$N RETURNING trace_id) INSERT INTO transfer_events … SELECT … FROM u`) carrying the #19 lease guard, so the event only fires when the UPDATE matched the leased row.

**`jobs.Enqueue`:** Validates required fields (`TraceID/Src/Dst` non-empty, `SrcProtocol/DstProtocol` non-empty). Defaults `MaxAttempts` to `5` if `<= 0`, and `Payload` to `[]byte("{}")` if `len(payload) == 0`. Opens a tx (`pool.BeginTx`) with `defer tx.Rollback`, then runs `INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol, payload, max_attempts) VALUES ($1..$7) ON CONFLICT (trace_id, dst) DO NOTHING RETURNING id`. This is a plain two-statement tx, **not** a writable CTE. On successful insert (scan ok): also inserts a `transfer_events` row `(trace_id, job_id, status='enqueue', detail=payload)`, commits, returns `(id, true, nil)`. On `pgx.ErrNoRows` (conflict): `SELECT id FROM transfer_jobs WHERE trace_id=$1 AND dst=$2`, commits, returns `(id, false, nil)` — `inserted=false` means the tuple already existed and `id` is the existing row. Any non-`ErrNoRows` scan error → `insert: %w`.

### Agent notes (gotchas, conventions, constraints)
- **`ctxReader` cancellation leaks a goroutine by design.** On ctx-cancel it returns and leaves the underlying `Read` goroutine running (it owns `scratch`); that goroutine only finishes when the underlying `Read` eventually returns. This is intentional to keep `scratch`/`p` race-free. Do not "fix" it by closing `pending` or reusing `scratch` from the caller side. The body MUST still be `Close`d by the worker (`process.go` close handling) to unblock that goroutine.
- `ctxReader` is **not concurrency-safe** and the returned type is `io.Reader` only (no `Close`) — it deliberately does not close the wrapped reader; ownership of `body.Close()` stays with the Source caller / worker.
- Interface contracts are load-bearing comments, not just docs: **MUST NOT buffer the entire body in memory** (both `Source.Open` and `Transport.Send`), and `Transport` MUST count written bytes + sha256 the stream — the worker relies on these return values for size verification (`process.go:57`). Changing return semantics breaks `process.go`.
- `size = -1` (Source) / `expectedSize = -1` (Transport) is the sentinel for "unknown size"; do not conflate with `0`. Both transports currently ignore `expectedSize` (the parameter is `_ int64`); the size check lives entirely in `process.go`.
- Sentinel errors are package-level `var`s compared via `errors.Is`. Always wrap (`fmt.Errorf("…: %w", … , transfer.ErrPermanent)`; note `localfs` tmp-create uses a double-`%w`: `"…: %w: %w", err, transfer.ErrPermanent`) — replacing or string-matching breaks worker classification. Context-cancel errors are deliberately NOT sentinels (kept retryable). **Size mismatch is `ErrPermanent` (→ dead), not `ErrSkippable`** — `ErrSkippable` in production means src-not-found only.
- FTP write/rename permanence is matched on the jlaffaye `*textproto.Error` reply code, not substrings: `550` (`StatusFileUnavailable`), `552` (`StatusExceededStorage`), `553` (`StatusBadFileName`) wrap `ErrPermanent`; all other 4xx/dial/connection errors are returned unchanged so the worker retries (`transports/ftp/transport.go:classify`).
- `Enqueue` idempotency key is the composite `(trace_id, dst)` unique constraint, NOT `trace_id` alone — same trace fanning out to multiple dsts inserts multiple rows. The redundant `transfer_jobs_trace_id_idx` was dropped in migration `0004`; the composite `UNIQUE(trace_id, dst)` index covers the lookup. `MaxAttempts` defaults to 5; `Payload` empty → `{}`. The `transfer_events` `enqueue` row is only written on the actual-insert path (not on conflict re-enqueue).
- `Enqueue` runs everything in one tx with `defer tx.Rollback`; both the success and conflict paths must `Commit` explicitly before returning — do not add an early return that skips the commit.

## Worker Pipeline (internal/worker)

The worker package owns the full lease→process→terminal job lifecycle. `Runner` drains `transfer_jobs` with N goroutines; each `loop` leases one row at a time via `LeaseJob` (`FOR UPDATE SKIP LOCKED`), then hands it to `processOne`, which wraps `ProcessJob` in a per-iteration panic recover. `ProcessJob` streams Source→Transport, verifies byte counts, and commits exactly one terminal outcome. All three terminal writers are single writable-CTE `pool.Exec` statements that carry the `status='leased' AND locked_by=$N` lease guard so a swept/re-leased row is a silent no-op (#53).

### Key files & symbols
- `internal/worker/job.go:Job` — snapshot of a `transfer_jobs` row at lease time (ID, TraceID, Src/Dst, SrcProtocol/DstProtocol, Payload, Status, Attempts, MaxAttempts, LockedAt/LockedBy, NextRunAt, Created/UpdatedAt).
- `internal/worker/job.go:LeaseJob` — one-statement CTE: `WITH next AS (SELECT id ... WHERE status='pending' AND next_run_at <= NOW() ORDER BY next_run_at, id FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE ... SET status='leased', locked_by=$1 ... RETURNING <full row>`; returns `(nil, nil)` on empty queue (maps `pgx.ErrNoRows`).
- `internal/worker/job.go:Job.Duration` — worker-side "lease→now" elapsed; 0 when `LockedAt==nil` (in-DB lease age lives in `imgsync_lease_lock_age_seconds`).
- `internal/worker/runner.go:Runner` — struct of `Pool/Workers/PodName/IdleBackoff`, `SourceFor`/`TransportFor` factories, and hooks `OnFinish func(*Job, string)`, `OnRetry func(*Job, string)`, `OnLeaseAttempt func(bool)`, `OnWorkerStart/OnWorkerStop func(string)`.
- `internal/worker/runner.go:Runner.loop` — per-goroutine drain loop; `lockedBy = fmt.Sprintf("%s-w%d", PodName, idx)`; defers `drainLease`. `Run` defaults `Workers→4`, `PodName→"imgsync-worker"`.
- `internal/worker/runner.go:Runner.processOne` — dispatches one job; recovers panics; routes factory errors to `dead`.
- `internal/worker/runner.go:Runner.drainLease` — best-effort SIGTERM requeue of this worker's leased rows (#21).
- `internal/worker/process.go:ProcessJob` — `(result string, error)` core; result ∈ {succeeded, skipped, dead, fail}.
- `internal/worker/process.go:classifyAndWrite` — maps job error → terminal label via `errors.Is` on `transfer.ErrSkippable`/`transfer.ErrPermanent`, else retry. (Signature discards its trailing `start time.Time` param: `_ time.Time`.)
- `internal/worker/process.go:writeSuccess` / `writeRetryOrDead` / `writeTerminalWithAttempts` (+ `writeTerminal` wrapper) — the writable-CTE terminal writers.
- `internal/worker/errdetail.go:retryBackoff` — exponential `1<<attempts` seconds with ±25% jitter (#55).
- `internal/worker/errdetail.go:sanitizeErrMsg` — scrubs URL userinfo creds + caps detail at `maxDetailLen=1024`.

### How it works / flow
1. `loop` calls `LeaseJob` → fires `OnLeaseAttempt(true/false)` and wakes (`IdleBackoff.WakeAll`) / sleeps (`IdleBackoff.WaitOnce`). A transient DB lease error and an empty queue currently share the same `OnLeaseAttempt(false)` + backoff path (see `TODO(F2)`). On a leased job it calls `processOne` (not `ProcessJob` directly).
2. `processOne` resolves `SourceFor(job.SrcProtocol)` / `TransportFor(job.DstProtocol)`; a factory error writes a terminal `dead` row (`stage` = `source-factory`/`transport-factory`, `bumpAttempts=true`) and fires `OnFinish(job, "dead")` via `r.fire`.
3. It then calls `ProcessJob` with `Deps{Pool, LockedBy, Source, Transport, OnRetry}` and fires `OnFinish` with the returned `result`. **`ProcessJob` returns `(result, err)`; the err is discarded here** (`result, _ :=`) — a DB-write failure surfaces only as `result==""` (which the metrics layer's `OnJobFinished` then coerces to the label `"unknown"`).
4. `ProcessJob`: `Source.Open` → on error `classifyAndWrite`. Else stream through a `counter` into `Transport.Send`. Size verification: when `srcSize>=0` requires `written==srcSize`; when `srcSize<0` requires `cw.n==written`; mismatch wraps `transfer.ErrPermanent` → `dead`. A post-send `body.Close()` error is treated as a retryable transport-class failure (`stage="source_close"`). On full success `writeSuccess` commits `status='succeeded'` + a `success` event and returns `"succeeded"`.
5. `classifyAndWrite`: `ErrSkippable`→`writeTerminal("skipped","skip", bumpAttempts=false)`→`"skipped"`; `ErrPermanent`→`writeTerminal("dead","dead", bumpAttempts=true)`→`"dead"`; default→`writeRetryOrDead`.
6. `writeRetryOrDead`: `nextAttempts = Attempts+1`; if `nextAttempts >= MaxAttempts` it caps to a terminal `dead` (`writeTerminalWithAttempts`, label `"dead"`). Otherwise it sets `status='pending', attempts=$2, next_run_at=NOW()+$3::INTERVAL` with `$3 = "<retryBackoff ms> milliseconds"`, inserts a `fail` event, and returns label `"fail"` (note: DB status is `pending` but the metric label is `fail`). It checks `ct.RowsAffected()==0` (the INSERT count) → silent no-op + skips `OnRetry`. Only on a real retry does it call `d.OnRetry(detail["stage"])`.
7. All three terminal writers use the same shape: `WITH u AS (UPDATE transfer_jobs SET ... WHERE id=$1 AND status='leased' AND locked_by=$N RETURNING trace_id) INSERT INTO transfer_events (trace_id, job_id, status, detail) SELECT ... FROM u`. If the lease was lost, the UPDATE matches 0 rows, the CTE is empty, and no event is inserted (silent no-op). Each writer also clears `locked_at=NULL, locked_by=NULL` and bumps `updated_at=NOW()`.
8. Panic path (`processOne` defer): logs `debug.Stack()`, builds a **fresh** `context.Background()` with `drainTimeout` (5s), and routes through `writeRetryOrDead` with `stage="panic"` so `attempts` advances — a poison job eventually caps to `dead` instead of re-panicking forever — then fires `OnFinish` (#23).
9. Shutdown: `loop` defers `drainLease(lockedBy)`, which on a fresh 5s context runs `UPDATE transfer_jobs SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW() WHERE status='leased' AND locked_by=$1` so this worker's in-flight row reschedules immediately rather than waiting for the ~5min sweeper.

### Agent notes (gotchas, conventions, constraints)
- **`ProcessJob` signature is `(string, error)`** — the string is the *metric* result label (succeeded/skipped/dead/fail), NOT the DB status. The retry path returns `"fail"` while writing DB `status='pending'`. Don't conflate the two when wiring metrics. An empty `""` result (DB write failed, or panic-path no-op) is coerced to label `"unknown"` by `metrics.OnJobFinished`.
- **All terminal writes are guarded by `status='leased' AND locked_by=$N`** (`$2` in `writeSuccess`, `$4` in `writeRetryOrDead`/`writeTerminalWithAttempts`). A 0-row UPDATE is intentional and silent (lease lost to the sweeper). Never drop or weaken that WHERE clause; never switch the writers back to plain UPDATE-then-separate-INSERT (the single CTE keeps event and status atomic) (#53).
- **`writeRetryOrDead` keys off `ct.RowsAffected()` = the INSERT row count from the CTE**, not the UPDATE. Empty CTE → 0 → no-op and `OnRetry` is skipped. If you ever add rows the INSERT may emit unconditionally, this no-op detection breaks.
- **Poison-job cap is `nextAttempts >= MaxAttempts`** (`>=`, not `>`). The panic recover path deliberately routes through `writeRetryOrDead` so panics also count toward this cap.
- **Panic / drain writes use a fresh `context.Background()` + `drainTimeout` (5s)**, because the loop ctx is already cancelled at that point. Don't pass the cancelled loop ctx into those writes.
- **Two hooks fire on different events:** `OnFinish(job, result)` fires on every terminal outcome (incl. factory-error `dead` and panic); `OnRetry(job, stage)` fires only on a successful reschedule (DB `pending`, attempts not exhausted, lease not lost). Both are nil-safe via `r.fire` / the `OnRetry` closure.
- `lockedBy` identity is `fmt.Sprintf("%s-w%d", PodName, idx)` — the lease guard and `drainLease` both depend on this exact string; changing the format changes which rows a worker can finalize/drain.
- **Error text is sanitized before persistence:** open/transport detail strings go through `sanitizeErrMsg` (strips `scheme://user:pass@` via `credRe`, caps at `maxDetailLen=1024`). Add new persisted error strings through it too.
- `retryBackoff` uses a package-global mutex-guarded RNG (`backoffMu`/`backoffRNG`, seeded once from `time.Now().UnixNano()`); nominal is `1<<attempts` seconds (so `nextAttempts` 1/2/3 → 2/4/8s) with ±25% jitter (#55). The `process.go:154` comment "~2,4,8,16,32... seconds" is the nominal series.
- Factory errors (`SourceFor`/`TransportFor`) are non-retryable `dead` with `bumpAttempts=true` and are handled in `processOne`, *before* `ProcessJob` — they never hit `classifyAndWrite`.
- `body.Close()` is guarded by a `closed bool` so it runs exactly once; a post-send close error is a *retryable* (default/transport-class) failure, not permanent.
- **The worker is protocol-agnostic about classification:** it only branches on the `transfer.ErrSkippable`/`transfer.ErrPermanent` sentinels via `errors.Is`. The decision of which underlying failure maps to which sentinel lives in the Source/Transport, not here — e.g. FTP `source.go:isNotFound` maps a 550 reply code to `ErrSkippable`, but carves out 550s whose message contains `permission`/`access denied` so they fall through to the default retry-then-dead path. Don't add protocol-specific 550/ErrNotExist logic into the worker package.

## Sources & Transports

Concrete I/O adapters implementing the `transfer.Source` (`Open(ctx, src) (body io.ReadCloser, size int64, err error)`) and `transfer.Transport` (`Send(ctx, dst, body, expectedSize) (writtenBytes int64, sha256Hex string, err error)`) contracts (`internal/transfer/interfaces.go`). Two backends each: LocalFS (reference/tests) and FTP (production). The shared discipline is error *classification*: every adapter wraps backend errors in `transfer.ErrSkippable` (missing source → worker marks job `skipped`, terminal/audit-only) or `transfer.ErrPermanent` (operator misconfig → worker marks job `dead`, no retry budget burn), or leaves them bare (retryable with backoff). Both sentinels live in `internal/transfer/errors.go`. FTP source and transport both draw their connections from a single per-host `*ftp.Pool` (`pool.go`).

### Key files & symbols
- `internal/sources/localfs/source.go:Source.Open` — ctx check, `os.Stat`+`os.Open`; `os.ErrNotExist`→`ErrSkippable`, other stat errors bare, dir→`ErrPermanent`.
- `internal/sources/ftp/source.go:Source.Open` — pooled `FileSize`(best-effort)+`Retr`; returns `*retrReader` releasing the conn on Close.
- `internal/sources/ftp/source.go:isNotFound` — 550-with-permission-carve-out classifier; exported for tests via `IsNotFoundForTest`.
- `internal/sources/ftp/source.go:retrReader` — wraps the `Retr` stream; tracks `ioErr` + a `released` guard so Close releases the conn (as broken on read/close failure) exactly once.
- `internal/transports/localfs/transport.go:Transport.Send` — `CreateTemp`+fsync+atomic rename writer; missing-parent → `ErrPermanent` (classification noted below).
- `internal/transports/ftp/transport.go:Transport.Send` — STOR-to-tmp + RNFR/RNTO rename; ctx-wrapped body; `classify`'d errors.
- `internal/transports/ftp/transport.go:classify` — maps permanent FTP reply codes on STOR/Rename to `ErrPermanent`.
- `internal/transports/ftp/pool.go:Pool` — per-host pool; `Acquire`/`release`, idle TTL, NOOP-after ping, waiter queue, `OnPoolChange` metrics hook.

### How it works / flow
**FTP transport `Send`** (`transport.go`): parses `dst` (must be `ftp://host/path` — failure returns a bare `"invalid dst"` error, *not* `ErrPermanent`), `Acquire`s a pooled conn, then a best-effort single-level `MakeDir` on `path.Dir(finalPath)` (skipped when dir is `/`, `.`, or empty; no recursive walk — operators provision dirs out-of-band; the error is ignored), then `Stor`s to `finalPath+".imgsync.tmp"`. The body is wrapped `io.TeeReader(transfer.NewCtxReader(ctx, body), cw)` — `cw` is a `countingHashWriter{h: sha256.New()}` computing sha256 + byte count. The `NewCtxReader` wrap (#49) is load-bearing: jlaffaye's `Stor` runs `io.Copy(dataConn, body)` with *no* ctx, so a cancelled ctx can only abort the in-flight STOR by making `Read` return `ctx.Err()`. On STOR success it `Rename`s tmp→final. Any STOR/Rename error: best-effort `Delete` of the tmp, `pc.Release(true)` (broken), and the error is passed through `classify`. Returns `(cw.n, sha256hex, nil)` on success (`pc.Release(false)`).

**`classify`** (#49): nil-safe early return; `errors.As` to `*textproto.Error`; if the code is `ftp.StatusFileUnavailable` (550), `ftp.StatusExceededStorage` (552), or `ftp.StatusBadFileName` (553) it wraps `fmt.Errorf("%w: %w", err, transfer.ErrPermanent)`. Everything else — transient 4xx, dial/conn failures, non-protocol errors — returns unchanged (retryable). Matching is strictly on the reply code, never substrings.

**FTP source `Open`**: validates scheme (`ftp`), host, and path (each empty/mismatch → `ErrPermanent`; a `url.Parse` failure also → `ErrPermanent`), `Acquire`s, calls `FileSize` (best-effort; `size` stays `-1` on error), then `Retr`. On `Retr` error: `pc.Release(true)`, and if `isNotFound(err)` → `ErrSkippable`, else bare error (retry). Success wraps the stream in `&retrReader{ReadCloser: r, pc: pc}`; `retrReader.Read` records any non-EOF error in `ioErr`, and `Close` (guarded by `released`) calls `pc.Release(broken)` where `broken = ioErr != nil || closeErr != nil`.

**`isNotFound`** (#55): `errors.As` to `*textproto.Error`; returns false unless `te.Code == ftp.StatusFileUnavailable` (550). Then it lowercases `te.Msg` and returns `!strings.Contains(msg, "permission") && !strings.Contains(msg, "access denied")`. So a 550 is "missing source → skip" *except* when the message mentions permission/access-denied — those are operator misconfigs that must surface (retry→dead), and message text is the only signal a generic 550 gives to tell them apart. (#55 reworked this to be code-first with a message carve-out, replacing earlier substring-only matching.)

**LocalFS source `Open`**: ctx check, `os.Stat` (`os.ErrNotExist`→`ErrSkippable`; other stat errors bare), reject dirs as `ErrPermanent`, then `os.Open` (its error returned bare); returns the `*os.File` and `st.Size()`. Caller owns Close.

**Pool** (`pool.go`): `Acquire` LIFO-pops idle conns, discarding any whose `enqueue` is older than `IdleTTL` and NOOP-pinging any whose `lastUse` is older than `NoopAfter`; a failed ping `Quit`s, decrements `inUse`, wakes one waiter, and re-loops. Below `MaxPerHost` it dials (`DialWithTimeout`+`DialWithContext` via `ftp.Dial`, then `Login`); at cap it parks on a per-host waiter channel until `release` or ctx cancel (with best-effort waiter eviction on cancel). `release` decrements `inUse`, `Quit`s if `broken || p.closed` else appends a fresh `idleEntry`, and wakes one waiter. Defaults (applied in `NewPool` when ≤0): `MaxPerHost=4`, `IdleTTL=5m`, `NoopAfter=60s`, `DialTimeout=10s`. `OnPoolChange(host, inUse, idle)` fires on every count change (metrics hook; must be O(1); nil-safe). `Close` Quits all idle conns and sets `closed`; in-use conns close on their next `Release`.

### Agent notes (gotchas, conventions, constraints)
- **Classify on reply code, never substrings** — the one carve-out (`isNotFound` permission/access-denied) is the *only* place message text is matched, and only because a generic 550 cannot otherwise distinguish missing-file from permission-denied. Don't add substring matching elsewhere.
- **550 means different things in the two files**: in `classify` (transport, write path) 550 → `ErrPermanent` unconditionally; in `isNotFound` (source, read path) 550 → skippable *unless* permission/access-denied. They are deliberately asymmetric — a 550 on STOR is a hard write failure, a 550 on RETR is usually a missing file.
- **Status constants come from `github.com/jlaffaye/ftp`**: `StatusFileUnavailable`=550, `StatusExceededStorage`=552, `StatusBadFileName`=553. Use these, not literals.
- **`transfer.NewCtxReader` wrap is mandatory** on both transports' bodies (FTP `TeeReader`, LocalFS `io.Copy` source) (#49). Removing it reintroduces the hang: jlaffaye `Stor` does ctx-less `io.Copy`, so cancellation only propagates through `Read`. Do not unwrap "to simplify."
- **LocalFS transport does NOT `MkdirAll`**: a missing parent dir surfaces as `os.ErrNotExist` from `CreateTemp` and is classified `ErrPermanent`. The FTP transport's `MakeDir` is single-level best-effort only (ignored error) — neither adapter creates multi-level parent trees; operators provision target dirs.
- **`Send`'s `expectedSize`/`size` arg is ignored by both transport impls** (`_ int64`) — they count bytes actually written and return that, not the source's hint. The worker reconciles them for D6 size verification.
- **Pool `OnPoolChange` is the metrics surface** — `pool.go` was last touched by the phase-1 monitoring work (#14) which added this hook. It is fired *outside* the mutex on every in_use/idle delta. Keep the callback O(1); it runs on the Acquire/Release hot path.
- **Broken-conn release discipline**: any STOR/Rename/RETR error path must `Release(true)` so the poisoned conn is `Quit`'d, not returned to idle. `retrReader.Close` derives `broken` from accumulated `ioErr`/`closeErr` and is guarded by `released` (single release) — preserve that, or you'll leak half-read conns back into the pool.
- **`retrReader.Read` ignores `io.EOF`** when setting `ioErr` (EOF is clean). Don't treat EOF as a broken stream.
- FTP paths are sent with leading `/` trimmed (`strings.TrimPrefix(..., "/")`) for STOR/Rename/Delete, but `MakeDir` (`path.Dir(finalPath)`) and the source `Retr`/`FileSize` use the raw `u.Path`. Keep this consistent if editing path handling.

## Sniffer / DB Connector (`internal/sniffer`)

Polls an external source DB and incrementally enqueues `transfer_jobs` rows using a per-source `(ts, pk)` high-watermark. Each poll loads the watermark, fetches a bounded ascending batch newer than it, renders src/dst paths per row, batch-inserts the renderable rows (dedup via `ON CONFLICT (trace_id, dst) DO NOTHING`), then advances the watermark to the last *fetched* row. The crash-safety contract: deterministic render failures (poison rows) are skipped and the watermark advances past them, while transient enqueue/DB errors early-return and preserve the old watermark so the whole batch is retried.

### Key files & symbols
- `internal/sniffer/sniffer.go:Config` — per-instance params (pools, `Query`, `Dst`, `SrcPattern`, `SrcProtocol`/`DstProtocol`) plus optional `OnEnqueue(source string, n int)` / `OnError(source string)` callbacks wired to metrics by the CLI.
- `internal/sniffer/sniffer.go:Sniffer` / `New` — composes `StateRepo`, `Enqueuer`, and a `DstTemplate` reused as the src-pattern renderer (`src` field, built from `cfg.SrcPattern`).
- `internal/sniffer/sniffer.go:RunOnce` / `runOnceImpl` — one poll iteration, both return `(int, error)`. `RunOnce` is the public entry that fires `OnEnqueue`/`OnError`; `runOnceImpl` holds the load→fetch→render→batch-enqueue→advance logic.
- `internal/sniffer/query.go:Query` / `Fetch` — windowed source SELECT; carries `BatchSize` (LIMIT), `BiasDuration`, and `QueryTimeout`.
- `internal/sniffer/enqueue.go:Enqueuer` — `Enqueue` (single-row, returns `(bool, error)`; used only by tests) and `EnqueueBatch` (one multi-row INSERT, returns `(int, error)`) over `transfer_jobs`.
- `internal/sniffer/enqueue.go:JobSpec` — `{TraceID, Src, Dst, SrcProtocol, DstProtocol}`; all required.
- `internal/sniffer/state.go:StateRepo` (`Load`/`Upsert`) + `State` — watermark persistence in `sniffer_state`.
- `internal/sniffer/traceid.go:TraceID` — `"<table>-<pk>"`; `DstTemplate.Render` — `text/template` with `Option("missingkey=error")` and optional `Shadow` bool that appends the exported `ShadowSuffix` const (`.imgsync_shadow_v1`).

### How it works / flow
1. `runOnceImpl` calls `state.Load(ctx, SourceID)` → `State{LastRunTS, LastRunPK}`. A missing row returns zero-TS / `LastRunPK==""` with nil error (first-run sentinel); `Load` populates `SourceID` even on miss so the value is safe to pass straight to `Upsert`.
2. `Query.Fetch` builds the windowed SQL. If `LastRunPK==""` it filters ts-only (`WHERE ts > $1`); otherwise it uses the expanded `WHERE (ts > $1 OR (ts = $1 AND pk > $2))` so Postgres compares the PK in its native type (avoids the 9→10 text-sort bug). Both add a bias clause `ts <= NOW() - ($n::INT || ' seconds')::INTERVAL`, `ORDER BY ts, pk`, `LIMIT BatchSize`. `Fetch` errors out early if `BatchSize <= 0`. **#30:** when `QueryTimeout > 0`, `Fetch` wraps `ctx` in `context.WithTimeout` so a hung source query is bounded even though the loop ctx (`signal.NotifyContext`) is deadline-less; `<= 0` disables it. Empty result → early `return nil, nil` (no watermark write).
3. **Render loop (#29):** every row is rendered up front. A `Dst.Render` or `src.Render` error is treated as a *deterministic poison row* — logged (`skipping un-renderable row pk=...`) and `continue`d, NOT counted, NOT returned as an error. Renderable rows accumulate into a `[]JobSpec` with `TraceID(Query.Table, r.PK)`.
4. **#31:** `enq.EnqueueBatch(ctx, specs)` issues a single multi-row `INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol) VALUES (...),(...) ON CONFLICT (trace_id, dst) DO NOTHING` and returns `tag.RowsAffected()` (newly-inserted count; UNIQUE conflicts count as 0). Empty/nil specs is a no-op returning `(0, nil)` with no query. A batch error → `return 0, fmt.Errorf("enqueue batch: ...")` *before* the watermark is touched (preserves it for whole-batch retry).
5. **Watermark advance:** only after a clean batch, `state.Upsert` writes `LastRunTS`/`LastRunPK` from `rows[len(rows)-1]` — the last *fetched* row, deliberately not the last *enqueued* one, so the watermark moves past a trailing poison row too. `LastRunPK==""` stores as SQL NULL.
6. CLI wiring (`internal/cli/sniffer.go`): `QueryTimeout: srcPool.QueryTimeout`, `OnEnqueue: m.OnSnifferEnqueue`, `OnError: m.OnSnifferError`; the loop additionally calls `m.OnSnifferRun(SourceID)` after each successful `RunOnce` (both the immediate first run and every tick). Metrics emitted: `imgsync_sniffer_enqueue_total{source}` (adds `n`), `imgsync_sniffer_run_errors_total{source}`, `imgsync_sniffer_last_run_timestamp{source}`, and the derived `imgsync_sniffer_watermark_lag_seconds{source}` (from `sniffer_lag.go`, computed at scrape time as `NOW()` minus the per-source last-run wall-clock recorded by `OnSnifferRun`).

### Agent notes (gotchas, conventions, constraints)
- **The skip/transient boundary is the core invariant.** Only `Dst.Render`/`src.Render` failures are skippable (deterministic — same row fails identically every retry under `missingkey=error`). Everything else (`state.Load`, `Query.Fetch`, `EnqueueBatch`, `state.Upsert`) returns an error that early-exits *without* advancing the watermark. Do not "helpfully" wrap render in retry logic or make Fetch/enqueue errors skippable — that breaks crash-safety or re-introduces the forever-stall #29 fixed. Regression tests: `poison_test.go` (`TestRunOnce_PoisonRowSkippedBatchContinues`, `TestRunOnce_PoisonRowDoesNotStallNextPoll`).
- **Watermark = last fetched row, NOT last enqueued.** `rows[len(rows)-1]` is intentional. Changing it to the last spec would re-pin the watermark whenever the final fetched row is poison, recreating the stall.
- **Inserted-count semantics must match per-row `Enqueue`.** `EnqueueBatch` returns *newly-inserted* rows (`RowsAffected`), not `len(specs)`. `RunOnce`'s return value and `OnEnqueue`'s `n` (→ `imgsync_sniffer_enqueue_total`) both depend on this; conflicts must count as 0. Pinned by `batch_enqueue_test.go:TestEnqueueBatch_PreservesInsertedCountSemantics` (plus `TestEnqueueBatch_InsertsAllRowsInOneCall` and `TestEnqueueBatch_EmptyIsNoop`).
- **`ON CONFLICT (trace_id, dst)` must match the schema constraint** `transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)` (`migrations/0001_initial.up.sql`). The standalone `transfer_jobs_trace_id_idx` was dropped in migration `0004` (issue #34 — it is a leading-column prefix of that UNIQUE index, so the only production lookup `WHERE trace_id=$1 AND dst=$2` is already fully covered) — do not rely on it.
- **`EnqueueBatch` builds the VALUES list with manual `$n` placeholders** (`strconv.Itoa`, 5 cols/row, via a `strings.Builder`). One poll = one round-trip; a very large `BatchSize` means a very wide statement (Postgres caps params at 65535 → ~13k rows/batch). Keep this in mind before raising `BatchSize` (CLI default `SNIFFER_BATCH_SIZE`=500).
- **`LastRunPK==""` is both first-run AND watermark-reset sentinel.** It selects the ts-only predicate and skips tie-break. Safe only because v1 source PKs are BIGINT; a TEXT PK that can be the empty string would alias to the sentinel (documented caveat in `Fetch`).
- **`LastRunTS` is TIMESTAMPTZ (microsecond)** — Go nanoseconds truncate on round-trip; never `==`-compare a TS that flowed through `Load`/`Upsert`.
- **`QueryTimeout` is fed from `srcPool.QueryTimeout`** in the CLI (source pool config; `sourcedb.Config.QueryTimeoutMs` is hardcoded to 30000 at pool construction, so `Fetch`'s internal deadline is effectively 30s in v1). The deadline-less loop ctx is the whole reason `Fetch`'s internal timeout exists; don't remove the `WithTimeout` assuming the caller deadline covers it. Regression: `query_timeout_test.go:TestQuery_QueryTimeoutBoundsSlowQuery`.
- **v1 assumes a single sniffer pod.** `sniffer_state` Upsert is atomic but last-writer-wins; no advisory lock (`migrations/0002` `COMMENT ON TABLE sniffer_state`: "v1 single sniffer pod, no advisory lock"). Don't add concurrent-pod assumptions without revisiting watermark contention.

## Sweeper, Eval & Retention

Three small, single-responsibility packages that keep the queue healthy without operator intervention. **`internal/sweeper`** reclaims dead leases back to `pending`. **`internal/eval`** is the cross-cutting black-box invariant suite (testcontainers-backed) that pins the queue's hardest behavioral guarantees. **`internal/retention`** is an opt-in, batched garbage-collector for terminal `transfer_jobs` rows. Sweeper and retention are structurally near-identical (`Sweep`/`Run`, advisory-xact-lock guarded, per-cycle timeout) but use **distinct** advisory keys and opposite default postures (sweeper always-on, retention disabled-by-default).

### Key files & symbols
- `internal/sweeper/sweeper.go:Sweep` — one transaction: try-lock, `UPDATE transfer_jobs … leased→pending` past `Threshold`, insert one `expire` event per recovered row, commit. Returns `(int, error)` = rows recovered.
- `internal/sweeper/sweeper.go:Run` — ticker loop (default 30s), per-cycle `2*Interval` timeout, panic-recover, calls `cfg.OnCycle()` on success.
- `internal/sweeper/sweeper.go:Config` — `Threshold` (default 5m), `Interval` (default 30s), `OnCycle func()`.
- `internal/sweeper/sweeper.go:sweeperLockKey` — `"imgsync_sweeper"`.
- `internal/retention/retention.go:Sweep` — try-lock, looped batched `DELETE FROM transfer_jobs WHERE id IN (SELECT id … WHERE status IN ('succeeded','skipped','dead') AND updated_at < NOW()-Window LIMIT BatchSize)`; events cascade via FK; returns total deleted.
- `internal/retention/retention.go:Run` — ticker loop (default 1h), per-cycle `2*Interval` timeout, panic-recover, calls `cfg.OnCycle(deleted)`.
- `internal/retention/retention.go:Config` — `Window` (`<=0` disables), `BatchSize` (default 1000), `Interval` (default 1h), `OnCycle func(deleted int)`.
- `internal/retention/retention.go:retentionLockKey` — `"imgsync_retention"`.
- `internal/eval/audit_invariants_test.go` — `TestC0_SizeUnknownMismatch_TransitionsToDead`, `TestC3_SkippedJob_ExactlyOneSkipEventWithReason`.
- `internal/eval/rss_contract_test.go` — `TestC1_LocalFS_StreamingRSSUnder250MB`, `TestC1_FTP_StreamingRSSUnder250MB`.
- `internal/eval/sweeper_audit_test.go` — `TestC2_SweeperRecoveredJob_HasAttemptsZero` (also defines the shared `mustDB(t)` testcontainers helper).
- `internal/eval/fixture_suite_test.go` — `TestC6_FixtureSuite` (multi-scenario fixture incl. F3 scoped-audit cross-check).
- `internal/retention/retention_test.go` — `TestSweep_DeletesOldTerminalRows_CascadesEvents`, `_PreservesRecentTerminalRows`, `_PreservesNonTerminalRows`, `_DisabledByDefault`, `_BatchesAcrossMultipleLoops`, `_AdvisoryLock_OnlyOneRetentionRunsAtATime`, `TestRun_LoopsUntilContextCancelled`.

### How it works / flow
**Sweeper:** `Sweep` opens a tx, runs `SELECT pg_try_advisory_xact_lock(hashtext('imgsync_sweeper'))`; if not acquired it returns `(0, nil)` (another pod holds it). It then `UPDATE transfer_jobs SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW() WHERE status='leased' AND locked_at < NOW() - $1::INTERVAL RETURNING id, trace_id` (the threshold is passed as a `"<n> seconds"` interval string, not inlined), collects rows, **checks `rows.Err()`** before acting (a deliberate guard so a mid-stream pgx failure can't silently commit a partial recovery — same pattern as `internal/db/migrate.go`), then inserts one `transfer_events(status='expire', detail='{"reason":"lease_expired"}'::JSONB)` (carrying the recovered row's `trace_id` and `job_id`) per recovered job, and commits. Recovered jobs return to the queue with their lease cleared. `Run` is wired in `cmd/imgsync/worker.go` with hardcoded `Threshold: 5m`, `Interval: 30s`, and `OnCycle` chaining `status.OnSweepCycle()` (feeds `/healthz` `last_sweep_ts`) + `m.OnSweepCycle()` (increments `imgsync_sweep_cycles_total`).

**Retention:** `Sweep` returns `(0,nil)` immediately if `Window<=0` (disabled). Otherwise it opens a tx, takes `pg_try_advisory_xact_lock(hashtext('imgsync_retention'))`, then loops `DELETE FROM transfer_jobs WHERE id IN (SELECT id … WHERE status IN ('succeeded','skipped','dead') AND updated_at < NOW()-$1::INTERVAL LIMIT $2)` until a batch returns fewer than `BatchSize` rows, accumulating `RowsAffected()`, and commits once. The matching `transfer_events` rows are removed by the FK `job_id BIGINT NOT NULL REFERENCES transfer_jobs(id) ON DELETE CASCADE` (`migrations/0001_initial.up.sql:47`) — retention never touches `transfer_events` directly, and never matches `pending`/`leased` rows. In `worker.go`, retention is OPT-IN: `retentionDays := env.Int("IMGSYNC_RETENTION_DAYS", 0)` and the goroutine only starts when `retentionDays > 0`. Config maps `Window = retentionDays*24h`, `BatchSize = env IMGSYNC_RETENTION_BATCH (default 1000)`, `Interval = env IMGSYNC_RETENTION_INTERVAL_SEC (default 3600)s`, `OnCycle = m.OnRetention` (adds to `imgsync_retention_rows_deleted_total`).

**Eval suite:** package `eval_test`, spins a real `postgres:16-alpine` via `testcontainers-go/modules/postgres`, applies `../../migrations`, and exercises the worker/transfer/sweeper end-to-end through `jobs.Enqueue` + `worker.LeaseJob`/`worker.ProcessJob` (which returns `(string, error)`). Invariants in play: **C0** size-unknown (`srcSize=-1`) with `bytesRead != writtenBytes` → `transfer.ErrPermanent` → `dead`; **C1** LocalFS+FTP streaming peak `runtime.MemStats.HeapInuse` stays `<= 250<<20` (250 MiB), sampled every 100ms (skipped under `-short`); **C2** a sweeper-recovered then succeeded job ends with `attempts==0`; **C3** a skipped job is `status='skipped'` with `attempts==0` and exactly one `skip` event whose `detail.reason` is non-empty, and a duplicate `(trace_id, dst)` re-enqueue is a no-op (`inserted==false`) leaving exactly 2 total event rows (`enqueue`+`skip`); **C6** the consolidated fixture suite (covers a 5-row sweeper-recovered scenario cross-checking the C2 `attempts==0` invariant via the `enqueue/expire/success` event trail, plus the F3 scoped-audit using `LEFT JOIN transfer_events e ON j.id = e.job_id`). There are no C4/C5 test functions in the tree — the implemented set is C0, C1, C2, C3, C6.

### Agent notes (gotchas, conventions, constraints)
- **Four distinct advisory keys, one global namespace.** `imgsync_sweeper` (sweeper, xact, `hashtext`), `imgsync_retention` (retention, xact, `hashtext`), the migration lock `migrationAdvisoryLockID` (`internal/db/migrate.go`, a fixed `int64 0x494d4753594e43` taken via session-scoped `pg_advisory_lock`), and per-host hostcap slot keys via `slotKey(host,slot)` → `fmt.Sprintf("ftp_host_%s_%d", host, slot)` (`internal/hostcap/hostcap.go`, session-scoped `pg_advisory_lock`/`pg_try_advisory_lock` over `hashtext`). Adding any new advisory lock MUST pick a fresh string/id — collision would silently serialize unrelated subsystems. The retention doc-comment explicitly calls this out.
- **Retention is conservative-by-default and must stay that way.** `Window<=0` ⇒ no-op; the only statuses it deletes are `succeeded`/`skipped`/`dead`; it filters on `updated_at`, not `created_at`. `TestSweep_DisabledByDefault`, `TestSweep_PreservesNonTerminalRows`, and `TestSweep_PreservesRecentTerminalRows` enforce this — do not loosen the status set or the `Window<=0` short-circuit.
- **Never `DELETE FROM transfer_events` in retention.** Cascade is the contract; `TestSweep_DeletesOldTerminalRows_CascadesEvents` asserts event rows vanish purely via FK. A manual delete would be both redundant and a divergence risk.
- **Sweeper's `rows.Err()` check is load-bearing, not boilerplate** — it prevents committing a partial lease-recovery on a mid-iteration pgx error. Keep it if you refactor the `UPDATE … RETURNING` loop.
- **Both `Run` loops use `cycleTimeout = 2*Interval`** to bound a wedged pgx connection holding the xact lock (else every pod's sweeper/retention jams). They disambiguate parent-ctx cancel (return) from per-cycle deadline (log + continue). Preserve this split.
- **Sweeper config is hardcoded** in `worker.go` (`5m`/`30s`), unlike retention which is fully env-driven (`IMGSYNC_RETENTION_DAYS`, `IMGSYNC_RETENTION_BATCH`, `IMGSYNC_RETENTION_INTERVAL_SEC`). Don't assume parallel env knobs exist for the sweeper.
- **`OnCycle` signatures differ:** sweeper `func()`, retention `func(deleted int)`. Metric methods are `Metrics.OnSweepCycle()` and `Metrics.OnRetention(deleted int)` in `internal/metrics/metrics.go` (backed by the `sweepCycles`/`retentionRows` plain Counters, no labels).
- **The C6 audit query joins on `j.id = e.job_id`, never `USING (trace_id)`** — the F3 fix. Since a single `trace_id` can fan out to multiple `dst` jobs, a `trace_id`-only join cross-contaminates events across destinations; the fixture's negative case asserts a scoped audit returns only the target dst's rows.
- **Eval is integration-grade** (Docker/testcontainers, real PG) — slow and not unit-runnable offline (C1 also self-skips under `-short`). The invariant set is C0/C1/C2/C3/C6; do not invent C4/C5 references when editing the suite.

Relevant absolute paths: `/Users/nineking/workspace/app/imgsync/internal/sweeper/sweeper.go`, `/Users/nineking/workspace/app/imgsync/internal/retention/retention.go`, `/Users/nineking/workspace/app/imgsync/internal/retention/retention_test.go`, `/Users/nineking/workspace/app/imgsync/internal/eval/{audit_invariants,rss_contract,sweeper_audit,fixture_suite}_test.go`, `/Users/nineking/workspace/app/imgsync/cmd/imgsync/worker.go`, `/Users/nineking/workspace/app/imgsync/migrations/0001_initial.up.sql`, `/Users/nineking/workspace/app/imgsync/internal/metrics/metrics.go`, `/Users/nineking/workspace/app/imgsync/internal/db/migrate.go`, `/Users/nineking/workspace/app/imgsync/internal/hostcap/hostcap.go`, `/Users/nineking/workspace/app/imgsync/internal/worker/process.go`.

## Infra Adapters (db, sourcedb, health, hostcap, backoff, metrics, env, ftpserver)

Supporting glue between the domain packages and Postgres/Prometheus/k8s. These are deliberately thin: connection-pool constructors, the migration runner, the advisory-lock host cap, shared idle backoff, the health/metrics HTTP surface, and a typed env helper. The recent #51–#55 merge batch gave hostcap its own dedicated pool (issue #18), added the `internal/env` helper, gated `/livez` on a recorded lease attempt (issue #36), expanded the metric set (retries, retention rows, oldest-pending, sniffer last-run + watermark lag), and dropped the redundant `transfer_jobs_trace_id_idx` (migration `0004`, issue #34).

### Key files & symbols
- `internal/db/pool.go:NewPool(ctx, PoolConfig)` — builds + pings a `*pgxpool.Pool`; zero `PoolConfig` fields mean "use the pgx default" (only positive values override). Caller owns `Close()`.
- `internal/db/migrate.go:ApplyMigrations(ctx, dsn, dir)` — runs `*.up.sql` in lexical order under a session advisory lock; skips versions already recorded in `schema_migrations`.
- `internal/sourcedb/pool.go:NewPool(ctx, Config)` — read-only source-DB pool; returns a `*sourcedb.Pool` that embeds `*pgxpool.Pool` and carries `QueryTimeout` (from `Config.QueryTimeoutMs`, default 30000ms; `Config.MaxConns` default 4).
- `internal/hostcap/hostcap.go:Wrap` / `CapTransport.Send` / `acquireSlot` — cluster-wide per-host concurrency cap via session-scoped `pg_advisory_lock` pinned to a dedicated connection.
- `internal/backoff/backoff.go:NewIdle` / `Idle.WaitOnce` / `Idle.WakeAll` — per-pod shared idle backoff (50ms→200ms→500ms→1s, ±25% jitter) with herd-free wakeup.
- `internal/health/server.go:NewServer` / `WithLivenessThreshold` / `WithMetrics` / `NewStatus` / `Status.OnLeaseAttempt` / `Status.OnSweepCycle` — `/livez`, `/readyz`, `/healthz` (+ optional `/metrics`).
- `internal/env/env.go:Int` / `Bool` — shared typed env accessors.
- `internal/metrics/metrics.go:New` / `Metrics` — owns every `imgsync_*` collector on a private registry; `Attach*` methods bind scrape-time DB collectors.
- `internal/ftpserver/testserver.go:Start(t *testing.T) *Server` — in-process FTP server for tests only (afero-backed, no TLS).
- `cmd/imgsync/worker.go:newHostcapTransport` — constructs the hostcap-dedicated pool.

### How it works / flow
- **hostcap dedicated pool (#18, #33):** `cmd/imgsync/worker.go:newHostcapTransport` builds its OWN `db.NewPool` with `MaxConns = int32(cap) + 2` (the legacy worker-pool arg is `_`-ignored in the signature) and returns `capPool.Close` for shutdown. `CapTransport.Send` derives the host (from `cfg.Host` or `url.Parse(dst).Host`; errors if neither yields a host), `Acquire`s one dedicated conn, calls `acquireSlot`, runs `inner.Send`, then `pg_advisory_unlock(hashtext($1))`s in a `defer` using `context.Background()` so unlock survives a cancelled ctx. `acquireSlot` probes `cap` slots starting at `slotHash(host,dst)%cap` and wrapping, so an uncontended acquire is one round-trip; under full contention it scans ALL `cap` slots before waiting, preserving the EXACT cluster-wide cap. Backoff is exponential (`nextBackoff`, doubling to `maxBackoff = 2s`) with a hash-seeded `jittered` (±25%, seed `= slotHash(host, dst+"#jitter", cap*7+13)` evolving via the LCG step `seed*1664525+1013904223`) — no wall-clock is read for jitter. Lock key is `fmt.Sprintf("ftp_host_%s_%d", host, slot)` hashed via `hashtext($1)`. Cap default 8, `AcquireBackoff` default 100ms.
- **migrations:** `ApplyMigrations` connects (single `pgx.Connect`), takes session lock `pg_advisory_lock(0x494d4753594e43)` ("IMGSYNC"), reads `schema_migrations` if the table exists, and applies each unapplied `<version>.up.sql` whole (version = filename minus `.up.sql`). Lock auto-releases on disconnect; the migration SQL itself (not this runner) `INSERT`s its row into `schema_migrations`. Current set: `0001_initial`, `0002_sniffer_state`, `0003_jobs_status_index`, `0004_drop_trace_id_index`.
- **health (#36):** `Status` (mutex-guarded, built via `NewStatus()`) records `LastLeaseAttemptTS` / `LastLeaseSuccessTS` / `LastSweepTS` via `OnLeaseAttempt(success bool)` / `OnSweepCycle()`. `/livez` returns 503 only when `liveAfter > 0` AND `LastLeaseAttemptTS` is non-zero AND `time.Since(last) > liveAfter` — so the sniffer (never calls `OnLeaseAttempt`, zero TS) always stays 200. Threshold = `DefaultLivenessThreshold` (10s) unless `WithLivenessThreshold` overrides; worker wires it from `IMGSYNC_LIVENESS_THRESHOLD_SEC` (default 10). `/readyz` pings the pool (2s timeout, writes the error body on failure); `/healthz` returns a JSON snapshot of the three timestamps + pool stat (`pool_in_use`/`pool_idle`/`pool_max`). The server sets `ReadHeaderTimeout: 5s`.
- **metrics:** `New()` registers all vec/scalar collectors plus the `snifferLagCollector`. `Attach*` methods add the four scrape-time DB collectors. Each scrape collector has a 2s timeout and degrades to 0/empty + warn log on error (never panics/blocks).

### Current metric set (verified against `internal/metrics`)
- `imgsync_lease_attempts_total{result}` — counter (`result` ∈ success/empty/error), via `OnLeaseAttempt(success, err)`.
- `imgsync_jobs_processed_total{src,dst,result}` — counter, via `OnJobFinished` (empty labels → `"unknown"`).
- `imgsync_job_retries_total{src,dst,stage}` — counter; `OnRetry`, empty labels → `"unknown"`.
- `imgsync_job_duration_seconds{src,dst,result}` — histogram, buckets `{0.1,0.5,1,2,5,10,30,60,300,1800}` (`defaultDurationBuckets`), observed in the same `OnJobFinished` call.
- `imgsync_sweep_cycles_total` — counter (no labels), `OnSweepCycle`.
- `imgsync_retention_rows_deleted_total` — counter (no labels), `OnRetention(deleted int)`.
- `imgsync_ftp_pool_size{host,state}` — gauge (`state` ∈ in_use/idle), `OnFTPPoolChange`.
- `imgsync_sniffer_enqueue_total{source}` / `imgsync_sniffer_run_errors_total{source}` — counters.
- `imgsync_sniffer_last_run_timestamp{source}` — gauge, Unix secs, set by `OnSnifferRun`.
- `imgsync_sniffer_watermark_lag_seconds{source}` — gauge via `snifferLagCollector`, computed `now - last` at scrape, clamped ≥0.
- `imgsync_workers_active{pod}` — gauge, `SetWorkersActive`.
- `imgsync_jobs_in_status{status}` — gauge, scrape SQL `SELECT status::text, COUNT(*)::bigint ... GROUP BY status` (`AttachQueueDepth`; index `transfer_jobs_status_idx`, migration 0003).
- `imgsync_db_pool_conns{state}` — gauge (in_use/idle/max) from `pool.Stat()` (`AttachDBPool`); in-process, no DB round-trip.
- `imgsync_lease_lock_age_seconds` — gauge (`GaugeFunc`), `MIN(locked_at)` WHERE `status='leased'` (`AttachLeaseLockAge`; index `transfer_jobs_leased_idx`).
- `imgsync_oldest_pending_age_seconds` — gauge (`GaugeFunc`), `MIN(next_run_at)` WHERE `status='pending' AND next_run_at<=NOW()` (`AttachOldestPending`; index `transfer_jobs_pending_idx`).

### Agent notes (gotchas, conventions, constraints)
- hostcap NO LONGER borrows the worker pool. If you change `cap`, the dedicated pool sizing lives in `cmd/imgsync/worker.go:newHostcapTransport` as `int32(cap)+2` — keep it ≥ cap+1 or acquires will starve. The `*pgxpool.Pool` param in that func signature is intentionally `_`-ignored; don't "wire it back in."
- `acquireSlot` correctness invariant: it MUST scan all `cap` slots before sleeping. Do not "optimize" it to break early at the hash offset — that would silently let concurrency exceed the cap. The hash offset is only a starting probe point.
- hostcap jitter is deterministic (hash/LCG seeded), NOT clock-derived — load-bearing for reproducibility/testing. Don't swap in `rand`/`time.Now()`. (Note: the unrelated `internal/backoff` idle jitter DOES use `math/rand` seeded from `time.Now().UnixNano()` — these are two different jitter implementations; don't conflate them.)
- The advisory unlock `defer` uses `context.Background()`, intentionally, so the lock releases even when the request ctx is already cancelled. Don't replace it with the request ctx.
- `/livez` 503 is gated on a NON-ZERO `LastLeaseAttemptTS`. Any new long-running non-worker process sharing this `Status` must not call `OnLeaseAttempt`, or it will start tripping liveness. A non-positive threshold disables the check entirely.
- `env.Bool` is strict: only `"1"` or `"true"` (case-insensitive) are true; every other non-empty value is false. Empty/absent → default. `env.Int` falls back to default on parse error (no error surfaced). All operational tuning flows through these helpers (`IMGSYNC_WORKERS`, `IMGSYNC_FTP_HOST_CAP`, `IMGSYNC_FTP_MAX_PER_HOST`, `IMGSYNC_FTP_IDLE_TTL_SEC`, `IMGSYNC_FTP_NOOP_AFTER_SEC`, `IMGSYNC_LIVENESS_THRESHOLD_SEC`, `IMGSYNC_RETENTION_DAYS`/`_BATCH`/`_INTERVAL_SEC`, etc.).
- `metrics.New()` uses a fresh `prometheus.Registry` per instance (parallel-test safe) — domain packages never import `internal/metrics`; they call `OnXxx`/`SetXxx` callbacks. `Attach*` methods `MustRegister` (panic on duplicate) — call each once per pool.
- Scrape collectors (`queue_depth`, `db_pool`, `lease_lock_age`, `oldest_pending`) compute at scrape time; the three DB-backed ones (`queue_depth`, `lease_lock_age`, `oldest_pending`) hit Postgres with a 2s timeout and return 0/empty on failure (`db_pool` is in-process and never queries). Adding a new one, follow the same fail-soft + warn-log pattern; never let a scrape panic or block.
- `internal/ftpserver` is `testing`-only (`Start(t *testing.T)`, afero `BasePathFs` rooted at `t.TempDir()`, fixed creds `imgsync`/`imgsync`, `GetTLSConfig` deliberately errors); never import it from production code paths.
- `sourcedb.Pool` embeds `*pgxpool.Pool` and adds `QueryTimeout` — callers are expected to apply that timeout themselves per query; the pool does not enforce it.

## CLI & Entry Points (cmd/imgsync)

A single `imgsync` binary built on cobra; `main.go` wires a SIGINT/SIGTERM `signal.NotifyContext` and registers four subcommands: `migrate`, `enqueue`, `worker`, `sniffer`. Each subcommand owns its own env reads, DB pool sizing, and (for the long-running `worker`/`sniffer`) its health/metrics server and graceful shutdown. All typed env reads go through the shared `internal/env` package (`env.Int`/`env.Bool`) — the old per-file `envInt`/`envBool` helpers are gone (verified: zero remaining references outside a historical comment in `internal/env/env_test.go`; 13 `env.Int`/`env.Bool` call sites — 9 in `cmd/imgsync/worker.go`, 4 in `internal/cli/sniffer.go`). DSN, pod name, pattern strings, and FTP creds are still read with plain `os.Getenv`.

### Key files & symbols
- `cmd/imgsync/main.go:main` — builds the cobra root (`SilenceUsage`/`SilenceErrors` true, `Version` set), registers the four subcommands, executes with the signal-aware context via `root.ExecuteContext(ctx)`.
- `cmd/imgsync/worker.go:newWorkerCmd` — the heavyweight command: worker pool, FTP transport pool, hostcap, health server, sweeper, opt-in retention, all metric callbacks.
- `cmd/imgsync/worker.go:newHostcapTransport` — builds a DEDICATED db pool for hostcap (issue #18) and returns `(transfer.Transport, func(), error)`.
- `cmd/imgsync/enqueue.go:newEnqueueCmd` — one-shot insert via `jobs.Enqueue` (idempotent on `(trace_id, dst)`); required flags `--trace-id/--src/--dst/--src-protocol/--dst-protocol`, `--max-attempts` default 5.
- `cmd/imgsync/migrate.go:newMigrateCmd` — runs `db.ApplyMigrations`; `--dir` defaults to `IMGSYNC_MIGRATIONS_DIR` else `/etc/imgsync/migrations` (forward-only `*.up.sql`).
- `cmd/imgsync/sniffer.go:newSnifferCmd` — thin 21-line shell: `cli.ParseSnifferConfig` then `cli.RunSniffer`.
- `internal/cli/sniffer.go:ParseSnifferConfig` / `:RunSniffer` — testable config parse + loop, where the real sniffer wiring lives (kept out of `cmd/` for unit testability).
- `internal/env/env.go:Int` / `:Bool` — shared typed env accessors; absent/empty/malformed → default. `Bool` is true only for `"1"` or case-insensitive `"true"`.
- `cmd/imgsync/worker_hostcap_pool_test.go:TestHostcapDoesNotStarveWorkerPool` — testcontainers Postgres test proving hostcap uses its own pool, not the worker pool.

### How it works / flow
`newWorkerCmd` requires `IMGSYNC_DSN`, reads `IMGSYNC_WORKERS` (default 4), and sizes the worker pool to `MaxConns = 2 + workers`. It attaches four metrics collectors to that pool: `AttachQueueDepth`, `AttachDBPool`, `AttachLeaseLockAge`, `AttachOldestPending`. It builds an FTP connection pool (`pftp.NewPool`, env-tuned `IMGSYNC_FTP_MAX_PER_HOST` (default 4)/`_IDLE_TTL_SEC` (300)/`_NOOP_AFTER_SEC` (60), creds from `IMGSYNC_FTP_USER`/`_PASSWORD`, `OnPoolChange = m.OnFTPPoolChange`), then wraps the raw FTP transport in a per-host cap via `newHostcapTransport(ctx, dsn, pool, ftpRaw, hostcap.Config{Cap: env.Int("IMGSYNC_FTP_HOST_CAP", 8)})`.

`newHostcapTransport` does NOT borrow the worker pool (the `*pgxpool.Pool` param is intentionally `_`-ignored). Because `hostcap.Wrap` pins a dedicated pgx connection holding a session-scoped `pg_advisory_lock` (`pg_try_advisory_lock(hashtext(slotKey))`) for the entire transfer, it opens its OWN pool sized `MaxConns = Cap + 2` (Cap defaults to 8 if `<= 0`) and returns `capPool.Close` as the closer. This prevents in-flight transfers from starving lease/commit/sweep/scrape of worker conns (issue #18).

The `worker.Runner` is wired with `SourceFor`/`TransportFor` closures dispatching on `"localfs"`/`"ftp"` (else `worker.ErrUnknownProtocol`), plus an idle backoff (`backoff.NewIdle`, 50ms→1s). Health: listens on `IMGSYNC_HEALTH_ADDR` (default `:8080`), serves `health.NewServer(pool, status, ...)` with `WithMetrics(m.Handler())` and `WithLivenessThreshold(livenessThreshold)` where `livenessThreshold = env.Int("IMGSYNC_LIVENESS_THRESHOLD_SEC", 10)` seconds (issue #36 — only a genuinely wedged lease loop trips `/livez`). A sweeper goroutine runs (`sweeper.Run`, 5m threshold, 30s interval) firing `status.OnSweepCycle`/`m.OnSweepCycle`.

Retention is OPT-IN (issue #28): `IMGSYNC_RETENTION_DAYS` defaults to 0 (disabled, nothing deleted). When `> 0`, a goroutine runs `retention.Run` with `Window = days*24h`, `BatchSize = IMGSYNC_RETENTION_BATCH` (1000), `Interval = IMGSYNC_RETENTION_INTERVAL_SEC` (3600s), and `OnCycle: m.OnRetention` (`func(deleted int)`).

Runner metric callbacks (composition pattern — the runner exposes single callback fields; cmd chains status + metrics):
- `OnLeaseAttempt = func(success bool)` → `status.OnLeaseAttempt(success)` + `m.OnLeaseAttempt(success, nil)` (runner field is `func(bool)`; the metrics method takes `(bool, error)`, cmd passes `nil`).
- `OnFinish = func(j *worker.Job, result string)` (signature changed in #17, confirmed by `worker/runner_finish_result_test.go`) → `m.OnJobFinished(j.SrcProtocol, j.DstProtocol, result, j.Duration())`.
- `OnRetry = func(j *worker.Job, stage string)` → `m.OnRetry(j.SrcProtocol, j.DstProtocol, stage)`.
- `OnWorkerStart`/`OnWorkerStop` (`func(pod string)`) → `atomic` increment/decrement of a local `int32` gauge, then `m.SetWorkersActive(pod, n)`.

`RunSniffer` (in `internal/cli`) opens a source pool (`sourcedb.NewPool`, 30s/30000ms query timeout) and an imgsync pool (`pgxpool.New`), attaches `AttachQueueDepth`/`AttachDBPool` (lease-lock age is the worker's job, not the sniffer's), serves health on `SNIFFER_HEALTH_ADDR` (default `:8080`), runs one poll immediately (`s.RunOnce`) then loops on a `SNIFFER_INTERVAL_SEC` ticker. The `sniffer.Config` callback fields `OnEnqueue`/`OnError` are wired to the metric methods `m.OnSnifferEnqueue`/`m.OnSnifferError`, and the loop calls `m.OnSnifferRun(cfg.SourceID)` after each successful `RunOnce`. It applies its own `signal.NotifyContext` (SIGTERM/SIGINT) as defense-in-depth for non-cobra callers.

### Agent notes (gotchas, conventions, constraints)
- Always use `env.Int`/`env.Bool` for typed env in this area — do NOT reintroduce per-file `envInt`/`envBool`. `env.Bool` only treats `"1"`/`"true"` (case-insensitive) as true; any other non-empty value is false; absent/empty falls back to the default.
- `newHostcapTransport`'s `*pgxpool.Pool` param is deliberately `_`-ignored. It opens a fresh pool sized `Cap + 2`. If you change hostcap to share the worker pool you will re-introduce the #18 starvation that `TestHostcapDoesNotStarveWorkerPool` guards against.
- The worker pool is sized `2 + IMGSYNC_WORKERS`; the hostcap pool is sized `IMGSYNC_FTP_HOST_CAP + 2`. These are independent — both connect to the same `IMGSYNC_DSN`, so the total Postgres connection budget is roughly the sum (plus enqueue `MaxConns=4`, migrate transient).
- Retention does nothing unless `IMGSYNC_RETENTION_DAYS > 0`. Don't assume terminal rows are pruned by default; the default is unbounded growth, bounded only when operators opt in (terminal rows older than the window are deleted, events cascade via the `ON DELETE CASCADE` FK on `transfer_events.job_id`).
- `OnFinish` is `func(*Job, result string)` and `OnRetry` is `func(*Job, stage string)` — both pass the full `*worker.Job` (which carries `SrcProtocol`/`DstProtocol`/`Duration()`); don't revert to a result-only signature. (`worker.ProcessJob` itself returns `(string, error)`; the runner discards the error and forwards the result string to `OnFinish`.)
- Sniffer real logic lives in `internal/cli/sniffer.go`, NOT `cmd/imgsync/sniffer.go` (the cobra shell). Edit config/loop behavior in `internal/cli` so the `_test.go` there still covers it. All `SNIFFER_*` env is parsed/validated in `ParseSnifferConfig` (10 required string vars; `INTERVAL_SEC` and `BATCH_SIZE` must each be `> 0`).
- Health addr env vars differ by command: worker uses `IMGSYNC_HEALTH_ADDR`, sniffer uses `SNIFFER_HEALTH_ADDR` (both default `:8080`, so don't co-locate worker+sniffer on one host without overriding one).
- Migrations on disk are `0001`–`0004`. `0001_initial` creates `transfer_jobs_trace_id_idx`; `0004_drop_trace_id_index` drops it (it's a redundant prefix of the `UNIQUE(trace_id, dst)` index `transfer_jobs_trace_id_dst_key`, issue #34) — so after applying all four, that single-column index is GONE. `0003_jobs_status_index` adds `transfer_jobs_status_idx (status)` for the status-group-by scrape. The migrate command applies only `*.up.sql` forward-only from `--dir`.

## Configuration & Environment Variables

imgsync is a single binary (`cmd/imgsync`) with four cobra subcommands — `migrate`, `enqueue`, `worker`, `sniffer` — wired in `cmd/imgsync/main.go`. The root command sets `SilenceUsage`/`SilenceErrors` and has no global/root flags beyond cobra's built-in `--version`/`--help`. Each subcommand reads its own env vars; `enqueue` is the only one driven by CLI flags. All numeric/bool env parsing routes through `internal/env` (`env.Int`, `env.Bool`), which silently falls back to the default on absent/empty/malformed input — there is no "warn to stderr on bad int." (Note: `docs/configuration/environment-variables.md` still tells reviewers to grep `os.Getenv|envInt|envBool`; the real symbols are `os.Getenv`, `env.Int`, `env.Bool`.)

### Key files & symbols
- `internal/env/env.go:Int` — `os.Getenv` + `strconv.Atoi`; empty or parse-error returns `def`, no logging.
- `internal/env/env.go:Bool` — true only when value is `"1"` or case-insensitive `"true"` (`v == "1" || strings.EqualFold(v, "true")`); any other non-empty value is false; empty returns `def`.
- `cmd/imgsync/worker.go:newWorkerCmd` — reads all `IMGSYNC_*` worker/FTP/retention/liveness vars.
- `cmd/imgsync/worker.go:newHostcapTransport` — builds the dedicated hostcap pool sized `int32(cap)+2` from `IMGSYNC_FTP_HOST_CAP` (`cap<=0` coerced to `8`).
- `cmd/imgsync/enqueue.go:newEnqueueCmd` — flag-driven (`--trace-id/--src/--dst/--src-protocol/--dst-protocol/--max-attempts`); only env is `IMGSYNC_DSN`.
- `cmd/imgsync/migrate.go:newMigrateCmd` — `IMGSYNC_DSN` + `--dir` flag (default from `IMGSYNC_MIGRATIONS_DIR`, else `/etc/imgsync/migrations`).
- `internal/cli/sniffer.go:ParseSnifferConfig` — parses all `SNIFFER_*` vars into `SnifferConfig`, validates ten required ones + positive `INTERVAL_SEC`/`BATCH_SIZE`.
- `internal/cli/sniffer.go:RunSniffer` — opens pools, also reads `SNIFFER_HEALTH_ADDR`.
- `cmd/imgsync/sniffer.go:newSnifferCmd` — thin cobra wrapper: `ParseSnifferConfig` then `RunSniffer`.
- `deploy/helm/imgsync/templates/configmap.yaml` — worker `IMGSYNC_*` ConfigMap (envFrom).
- `deploy/helm/imgsync/templates/sniffer-configmap.yaml` — sniffer `SNIFFER_*` ConfigMap (gated on `.Values.sniffer.enabled`).

### Env vars & flags per subcommand

**Common / all subcommands**
| Var | Default | Notes |
|---|---|---|
| `IMGSYNC_DSN` | (required) | Postgres DSN for worker/enqueue/migrate. Empty → command errors. In Helm: `secretKeyRef` from `dsnSecretRef` (default secret `imgsync-dsn`, key `dsn`). |

**`migrate`**
| Var / Flag | Default | Notes |
|---|---|---|
| `IMGSYNC_DSN` | (required) | — |
| `IMGSYNC_MIGRATIONS_DIR` | `/etc/imgsync/migrations` | Only sets the *default* for `--dir`. |
| `--dir` | `$IMGSYNC_MIGRATIONS_DIR` or `/etc/imgsync/migrations` | Directory of `*.up.sql` files (passed to `db.ApplyMigrations`). The `/etc/imgsync/migrations` default is the in-container mount path; the repo's migrations live at `./migrations/` (`0001_initial`, `0002_sniffer_state`, `0003_jobs_status_index`, `0004_drop_trace_id_index`). |

**`enqueue`** (flag-driven; no env except DSN)
| Flag | Default | Required |
|---|---|---|
| `--trace-id` | "" | yes |
| `--src` | "" | yes |
| `--dst` | "" | yes |
| `--src-protocol` | "" | yes (e.g. `localfs`, `ftp`) |
| `--dst-protocol` | "" | yes |
| `--max-attempts` | `5` | no |

**`worker`**
| Var | Default | Parser | Notes |
|---|---|---|---|
| `IMGSYNC_DSN` | (required) | Getenv | empty → error |
| `IMGSYNC_WORKERS` | `4` | env.Int | goroutines; worker pool `MaxConns` sized `int32(2+workers)` |
| `IMGSYNC_POD_NAME` | `os.Hostname()` | Getenv | lease `locked_by` identifier; Helm injects `metadata.name` via downward API |
| `IMGSYNC_FTP_MAX_PER_HOST` | `4` | env.Int | per-pod FTP pool max conns/host |
| `IMGSYNC_FTP_IDLE_TTL_SEC` | `300` | env.Int (→ seconds) | idle conn TTL |
| `IMGSYNC_FTP_NOOP_AFTER_SEC` | `60` | env.Int (→ seconds) | NOOP keepalive after idle |
| `IMGSYNC_FTP_HOST_CAP` | `8` | env.Int | cluster-wide per-host cap (advisory lock); `≤0` coerced to `8` in `newHostcapTransport` |
| `IMGSYNC_FTP_USER` | "" | Getenv | FTP auth; Helm: `secretKeyRef` from `ftpSecretRef` (only rendered when `ftpSecretRef.name` set) |
| `IMGSYNC_FTP_PASSWORD` | "" | Getenv | FTP auth; Helm: `secretKeyRef` from `ftpSecretRef` |
| `IMGSYNC_HEALTH_ADDR` | `:8080` | Getenv | health/metrics listen addr (`/livez`,`/readyz`,`/healthz`,`/metrics` share this port) |
| `IMGSYNC_LIVENESS_THRESHOLD_SEC` | `10` | env.Int (→ seconds) | **issue #36**. `/livez` staleness bound (~10× idle MaxDelay of 1s). Read by binary but NOT in `configmap.yaml`/`values.yaml`/docs — set manually. |
| `IMGSYNC_RETENTION_DAYS` | `0` | env.Int | OPT-IN; `0` disables retention entirely. `>0` deletes terminal rows older than N days (window = `days*24h`) |
| `IMGSYNC_RETENTION_BATCH` | `1000` | env.Int | rows per batched DELETE; only read when retention enabled |
| `IMGSYNC_RETENTION_INTERVAL_SEC` | `3600` | env.Int (→ seconds) | retention loop interval; only read when enabled |

**`sniffer`** (all via `ParseSnifferConfig`)
| Var | Default | Parser | Required |
|---|---|---|---|
| `SNIFFER_SOURCE_ID` | "" | Getenv | yes |
| `SNIFFER_SOURCE_DSN` | "" | Getenv | yes (Helm: secret from `sniffer.secrets.sourceDSNSecretRef`, key `SNIFFER_SOURCE_DSN`) |
| `SNIFFER_IMGSYNC_DSN` | "" | Getenv | yes (Helm: secret from `sniffer.secrets.imgsyncDSNSecretRef`, key `SNIFFER_IMGSYNC_DSN`) |
| `SNIFFER_TABLE` | "" | Getenv | yes |
| `SNIFFER_PK_COLUMN` | "" | Getenv | yes |
| `SNIFFER_TS_COLUMN` | "" | Getenv | yes |
| `SNIFFER_DST_PATTERN` | "" | Getenv | yes (Go `text/template`) |
| `SNIFFER_SRC_PATTERN` | "" | Getenv | yes (Go `text/template`) |
| `SNIFFER_SRC_PROTOCOL` | "" | Getenv | yes |
| `SNIFFER_DST_PROTOCOL` | "" | Getenv | yes |
| `SNIFFER_EXTRA_COLUMNS` | "" | Getenv (CSV split) | no; trimmed, empties dropped |
| `SNIFFER_SHADOW` | `true` | env.Bool | no |
| `SNIFFER_BATCH_SIZE` | `500` | env.Int | no; must be `>0` or `ParseSnifferConfig` errors |
| `SNIFFER_BIAS_SEC` | `5` | env.Int (→ seconds) | no |
| `SNIFFER_INTERVAL_SEC` | `60` | env.Int | no; must be `>0` or `ParseSnifferConfig` errors |
| `SNIFFER_HEALTH_ADDR` | `:8080` | Getenv (in `RunSniffer`) | no |

### How it works / flow
- `worker` requires `IMGSYNC_DSN` (hard error if empty), sizes the worker pool at `int32(2+IMGSYNC_WORKERS)`, and builds a *separate* hostcap pool (`int32(cap)+2`) so in-flight FTP transfers holding a session advisory lock don't starve lease/commit/sweep/scrape conns (issue #18). `_SEC`-suffixed vars are plain integer seconds multiplied into `time.Duration` — not Go duration strings.
- Retention is fully opt-in: `IMGSYNC_RETENTION_DAYS` defaults to `0`, which skips starting the `retention.Run` goroutine entirely; `_BATCH` and `_INTERVAL_SEC` are read only when days `>0`.
- `sniffer` validation lives in `ParseSnifferConfig`: ten required vars, plus positivity checks on interval/batch. Unlike the worker, the sniffer returns descriptive errors (`required env X missing`, `SNIFFER_INTERVAL_SEC must be > 0, got N`) before the loop starts. `RunSniffer` runs one poll immediately, then loops on a ticker.
- Helm wiring: worker non-secret vars come from `configmap.yaml` via `envFrom`; DSN and FTP creds are injected as explicit `secretKeyRef` entries in `deployment.yaml` (FTP block only renders when `ftpSecretRef.name` is set); `IMGSYNC_POD_NAME` is the downward-API pod name. Sniffer mirrors this with `sniffer-configmap.yaml` + secret refs in `sniffer-deployment.yaml` (which also `fail`s the render if `sniffer.replicas > 1`). ConfigMap `checksum/config` / `checksum/sniffer-config` annotations roll pods on config change.

### Agent notes (gotchas, conventions, constraints)
- **Silent defaults:** `env.Int`/`env.Bool` never log on bad input. A typo like `IMGSYNC_WORKERS=four` silently runs 4 workers. Do NOT reintroduce stderr warnings — the policy is "fall back to default, no noise." If you need rejection-on-bad-value, that's a behavior change, not a bug.
- **`env.Bool` is strict:** only `"1"`/`"true"` (case-insensitive) are true; `"yes"`, `"on"`, `"True "` (trailing space) all evaluate false. `SNIFFER_SHADOW` defaults true, so shadow mode is on unless explicitly set to a falsey value.
- **Seconds, not durations:** every `*_SEC`/`*_TTL_SEC`/`*_AFTER_SEC` var is an integer count of seconds (`env.Int(...) * time.Second`). Never pass `"5m"` or `"300s"` — they parse-fail and silently fall to default.
- **`IMGSYNC_LIVENESS_THRESHOLD_SEC` is code-only:** the binary reads it (default 10s) but it is absent from `configmap.yaml`, `values.yaml`, and `docs/configuration/environment-variables.md`. To tune it via Helm you must add the key to both `configmap.yaml` and the `worker.*` block in `values.yaml` — currently it can only be overridden by editing the Deployment directly.
- **ConfigMap key names must match the binary exactly (issue #20):** worker FTP keys are `IMGSYNC_FTP_MAX_PER_HOST` / `IMGSYNC_FTP_IDLE_TTL_SEC` / `IMGSYNC_FTP_NOOP_AFTER_SEC` / `IMGSYNC_FTP_HOST_CAP` (asserted by `deploy/helm/imgsync/tests/template_test.sh`). If you add a worker env var, add it in three places: the `env.Int`/`Getenv` call in `worker.go`, `configmap.yaml`, and the `worker.*` block in `values.yaml`.
- **DSN/creds are never in ConfigMaps:** `IMGSYNC_DSN`, `IMGSYNC_FTP_USER/PASSWORD`, `SNIFFER_SOURCE_DSN`, `SNIFFER_IMGSYNC_DSN` come from Secrets via `secretKeyRef`. Keep them out of the ConfigMap templates.
- **Sniffer patterns are Go `text/template`** rendered per-row at runtime; `values.yaml` stores raw `{{.field}}` strings (it is plain YAML, not Helm-templated), e.g. `dstPattern: "/incoming/{{.file_path}}"`. Note `batchSize`/`biasSec`/`intervalSec` are stored as quoted strings (`"500"`, `"5"`, `"60"`) in `values.yaml`.
- **`enqueue` is the only flag-driven command** — its five `MarkFlagRequired` flags (`trace-id`, `src`, `dst`, `src-protocol`, `dst-protocol`) fail fast; `--max-attempts` is not required and defaults to 5.

Paths referenced (all absolute under `/Users/nineking/workspace/app/imgsync/`): `internal/env/env.go`, `cmd/imgsync/{main,worker,enqueue,migrate,sniffer}.go`, `internal/cli/sniffer.go`, `deploy/helm/imgsync/values.yaml`, `deploy/helm/imgsync/templates/{configmap,sniffer-configmap,deployment,sniffer-deployment}.yaml`, `deploy/helm/imgsync/tests/template_test.sh`, `docs/configuration/environment-variables.md`.

## Build, Packaging & Deployment (Docker, Helm, Makefile, CI)

Single static binary built via multi-stage Dockerfile (distroless `nonroot`), deployed by a Helm chart at `deploy/helm/imgsync` with separate **worker** and **sniffer** Deployments plus a pre-install/pre-upgrade **migrate Job**. `Makefile` is the task entrypoint for build/lint/test/docker/helm/e2e/docs. Two GitHub Actions workflows: `ci.yml` (lint + race tests + streaming guard, optional `e2e` label-gated job) and `docs.yml` (mkdocs → GitHub Pages). After #20/#25/#26/#28/#35/#55, the configmap FTP keys, sniffer pod hardening, CI integration-suite wiring, the streaming guard regex, and the shared `internal/env` reader are all current.

### Key files & symbols
- `Dockerfile` — 2-stage build: `golang:${GO_VERSION}-alpine` builder (`GO_VERSION=1.25` ARG; `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, `-trimpath -ldflags="-s -w -X main.version=${VERSION}"`) → `gcr.io/distroless/static-debian12:nonroot`. Migrations copied to `/app/migrations`; `ENV IMGSYNC_MIGRATIONS_DIR=/app/migrations`; `USER nonroot:nonroot`; `ENTRYPOINT ["/app/imgsync"]`, `CMD ["--help"]`.
- `Makefile` — targets: `build test lint streaming-check tidy ci docker-build docker-test helm-lint helm-template helm-test e2e-throughput e2e-dirty-state e2e-sniffer e2e-up/down docs-build` (plus `dev-up/down/seed/smoke`, `test-integration-sniffer/-metrics`, real-cluster `e2e-*-real`). `ci: lint streaming-check test`; `test: go test ./... -race -count=1`.
- `.github/workflows/ci.yml` — `lint-and-test` job runs, in order: streaming guard (`scripts/check-streaming.sh`) + streaming self-test (`scripts/check-streaming.sh.test.sh`) + `golangci/golangci-lint-action@v7` (version `v2.4.0`) + `go test ./... -race -count=1`. `e2e` job is gated on PR label `e2e` (`contains(github.event.pull_request.labels.*.name, 'e2e')`), `needs: [lint-and-test]`.
- `.github/workflows/docs.yml` — `build` job runs `mkdocs build --strict` → `actions/upload-pages-artifact@v3`; `deploy` job (`if: github.ref == 'refs/heads/main'`) runs `actions/deploy-pages@v4`. Path-filtered on `docs/**`, `mkdocs.yml`, `requirements-docs.txt`.
- `scripts/check-streaming.sh` — CI guard forbidding full-body buffering in `internal/sources`, `internal/transports`, `internal/transfer`.
- `scripts/check-streaming.sh.test.sh` — meta-test asserting the guard catches both `io.ReadAll` (under `internal/sources`) and `bytes.Buffer`+`io.Copy(buf, body)` (under `internal/transports`).
- `scripts/ci-wiring.test.sh` — asserts integration suites, `make e2e-sniffer`, and the streaming self-test are wired into `ci.yml` (issue #25 regression guard). Note: this script is NOT invoked by `ci.yml` itself — it is a local invariant check.
- `deploy/helm/imgsync/templates/sniffer-deployment.yaml` — sniffer Deployment (`strategy: Recreate`, `replicas` hard-failed via `{{ fail }}` if >1).
- `deploy/helm/imgsync/templates/configmap.yaml` — worker non-secret env (FTP + retention keys + `IMGSYNC_WORKERS` + `IMGSYNC_HEALTH_ADDR`).
- `deploy/helm/imgsync/templates/deployment.yaml` / `migrate-job.yaml` / `serviceaccount.yaml` — worker (`strategy: RollingUpdate`, `terminationGracePeriodSeconds: 60`), `migrate up` Job, ServiceAccount.
- `deploy/helm/imgsync/values.yaml` — `worker.*`, `sniffer.*`, `podSecurityContext`, `securityContext`, `migrationJob.*`, `health.*`, `monitoring.serviceMonitor.*`.
- `internal/env/env.go:Int` / `env.Bool` — shared typed env reader. `Int`: absent/empty/malformed → default. `Bool`: absent/empty → default, otherwise true only for `"1"`/`"true"` (case-insensitive), every other present value false (no malformed→default path). The worker reads all `IMGSYNC_FTP_*` / `IMGSYNC_RETENTION_*` (and `IMGSYNC_WORKERS`, `IMGSYNC_LIVENESS_THRESHOLD_SEC`) through `env.Int` (#55).

### How it works / flow
- **Image:** `make docker-build` passes `--build-arg VERSION=$(VERSION)` (from `git describe --tags --always --dirty`), tags `imgsync:$(VERSION)` + `imgsync:dev`. `scripts/test-docker-build.sh` (via `make docker-test`) asserts `--help` works for the `migrate`/`enqueue`/`worker` subcommands, the image runs as `nonroot:nonroot` / `65532:65532`, and the image is under a 50MB budget.
- **Worker env (configmap.yaml):** keys now match the binary 1:1 — `IMGSYNC_WORKERS`, `IMGSYNC_FTP_MAX_PER_HOST`, `IMGSYNC_FTP_IDLE_TTL_SEC`, `IMGSYNC_FTP_NOOP_AFTER_SEC`, `IMGSYNC_FTP_HOST_CAP`, `IMGSYNC_RETENTION_DAYS`, `IMGSYNC_RETENTION_BATCH`, `IMGSYNC_RETENTION_INTERVAL_SEC`, `IMGSYNC_HEALTH_ADDR` (#20/#28). `cmd/imgsync/worker.go` reads the integer keys via `env.Int(...)` with matching defaults (workers 4 / max-per-host 4 / idle-ttl 300 / noop 60 / host-cap 8 / retention-days 0=disabled / retention-batch 1000 / retention-interval 3600). `IMGSYNC_HEALTH_ADDR` is read via plain `os.Getenv` (default `:8080`), not `env.Int`. Retention is **opt-in**: `IMGSYNC_RETENTION_DAYS=0` (default) disables the sweep. FTP user/password (`IMGSYNC_FTP_USER`/`_PASSWORD`) come from a Secret via `env:`/`secretKeyRef` in the worker Deployment (rendered only when `ftpSecretRef.name` is set), via `os.Getenv` in the binary — not the ConfigMap.
- **Hostcap (#18):** the per-host FTP concurrency cap (`hostcap.Wrap`) draws from its OWN dedicated pgx pool sized `Cap+2` (`newHostcapTransport` in `worker.go`), not the worker pool, so in-flight transfers can't starve lease/commit/sweep/scrape of worker conns.
- **Sniffer pod hardening (#35):** `sniffer-deployment.yaml` sets pod-level `securityContext: {{ toYaml .Values.podSecurityContext }}`, container-level `securityContext: {{ toYaml .Values.securityContext }}` (`readOnlyRootFilesystem: true`, drop ALL caps, `runAsUser: 65532`), `serviceAccountName: {{ include "imgsync.serviceAccountName" . }}`, and a writable `/tmp` `emptyDir` (`sizeLimit: 64Mi`) required because the root FS is read-only. Sniffer config comes from `envFrom` the sniffer ConfigMap with a `checksum/sniffer-config` annotation forcing pod restart on config change; DSNs (`SNIFFER_SOURCE_DSN`, `SNIFFER_IMGSYNC_DSN`) come from `secretKeyRef`. `sniffer.replicas > 1` is a hard `{{ fail }}` (no advisory lock on `sniffer_state` in v1).
- **CI integration wiring (#25):** the `//go:build integration` tag was **dropped** from `internal/sniffer/integration_test.go`, `internal/metrics/integration_test.go`, and `internal/db/migrate_integration_test.go` — they now run in the default `go test ./...` job. `ci.yml` runs `bash scripts/check-streaming.sh.test.sh` as its own step; the label-gated `e2e` job runs `make e2e-throughput`, `make e2e-dirty-state`, and `make e2e-sniffer` (the latter two guarded `if: success() || failure()` so all surface), then `make e2e-down` (`if: always()`). Note: `ci.yml` does **not** call `make e2e-sniffer` outside the e2e job, nor `scripts/ci-wiring.test.sh` directly — `ci-wiring.test.sh` is the local invariant check, satisfied because the e2e job invokes `make e2e-sniffer` and `lint-and-test` runs the streaming self-test.
- **Streaming guard regex (#26):** `check-streaming.sh` greps `\b(io|ioutil)\.ReadAll\b|bytes\.(NewBuffer|Buffer)\b` over `*.go` (excluding `*_test.go`), filtering full-line `//` comments (`^[^:]+:[0-9]+:[[:space:]]*//`), across the three streaming dirs. Broadened from the old body-only pattern to catch `bytes.Buffer` accumulation + `io.Copy(buf, body)` that never names "body" next to the buffer.

### Agent notes (gotchas, conventions, constraints)
- **ConfigMap keys must stay identical to the binary's `env.Int(...)` keys.** Renaming a key in `configmap.yaml` without updating `cmd/imgsync/worker.go` (or vice versa) silently falls back to the binary default — `env.Int` swallows absent/malformed values. This exact drift was #20/#28. `template_test.sh` Test 13 asserts the rendered ConfigMap's `IMGSYNC_FTP_*` key set equals the binary's read set exactly.
- **Values are integer SECONDS, not Go duration strings.** `ftpIdleTTLSec`, `retentionIntervalSec`, etc. are plain ints; the binary multiplies by `time.Second`. Do not write `"5m"`.
- **`values.yaml` comment is stale (non-load-bearing):** line 37 still says worker env is read "via strconv.Atoi (cmd/imgsync/worker.go)" — it now routes through `internal/env.Int` after #55 (which itself still calls `strconv.Atoi` internally, so the comment is half-true but names the wrong layer). Code, not the comment, is ground truth.
- **Sniffer (and worker, and migrate Job) need a `/tmp` emptyDir** because `securityContext.readOnlyRootFilesystem: true`. Worker/sniffer use `sizeLimit: 64Mi`; the migrate Job uses `16Mi`. Removing the volume (or the volumeMount) breaks the pod at runtime even though Helm renders fine.
- **`sniffer.replicas` is capped at 1 by a template `{{ fail }}`** and the sniffer uses `strategy: Recreate` (the worker uses `RollingUpdate`, `maxSurge: 1 / maxUnavailable: 0`) — never relax the sniffer cap in v1 (watermark race on `sniffer_state`).
- **Migrate Job is per-revision and hook-driven:** name is `…-migrate-{{ .Release.Revision }}`, `args: ["migrate", "up"]`, `restartPolicy: Never`, runs as pre-install/pre-upgrade hook (`hook-weight: "0"`) with `before-hook-creation,hook-succeeded` delete policy. `template_test.sh` Test 6 isolates the migrate-job manifest and asserts the exact `args: ["migrate", "up"]` array plus nonroot/RO-rootFS context — keep both in sync.
- **Don't re-add `//go:build integration` to the three integration test files** — they are intentionally untagged now so CI's default `go test ./...` covers them. `scripts/ci-wiring.test.sh` will fail (red) if they regain the tag while `ci.yml` lacks a `-tags integration` step. (`make test-integration-sniffer`/`-metrics` still use `-tags integration` for the separately-tagged S0-S3 / scrape suites.)
- **When editing streaming hot paths**, run `make streaming-check` locally; the guard runs in `lint-and-test` and any `io.ReadAll` / `ioutil.ReadAll` / `bytes.Buffer` / `bytes.NewBuffer` in `internal/{sources,transports,transfer}` (non-test, non-comment) fails CI.
- **Helm/Docker structural tests are shell, not Go:** `make helm-test` (`deploy/helm/imgsync/tests/template_test.sh`) and `make docker-test` (`scripts/test-docker-build.sh`) assert security context, probe paths (`/livez`, `/readyz`), the `http-metrics` Service port, secret refs, PDB gating (rendered only at `replicaCount >= 2`), ServiceMonitor opt-in, and the nonroot user — they are not in the `go test` suite, so run them when touching chart templates or the Dockerfile.

## Testing Strategy, Build Tags & E2E

Three test layers gated differently: **unit** (pure Go, no deps), **integration** (Postgres via `testcontainers-go`, Docker required, runs in the default `go test ./...`), and **E2E** (kind cluster + Helm chart, gated by both the `e2e` build tag and `IMGSYNC_E2E=1`). After #25, the `//go:build integration` tag was **removed** from all three former integration suites — they now run unconditionally in `make test` / CI (and skip nothing; they simply RED if Docker is absent). Two shell guards run in CI: the streaming-buffer guard and its own self-test.

### Key files & symbols
- `scripts/check-streaming.sh` — CI guard over `internal/sources`, `internal/transports`, `internal/transfer`; greps `*.go` (excludes `*_test.go`) for `\b(io|ioutil)\.ReadAll\b|bytes\.(NewBuffer|Buffer)\b`, then filters out lines whose content begins with `//` (regex `^[^:]+:[0-9]+:[[:space:]]*//`), exits 1 on any remaining match.
- `scripts/check-streaming.sh.test.sh` — meta-test (#26); asserts the guard rejects an `io.ReadAll` fixture AND a `bytes.Buffer{}` + `io.Copy(buf, body)` fixture (the shape the old regex missed).
- `scripts/ci-wiring.test.sh` — asserts `ci.yml` is valid YAML and that the (now untagged) integration suites, `make e2e-sniffer`, and the streaming self-test are all reachable from CI; step 1 encodes the tag-removal invariant (passes if CI runs `-tags integration` OR no suite still carries `//go:build integration`).
- `.github/workflows/ci.yml:lint-and-test` — runs streaming guard, its self-test, golangci-lint (action `v7`, lint `v2.4.0`), then `go test ./... -race -count=1` on `ubuntu-latest`, Go `1.25`.
- `.github/workflows/ci.yml:e2e` — label-gated (`if: contains(github.event.pull_request.labels.*.name, 'e2e')`), `needs: [lint-and-test]`; runs `make e2e-throughput` (first, unconditional), then `make e2e-dirty-state` and `make e2e-sniffer` (these two each carry `if: success() || failure()` to surface all), cleanup `make e2e-down` with `if: always()`.
- `internal/sniffer/integration_test.go` — `TestS0_PollingOverlapNoDuplicate`, `TestS1_CrashRecoveryNoLossNoDup`, `TestS2_TieBreakBatchCorrectness`, `TestS3_QueryTimeoutLeavesWatermarkUnchanged`; uses cross-file helpers `setupImgsyncDBWithTransferJobs` (in `state_test.go`) and `setupSourceDB` (in `query_test.go`), all in package `sniffer_test`.
- `internal/metrics/integration_test.go` — spins `postgres.Run(ctx, "postgres:16-alpine", ...)`, exercises the scrape collectors against real SQL (`TestQueueDepthCollector_*`, `TestLeaseLockAge_*`, `TestDBPoolCollector_*`).
- `internal/db/migrate_integration_test.go` — `TestMigrate_0003_StatusIndex` (asserts `transfer_jobs_status_idx` exists) and `TestMigrate_0004_DropTraceIDIndex` (asserts `transfer_jobs_trace_id_idx` is **gone** but the UNIQUE composite `transfer_jobs_trace_id_dst_key` remains, and `0004_drop_trace_id_index` is recorded in `schema_migrations`).
- `e2e/throughput_test.go:TestC7_ThroughputScaleOut`, `e2e/dirty_state_test.go:TestF5_DirtyStateRecovery`, `e2e/sniffer_test.go:TestC5Prime_SnifferSelfAudit` — all carry `//go:build e2e` and an `os.Getenv("IMGSYNC_E2E") != "1"` skip.
- `e2e/helpers.go`, `e2e/doc.go` — `//go:build e2e`-tagged support (kind/Helm orchestration).
- New unit/integration suites: `internal/retention/retention_test.go` (`TestSweep_*`, incl. `TestSweep_AdvisoryLock_OnlyOneRetentionRunsAtATime`, PG-backed), `internal/env/env_test.go` (`TestInt_*`/`TestBool_*`, pure), `internal/sniffer/poison_test.go` (`TestRunOnce_PoisonRow*`), `internal/sniffer/batch_enqueue_test.go` (`TestEnqueueBatch_*`), `internal/health/livez_progress_test.go` (`TestLivez_*`), `internal/backoff/backoff_test.go` (`TestIdle_*`), `internal/worker/process_lease_guard_test.go` (`TestProcessJob_LeaseLost_TerminalWriteIsNoOp`, `TestProcessJob_LeaseHeld_TerminalWriteSucceeds`) + `process_retry_lease_guard_test.go` (`TestProcessJob_LeaseLost_RetryWriteIsNoOp`), `internal/worker/process_sanitize_test.go` (`TestSanitizeErrMsg_Scrubs*`, exercising `sanitizeErrMsg` in `internal/worker/errdetail.go`), `internal/worker/process_backoff_test.go:TestRetryBackoff_WithinJitterBand` (exercising `retryBackoff` in `errdetail.go`) + `internal/hostcap/backoff_internal_test.go:TestJittered_IsHashSeededAndBounded`, `internal/sources/ftp/source_textproto_test.go` (`TestFTPSource_isNotFound_*`, incl. `_550_PermissionDenied_NotSkippable`), `internal/transports/ftp/ctxerr_test.go` + `internal/transports/localfs/ctxerr_test.go` (`..._Send_CtxCancel_AbortsInFlightPromptly`, exercising `internal/transfer/ctxreader.go:NewCtxReader`), and metrics observability suites (#51) `internal/metrics/{retries,oldest_pending,sniffer_lag}_test.go`.
- `cmd/imgsync/worker_hostcap_pool_test.go:TestHostcapDoesNotStarveWorkerPool` — PG-backed; asserts the **dedicated** hostcap pool (see below) doesn't starve the worker pool.

### How it works / flow
- **Unit layer** — runs everywhere with `make test` (`go test ./... -race -count=1`); no external deps. Examples: `env_test`, `backoff_test`, `process_sanitize_test`, `process_backoff_test`, `process_lease_guard_test`/`process_retry_lease_guard_test` (mock `Deps`, not PG-backed), `source_textproto_test`, `ctxerr_test`, `internal/cli/sniffer_test.go`.
- **Integration layer** — same `go test ./...` invocation. Each PG-backed test calls `postgres.Run(ctx, "postgres:16-alpine", postgres.WithDatabase("imgsync"), postgres.WithUsername/Password(...), postgres.BasicWaitStrategies())` then `ConnectionString(ctx, "sslmode=disable")`, with `t.Cleanup` terminating the container. There is **no skip-if-no-Docker** guard — the test `require.NoError`s on `postgres.Run`, so a Docker-less `go test ./...` will RED on these packages: `db`, `eval`, `health`, `hostcap`, `jobs`, `metrics`, `retention`, `sniffer`, `sourcedb`, `sweeper`, `worker` (only `job_test.go`), and `cmd/imgsync` (`worker_hostcap_pool_test.go`). CI's `ubuntu-latest` runner provides Docker.
- **Makefile `test-integration-sniffer`/`test-integration-metrics`** still exist and pass `-tags integration`, but since the tag was removed from the source files that flag is now a **no-op** — they just narrow the run via `-run "TestS[0-3]_"` / package path. Plain `make test` already covers them.
- **E2E layer** — double-gated. The `//go:build e2e` tag excludes the files from normal builds; `make e2e-throughput`/`e2e-dirty-state`/`e2e-sniffer` invoke `IMGSYNC_E2E=1 go test -tags e2e -timeout {35m,30m,20m} -v ./e2e/... -run {TestC7_ThroughputScaleOut,TestF5_DirtyStateRecovery,TestC5Prime_}`. Without `IMGSYNC_E2E=1` the tests `t.Skip`. They require a kind cluster + the `deploy/helm/imgsync` chart. Real-cluster variants (`e2e-up-real`, `e2e-down-real`, `e2e-seed-real`, `e2e-push-real`) read `IMGSYNC_E2E_NAMESPACE`/`IMGSYNC_E2E_REGISTRY`/`IMGSYNC_E2E_TAG`/`IMGSYNC_E2E_PLATFORMS`/`IMGSYNC_E2E_KEEP_NS`.
- **Migrations** — applied by `internal/db/migrate.go:ApplyMigrations`, which reads every `*.up.sql` under the top-level `migrations/` dir in lexical order, skipping versions already in `schema_migrations`. Current set: `0001_initial`, `0003_jobs_status_index` (adds `transfer_jobs_status_idx`), `0004_drop_trace_id_index` (drops the redundant single-column `transfer_jobs_trace_id_idx`, which was a leading-column prefix of UNIQUE `transfer_jobs_trace_id_dst_key`; issue #34).
- **Streaming guard flow** — `make ci` = `lint streaming-check test`; CI additionally runs `check-streaming.sh.test.sh` as a separate step. The guard catches `bytes.Buffer`/`bytes.NewBuffer` in addition to `io.ReadAll`/`ioutil.ReadAll`, and drops lines whose content begins with `//` to avoid commented false-positives.

### Agent notes (gotchas, conventions, constraints)
- **The `integration` build tag is gone.** Do NOT re-add `//go:build integration` to `internal/{sniffer,metrics,db}` — `scripts/ci-wiring.test.sh` step 1 will FAIL (it passes only if CI runs `-tags integration` OR no suite carries the tag; CI does neither, relying on the latter). Treat those suites as part of the default `./...` run.
- **PG-backed tests have no Docker skip.** Running `go test ./...` or `make test` locally without a Docker daemon RED's, not skips — intentional. Don't "fix" it by adding skips.
- **Sniffer integration tests depend on cross-file helpers in package `sniffer_test`** — `setupImgsyncDBWithTransferJobs` lives in `state_test.go` and `setupSourceDB` in `query_test.go`, not in `integration_test.go`. Keep them all in package `sniffer_test`.
- **E2E is double-gated** (`e2e` tag + `IMGSYNC_E2E=1`). Both are required; setting only one runs nothing. The E2E CI job is label-gated on a PR label named `e2e` and `needs` `lint-and-test`.
- **Worker terminal writes are single writable-CTE statements** (`WITH u AS (UPDATE transfer_jobs ... WHERE ... <lease guard> RETURNING trace_id) INSERT INTO transfer_events ... SELECT ... FROM u`, in `internal/worker/process.go`). The #19 lease guard lives in the UPDATE's `WHERE`; when the lease is lost the UPDATE matches 0 rows, the CTE is empty, and no event is inserted (the lease-guard tests assert this no-op). `ProcessJob` has signature `func(ctx, Deps, *Job) (string, error)`.
- **FTP 550 carve-out** — `internal/sources/ftp/source.go:isNotFound` keys on the jlaffaye `*textproto.Error` reply *code* (`ftp.StatusFileUnavailable` = 550), unwrapping via `errors.As`, to avoid locale-dependent message matching; but it carves out messages containing `"permission"` or `"access denied"` so permission/access denials surface (retry → dead) rather than being treated as `transfer.ErrSkippable` missing-source.
- **Hostcap uses a dedicated pgx pool.** `cmd/imgsync/worker.go` builds a separate `capPool` (`db.NewPool` with `MaxConns: cap+2`) passed to `hostcap.Wrap`; each `CapTransport.Send` `Acquire`s one connection and pins a session-scoped `pg_advisory_lock`/`pg_try_advisory_lock` slot per host for the whole transfer. Don't share the worker pool — `TestHostcapDoesNotStarveWorkerPool` guards against starvation.
- **When editing streaming hot paths** (`internal/sources`, `internal/transports`, `internal/transfer`): never introduce `io.ReadAll`, `bytes.Buffer`, or `bytes.NewBuffer` in non-test code — use streaming `io.Copy` with `internal/transfer/ctxreader.go:NewCtxReader` for context-cancellable reads (already wired in `transports/ftp/transport.go` via `io.TeeReader` and `transports/localfs/transport.go` via `io.Copy`). If you must reference a forbidden token in a comment, the guard skips only lines whose content starts with `//`.
- **The streaming self-test is load-bearing** — if you change `check-streaming.sh`'s regex, update `check-streaming.sh.test.sh` fixtures in lockstep; CI runs the self-test as a separate step.
- Test IDs follow a code: `S0–S3` (sniffer integration), `C5'`/`C7`/`F5` (E2E scenarios). Preserve these prefixes when adding cases — `make e2e-sniffer` matches `-run TestC5Prime_` and `test-integration-sniffer` matches `-run "TestS[0-3]_"`.

## Repository & Docs Map (where to find more)

imgsync is a Go + PostgreSQL file-transfer work queue replacing an in-house NiFi pipeline. Product intent lives in `PRD.txt`; the human-facing guide is a Korean MkDocs site under `docs/` (served via `mkdocs.yml`); deepest engineering ground-truth is the **external design doc** (not in-repo) plus in-repo `docs/superpowers/{plans,specs}` and `docs/test-reports`. **CRITICAL FOR THIS HEAD:** a wave of code-only fix PRs just landed (through #55 — `fix(tech-debt)` is the tip); the public `docs/` MkDocs site was NOT regenerated by those PRs, so `docs/` now **lags the code** in several concrete places (see drift table below). Trust code + this AGENTS.md over `docs/` for metrics, env-var names, FTP error handling, the migration list, and the new retention feature.

### Key files & symbols
- `PRD.txt` — product intent (Korean): 대량 파일 전송, 파일 단위 traceability, FTP 세션 재사용, worker scale-out. Modes `remote→local` (default) + future local/remote variants; protocols FTP (default) + S3 (intent only). S3 + Client TUI + Backend Server remain roadmap — only FTP + LocalFS exist in `internal/{sources,transports}/{ftp,localfs}`.
- `README.md` — English quickstart: `make docker-build/dev-up/dev-seed/dev-smoke` and Helm install (DSN secret `imgsync-dsn`, FTP secret `imgsync-ftp`, chart `deploy/helm/imgsync`, pre-install migration hook). Full `make` target table lives here. Excluded from the public site.
- `mkdocs.yml` — site config + `nav`; `strict: true`; ReadTheDocs theme (`theme.name: readthedocs`), `language: ko`. `exclude_docs` hides exactly four paths — `superpowers/`, `README.md`, `test-reports/`, `e2e-real-cluster-guide.md` — from the public site (still in-repo for agents). `mermaid` superfence uses `mermaid2.fence_mermaid` (plugin `mermaid2`).
- `migrations/000{1..4}_*.{up,down}.sql` — **four** migrations now: `0001_initial` (creates `transfer_jobs`, `transfer_events`, `schema_migrations`; the `job_status` ENUM is `pending/leased/succeeded/skipped/dead` — no `failed`); `0002_sniffer_state` (creates the `sniffer_state` table — NOT `0001`); `0003_jobs_status_index` (creates `transfer_jobs_status_idx`); `0004_drop_trace_id_index` (drops redundant `transfer_jobs_trace_id_idx`, GitHub issue #34). Forward-only, lexical order via `internal/db.ApplyMigrations(ctx, dsn, dir)`. `0001` has **no** `.down.sql`. The `(trace_id, dst)` idempotency UNIQUE is named `transfer_jobs_trace_id_dst_key`; after `0004` the only remaining trace_id index is that composite (the single-column `transfer_jobs_trace_id_idx` is gone).
- **External design doc (NOT in repo):** `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` (Status APPROVED, revision 5) + eng-review test plan `...-eng-review-test-plan-20260427-040000.md`. Canonical architecture/invariant source.

### docs/ site map (mkdocs nav, one line per page)
- `docs/index.md` — landing.
- **getting-started/** — `index.md` (path table); `quickstart-docker-compose.md`; `quickstart-kind.md`.
- **concepts/** — `index.md`; `architecture.md`; `job-queue-model.md` (Two-Table Minimal `transfer_jobs`+`transfer_events`, state transitions); `components.md`; `sources-and-transports.md`; `glossary.md`.
- **installation/** — `index.md`; `helm.md`; `secrets.md`; `values-reference.md`.
- **configuration/** — `index.md`; `environment-variables.md`; `worker.md`; `sniffer.md`; `sweeper.md`; `protocols.md` (`s3` = planned, not implemented).
- **cli/** — `index.md` (cobra binary, subcommands `migrate`/`enqueue`/`worker`/`sniffer`); `migrate.md`; `worker.md`; `sniffer.md`; `enqueue.md` (idempotent via `(trace_id, dst)` UNIQUE + `ON CONFLICT DO NOTHING`).
- **operating/** — `index.md`; `runbook.md` (증상→SQL→조치 — start here for ops); `monitoring.md`; `dashboards.md` (Grafana); `scaling.md`; `upgrades-and-rollback.md`; `troubleshooting.md`.
- **developer/** — `index.md`; `build-and-test.md`; `architecture-deep-dive.md`; `e2e-manual.md`; `contributing.md`; `release-process.md`.
- `docs/faq.md` — design Q&A (Postgres-not-Kafka, `FOR UPDATE SKIP LOCKED`, `(trace_id, dst)` idempotency).
- `docs/e2e-real-cluster-guide.md` — real Talos homelab E2E guide (excluded from public site).

### docs/superpowers/plans/ & specs/ — phased history (treat as already-executed)
- **plans/** (chronological): `2026-04-27-...week1-foundation` (module, schema, streaming Source/Transport, LocalFS, `enqueue`); `2026-04-27-...week2a-ftp-worker-core` (FTP `jlaffaye/ftp`, per-host conn pool, worker `FOR UPDATE SKIP LOCKED` lease); `2026-04-27-...week2b-sweeper-eval` (advisory-lock sweeper, per-host cap, `/livez /readyz /healthz`, EVAL C-invariants); `2026-04-27-...week3-helm-cutover` (distroless, Helm chart, cutover gates); `2026-04-27-...shadow-sniffer` (`imgsync sniffer`, `sniffer_state` watermark, migration 0002); `2026-05-03-...e2e-real-cluster` (Talos homelab E2E); `2026-05-05-...public-docs-site` (this `docs/` tree); `2026-05-05-monitoring-phase-1` (`internal/metrics`); `2026-05-05-monitoring-phase-1-5-status-index` / `...phase-1-5-explain` (migration 0003); `2026-05-06-monitoring-phase-1-guide-docs` (current branch's lineage).
- **specs/**: `2026-04-27-imgsync-shadow-sniffer-design.md`; `2026-05-05-monitoring-stack-integration-design.md`.

### docs/test-reports/ — verification records
- `2026-05-01-imgsync-a69bcb0.md` — full test run at `a69bcb0` (all PASS; E2E gated on an env flag).
- `2026-05-03-imgsync-real-cluster-0612277.md` — real Talos k8s, 5/5 scenarios PASS.
- **Note:** both predate this fix wave; they do NOT cover retention, retry metrics, the writable-CTE terminal writes (#53), or code-based FTP classification.

### Agent notes (gotchas, conventions, constraints) — DRIFT IS THE HEADLINE
- **`docs/` LAGS code post-merge. Verified drifts (trust code, not docs):**
  - **Metrics** (`internal/metrics/metrics.go`) — new/changed series include `imgsync_job_retries_total{src,dst,stage}`, `imgsync_retention_rows_deleted_total` (plain counter), `imgsync_sniffer_last_run_timestamp{source}`, and a scrape-time watermark-lag collector (`newSnifferLagCollector`), plus gauges attached at runtime: `imgsync_oldest_pending_age_seconds` (`AttachOldestPending`), queue-depth, db-pool, and lease-lock-age. `imgsync_jobs_processed_total` and `imgsync_job_duration_seconds` are labeled `{src,dst,result}` where `result` is the terminal outcome (`succeeded`/`skipped`/`dead`/`fail`) emitted by `OnJobFinished`. `docs/operating/{monitoring,dashboards}.md` document none of the new series. (Exact metric name = `imgsync_oldest_pending_age_seconds`; there is no `imgsync_sniffer_watermark_lag_seconds` constant — lag is exposed by the dynamic collector, verify its emitted name in `internal/metrics/sniffer_lag.go` before citing it.)
  - **Retention feature (NEW):** `internal/retention/retention.go` (`Sweep`/`Run`, advisory-lock-guarded), wired in `cmd/imgsync/worker.go`. **Opt-in by days:** `IMGSYNC_RETENTION_DAYS` (default 0 = disabled; positive = delete terminal rows older than N×24h), `IMGSYNC_RETENTION_INTERVAL_SEC` (default 3600), `IMGSYNC_RETENTION_BATCH` (default 1000). It batch-deletes terminal `transfer_jobs` (`status IN ('succeeded','skipped','dead')` AND `updated_at < NOW() - $1::INTERVAL`) via a `SELECT … LIMIT` subquery (NOT a writable CTE); events cascade via FK. Single-writer enforced by `pg_try_advisory_xact_lock(hashtext('imgsync_retention'))`. **Zero coverage in `docs/` config pages.**
  - **Env-var names:** `IMGSYNC_LIVENESS_THRESHOLD_SEC` (default 10) and the three `IMGSYNC_RETENTION_*` vars exist in code (`env.Int(...)`) but are absent from `docs/configuration/`. Cross-check `cmd/` env reads over Helm/docs.
  - **Two distinct per-host limits — do not conflate:** (1) the FTP **connection pool** cap `Pool.MaxPerHost` via `IMGSYNC_FTP_MAX_PER_HOST` (default 4, `internal/transports/ftp/pool.go`); (2) a **dedicated cluster-wide host-cap limiter** in `internal/hostcap/hostcap.go` using session-scoped `pg_advisory_lock` slots pinned to a dedicated pgx connection, wired via `IMGSYNC_FTP_HOST_CAP` (default 8). The week2b plan's "host cap" refers to the `hostcap` advisory-lock limiter, not the conn pool.
  - **FTP error handling (Issue #24):** classification is **code-based** in `internal/transports/ftp/transport.go` `classify()` — it `errors.As`-extracts a `*textproto.Error` and maps reply codes `ftp.StatusFileUnavailable` (550) / `ftp.StatusExceededStorage` (552) / `ftp.StatusBadFileName` (553) on **either STOR or Rename** to `transfer.ErrPermanent`; everything else (transient 4xx, dial/connection failures) is returned unchanged for retry. Sentinels live in `internal/transfer/errors.go`: `ErrPermanent` (→ mark `dead`) and `ErrSkippable` (→ mark `skipped`). Any docs prose describing string/substring 550-matching is stale.
  - **Terminal writes are single writable-CTE statements (#53):** `internal/worker/process.go` writes success/retry/terminal via `WITH u AS (UPDATE transfer_jobs … WHERE id=$1 AND status='leased' AND locked_by=$N RETURNING trace_id) INSERT INTO transfer_events … SELECT … FROM u` (the `#19` lease guard lives in the UPDATE `WHERE`; a lost lease → 0 rows updated → empty CTE → no event, a silent no-op). `ProcessJob(ctx, Deps, *Job)` returns `(string, error)` — the string is the metric `result` label (note the retry path writes DB `status='pending'` but reports `result="fail"`).
  - **Migration list in `docs/cli/migrate.md` is WRONG:** it lists a nonexistent `0002_add_extra_columns` (real file is `0002_sniffer_state`) and claims that migration adds `attempts`/`last_error`/`last_attempt_at` columns — but `attempts`, `max_attempts`, `locked_by`, `next_run_at`, `updated_at` all live in `0001_initial`, and there are no `last_error`/`last_attempt_at` columns at all. It also omits `0004_drop_trace_id_index`. Use the `migrations/` dir as ground truth.
- **External design doc + eng-review test plan (`~/.gstack/projects/nineking424-imgsync/...`) are the architecture/invariant ground truth, not in-repo docs.** Read both before touching queue/lease/sweeper/streaming semantics. Design doc on disk is APPROVED, revision 5.
- `mkdocs.yml` has `strict: true`; any nav/link break fails `make docs-build` (`mkdocs build --strict`). Adding a page under a nav'd dir without updating `nav` breaks the build. Docs venv `.venv-docs/` (`requirements-docs.txt`). `site/` is generated and **untracked** — never hand-edit it. Public docs prose is Korean; code/commands stay English.
- Plans use `- [ ]` checkbox syntax and cite REQUIRED SUB-SKILLs; treat them as already-executed history describing how code was built, file-by-file.
- Commit convention in-tree: `fix(<area>): <subject>` / `docs(<area>): <subject>` with a `Co-Authored-By: Claude ...` trailer. The recent fix wave is `fix(...)` code/Helm/CI PRs that deliberately did NOT touch the public guide — that boundary is exactly why `docs/` now lags.

---

## Working agreements & completion checklist

### Before every PR — the CI gate (must be green)

```bash
make ci   # = lint + streaming-check + test
```

If any of the three is red, CI is red — do not push. The three steps:
- `make lint` — `golangci-lint run` (gofmt, goimports, revive `exported`/`var-naming`/`error-return`/`error-strings`, bodyclose, misspell).
- `make streaming-check` — `scripts/check-streaming.sh` (no in-memory body buffering in `internal/{sources,transports,transfer}`).
- `make test` — `go test ./... -race -count=1`.

Pre-commit extras: run `make tidy` (`go mod tidy`) if deps changed; new exported symbols need godoc (revive `exported`); new error strings must be **lowercase, no trailing period** (revive `error-strings`).

### Area-specific verification (run the layer your change touches)

| You changed… | Also run |
|---|---|
| Helm chart (`deploy/helm/`) | `make helm-lint && make helm-template && make helm-test` |
| Dockerfile / container contract | `make docker-build && make docker-test` |
| Sniffer (`internal/sniffer`) | `make test-integration-sniffer` (S0–S3, Docker) |
| Metrics collectors | `make test-integration-metrics` (Docker) |
| New migration | add `NNNN_*.up.sql` **+** `.down.sql`; ensure self-`INSERT INTO schema_migrations`; verify the pre-install hook applies idempotently (F5c) |
| Worker / Transport (large) | `make dev-up && make dev-seed && make dev-smoke && make dev-down`, then kind E2E (`make e2e-throughput` ~35 m, `make e2e-dirty-state` ~30 m, `make e2e-sniffer` ~20 m) as a last step |
| Streaming hot-path code | confirm `scripts/check-streaming.sh` covers the new package (add to its `DIRS` if needed) — see the [known regex gap](#testing-strategy-build-tags--e2e) |
| Env var add/remove | update the master table in `docs/configuration/environment-variables.md`; keep `grep -rn 'os.Getenv\|envInt\|envBool' cmd/ internal/cli/` clean (env reads live only there) |

### Ground truth beyond this file

- **External design doc + eng-review test plan** (NOT in repo): `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` (**rev 5 APPROVED**) and `…-eng-review-test-plan-20260427-040000.md`. These define the `C0–C9` / `C5'` / `F1–F5` / `D7` invariant IDs referenced throughout the plans. Read both before changing queue/lease/sweeper/streaming semantics.
- **In-repo authoritative detail**: `docs/concepts/` (architecture, job-queue-model), `docs/operating/runbook.md` (incident procedures), `docs/superpowers/plans/` (how each piece was built, chronologically). See [Repository & Docs Map](#repository--docs-map-where-to-find-more).

### Commit & PR conventions

- Commit style seen in-tree: `type(area): subject` (e.g. `docs(model): …`, `feat(worker): …`) with a `Co-Authored-By:` trailer.
- Docs-only PRs stay docs-only (no code/chart changes); code PRs keep that boundary too.
- User preference: agree on a **minimal vs full** scope quickly, then ship a **bundled PR** over many tiny ones where it makes sense.
- Report concisely — summarize the change + 1–2 next steps; the user reads diffs.

---

<sub>This file was generated by exploring the codebase with a 12-area parallel deep-read, each section adversarially fact-checked against source. When code and this doc disagree, the code wins — and please update the relevant section.</sub>
