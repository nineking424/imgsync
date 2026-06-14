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
5. **Error class is the control plane.** Status routing is `errors.Is` against `transfer.ErrSkippable` (→ `skipped`, attempts unchanged) and `transfer.ErrPermanent` (→ `dead`, no retry). Anything else → retry with backoff `1<<(attempts+1)` seconds. **Wrap with `%w`, never compare error strings.**
6. **Streaming is sacred (hard CI gate).** `Source`/`Transport` impls must stream — never `io.ReadAll`/`ioutil.ReadAll` or `bytes.NewBuffer(...body...)` in `internal/{sources,transports,transfer}`. `scripts/check-streaming.sh` greps for these and fails the build. Use `io.Copy`/`io.TeeReader`/`io.MultiWriter`.
7. **Protocol registration is hard-coded in `cmd/imgsync/worker.go`** (the `SourceFor`/`TransportFor` switch closures, `"localfs"`/`"ftp"`). Adding a protocol = edit *both* closures there; `internal/worker` only knows `ErrUnknownProtocol`. FTP transport must stay wrapped by `hostcap.Wrap`.
8. **Sniffer is single-pod by contract.** One `Sniffer` per source maintains a `(ts, pk)` high-watermark in `sniffer_state` (no advisory lock). The watermark advances **only after a whole batch enqueues cleanly** — that's the crash-safety guarantee. Helm hard-`fail`s if `sniffer.replicas > 1`.
9. **`hostcap` is NOT host-capacity measurement.** Despite the name, it does not use gopsutil (which exists only as a transitive `// indirect` dep, never imported in source). It's a **cluster-wide per-FTP-host concurrency cap** via Postgres advisory locks. Worker concurrency is the static `IMGSYNC_WORKERS` env (default 4).
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
  transfer/              # domain boundary: Source/Transport interfaces + ErrSkippable/ErrPermanent sentinels
  jobs/                  # idempotent Enqueue — the ONLY transfer_jobs insert path
  worker/                # lease→process→complete lifecycle (Runner, ProcessJob, LeaseJob, classifyAndWrite)
  sources/{localfs,ftp}/      # input adapters (Source impls)
  transports/{localfs,ftp}/   # output adapters (Transport impls); transports/ftp/pool.go = per-host session pool
  sniffer/               # poll external source DB → incremental enqueue (high-watermark in sniffer_state)
  sweeper/               # reclaim dead/stale leases (pg advisory xact lock)
  db/                    # pgx pool builder + migration runner
  sourcedb/              # separate pool for sniffer's source DB
  health/                # /livez /readyz /healthz /metrics HTTP server
  hostcap/               # per-FTP-host concurrency cap via advisory locks (NOT gopsutil)
  backoff/               # shared jittered idle backoff for empty-queue workers
  metrics/               # Prometheus collectors + private registry (imgsync_*)
  cli/                   # sniffer env parsing + poll-loop wiring (logic NOT in cmd/)
  ftpserver/             # in-process FTP server for tests (afero-backed)
  eval/                  # executable correctness spec: C0–C6 invariant/contract tests
migrations/              # 0001–0003 forward-only SQL pairs — the data model IS the queue
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
- **Process**: `worker` runs N goroutines, each leasing a row (`SKIP LOCKED` → disjoint claims), opening a streaming `Source`, piping bytes through a byte-counter into `Transport.Send` (computes sha256 + verifies size), then writing terminal status + audit event in one transaction.
- **Recover**: `sweeper` (a goroutine inside `worker`) resets leases older than 5 min back to `pending` and emits an `expire` event — guarded by a single-writer advisory lock so multiple pods cooperate.
- **Observe**: every daemon serves `/healthz` + `/metrics` on `:8080`; scrape-time collectors run `GROUP BY status` / lease-age SQL per Prometheus scrape.

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

Detail on every target, the Helm chart, and CI lives in [Build, Packaging & Deployment](#build-packaging--deployment); the full test pyramid + build tags in [Testing Strategy](#testing-strategy-build-tags--e2e).

## Platform notes (macOS / BSD dev host)

- This dev host is **macOS/zsh with BSD coreutils**. `find`/`sed`/`date`/`grep` differ from GNU. Prefer `rg` (ripgrep), `gdate`/`gsed`/`gfind` (coreutils) — `sed -i` needs `sed -i ''`; BSD `date` lacks `+%s%N`.
- `gopls` (Serena's symbol backend) is at `~/go/bin/gopls`; `$HOME/go/bin` may not be on `PATH` permanently.
- For semantic code navigation/edits prefer **Serena** symbol tools over raw grep/read where available.

## Table of contents

1. [Database Schema & Migrations](#database-schema--migrations) — the data model = the queue
2. [Domain Interfaces (`internal/transfer`, `internal/jobs`)](#domain-interfaces-internaltransfer-internaljobs) — Source/Transport contract + sentinels + Enqueue
3. [Worker Pipeline (`internal/worker`)](#worker-pipeline-internalworker) — lease→process→complete lifecycle
4. [Sources & Transports](#sources--transports-io-adapters) — localfs/ftp I/O adapters + FTP session pool
5. [Sniffer / DB Connector (`internal/sniffer`)](#sniffer--db-connector-internalsniffer) — incremental ingest + watermark
6. [Sweeper & Eval Suite](#sweeper--eval-suite-internalsweeper-internaleval) — lease recovery + executable invariants
7. [Infra Adapters](#infra-adapters-db-sourcedb-health-hostcap-backoff-metrics-ftpserver) — pools, health, hostcap, metrics
8. [CLI & Entry Points (`cmd/imgsync`)](#cli--entry-points-cmdimgsync) — subcommand wiring + signals
9. [Configuration & Environment Variables](#configuration--environment-variables) — every env var + flag
10. [Build, Packaging & Deployment](#build-packaging--deployment) — Docker / Helm / Makefile / CI
11. [Testing Strategy, Build Tags & E2E](#testing-strategy-build-tags--e2e) — unit / integration / E2E + guards
12. [Repository & Docs Map](#repository--docs-map-where-to-find-more) — where to find authoritative detail

---

## Database Schema & Migrations

The Postgres schema **is** the queue — imgsync has no broker, no Redis, no in-memory state. Every job's lifecycle, lease, retry, and audit trail lives in two tables driven by `SELECT ... FOR UPDATE SKIP LOCKED`. This is the highest-leverage area in the repo: any agent writing a query, adding a column, or reasoning about worker/sweeper/monitoring behavior must treat these `.sql` files as ground truth. Migrations are plain numbered SQL pairs (`NNNN_name.up.sql` / `.down.sql`) applied in order; each up-migration self-registers via `INSERT INTO schema_migrations (version)`.

### Key files & symbols
- `migrations/0001_initial.up.sql` — `job_status` enum, `transfer_jobs`, `transfer_events`, `schema_migrations`, all initial indexes. **No `0001_initial.down.sql` exists** (initial schema is not reversible by design).
- `migrations/0002_sniffer_state.{up,down}.sql` — `sniffer_state` watermark table (one row per polled source); has a down migration.
- `migrations/0003_jobs_status_index.{up,down}.sql` — adds `transfer_jobs_status_idx` (single-column b-tree on `status`) for the monitoring scrape.
- `internal/worker/job.go:LeaseJob` — the dispatch SQL: claims oldest due `pending` row via a CTE with `FOR UPDATE SKIP LOCKED LIMIT 1`, flips to `leased`. Returns `(nil, nil)` on empty queue (`pgx.ErrNoRows`). `Job` struct is the scanned row snapshot.
- `internal/worker/process.go` — terminal/retry writes: `writeSuccess` (status `succeeded`, event `success`); `classifyAndWrite` routes `ErrSkippable`→`writeTerminal`(`skipped`) / `ErrPermanent`→`writeTerminal`(`dead`, attempts bumped) / otherwise `writeRetryOrDead`. Each transition appends a `transfer_events` row in the **same tx** as the `transfer_jobs` UPDATE.
- `internal/sweeper/sweeper.go:Sweep` — recovers crashed leases: `UPDATE ... SET status='pending' ... WHERE status='leased' AND locked_at < NOW() - $1::INTERVAL`, then INSERTs an `expire` event per recovered row. Single-writer guarded by `pg_try_advisory_xact_lock(hashtext('imgsync_sweeper'))` (tx-scoped); default threshold 5m, interval 30s.
- `internal/transfer/errors.go:ErrSkippable,ErrPermanent` — Go-side sentinels that decide `skipped` vs `dead`.
- `internal/metrics/lease_lock_age.go:newLeaseLockAge` — scrapes `WHERE status='leased'` (`MIN(locked_at)`) for gauge `imgsync_lease_lock_age_seconds`.
- `internal/metrics/queue_depth.go:queueDepthCollector` — `GROUP BY status` scrape for `imgsync_jobs_in_status{status}`.

### How it works / flow

**Two-Table Minimal design.** Mutable queue state and immutable audit history are split:
- `transfer_jobs` — current state, one row per job, updated in place (`status`, `attempts`, `locked_*`, `next_run_at`).
- `transfer_events` — append-only log, one row per state transition, `job_id BIGINT REFERENCES transfer_jobs(id) ON DELETE CASCADE`. Workers never UPDATE events; they only INSERT.

**`transfer_jobs` (0001)** — every column:
```sql
CREATE TYPE job_status AS ENUM ('pending','leased','succeeded','skipped','dead');

CREATE TABLE transfer_jobs (
    id            BIGSERIAL PRIMARY KEY,
    trace_id      TEXT        NOT NULL,
    src           TEXT        NOT NULL,
    dst           TEXT        NOT NULL,
    src_protocol  TEXT        NOT NULL,
    dst_protocol  TEXT        NOT NULL,
    payload       JSONB       NOT NULL DEFAULT '{}'::JSONB,
    status        job_status  NOT NULL DEFAULT 'pending',
    attempts      INT         NOT NULL DEFAULT 0,
    max_attempts  INT         NOT NULL DEFAULT 5,
    locked_at     TIMESTAMPTZ,
    locked_by     TEXT,
    next_run_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)  -- enqueue idempotency
);
CREATE INDEX transfer_jobs_pending_idx ON transfer_jobs (next_run_at, id) WHERE status = 'pending'; -- lease path
CREATE INDEX transfer_jobs_leased_idx  ON transfer_jobs (locked_at)       WHERE status = 'leased';  -- sweeper path
CREATE INDEX transfer_jobs_trace_id_idx ON transfer_jobs (trace_id);
```

**`transfer_events` (0001)** — note: `status` here is a **`TEXT` with CHECK**, NOT the `job_status` enum, and uses a *different vocabulary* (verbs, not states):
```sql
CREATE TABLE transfer_events (
    id        BIGSERIAL PRIMARY KEY,
    trace_id  TEXT        NOT NULL,
    job_id    BIGINT      NOT NULL REFERENCES transfer_jobs(id) ON DELETE CASCADE,
    ts        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status    TEXT        NOT NULL,
    detail    JSONB       NOT NULL DEFAULT '{}'::JSONB,
    CONSTRAINT transfer_events_status_check
        CHECK (status IN ('enqueue','lease','success','skip','fail','expire','dead'))
);
CREATE INDEX transfer_events_job_id_idx ON transfer_events (job_id);
CREATE INDEX transfer_events_trace_id_ts_idx ON transfer_events (trace_id, ts);
```

**`schema_migrations` (0001)** — `version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`. Each up-migration inserts its own version string (`'0001_initial'`, `'0002_sniffer_state'`, `'0003_jobs_status_index'`); each down deletes it.

**`sniffer_state` (0002)** — poll watermark, `source_id TEXT PRIMARY KEY`, `last_run_ts TIMESTAMPTZ NOT NULL`, `last_run_pk TEXT` (tie-break key, NULL on first poll), `updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`. v1 assumes a single sniffer pod — no advisory lock on this table (per table COMMENT).

**Status state machine** (`job_status` enum, 5 values). Terminal states = `succeeded`, `skipped`, `dead`:
```
pending ──LeaseJob──▶ leased ──success──▶ succeeded   (writeSuccess: status='succeeded')
   ▲                    │
   │                    ├──ErrSkippable──▶ skipped     (classifyAndWrite→writeTerminal, attempts NOT bumped)
   │                    ├──ErrPermanent──▶ dead         (classifyAndWrite→writeTerminal, attempts bumped)
   │                    ├──retryable err & attempts+1 < max_attempts──┐
   │                    │   next_run_at = NOW()+2^(attempts+1) s       │
   └────────────────────┘  (backoff 2,4,8,16,32...) ◀─────────────────┘
   ▲                    └──retryable err & attempts+1 >= max_attempts──▶ dead
   │
   └── leased ──(crash/timeout)──▶ pending   (sweeper.go: locked_at < NOW()-threshold; writes 'expire' event)
```
- **Lease** (`LeaseJob`): CTE picks `WHERE status='pending' AND next_run_at <= NOW() ORDER BY next_run_at, id FOR UPDATE SKIP LOCKED LIMIT 1`, then `UPDATE ... SET status='leased', locked_at=NOW(), locked_by=$1, updated_at=NOW()`. Served by `transfer_jobs_pending_idx`.
- **Sweeper** requeues stale leases (`leased` → `pending`, clearing `locked_*`) and INSERTs a `transfer_events` row with `status='expire'`, `detail='{"reason":"lease_expired"}'`. Served by `transfer_jobs_leased_idx`.
- On every terminal/retry transition, `locked_at`/`locked_by` are set back to `NULL` and `updated_at=NOW()`.

**`transfer_jobs_status_idx` (0003)** — single-column b-tree on `status`. Serves the **monitoring scrape**, not the lease path: `SELECT status::text, COUNT(*)::bigint FROM transfer_jobs GROUP BY status` (metric `imgsync_jobs_in_status{status}`). Without it, that GROUP BY degrades to a full heap scan once `succeeded`/`skipped` rows accumulate; the b-tree lets PG do an index-only scan + HashAggregate. The two partial indexes (`pending_idx`, `leased_idx`) do not cover terminal states, which is why a dedicated full-coverage index was needed.

### Agent notes (gotchas, conventions, constraints)
- **Two distinct status vocabularies.** `transfer_jobs.status` is the `job_status` **enum** (`pending/leased/succeeded/skipped/dead`). `transfer_events.status` is a **TEXT CHECK** with verbs (`enqueue/lease/success/skip/fail/expire/dead`). Do not cross them. There is **no `processing`** value — `e2e/helpers.go:239` carries an explicit `Bug fix #2` comment about this; use `leased`.
- **Enum changes are migration-only.** Adding a status value requires `ALTER TYPE job_status ADD VALUE` in a new migration. Postgres restricts `ADD VALUE` inside a transaction in older versions — verify before wrapping in `BEGIN`.
- **`0001` has no down migration.** Don't assume a symmetric down exists; only `0002` and `0003` are reversible.
- **Idempotent enqueue** rests on `transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)`. Removing/altering it breaks dedup. The same `(trace_id, dst)` pair cannot be enqueued twice.
- **Indexes are query-shaped, not decorative.** `pending_idx` is `(next_run_at, id) WHERE status='pending'` to match the lease `ORDER BY next_run_at, id`. If you change the lease ORDER BY or WHERE, update this partial index or the queue silently does seq scans under load. Same for `leased_idx` ↔ sweeper (`locked_at`).
- **`status_idx` (0003) is for metrics, not lease.** Don't "consolidate" it into the partial indexes — those exclude terminal rows that the GROUP BY scrape must count.
- **Migrations self-register.** Any new `NNNN_*.up.sql` must `INSERT INTO schema_migrations (version) VALUES ('NNNN_name')` and the `.down.sql` must `DELETE` it, matching the existing pattern. **Only the `.up.sql` files wrap their body in `BEGIN;/COMMIT;`** — the existing `0002`/`0003` `.down.sql` files are bare statements (no transaction wrapper); match that convention.
- **`ON DELETE CASCADE`**: deleting a `transfer_jobs` row drops its `transfer_events`. Events are not independently retained — don't rely on them surviving job deletion.
- **Backoff is computed in Go** (`writeRetryOrDead`: `backoff = 1<<nextAttempts` seconds where `nextAttempts = attempts+1`, i.e. `2^(attempts+1)` → 2,4,8,16,32...), not in SQL/DB defaults; `next_run_at` is set explicitly per retry via `NOW()+$3::INTERVAL`.

Relevant absolute paths: `/Users/nineking/workspace/app/imgsync/migrations/{0001_initial.up,0002_sniffer_state.up,0002_sniffer_state.down,0003_jobs_status_index.up,0003_jobs_status_index.down}.sql`; lease/transition logic in `/Users/nineking/workspace/app/imgsync/internal/worker/{job.go,process.go}` and `/Users/nineking/workspace/app/imgsync/internal/sweeper/sweeper.go`.

## Domain Interfaces (internal/transfer, internal/jobs)

The core domain boundary every source, transport, and worker obeys. `internal/transfer` defines two streaming interfaces (`Source`, `Transport`) and two sentinel errors (`ErrSkippable`, `ErrPermanent`) that drive terminal job-state classification. `internal/jobs` is the single idempotent entry point for inserting a `transfer_jobs` row. An AI agent adding a new protocol or worker behavior touches these contracts first — they are deliberately tiny and load-bearing.

### Key files & symbols
- `internal/transfer/interfaces.go:Source` — `Open(ctx, src) (body io.ReadCloser, size int64, err error)`; opens a streaming reader. Caller MUST Close. `size` is source byte count or `-1` if unknown.
- `internal/transfer/interfaces.go:Transport` — `Send(ctx, dst, body io.Reader, expectedSize int64) (writtenBytes int64, sha256Hex string, err error)`; streams body to dst, counts bytes + computes sha256.
- `internal/transfer/errors.go:ErrSkippable` — sentinel → job marked `skipped` (terminal, audit-only).
- `internal/transfer/errors.go:ErrPermanent` — sentinel → job marked `dead` immediately, bypassing retry budget.
- `internal/jobs/enqueue.go:EnqueueArgs` — input struct: `TraceID, Src, Dst, SrcProtocol, DstProtocol string; Payload []byte; MaxAttempts int`.
- `internal/jobs/enqueue.go:Enqueue` — `func(ctx, pool *pgxpool.Pool, a EnqueueArgs) (int64, bool, error)` → `(id, inserted, err)`; idempotent insert.
- `internal/worker/process.go:classifyAndWrite` — consumer that maps the sentinels to job states (the contract's enforcement point). Called from `ProcessJob`.

### How it works / flow

**Interfaces (exact signatures):**
```go
type Source interface {
    Open(ctx context.Context, src string) (body io.ReadCloser, size int64, err error)
}
type Transport interface {
    Send(ctx context.Context, dst string, body io.Reader, expectedSize int64) (writtenBytes int64, sha256Hex string, err error)
}
```
Both contracts MUST NOT buffer the full body in memory (streaming-only; there is a CI guard against in-memory buffering). `Source.Open` returns `size` = reported byte count or `-1` if unknown; the worker passes that `size` to `Transport.Send` as `expectedSize`. `Send` returns the actual `writtenBytes` and `sha256Hex` over the streamed bytes, which the worker uses for size verification (labeled **F4** in `process.go`).

**Size verification has two branches** (`internal/worker/process.go`, both → `ErrPermanent`):
- `srcSize >= 0`: if `written != srcSize` → `ErrPermanent` (`reason: size_mismatch`, line ~51).
- `srcSize < 0` (unknown): compares bytes *read* through the worker's `counter` vs `written`; if `cw.n != written` → `ErrPermanent` (`reason: size_mismatch_unknown_src`, line ~57).

**Sentinel error semantics** (consumed in `internal/worker/process.go:classifyAndWrite` via `errors.Is`):
- `errors.Is(err, transfer.ErrSkippable)` → `writeTerminal(... "skipped", "skip", detail, false)` — terminal, attempts NOT bumped, `transfer_events` gets a `skip` row.
- `errors.Is(err, transfer.ErrPermanent)` → `writeTerminal(... "dead", "dead", detail, true)` — terminal, attempts bumped, bypasses retry budget.
- default (any other error) → `writeRetryOrDead`: `nextAttempts = attempts+1`; if `nextAttempts >= MaxAttempts` → `dead`; else status back to `pending` with exponential backoff `next_run_at = NOW() + (1<<nextAttempts) seconds` (2,4,8,16,32...) and a `fail` event.
- Also: a successful `Send` followed by a non-nil `body.Close()` is treated as a retryable transport-class error (re-enters `classifyAndWrite` as a non-sentinel error → retry budget).

Producers wrap sentinels with `fmt.Errorf("...: %w", transfer.ErrSkippable)` (e.g. `internal/sources/localfs/source.go:28` for missing file via `os.ErrNotExist`, `internal/sources/ftp/source.go:57` for RETR not-found; `ErrPermanent` for directories (`localfs:33`), malformed/unsupported FTP URIs (`ftp:28-39`), and worker size-mismatch (`process.go:51/57`)).

**Enqueue flow** (`internal/jobs/enqueue.go:Enqueue`):
1. Validates required fields: `TraceID`, `Src`, `Dst` non-empty (one error) and `SrcProtocol`/`DstProtocol` non-empty (separate error), else returns a plain `errors.New`.
2. Defaults: `MaxAttempts <= 0` → `5`; empty/nil `Payload` (`len(payload)==0`) → `` []byte(`{}`) ``.
3. Opens a transaction (`pool.BeginTx(ctx, pgx.TxOptions{})`, deferred `Rollback`), runs:
   `INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol, payload, max_attempts) VALUES ($1..$7) ON CONFLICT (trace_id, dst) DO NOTHING RETURNING id`.
4. On insert success (`scanErr == nil`): also inserts `INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,'enqueue',$3)` (status literal `'enqueue'`, `detail` = the payload bytes), commits, returns `(id, true, nil)`.
5. On conflict (`scanErr` is `pgx.ErrNoRows`): re-`SELECT id FROM transfer_jobs WHERE trace_id=$1 AND dst=$2`, commits, returns `(id, false, nil)` — the existing row id. Any other `scanErr` returns a wrapped `insert:` error.

**Idempotency key invariant:** uniqueness is the `(trace_id, dst)` tuple, enforced by constraint `transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)` (migration `0001_initial`). `inserted=false` means the tuple already existed; the returned `id` is the existing row.

### Agent notes (gotchas, conventions, constraints)
- The streaming contract is sacred: NEVER `io.ReadAll`/`ioutil.ReadAll` or buffer the whole body in a `Source` or `Transport` impl. The CI guard `scripts/check-streaming.sh` greps regex `\b(io|ioutil)\.ReadAll\b|bytes\.NewBuffer\b.*\bbody\b` across `internal/sources`, `internal/transports`, `internal/transfer` (NOT `internal/worker`), skipping `*_test.go` and `//`-comment lines; matches fail the build.
- Sentinel errors are matched with `errors.Is`, so you MUST wrap with `%w` (not `%s`/`%v`) when returning them, or the worker silently falls through to the retry path.
- Choosing the wrong sentinel changes terminal state and the audit trail: `ErrSkippable` → `skipped` (attempts unchanged, `skip` event), `ErrPermanent` → `dead` (attempts bumped, no retries, `dead` event). A non-sentinel error consumes retry budget (`fail` event, or `dead` event once exhausted). Pick deliberately.
- `Source.Open` callers MUST `Close()` the returned `ReadCloser`; document/honor this in new impls. The worker also treats a post-`Send` close error as retryable.
- `size == -1` (unknown) is a valid, expected value end-to-end — don't assume a positive size in `Transport.Send`; the worker falls back to read-vs-written equality when src size is unknown.
- `Enqueue` requires `SrcProtocol` and `DstProtocol`; they are NOT inferred from the URI. Forgetting them returns a validation error, not a silent default.
- `Enqueue` is the ONLY sanctioned insert path for `transfer_jobs` — do not hand-roll inserts; it guarantees idempotency and the paired `enqueue` event in one transaction. (`transfer_jobs` carries indexes `transfer_jobs_pending_idx`, `transfer_jobs_leased_idx`, `transfer_jobs_trace_id_idx`, plus `transfer_jobs_status_idx` from migration `0003_jobs_status_index` for monitoring scrapes.)
- `job_status` ENUM values are exactly `pending, leased, succeeded, skipped, dead` (no `running`/`failed`). `transfer_events.status` is a `TEXT` CHECK over `enqueue, lease, success, skip, fail, expire, dead`.
- LocalFS convention precedent: missing source file → `ErrSkippable` (not `ErrPermanent`); FTP source follows the same pattern (RETR not-found → `ErrSkippable`, but a bare `550 Permission/access denied` deliberately falls through to a non-sentinel error). Match this when adding sources.

## Worker Pipeline (`internal/worker`)

The heart of job execution: a pool of goroutines leases pending rows from the `transfer_jobs` Postgres queue, streams bytes Source→Transport, verifies size, and writes back a terminal status plus an audit event — all without buffering the body in memory. An AI agent editing here must respect the queue lease protocol, the error-class→status mapping, and the exactly-once body-close discipline; getting any of these wrong silently corrupts the queue or leaks file handles.

### Key files & symbols
- `internal/worker/runner.go:Runner` — struct holding `Pool`, `Workers`, `PodName`, `IdleBackoff`, and the `SourceFor`/`TransportFor func(protocol string)` factories plus nil-safe hooks (`OnFinish`, `OnLeaseAttempt`, `OnWorkerStart`, `OnWorkerStop`).
- `internal/worker/runner.go:Runner.Run` — defaults `Workers` to 4, `IdleBackoff` to `backoff.NewIdle(backoff.Config{})`, `PodName` to `"imgsync-worker"`; spawns `Workers` goroutines, blocks on `WaitGroup` until ctx cancel.
- `internal/worker/runner.go:Runner.loop` — per-worker lease→dispatch→process loop; `recover()`s panics to stderr; computes `lockedBy = "<PodName>-w<idx>"`.
- `internal/worker/runner.go:Runner.fire` / `emitStart` / `emitStop` — nil-safe wrappers calling `OnFinish` / `OnWorkerStart` / `OnWorkerStop`.
- `internal/worker/runner.go:SourceLike` / `TransportLike` — type aliases (`=`) for `transfer.Source` / `transfer.Transport`.
- `internal/worker/runner.go:ErrUnknownProtocol` — `errors.New("unknown protocol")`, returned by the factories for unregistered protocols.
- `internal/worker/job.go:Job` — snapshot of a `transfer_jobs` row at lease time (fields below).
- `internal/worker/job.go:LeaseJob(ctx, pool, lockedBy)` — the `FOR UPDATE SKIP LOCKED` claim+lease SQL; returns `(nil, nil)` on empty queue.
- `internal/worker/job.go:Job.Duration` — worker-side "lease→now" elapsed (0 if `LockedAt` nil); doc-comment points at `imgsync_lease_lock_age_seconds` for the in-DB lease age.
- `internal/worker/process.go:ProcessJob(ctx, Deps, *Job) error` — drives one job to terminal status; `Deps{Pool, LockedBy, Source, Transport}`.
- `internal/worker/process.go:classifyAndWrite` — maps error class to status (skipped/dead/retry).
- `internal/worker/process.go:writeRetryOrDead`, `writeTerminal`, `writeTerminalWithAttempts`, `writeSuccess` — the status-writeback transactions.
- `internal/worker/process.go:counter` — `io.Reader` wrapper counting bytes read into field `n` (for unknown-size verification).
- `internal/transfer/interfaces.go:Source.Open(ctx, src) (io.ReadCloser, int64, error)` / `Transport.Send(ctx, dst, io.Reader, expectedSize int64) (writtenBytes int64, sha256Hex string, err error)` — streaming contract: never buffer body in memory; `Open` returns `size=-1` if unknown.
- `internal/transfer/errors.go:ErrSkippable` / `ErrPermanent` — sentinel errors steering classification.
- `cmd/imgsync/worker.go` (L83-101) — concrete factory wiring: `switch proto { "localfs", "ftp" }`, else `ErrUnknownProtocol`; FTP transport wrapped in `hostcap.Wrap`.

### Job model fields (`Job`)
`ID int64`, `TraceID string`, `Src string`, `Dst string`, `SrcProtocol string`, `DstProtocol string`, `Payload []byte`, `Status string`, `Attempts int`, `MaxAttempts int`, `LockedAt *time.Time`, `LockedBy string`, `NextRunAt time.Time`, `CreatedAt time.Time`, `UpdatedAt time.Time` (15 fields). Columns map 1:1 to `transfer_jobs` (`src_protocol`, `dst_protocol`, `next_run_at`, etc.). DB defaults: `max_attempts=5`, `status='pending'`.

### How it works / flow
1. **Lease (`LeaseJob`).** Single CTE statement: `WITH next AS (SELECT id FROM transfer_jobs WHERE status='pending' AND next_run_at <= NOW() ORDER BY next_run_at, id FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE transfer_jobs j SET status='leased', locked_at=NOW(), locked_by=$1, updated_at=NOW() FROM next WHERE j.id = next.id RETURNING …` the full row. `SKIP LOCKED` lets N workers claim disjoint rows concurrently. `pgx.ErrNoRows` → `(nil, nil)` (empty queue, not an error); any other error is wrapped as `lease: %w`.
2. **Loop backoff (`Runner.loop`).** On lease error → log to stderr, `OnLeaseAttempt(false)`, `IdleBackoff.WaitOnce(ctx)`. On `nil` job (empty) → same backoff (inline `TODO(F2)`: DB-error and empty-queue currently share one backoff schedule). On a real job → `OnLeaseAttempt(true)`, `IdleBackoff.WakeAll()`.
3. **Protocol dispatch.** `SourceFor(job.SrcProtocol)` and `TransportFor(job.DstProtocol)`. A factory error (e.g. `ErrUnknownProtocol`) → immediate `writeTerminal(..., "dead", "dead", {error, stage:"source-factory"|"transport-factory"}, bumpAttempts=true)`, then `fire(job)` and `continue` — it never reaches `ProcessJob`.
4. **Process (`ProcessJob`).** `Source.Open` (open error → `classifyAndWrite` with `openErrDetails`) → wrap body in `counter` → `Transport.Send(ctx, job.Dst, cw, srcSize)`. **Size verification (F4/D6):** if `srcSize >= 0` require `written == srcSize`; if `srcSize < 0` require `cw.n == written`; mismatch → `fmt.Errorf("…: %w", transfer.ErrPermanent)` (→ dead). A post-send `body.Close()` error is treated as a **retryable transport-class** failure (e.g. FTP 226 not received cleanly) with `stage:"source_close"`. Success → `writeSuccess`.
5. **Classification (`classifyAndWrite`).** `errors.Is(err, ErrSkippable)` → status `skipped` / event `skip` (attempts NOT bumped). `errors.Is(err, ErrPermanent)` → status `dead` / event `dead` (attempts bumped). Otherwise → `writeRetryOrDead`.
6. **Retry/backoff (`writeRetryOrDead`).** `nextAttempts = Attempts+1`; if `>= MaxAttempts` → terminal `dead` via `writeTerminalWithAttempts`. Else `UPDATE … SET status='pending', attempts=$2, next_run_at=NOW()+$3::INTERVAL, locked_at=NULL, locked_by=NULL, updated_at=NOW()` where backoff = `1<<nextAttempts` seconds (2, 4, 8, 16, 32…), and inserts a `transfer_events` row with status `fail`.
7. **Writeback transactionality.** Every terminal/retry/success write is one tx that `UPDATE`s `transfer_jobs` AND `INSERT`s into `transfer_events (trace_id, job_id, status, detail)` so the audit event is committed atomically with the row. Note the event vocabulary differs from the job vocabulary: success writes job status `succeeded` but event status `success`; retry writes job `pending` + event `fail`; skip/dead align as `skipped`/`skip` and `dead`/`dead`. (`transfer_events.status` CHECK: `enqueue,lease,success,skip,fail,expire,dead`.) Success/terminal/retry all clear `locked_at`/`locked_by`. `detail` is a JSONB map; `writeSuccess` carries `size`, `sha256`, `duration_ms`; failure paths carry `error`, `stage`, and `reason`/byte counts (`src_size`/`written`/`read`).

### Agent notes (gotchas, conventions, constraints)
- **Exactly-once body close.** `ProcessJob` uses a `closed bool` + deferred close. The success path and the source-close-error path set `closed=true` after an explicit `body.Close()`. If you add an early-return between `Open` and the final close, ensure the defer still covers it — double-close or leaked handles are silent bugs.
- **Error class is the control plane.** Status routing is driven entirely by `errors.Is` against `transfer.ErrSkippable` / `transfer.ErrPermanent`. To make a failure terminal vs retryable, wrap with `%w` and one of those sentinels (see `process.go` size-mismatch). Anything not wrapping a sentinel defaults to retry-with-backoff. Do NOT compare error strings.
- **`ProcessJob` swallows job-level outcomes.** It returns a non-nil error ONLY on DB write failure; a "dead"/"skipped"/"failed" job is a successful return. The `loop` ignores `ProcessJob`'s return (`_ =`). Don't add logic expecting the return value to signal job failure.
- **Attempts semantics differ by path.** `skipped` does NOT bump `attempts`; `dead`/`fail` do. The backoff formula `1<<nextAttempts` is inline in `writeRetryOrDead` — keep it consistent if you touch it.
- **Lease SQL ordering is load-bearing.** `WHERE status='pending' AND next_run_at <= NOW() ORDER BY next_run_at, id` + `FOR UPDATE SKIP LOCKED LIMIT 1` is the fairness/concurrency contract. Removing `SKIP LOCKED` would serialize all workers; removing the `next_run_at <= NOW()` predicate would re-run jobs still in backoff. The supporting index is the **partial** `transfer_jobs_pending_idx ON transfer_jobs (next_run_at, id) WHERE status='pending'` (migration 0001) — it matches the predicate + ORDER BY. (Do NOT confuse with `transfer_jobs_status_idx` from migration 0003: that single-column `(status)` index exists for the monitoring `GROUP BY status` scrape, not for leasing.)
- **Factories are injected, not registered globally.** Protocol→impl mapping lives in `cmd/imgsync/worker.go` (`switch proto`), not in `internal/worker`. Adding a protocol means editing both `SourceFor` and `TransportFor` there; `internal/worker` only knows the `SourceLike`/`TransportLike` aliases and `ErrUnknownProtocol`.
- **Hooks are nil-safe and metrics-facing.** `OnLeaseAttempt`/`OnWorkerStart`/`OnWorkerStop`/`OnFinish` are wired in `cmd/imgsync/worker.go` to `internal/metrics` (`m.OnLeaseAttempt`, `m.SetWorkersActive`, `m.OnJobFinished(src, dst, status, dur)`); all are guarded with nil checks. `OnFinish` (invoked via `fire`) is also documented as a test hook. Keep them optional.
- **Worker identity format.** `lockedBy = "<PodName>-w<idx>"` (e.g. `imgsync-worker-w0`); lands in `locked_by` and panic logs — don't change the format casually (monitoring may parse it). `Runner.Run` defaults an empty `PodName` to `"imgsync-worker"`, but the `worker` CLI sets `PodName` from `IMGSYNC_POD_NAME` or the OS hostname, so that literal default only appears when `Runner` is driven without the CLI.
- Streaming invariant from `interfaces.go`: implementations MUST NOT buffer the whole body in memory; there is a CI streaming-guard concern around `io.ReadAll`/`bytes.NewBuffer` patterns — new Source/Transport code should stay streaming.

## Sources & Transports (I/O adapters)

Concrete I/O adapters implementing the `transfer.Source` and `transfer.Transport` interfaces for `localfs` and `ftp` on both the read (Source) and write (Transport) sides. Everything here is **streaming-only** — bodies must flow through `io.Reader`/`io.ReadCloser` and never be fully buffered (a CI guard enforces this). The FTP side adds a per-host connection pool that is a PRD-core requirement. An AI agent editing these files must preserve the streaming contract, the pooled-conn lifecycle, and the `os.ErrNotExist`/550 → `ErrSkippable` classification.

### Key files & symbols
- `internal/transfer/interfaces.go:Source` / `Transport` — the two interfaces both packages implement. `Source.Open(ctx, src) (io.ReadCloser, size, err)`; `Transport.Send(ctx, dst, body io.Reader, expectedSize) (writtenBytes, sha256Hex, err)`. Doc comments say "MUST NOT buffer the body in memory." `Transport` doc notes the worker uses written-bytes + sha256 for "D6 size verification."
- `internal/transfer/errors.go:ErrSkippable` / `ErrPermanent` — sentinel errors adapters wrap with `%w` to signal worker behavior. `ErrSkippable` → mark job `skipped` (terminal, audit-only); `ErrPermanent` → mark job `dead` immediately, bypassing the retry budget.
- `internal/sources/localfs/source.go:Source.Open` — `os.Stat` then `os.Open`, returns the `*os.File` as the `io.ReadCloser`; caller Closes.
- `internal/sources/ftp/source.go:Source.Open` — parses `ftp://host[:port]/path`, `Pool.Acquire` → `FileSize` (SIZE) → `Retr` (RETR), wraps result in `retrReader`.
- `internal/sources/ftp/source.go:retrReader` — `io.ReadCloser` wrapper whose `Close()` calls `pc.Release(broken)`; tracks `ioErr` (and a `released` bool) to mark the conn broken and guard against double-Release.
- `internal/sources/ftp/source.go:isNotFound` (+ `IsNotFoundForTest`) — classifies RETR errors into the skippable bucket.
- `internal/transports/localfs/transport.go:Transport.Send` — temp file (`os.CreateTemp(dir, ".imgsync-*.tmp")`) + `io.MultiWriter(tmp, sha256)` + `tmp.Sync()` + atomic `os.Rename`.
- `internal/transports/ftp/transport.go:Transport.Send` — STOR to `finalPath+".imgsync.tmp"` then `conn.Rename` (RNFR/RNTO); `countingHashWriter` via `io.TeeReader` counts bytes + hashes.
- `internal/transports/ftp/pool.go:Pool` / `PooledConn` / `hostPool` / `idleEntry` — the per-host pool. Exported surface: `Pool.Acquire`, `PooledConn.Release(broken)`, `PooledConn.Conn()`, `PoolConfig`, `NewPool`, `Pool.Close`, `Pool.IdleCount`, `ErrPoolClosed`. Internal helpers: `Pool.release`, `Pool.getHost`, `Pool.dial`, `wakeOneWaiter`.

### How it works / flow

**LocalFS Source (`source.go:Open`):** checks `ctx.Err()`, `os.Stat`. On stat `os.ErrNotExist` → wraps `transfer.ErrSkippable`; other stat errors pass through raw. A directory → `transfer.ErrPermanent`. Otherwise `os.Open` and returns `(f, st.Size(), nil)`. The `*os.File` itself is the streaming reader.

**FTP Source (`source.go:Open`):** validates URL (`scheme == "ftp"`, non-empty host and path — all failures, including a `url.Parse` error, → `ErrPermanent`). `pool.Acquire(ctx, host)`, then `conn.FileSize(path)` (size defaults to `-1` if SIZE fails — non-fatal), then `conn.Retr(path)`. On RETR failure: `pc.Release(true)` (mark broken), and if `isNotFound(err)` → `ErrSkippable`, else raw error. Success returns a `*retrReader`. `retrReader.Read` records any non-EOF error into `ioErr`; `Close()` closes the underlying RETR stream and `Release(broken)` where `broken = ioErr != nil || closeErr != nil`, guarded by `released` so Close is idempotent. **Invariant: the pooled conn is released exactly once — on Close for the success path, immediately on the RETR error path.** Forgetting to Close a `retrReader` leaks a pool slot.

**`isNotFound` classifier:** lowercases the error string. Skippable if it contains `no such file` / `not found` / `file unavailable` / `does not exist`, OR contains `550` **and not** `permission`/`access denied`. Bare 550s are skippable; 550-with-permission falls through to the raw error so the worker treats it as misconfiguration (not silently skipped).

**LocalFS Transport (`transport.go:Send`):** `ctx.Err()` check → `os.CreateTemp(filepath.Dir(dst), ".imgsync-*.tmp")` → `io.Copy(io.MultiWriter(tmp, sha256), body)` → `tmp.Sync()` (fsync) → `tmp.Close()` → atomic `os.Rename(tmpPath, dst)`. Every failure path after temp creation removes the temp file (`cleanupTmp`). Returns `(written, lowercase-hex sha256, nil)`. Atomicity: dst never appears partially written.

**FTP Transport (`transport.go:Send`):** validates `ftp://host/path` (any malformation — parse error, non-`ftp` scheme, empty host or path → plain error, **not** an `ErrPermanent` wrap). Wire tmp path during transfer is `finalPath + ".imgsync.tmp"`. `pool.Acquire`, best-effort single-level `conn.MakeDir(path.Dir(finalPath))` (skipped when dir is `/`, `.`, or empty; multi-level parents must pre-exist; recursive mkdir intentionally omitted for v1). `countingHashWriter{h: sha256}` + `io.TeeReader(body, cw)` feeds `conn.Stor(tmp, tee)` — bytes/sha are computed as they stream. Each wire call (`Stor`/`Delete`/`Rename`) is `strings.TrimPrefix(.., "/")`-ed. On STOR ACK: `conn.Rename(tmp, final)` (RNFR/RNTO). On STOR or Rename error: best-effort `conn.Delete(tmp)`, then `pc.Release(true)`. Success: `pc.Release(false)`, returns `(cw.n, hex sha256, nil)`. Note `countingHashWriter.Write` counts the bytes the hasher accepted (`w.n += n`).

**FTP Pool (`pool.go`) — keying/reuse/eviction/concurrency:**
- **Keying:** by `host` string (the full `u.Host`, including any `:port`). `hosts map[string]*hostPool`, lazily created in `getHost`.
- **Capacity:** `MaxPerHost` caps `inUse` per host (default 4). `inUse` is incremented before dialing and counts checked-out conns; idle conns are NOT counted in `inUse`.
- **Acquire** loops: pop newest idle (LIFO, from tail of `hp.idle`); if `time.Since(enqueue) > IdleTTL` (default 5m) → `Quit()` and keep scanning; else take it, `inUse++`, and set `needsPing` if `time.Since(lastUse) > NoopAfter` (default 60s). If `needsPing`, run `NoOp()` **outside the lock**; on NOOP failure `Quit()`, decrement `inUse`, `wakeOneWaiter`, and `continue` the loop (retry idle or dial). If no idle and `inUse < MaxPerHost`: `inUse++`, dial outside lock (`p.dial` → `ftp.Dial` with `DialWithTimeout`/`DialWithContext` + `Login`); dial failure rolls back `inUse`, wakes a waiter, returns the error. At cap: append a buffered (cap-1) `chan struct{}` to `hp.waiters`, unlock, and block on `select{ ctx.Done() | ch }`. Ctx cancellation does best-effort waiter eviction (and, if `release` already popped it, drains the stolen token from `ch` and forwards it via `wakeOneWaiter`).
- **Reuse window:** two clocks per `idleEntry` — `enqueue` (IdleTTL eviction) and `lastUse` (NoopAfter liveness ping). `lastUsed` on `PooledConn` is informational only.
- **release (`pool.go:release`):** if host pool is gone, `Quit()` and return. Else `inUse--`; if `broken || pool.closed` → `Quit()`; else append a fresh `idleEntry` (both clocks = `now`). Always `wakeOneWaiter`.
- **Eviction:** stale-idle on Acquire (IdleTTL), broken-on-release, NOOP-failure, dial-failure, and `Close()` (Quits all idle; in-use conns are Quit when their holder calls Release, because `closed` is set).
- **DialTimeout:** `PoolConfig.DialTimeout` defaults to 10s when ≤0 (set in `NewPool`).
- **Observability:** `PoolConfig.OnPoolChange(host, inUse, idle)` is invoked (nil-checked) on every count change, always **outside** `p.mu`, with snapshot values captured under lock. It must stay O(1) — it runs on the Acquire/Release hot path.

### Agent notes (gotchas, conventions, constraints)
- **CI streaming guard — hard gate.** `scripts/check-streaming.sh` greps `internal/sources`, `internal/transports`, `internal/transfer` for `\b(io|ioutil)\.ReadAll\b` OR `bytes\.NewBuffer\b.*\bbody\b` in non-`_test.go` `*.go` files, excluding lines that are pure `//` comments (`grep -vE '^[^:]+:[0-9]+:[[:space:]]*//'`). Introducing `io.ReadAll`/`ioutil.ReadAll` or a `bytes.NewBuffer(...body...)` in these trees fails CI. Stream via `io.Copy`/`io.TeeReader`/`io.MultiWriter` instead. (Verify the exact `make` target / CI wiring before quoting them — only `scripts/check-streaming.sh` was inspected here.)
- **`ErrSkippable` vs `ErrPermanent` wrapping is load-bearing.** The worker switches on these sentinels (skipped vs dead). LocalFS: missing file → `ErrSkippable`, directory → `ErrPermanent`. FTP Source: `isNotFound` (incl. bare 550) → `ErrSkippable`; URL/scheme/host/path errors → `ErrPermanent`. **FTP Transport does NOT wrap with these sentinels** — an invalid dst returns a plain `fmt.Errorf` (no `%w` sentinel), and acquire/stor/rename failures wrap only the underlying error. Do NOT widen `isNotFound` to swallow 550-permission errors — that distinction is deliberate (misconfig must surface, not silently skip).
- **FTP conn release must happen exactly once, with the correct `broken` flag.** Success → `Release(false)` (conn returns to idle); any I/O error → `Release(true)` (conn is Quit, never reused). The `retrReader` defers Release to `Close()` — a leaked/un-Closed reader permanently consumes a `MaxPerHost` slot. `PooledConn.Release` nils `p.c` (and `retrReader` guards with `released`) so double-Release is a no-op.
- **Pool locking convention:** all blocking/external calls (`dial`, `NoOp`, `OnPoolChange`) run with `p.mu` released; mutate `hp.inUse`/`hp.idle`/`hp.waiters` only under the lock. Re-fetch `hp := p.hosts[host]` after re-locking (the code uses `hp2`) — don't reuse a stale pointer across an unlock.
- **`size == -1` is a valid "unknown".** FTP SIZE failures are non-fatal; both interfaces document `-1` as unknown. Don't treat `-1` as an error.
- **FTP paths are server-relative.** Wire calls strip the leading `/` (`strings.TrimPrefix(..,"/")`) at each call site. FTP Transport only does single-level `MakeDir`; multi-level destination parents must be provisioned out-of-band (v1 decision, not a bug).
- **Atomicity convention is shared:** both Transports write to a temp path and rename on success (LocalFS `os.Rename` after `fsync`; FTP RNFR/RNTO via `conn.Rename` after STOR ACK), cleaning up temp on every error path. Preserve this — partial destination files must never be observable.
- **`Pool.Close()` does not kill in-use conns**; it only Quits idle ones and sets `closed` so subsequent releases Quit. Callers MUST `Close` the pool on shutdown (per `NewPool` doc: "Caller MUST call Close on shutdown").
- Tests: `IdleCount(host)` and `IsNotFoundForTest` exist solely for unit tests — don't call them in production paths.

Relevant absolute paths: `/Users/nineking/workspace/app/imgsync/internal/sources/localfs/source.go`, `/Users/nineking/workspace/app/imgsync/internal/sources/ftp/source.go`, `/Users/nineking/workspace/app/imgsync/internal/transports/localfs/transport.go`, `/Users/nineking/workspace/app/imgsync/internal/transports/ftp/transport.go`, `/Users/nineking/workspace/app/imgsync/internal/transports/ftp/pool.go`, `/Users/nineking/workspace/app/imgsync/internal/transfer/interfaces.go`, `/Users/nineking/workspace/app/imgsync/internal/transfer/errors.go`, `/Users/nineking/workspace/app/imgsync/scripts/check-streaming.sh`.

## Sniffer / DB Connector (`internal/sniffer`)

Polls an external source database on a fixed interval and incrementally enqueues `transfer_jobs` rows for the worker to process. It is the ingest front-door: a single `Sniffer` per source maintains a `(timestamp, pk)` high-watermark in `sniffer_state`, fetches only rows newer than that watermark, renders src/dst paths via Go templates, and inserts deduplicated jobs keyed by a deterministic `trace_id`. An AI agent editing here is touching the **incremental-ingest correctness contract** — watermark advancement, idempotency, and pagination tie-breaking are all easy to break silently.

### Key files & symbols
- `internal/sniffer/sniffer.go:Config` — all params for one sniffer: `SourceID`, `Query`, `Dst DstTemplate`, `SrcPattern`, `SrcProtocol`, `DstProtocol`, `ImgsyncPool`, `SourcePool`, plus `OnEnqueue func(source string, n int)` / `OnError func(source string)` metrics hooks.
- `internal/sniffer/sniffer.go:Sniffer` / `New(cfg Config) *Sniffer` — composes `StateRepo`, `Enqueuer`, and a `DstTemplate` (`src`) reused as the **src**-pattern renderer (`src: DstTemplate{Pattern: cfg.SrcPattern}` — note: never sets `Shadow`).
- `internal/sniffer/sniffer.go:Sniffer.RunOnce` / `runOnceImpl` — one poll iteration: load watermark → fetch → render+enqueue each row → advance watermark. Returns count inserted. (`runOnceImpl:92` is the `s.src.Render` call.)
- `internal/sniffer/query.go:Query` — source-table descriptor: `Table`, `PKColumn`, `TSColumn`, `ExtraColumns`, `BatchSize` (LIMIT), `BiasDuration`.
- `internal/sniffer/query.go:Query.Fetch(ctx, pool, from State) ([]Row, error)` — the windowed SELECT against the **source** pool. `Row{PK, TS, Fields}`.
- `internal/sniffer/state.go:State` / `StateRepo` / `Load` / `Upsert` — watermark persistence in `sniffer_state` on the **imgsync** pool.
- `internal/sniffer/enqueue.go:Enqueuer.Enqueue(ctx, JobSpec) (bool, error)` — single `INSERT ... ON CONFLICT (trace_id, dst) DO NOTHING` into `transfer_jobs`; returns `inserted=true` only when `RowsAffected()==1`. `JobSpec{TraceID, Src, Dst, SrcProtocol, DstProtocol}` (all required).
- `internal/sniffer/traceid.go:TraceID(sourceTable, pk)` — returns `"<table>-<pk>"`. `DstTemplate.Render` + `ShadowSuffix = ".imgsync_shadow_v1"`.
- `migrations/0002_sniffer_state.up.sql` — `sniffer_state(source_id TEXT PK, last_run_ts TIMESTAMPTZ NOT NULL, last_run_pk TEXT, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`. The `(trace_id, dst)` dedup target is `CONSTRAINT transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)` in `0001_initial.up.sql`.
- `internal/cli/sniffer.go:132-177` — the only production caller: builds `Config` from env-derived `SnifferConfig` (`SNIFFER_*` vars), wires `m.OnSnifferEnqueue`/`m.OnSnifferError`, runs once immediately, then loops on a `time.NewTicker(IntervalSec)`.

### How it works / flow
1. **Load watermark** — `StateRepo.Load` reads `SELECT last_run_ts, COALESCE(last_run_pk, '')` for `source_id`. On `pgx.ErrNoRows` it returns `State{SourceID:id, LastRunTS:zero, LastRunPK:""}` with **nil error** — "first run" is signaled by `LastRunTS.IsZero()` (and `LastRunPK == ""`), not by an error.
2. **Fetch batch** — `Query.Fetch` runs one of two SQL shapes against the source pool:
   - First run / reset (`from.LastRunPK == ""`): `WHERE ts > $1 AND ts <= NOW() - ($2::INT||' seconds')::INTERVAL ORDER BY ts, pk LIMIT N`.
   - Subsequent (`LastRunPK != ""`): expanded keyset `WHERE (ts > $1 OR (ts = $1 AND pk > $2)) AND ts <= NOW()-bias ORDER BY ts, pk LIMIT N`. The OR form is deliberate so Postgres compares `pk` in its **native type** (avoids text-sort pagination bugs at boundaries like 9→10).
   - `BiasDuration` excludes too-fresh rows (stored at **second** resolution via `int(d.Seconds())`; sub-second truncates to 0 = bias disabled). `BatchSize <= 0` is a hard error (`"batch_size must be > 0"`).
   - Each result column is stringified into `Row.Fields` (`time.Time`→`UTC RFC3339Nano`, `nil`→`""`, else `fmt.Sprintf("%v", v)`); `Row.PK = Fields[PKColumn]`, `Row.TS` taken from `vals[1]` (must be `time.Time`, else `"unexpected ts type"` error).
3. **Render + enqueue per row** — for each `Row`: `cfg.Dst.Render(r.Fields)` → dst, `s.src.Render(r.Fields)` → src (templates use `missingkey=error`, so a missing field aborts the batch; an empty `Pattern` errors `"dst template: empty pattern"`). Then `Enqueuer.Enqueue` with `TraceID = TraceID(Query.Table, r.PK)`. UNIQUE conflicts return `ok=false` and are **not** counted in `inserted`.
4. **Advance watermark LAST** — only after the *entire* batch enqueues without error, `StateRepo.Upsert` writes `LastRunTS/PK` from the **last** row (`rows[len(rows)-1]`). `Upsert` stores `last_run_pk == ""` as SQL `NULL` via `ON CONFLICT (source_id) DO UPDATE`, and sets `updated_at = NOW()`.
5. **Outcome hooks** — `RunOnce` fires `OnEnqueue(SourceID, n)` on success (including `n==0`) or `OnError(SourceID)` on error, exactly once.

**Key invariants:**
- **Crash safety / at-least-once**: any error mid-batch returns early *before* the `Upsert`, so the old watermark persists and the whole batch is re-fetched next run. Re-enqueue is harmless because `ON CONFLICT (trace_id, dst) DO NOTHING` dedups.
- **Idempotency anchor**: same source row → same `trace_id` (`<table>-<pk>`) → at most one job per `(trace_id, dst)`. This is the dedup contract; don't change `TraceID` format or the UNIQUE key independently.
- Rows are ordered `ts, pk` and the watermark is the last row's `(ts, pk)`, so the keyset predicate resumes exactly past it without re-emitting the boundary row.

### Agent notes (gotchas, conventions, constraints)
- **Two distinct pools**: `SourcePool` (external source DB, read-only `Query.Fetch`) vs `ImgsyncPool` (our DB, holds `sniffer_state` + `transfer_jobs`). `StateRepo`/`Enqueuer` use `ImgsyncPool`; `Query.Fetch` uses `SourcePool`. Don't cross them. Pool lifecycle is **caller-owned** (never closed in this package; `RunSniffer` opens and `defer`-closes both).
- **`LastRunPK == ""` is an overloaded sentinel**: it means both "first poll" and "post-reset (pk NULL)". A source with a **TEXT pk that can be the empty string** would alias to this sentinel and skip tie-break filtering (documented caveat in `query.go`). v1 source schemas use BIGINT pks, so it's safe — but flag it if a TEXT-pk source is added.
- **Watermark must stay the last write in `runOnceImpl`** — moving `Upsert` earlier, or advancing it on partial success, breaks the retry-whole-batch crash-safety guarantee. The `inserted` counter is for metrics only; never use it to decide whether to advance the watermark.
- **Timestamp precision**: `last_run_ts` is TIMESTAMPTZ (microsecond); Go nanoseconds truncate on round-trip. Do **not** compare `State.LastRunTS` with `==` after it has flowed through Load/Upsert (noted in `state.go`).
- **SQL is built with `fmt.Sprintf`** for identifiers (`Table`, columns, `LIMIT`, `colList`) — only `ts/pk/bias` *values* are parameterized (`$1..$3`). Column/table names come from `SNIFFER_*` env config and are **not** sanitized; treat them as trusted operator input, not user input. Don't extend this to interpolate row data.
- **Shadow mode** is dst-only: `DstTemplate.Shadow=true` appends `ShadowSuffix = ".imgsync_shadow_v1"` so output won't collide with NiFi production paths. It is wired from `cfg.Shadow` (env `SNIFFER_SHADOW`, default true) into `Config.Dst` only (`internal/cli/sniffer.go:142`); the `src` renderer never gets `Shadow`. It's an operational-safety flag, **not** a reconcile/cross-system mechanism.
- **Templates fail closed**: `missingkey=error` means any `{{.col}}` not present in `Fields` aborts the row (and thus the batch, preserving the watermark). Adding a template var without adding the column to `ExtraColumns`/`PKColumn`/`TSColumn` will hard-error every poll.
- **`Config.OnEnqueue` fires even on `n==0`** and `runOnceImpl` early-returns `(0, nil)` when the fetch is empty — metrics consumers must tolerate zero-row ticks.
- **Concurrency**: `sniffer_state` has no advisory lock; `Upsert` is atomic but last-writer-wins. v1 assumes a **single sniffer pod per source** (see migration 0002 table comment). Don't add a second poller for the same `source_id` without adding locking.
- Tests live alongside (`*_test.go`: `sniffer_test.go`, `query_test.go`, `state_test.go`, `enqueue_test.go`, `traceid_test.go`) plus `integration_test.go`; the DB-backed ones need a live Postgres (run via the repo's integration harness, not plain `go test ./...`).

## Sweeper & Eval Suite (internal/sweeper, internal/eval)

The sweeper recovers jobs whose worker died mid-transfer: rows stuck in `status='leased'` past a lease-age threshold are reset to `pending` so another worker can re-lease them, with an `expire` audit event emitted per recovered row. Single-writer safety across pods comes from a Postgres transaction-scoped advisory lock, not a distributed lock — multiple pods can run `Run` concurrently and only one sweeps per cycle. The `internal/eval` package is the system's executable spec: a small set of correctness invariants (C0–C6, F3/F4) that an agent must keep green; breaking them silently breaks at-most-once-effect, audit-trail integrity, or streaming memory bounds.

### Key files & symbols
- `internal/sweeper/sweeper.go:Sweep(ctx, *pgxpool.Pool, Config) (int, error)` — one cycle in one tx: acquire advisory lock, UPDATE timed-out leases → `pending`, INSERT one `expire` event per recovered row, commit. Returns recovered-row count.
- `internal/sweeper/sweeper.go:Run(ctx, *pgxpool.Pool, Config) error` — loops `Sweep` on `cfg.Interval` ticks until ctx cancel; each cycle gets a derived `2*Interval` timeout; recovers from panics in a top-level `defer recover()`; calls `cfg.OnCycle` on success.
- `internal/sweeper/sweeper.go:Config` — `Threshold time.Duration` (lease age to recover; defaulted to 5m inside `Sweep` when ≤0), `Interval time.Duration` (loop period; defaulted to 30s inside `Run` when ≤0), `OnCycle func()` (healthz `last_sweep_ts` hook). Defaults are applied in code, not struct tags.
- `internal/sweeper/sweeper.go:sweeperLockKey` — const `"imgsync_sweeper"`, passed through `hashtext($1)` into `pg_try_advisory_xact_lock`.
- `internal/eval/sweeper_audit_test.go:mustDB(t) *pgxpool.Pool` — shared test harness: spins a `postgres:16-alpine` testcontainer (db/user/pass all `imgsync*`), calls `db.ApplyMigrations(ctx, dsn, "../../migrations")`, returns a pool via `db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 8})`. Used by every DB-backed eval test.
- `internal/eval/sweeper_audit_test.go:TestC2_SweeperRecoveredJob_HasAttemptsZero` — sweeper-recovered-then-succeeded job invariant.
- `internal/eval/audit_invariants_test.go:TestC0_SizeUnknownMismatch_TransitionsToDead`, `TestC3_SkippedJob_ExactlyOneSkipEventWithReason`.
- `internal/eval/rss_contract_test.go:TestC1_LocalFS_StreamingRSSUnder250MB`, `TestC1_FTP_StreamingRSSUnder250MB`, `startRSSWatcher`, `make2GBSparseFile` (and `hardLinkOrCopy` helper for the FTP path).
- `internal/eval/fixture_suite_test.go:TestC6_FixtureSuite` + `auditQuery` const — the canonical SRE audit JOIN, exercised across 53 fixture jobs.

### How it works / flow
- **Lock semantics:** `Sweep` opens a tx, runs `SELECT pg_try_advisory_xact_lock(hashtext($1))`. If `locked=false` (another sweeper holds it) it returns `(0, nil)` — a no-op, NOT an error. The lock is xact-scoped, so COMMIT/ROLLBACK releases it; the `defer tx.Rollback` covers every error path with no manual unlock.
- **Recovery SQL:** `UPDATE transfer_jobs SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW() WHERE status='leased' AND locked_at < NOW() - $1::INTERVAL RETURNING id, trace_id`. `$1` is built as `"<N> seconds"` (`fmt.Sprintf("%d seconds", int(cfg.Threshold.Seconds()))`). Note it does **not** touch `attempts` — that is why C2 asserts a recovered job keeps `attempts==0` (lease loss is not a delivery attempt).
- **Event emission:** for each recovered row it INSERTs into `transfer_events (trace_id, job_id, status, detail)` with `status='expire'` and `detail='{"reason":"lease_expired"}'::JSONB`. All recovery + events commit atomically; if any insert fails the whole cycle rolls back.
- **rows.Err() guard:** it collects RETURNING rows into a slice, calls `rows.Close()`, then explicitly checks `rows.Err()` before doing the event INSERTs — without it a mid-stream pgx failure would silently commit a partial recovery (comment mirrors `internal/db/migrate.go`).
- **Run loop resilience:** per-cycle `2*Interval` timeout prevents a wedged connection from holding the advisory lock and starving other pods. On ctx/deadline error it disambiguates: parent-ctx done (`ctx.Err() != nil`) → return; cycle-only timeout → `fmt.Fprintf(os.Stderr, ...)` and `continue`. Other errors are logged to stderr but the loop continues.
- **Eval invariants asserted:**
  - **C0** (`audit_invariants`): when `srcSize=-1` (unknown) and `bytesRead != writtenBytes`, the job must become `dead` (treated as `ErrPermanent`), not retried. Worker tags this `detail.reason = "size_mismatch_unknown_src"`.
  - **C2** (`sweeper_audit`): SIGKILL'd lease (simulated by `UPDATE ... locked_at = NOW() - INTERVAL '6 minutes'`) → sweeper resets → worker B re-leases and succeeds with `attempts==0` and `status='succeeded'`; event sequence must be exactly `enqueue, expire, success`.
  - **C3** (`audit_invariants`): a missing localfs source yields `status='skipped'`, `attempts==0`, exactly one `skip` event whose `detail->>'reason'` is non-empty; re-enqueuing the same `(trace_id, dst)` is a no-op (`inserted=false`) and adds no event (final event count stays 2).
  - **C1** (`rss_contract`): streaming a 2 GiB sparse file through localfs and FTP must keep sampled `runtime.MemStats.HeapInuse` peak ≤ `250<<20` bytes — proves no full-body buffering. Skipped under `-short`.
  - **C6** (`fixture_suite`): 53 jobs (10 plain success, 10 retry-then-success, 10 skip, 10 dead, 5 duplicate-enqueue, 5 sweeper-recovered, plus 1 F3 fan-out trace producing 2 jobs across distinct `dst` and 1 F3 re-enqueue job) each audited via `auditQuery`, asserting `(jobStatus, jobAttempts, ordered event sequence, last-event detail substring)`.
  - **F3** (in C6): the audit JOIN is `ON j.id = e.job_id` scoped by `WHERE j.trace_id=$1 AND j.dst=$2` (not `USING (trace_id)`), so fan-out to different `dst` under one `trace_id` does not cross-contaminate event sequences.
  - **F4** (cross-ref `internal/worker/process_test.go:202`): `writtenBytes != srcSize` demotes to `ErrPermanent` → `dead`, bumping `attempts` once on the dead transition.

### Agent notes (gotchas, conventions, constraints)
- **`Sweep` returning `(0, nil)` is correct on lock contention** — do not "fix" it into an error; that is the multi-pod no-op path.
- **Never make the sweeper increment `attempts`.** The C2 / C6 `recov-*` invariant (`attempts==0` after recovery+success) is load-bearing for distinguishing lease loss from real delivery attempts.
- **Recovery only matches `status='leased' AND locked_at < threshold`.** `locked_at` is set by the worker lease; the sweeper nulls it. The supporting index is `transfer_jobs_leased_idx` — a partial index `ON transfer_jobs (locked_at) WHERE status = 'leased'` (migration 0001); `transfer_jobs_status_idx ON transfer_jobs (status)` (migration 0003) backs the status-rollup scrape query. Keep the WHERE clause sargable against the partial `leased` index.
- **Event status vocabulary is fixed by the `transfer_events_status_check` CHECK constraint (migration 0001)**: `enqueue`, `lease`, `success`, `skip`, `fail`, `expire`, `dead`. The worker emits `enqueue/success/fail/skip/dead`; the sweeper owns `expire`. `lease` is in the allowed set but not currently emitted, so observed sequences never contain it. Do not reuse `expire` elsewhere. `skip` detail must carry `reason` (`source_not_found`, set in `internal/worker/process.go:184`).
- **Job-status enum (`job_status`, migration 0001)** is `pending, leased, succeeded, skipped, dead` — note the terminal success state is `succeeded` (not `success`, which is the *event* status).
- **Error-classification contract lives in `internal/transfer/errors.go`**: `ErrSkippable` → `skipped` (terminal, audit-only), `ErrPermanent` → `dead` (no retry). Sources wrap with `%w`; the worker dispatches via `errors.Is` (`process.go:116`/`118`). Eval tests assert the resulting terminal state, so changing classification breaks C0/C3/C6.
- **`mustDB` requires Docker/testcontainers and applies real migrations from `../../migrations`** — all eval tests except C1 hit a live Postgres 16. There is no build tag; they run under plain `go test ./internal/eval/...`.
- **C1 is gated by `testing.Short()`** — it allocates a 2 GiB sparse file and samples `HeapInuse`; run the full (non-`-short`) suite before claiming streaming behavior is preserved. The `250<<20` (250 MiB) cap is the hard ceiling; any code path that does `io.ReadAll`/`bytes.Buffer` on the body will blow it.
- **`auditQuery` in `fixture_suite_test.go` is the canonical SRE one-liner** — if you change the `transfer_jobs`/`transfer_events` join key or `dst` scoping, update this and re-verify F3, since it doubles as the documented operator query.

## Infra Adapters (db, sourcedb, health, hostcap, backoff, metrics, ftpserver)

Supporting adapters that wrap external systems for the worker/sweeper/sniffer domain packages: two pgx connection pools (primary + sniffer source), a migration runner, the health/metrics HTTP surface, a per-FTP-host concurrency cap backed by Postgres advisory locks, a shared idle backoff, the full Prometheus collector set, and an in-process FTP server for tests. An AI agent cares because these define the process's observability contract (every `imgsync_*` metric, the `/livez|/readyz|/healthz|/metrics` paths), the DB connection lifecycle, and the cluster-wide invariants (migration serialization, host-cap fairness) that break silently if changed.

### Key files & symbols
- `internal/db/pool.go:NewPool(ctx, PoolConfig)` — builds + `Ping`s a `*pgxpool.Pool` from `PoolConfig`; only overrides pgx defaults when a field is `> 0`; closes pool on ping failure. Caller owns `Close()`. Fields: `DSN, MaxConns, MinConns, MaxConnLifetime, MaxConnIdleTime, HealthCheckPeriod`.
- `internal/db/migrate.go:ApplyMigrations(ctx, dsn, dir)` — runs `*.up.sql` in lexical order under a session advisory lock; `migrationAdvisoryLockID int64 = 0x494d4753594e43` ("IMGSYNC"); `hasTable(ctx, conn, name)` gates reading `schema_migrations`.
- `internal/sourcedb/pool.go:NewPool(ctx, Config)` → `*sourcedb.Pool` — separate pool for the sniffer's source DB; `Pool` embeds `*pgxpool.Pool` and carries `QueryTimeout time.Duration`. `Config{DSN, MaxConns, QueryTimeoutMs}`; defaults `MaxConns=4`, `QueryTimeoutMs=30000`. Note: unlike `db.NewPool`, this **always** sets `MaxConns` on the pgx config (default-fills when `==0`).
- `internal/health/server.go:Server`, `NewServer(pool, st *Status, opts...)`, `Status`, `NewStatus()` — HTTP health server; `livez`/`readyz`/`healthz` handlers; `WithMetrics(h http.Handler)` option mounts `/metrics`; `Status.OnLeaseAttempt(success bool)`/`OnSweepCycle()` are the worker/sweeper hooks.
- `internal/hostcap/hostcap.go:Wrap(pool, inner, Config)`, `CapTransport.Send`, `acquireSlot`, `slotKey` — wraps a `transfer.Transport`; acquires one of `Cap` per-host slots via `pg_try_advisory_lock(hashtext(...))` on a dedicated pinned conn for the whole transfer. `Config{Cap, Host, AcquireBackoff}`.
- `internal/backoff/backoff.go:Idle`, `NewIdle(Config)`, `WaitOnce(ctx)`, `WakeAll()`, `advance`, `jitter` — shared idle backoff for empty-queue workers (50ms→200ms→500ms→1s, ±25% jitter). `Config{BaseDelay, MaxDelay}`.
- `internal/metrics/metrics.go:Metrics`, `New()`, `Handler()`, `Attach{QueueDepth,DBPool,LeaseLockAge}(pool)`, `On*`/`SetWorkersActive` — owns all collectors + private registry. `RegistryForTest()` exposes the registry for external-package tests.
- `internal/metrics/queue_depth.go:queueDepthCollector` (`newQueueDepthCollector`), `db_pool.go:dbPoolCollector` (`newDBPoolCollector`), `lease_lock_age.go:newLeaseLockAge` (returns `prometheus.Collector` via `NewGaugeFunc`), `buckets.go:defaultDurationBuckets` — scrape-time collectors + histogram buckets.
- `internal/ftpserver/testserver.go:Start(t)`, `Server`, `driver`, `clientDriver` — in-process FTP server over `afero.NewBasePathFs(afero.NewOsFs(), tmpdir)`; test-only.

### How it works / flow
- **Pool construction (`db.NewPool`):** `pgxpool.ParseConfig(DSN)` → conditionally override `MaxConns/MinConns/MaxConnLifetime/MaxConnIdleTime/HealthCheckPeriod` only when caller value `>0` (zero = pgx default: MaxConns `max(4,NumCPU)`, MinConns 0, lifetime 1h, idle 30m, healthcheck 1m) → `NewWithConfig` → `Ping`; ping failure calls `pool.Close()` (stops background goroutines) before returning the error. The env→`PoolConfig` mapping lives outside this package (in `cmd/imgsync`). Note `cmd/imgsync/worker.go` sizes the pool as `MaxConns: int32(2 + workers)` where `workers` = `envInt("IMGSYNC_WORKERS", 4)`.
- **Migrations (`ApplyMigrations`):** reads `dir`, filters `*.up.sql`, `sort.Strings` (so filenames must sort to intended order), opens a single `pgx.Connect`, takes `pg_advisory_lock(migrationAdvisoryLockID)` to serialize across pods. Lock is **session-scoped** — it auto-releases on disconnect, so a crashed migrate strands no lock (the code never explicitly unlocks; `conn.Close` does it). It reads applied `version`s from `schema_migrations` (only if `hasTable` returns true — `0001_initial` creates that table), then `Exec`s each unapplied file's full body. `version = name` minus `.up.sql`. Each file is one `Exec`; there is no per-file transaction wrapper here and no row is written back into `schema_migrations` by this runner — each `*.up.sql` does its own `INSERT INTO schema_migrations (version) VALUES (...)`.
- **Health endpoints:** `/livez` → always 200 (liveness). `/readyz` → `pool.Ping` with a 2s timeout; 200 or 503 + error body (readiness). `/healthz` → 200 JSON with `last_lease_attempt_ts`, `last_lease_success_ts`, `last_sweep_ts` (from `Status`, mutex-guarded) plus `pool_in_use` (`AcquiredConns()`) / `pool_idle` (`IdleConns()`) / `pool_max` (`MaxConns()`) from `pool.Stat()`. `Status.OnLeaseAttempt(success bool)` and `OnSweepCycle()` are called by domain loops. Server uses `ReadHeaderTimeout: 5s`; `Serve(l net.Listener)` binds, `Close()` stops, `MuxForTest()` exposes the mux for socketless tests.
- **Hostcap:** `Send` derives `host` from `cfg.Host` or `url.Parse(dst).Host` (errors if empty), `pool.Acquire`s a dedicated conn held for the entire transfer, then `acquireSlot` loops `slot` `0..Cap-1` calling `pg_try_advisory_lock(hashtext("ftp_host_<host>_<slot>"))`; first success returns. If all slots busy it sleeps `AcquireBackoff` (default 100ms) and retries until ctx cancels. On return it `pg_advisory_unlock`s the slot using `context.Background()` (so unlock survives a cancelled transfer ctx). Defaults: `Cap=8`, `AcquireBackoff=100ms`. Wired in `cmd/imgsync/worker.go` via `hostcap.Wrap(pool, ftpRaw, hostcap.Config{Cap: envInt("IMGSYNC_FTP_HOST_CAP", 8)})`. This is a **cluster-wide** cap because advisory locks are global to the Postgres instance.
- **Backoff:** `WaitOnce` snapshots the jittered current `nominal`, calls `advance` (steps 50→200→500→1000ms; generic doubling capped at `MaxDelay` for non-canonical configs), registers a buffered `wake` channel, then selects on timer / wake / ctx, and self-evicts its waker afterward. `WakeAll` resets `nominal` to `BaseDelay` and signals every parked waker — called on a successful lease so latency-to-next-lease is ctx-switch bound, not delay-step bound. Test helpers: `CurrentNominalDelay()`, `NumParked()`.
- **Metrics registry + collectors:** `New()` builds a private `prometheus.NewRegistry()` (one per process; per-instance registry lets tests run in parallel) and `MustRegister`s eight push-style collectors. `Handler()` returns `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})`. The three `Attach*(pool)` methods register **scrape-time** collectors that hit the DB on each Prometheus scrape (each `MustRegister` panics on duplicate). `OnJobFinished` defaults empty `src/dst/result` to `"unknown"` to avoid empty label values. Note the two-arg metrics signature `OnLeaseAttempt(success bool, err error)` — distinct from `Status.OnLeaseAttempt(success bool)`; in `cmd/imgsync` it is called `m.OnLeaseAttempt(success, nil)`.

**Exported Prometheus metrics (all `imgsync_` prefixed):**

| Name | Type | Labels | Source |
|---|---|---|---|
| `imgsync_lease_attempts_total` | CounterVec | `result` (`success`/`empty`/`error`) | `OnLeaseAttempt(success, err)` — `error` if err!=nil, else `success`/`empty` |
| `imgsync_jobs_processed_total` | CounterVec | `src,dst,result` | `OnJobFinished` |
| `imgsync_job_duration_seconds` | HistogramVec | `src,dst,result` | `OnJobFinished`; buckets `0.1,0.5,1,2,5,10,30,60,300,1800` |
| `imgsync_sweep_cycles_total` | Counter | — | `OnSweepCycle` |
| `imgsync_ftp_pool_size` | GaugeVec | `host,state` (`in_use`/`idle`) | `OnFTPPoolChange(host, inUse, idle)` |
| `imgsync_sniffer_enqueue_total` | CounterVec | `source` | `OnSnifferEnqueue(source, n)` (uses `.Add`) |
| `imgsync_sniffer_run_errors_total` | CounterVec | `source` | `OnSnifferError(source)` |
| `imgsync_workers_active` | GaugeVec | `pod` | `SetWorkersActive(pod, n)` |
| `imgsync_jobs_in_status` | const Gauge (collector) | `status` | `queueDepthCollector`: `SELECT status::text, COUNT(*)::bigint FROM transfer_jobs GROUP BY status`, 2s timeout |
| `imgsync_db_pool_conns` | const Gauge (collector) | `state` (`in_use`/`idle`/`max`) | `dbPoolCollector` from `pool.Stat()`, in-process/zero-cost |
| `imgsync_lease_lock_age_seconds` | GaugeFunc | — | `EXTRACT(EPOCH FROM NOW()-MIN(locked_at))::double precision FROM transfer_jobs WHERE status='leased'`, 2s timeout, 0 if NULL |

- **ftpserver:** `Start(t)` listens on `127.0.0.1:0`, builds a `driver` (user/pass both `"imgsync"`), `srv.Listen()` then `go srv.Serve()`, registers `t.Cleanup(srv.Stop)`. `AuthUser` returns a `clientDriver` over a base-path-jailed afero FS rooted at `t.TempDir()`. Returns a `Server{Addr, User, Pass, RootDir}`. `GetTLSConfig` intentionally returns an error (no TLS); `GetSettings` sets `DisableMLSD`/`DisableMLST` true and `DefaultTransferType: TransferTypeBinary`.

### Agent notes (gotchas, conventions, constraints)
- **The brief's description of `hostcap` is wrong — trust the code.** `hostcap` does NOT use gopsutil and does NOT measure host CPU/RAM. gopsutil is present only as a **transitive `// indirect` dependency** (`go.mod:66`, `go.sum:128` — pulled in by e.g. testcontainers/gopsutil consumers), and is **never imported in any `*.go`** (0 source hits). Worker concurrency is a static env var `IMGSYNC_WORKERS` (default 4) read in `cmd/imgsync/worker.go`; it is not derived from host capacity. `hostcap` is purely a per-FTP-host concurrency cap via Postgres advisory locks.
- **Advisory locks are global to the Postgres instance.** Both `migrationAdvisoryLockID` (0x494d4753594e43) and hostcap's `hashtext("ftp_host_*")` keys share the same `pg_advisory_lock` namespace. A `hashtext` collision with the migration ID (or between two host/slot strings) would silently serialize unrelated work. Don't reuse these keys elsewhere.
- **Migration runner does not write `schema_migrations`.** `ApplyMigrations` only *reads* applied versions; each `*.up.sql` file is responsible for inserting its own row (e.g. `INSERT INTO schema_migrations (version) VALUES ('0003_jobs_status_index')`). A file that forgets to record itself will re-run every boot. Files are ordered by `sort.Strings` on filename — keep the `NNNN_name.up.sql` zero-padded numeric prefix convention so lexical order = intended order (`0001_initial`, `0002_sniffer_state`, `0003_jobs_status_index`).
- **Migration is one `Exec` per whole file, no wrapping transaction.** A multi-statement file that fails midway is not rolled back by this code (pgx simple-protocol multi-statement Exec has its own implicit-transaction semantics; do not assume atomicity). Don't put `CONCURRENTLY` index builds in a file that also runs inside an implicit txn.
- **`Attach*` collectors run SQL on every Prometheus scrape.** `queue_depth` and `lease_lock_age` each open a 2s-timeout query against `transfer_jobs` per scrape; failures log a warn (`log.Printf`) and emit zero metrics (never panic, never block). They rely on indexes — `transfer_jobs_leased_idx` (partial on `locked_at WHERE status='leased'`, from `0001_initial`) for lease-lock-age, and the Phase 1.5 `transfer_jobs_status_idx` (b-tree on `status`, from `0003_jobs_status_index`) for queue-depth. A query plan regression here directly slows every scrape. `db_pool` is free (in-process `Stat()`).
- **Each `Metrics` has its own registry; `Attach*`/`MustRegister` panic on duplicate registration.** Call each `Attach*` at most once per pool. Don't add a global/default registry.
- **Domain packages must NOT import `internal/metrics`.** The convention is the `OnXxx` callback pattern (`OnLeaseAttempt`, `OnJobFinished`, `OnSweepCycle`, `OnSnifferEnqueue/Error`, `OnFTPPoolChange`, `SetWorkersActive`) wired from `cmd/imgsync` (e.g. `OnPoolChange: m.OnFTPPoolChange`). Preserve this — `health.WithMetrics(m.Handler())` is how `/metrics` gets mounted; metrics is not a self-serving HTTP server.
- **`db.NewPool` overrides only on `>0`.** Setting a `PoolConfig` field to a negative or zero value silently keeps the pgx default — there is no validation. `MinConns` of 0 means pgx default 0. (`sourcedb.NewPool` differs: it default-fills `MaxConns`/`QueryTimeoutMs` only on `==0` and always applies `MaxConns`.)
- **Hostcap unlock uses `context.Background()` deliberately** so the slot is released even when the transfer ctx was cancelled. Don't "fix" this to use the request ctx — it would leak slots cluster-wide on cancellation.
- **Backoff `advance` has a hardcoded canonical ladder** (50/200/500/1000ms). Changing `BaseDelay`/`MaxDelay` away from these values silently switches to generic doubling. `WakeAll` empties the waker slice each call; `WaitOnce` does best-effort self-eviction to avoid slice growth. `jitter` is ±25% (50% total span).
- **ftpserver is test-only.** The file imports `testing` and `Start(t *testing.T)` — it is in a non-`_test.go` file but is strictly for tests (no build tag; do not call from prod paths). `GetTLSConfig` returning an error is intentional, not a bug; the comment notes the `*tls.Config` signature requirement for `ftpserverlib` v0.30+.
- **`/readyz` (DB ping) is the readiness gate; `/livez` is unconditional.** Don't add DB checks to `/livez` — a DB blip would otherwise trigger pod restarts instead of just removing from rotation.

## CLI & Entry Points (cmd/imgsync)

A single static binary (`package main`) wired with cobra into four subcommands — `migrate`, `enqueue`, `worker`, `sniffer`. Each subcommand is a thin assembly layer: it reads env vars (and flags for `enqueue`/`migrate`), opens pgx pools, constructs the concrete source/transport/runner objects, and hands off to an `internal/*` package. AI agents touching deployment, config, signal handling, or "where does protocol X get registered" start here. The heavy logic lives in `internal/*`; these files are wiring only.

### Key files & symbols
- `cmd/imgsync/main.go:main` — builds the cobra root cmd, registers all four subcommands, runs `root.ExecuteContext(ctx)`. Root has `SilenceUsage: true` + `SilenceErrors: true` (errors printed once to stderr + `os.Exit(1)`).
- `cmd/imgsync/main.go:version` — `var version = "dev"`; the cobra `Version` field. Set via `-ldflags -X main.version=...` at build time.
- `cmd/imgsync/worker.go:newWorkerCmd` — assembles DB pool, metrics, FTP pool, sources/transports, `worker.Runner`, health server, and sweeper.
- `cmd/imgsync/worker.go:envInt` — local env-int parser (warns to stderr + falls back to default on bad value). NOTE: a *second, separate* `envInt` exists in `internal/cli/sniffer.go` with different fallback behavior (silent).
- `cmd/imgsync/enqueue.go:newEnqueueCmd` — flag-driven; calls `jobs.Enqueue` (idempotent on `(trace_id, dst)`).
- `cmd/imgsync/migrate.go:newMigrateCmd` — calls `db.ApplyMigrations(ctx, dsn, dir)`.
- `cmd/imgsync/sniffer.go:newSnifferCmd` — 2-line delegate to `cli.ParseSnifferConfig` + `cli.RunSniffer`.
- `internal/cli/sniffer.go:ParseSnifferConfig` / `RunSniffer` / `SnifferConfig` — all sniffer env parsing, validation, pool/health wiring, and the poll loop live here (not in `cmd/`).

### How it works / flow
**Signal/context (top-level):** `main` creates `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` and passes it through `ExecuteContext`. Every subcommand reads `cmd.Context()` and propagates it down; cancellation = graceful shutdown. `RunSniffer` *re-wraps* the inherited ctx with its own `signal.NotifyContext(ctx, SIGTERM, SIGINT)` as defense-in-depth for non-cobra callers.

**worker:** requires `IMGSYNC_DSN`. Reads `IMGSYNC_WORKERS` (default 4), `IMGSYNC_POD_NAME` (falls back to `os.Hostname()`). Opens pool with `MaxConns = 2 + workers`. Builds `metrics.New()` and attaches queue-depth / DB-pool / lease-lock-age collectors. Builds FTP pool (`IMGSYNC_FTP_MAX_PER_HOST` default 4, `IMGSYNC_FTP_IDLE_TTL_SEC` default 300, `IMGSYNC_FTP_NOOP_AFTER_SEC` default 60, `IMGSYNC_FTP_USER`, `IMGSYNC_FTP_PASSWORD`). Registers protocols via `SourceFor`/`TransportFor` closures: `"localfs"` and `"ftp"` only — anything else returns `worker.ErrUnknownProtocol`. FTP transport is wrapped by `hostcap.Wrap(pool, ftpRaw, ...)` with `IMGSYNC_FTP_HOST_CAP` (default 8). Starts a health HTTP server on `IMGSYNC_HEALTH_ADDR` (default `:8080`, serves metrics handler), and a `sweeper.Run` goroutine (Threshold 5m, Interval 30s) reclaiming stale leases. Metrics callbacks (`OnLeaseAttempt`, `OnFinish`, `OnWorkerStart/Stop`) are chained onto the runner; `workersGauge` is an `int32` mutated via `atomic`. Finally calls `r.Run(ctx)`, which spawns `workers` goroutines each calling `LeaseJob` in a loop with `IdleBackoff` (50ms→1s) on empty-queue/error; ctx-cancel drains all goroutines via `wg.Wait()`. Worker goroutine panics are recovered + logged (not propagated).

**enqueue:** requires `IMGSYNC_DSN`; pool `MaxConns: 4`. Flags `--trace-id --src --dst --src-protocol --dst-protocol` (all `MarkFlagRequired`) + `--max-attempts` (default 5). Prints `enqueued id=… trace_id=…` vs `exists id=… trace_id=… (no-op)` based on the `inserted` bool from `jobs.Enqueue`.

**migrate:** requires `IMGSYNC_DSN`. `--dir` defaults to `$IMGSYNC_MIGRATIONS_DIR` else `/etc/imgsync/migrations`. `db.ApplyMigrations` reads `*.up.sql` files in the dir, sorts lexically (`sort.Strings`), takes a session-level `pg_advisory_lock(0x494d4753594e43)` (`migrationAdvisoryLockID`, ASCII "IMGSYNC") to serialize concurrent pods, and skips versions already in `schema_migrations` (version = filename minus `.up.sql`). Forward-only — `.down.sql` files exist on disk (e.g. `0002_sniffer_state.down.sql`, `0003_jobs_status_index.down.sql`) but are never applied by this command.

**sniffer:** all required env is `SNIFFER_*`-prefixed (note: `SNIFFER_IMGSYNC_DSN`, *not* `IMGSYNC_DSN`). `ParseSnifferConfig` requires 10 vars: `SNIFFER_SOURCE_ID`, `SNIFFER_SOURCE_DSN`, `SNIFFER_IMGSYNC_DSN`, `SNIFFER_TABLE`, `SNIFFER_PK_COLUMN`, `SNIFFER_TS_COLUMN`, `SNIFFER_DST_PATTERN`, `SNIFFER_SRC_PATTERN`, `SNIFFER_SRC_PROTOCOL`, `SNIFFER_DST_PROTOCOL`. Optional: `SNIFFER_SHADOW` (default true), `SNIFFER_BATCH_SIZE` (500), `SNIFFER_BIAS_SEC` (5), `SNIFFER_INTERVAL_SEC` (60), `SNIFFER_EXTRA_COLUMNS` (comma-split, whitespace-trimmed), `SNIFFER_HEALTH_ADDR` (`:8080`). Validation rejects `INTERVAL_SEC <= 0` and `BATCH_SIZE <= 0`. `RunSniffer` opens two pools (source via `sourcedb.NewPool` with 30s/30000ms query timeout; imgsync via `pgxpool.New`), starts a health server, then calls `s.RunOnce` **immediately** (so failures surface fast) and loops on a `time.Ticker`. Per-cycle errors are logged and the loop `continue`s — they do not stop the sniffer.

### Agent notes (gotchas, conventions, constraints)
- **Two distinct `envInt`s.** `cmd/imgsync/worker.go:envInt` warns to stderr on unparseable values; `internal/cli/sniffer.go:envInt` is silent. They are not shared — don't "dedupe" them without checking both call sites' contracts. Also note `enqueue`/`migrate` use cobra flags, not env, for their non-DSN config.
- **DSN env var name differs per command:** worker/enqueue/migrate read `IMGSYNC_DSN`; sniffer reads `SNIFFER_IMGSYNC_DSN` (+ `SNIFFER_SOURCE_DSN`). Easy to conflate.
- **Protocol registration is hard-coded** in the `SourceFor`/`TransportFor` switch closures in `worker.go` (`"localfs"`, `"ftp"`). Adding a protocol means editing *both* closures here; an unmatched string returns `worker.ErrUnknownProtocol` (not a panic). FTP transport must stay wrapped by `hostcap.Wrap` to enforce per-host concurrency caps — don't bypass it.
- **Pool sizing invariant:** worker pool is `MaxConns = 2 + workers` (2 reserved for sweeper/health/metrics). If you add background DB consumers, bump this margin or you'll starve workers.
- **Health server + sweeper run as fire-and-forget goroutines** (`go func(){ _ = ... }()`); their errors are swallowed. The health listener bind (`net.Listen`) failure *does* abort startup (it's checked before the goroutine spawns), but `Serve` errors do not.
- **Graceful shutdown relies entirely on ctx propagation** — there is no explicit drain/timeout in the subcommand. (Note: the *sweeper* derives a per-cycle `2*Interval` timeout internally, but that's in `internal/sweeper`, not here.) Anything you add in a subcommand must honor `cmd.Context()` cancellation or it will hang SIGTERM.
- `version` defaults to `"dev"`; release builds inject it via `-ldflags -X main.version=`. Don't hardcode a version string.
- Subcommands print via `cmd.OutOrStdout()` (testable), not bare `fmt.Println` — keep that convention for new output.
- `main.go` sets `SilenceErrors`/`SilenceUsage`, so RunE errors are surfaced exactly once by `main` to stderr. Don't also print-and-return the same error from a RunE, or it double-prints.

## Configuration & Environment Variables

imgsync is a single Go binary (`imgsync`) with four cobra subcommands — `worker`, `sniffer`, `enqueue`, `migrate` (registered in `cmd/imgsync/main.go`) — and is configured almost entirely by environment variables; only `enqueue` and `migrate --dir` take CLI flags. There is no config file: every runtime knob is an env read at startup. An agent configuring a deployment must know which subcommand reads which var, the defaults, and the required-vs-optional split (missing required vars cause immediate startup failure). Note: the **sweeper is not a subcommand** — it runs as a goroutine inside `worker` with hardcoded (non-env) `Threshold=5*time.Minute` / `Interval=30*time.Second` (worker.go:118-119; sweeper's own zero-value defaults are also 5m/30s).

### Key files & symbols
- `cmd/imgsync/worker.go:newWorkerCmd` — worker entrypoint; reads all `IMGSYNC_*` vars (DSN, WORKERS, POD_NAME, FTP_*, HEALTH_ADDR); also has its own `envInt(key, def)` helper (line 155) that warns-to-stderr-and-defaults on parse error.
- `cmd/imgsync/migrate.go:newMigrateCmd` — reads `IMGSYNC_DSN` (required) and `IMGSYNC_MIGRATIONS_DIR` (used only as the *default* for the `--dir` flag).
- `cmd/imgsync/enqueue.go:newEnqueueCmd` — reads only `IMGSYNC_DSN`; everything else is cobra flags (`--trace-id`, `--src`, `--dst`, `--src-protocol`, `--dst-protocol`, `--max-attempts`; the first five are `MarkFlagRequired`).
- `cmd/imgsync/sniffer.go:newSnifferCmd` — thin wrapper; delegates to `cli.ParseSnifferConfig` + `cli.RunSniffer`.
- `internal/cli/sniffer.go:ParseSnifferConfig` — reads all `SNIFFER_*` config vars into `SnifferConfig`, enforces 10 required vars + 2 range checks; has its own `envInt`/`envBool` helpers (lines 182/193). `RunSniffer` separately reads `SNIFFER_HEALTH_ADDR`.
- `internal/cli/sniffer.go:SnifferConfig` — the typed struct all sniffer config env vars map into.

### How it works / flow
- **DSN is universal & required** for all four subcommands (`worker`/`enqueue`/`migrate` use `IMGSYNC_DSN`; sniffer uses `SNIFFER_IMGSYNC_DSN` for control DB + `SNIFFER_SOURCE_DSN` for source DB). For worker/enqueue/migrate an empty DSN → `errors.New("IMGSYNC_DSN is required")` and non-zero exit; for sniffer the two DSNs are part of the 10-var required set below.
- **`envInt` semantics differ from required-string semantics.** `envInt(key, def)` returns `def` when the var is absent *or unparseable* (worker variant prints a stderr warning, cli variant silently defaults). So a typo in `IMGSYNC_WORKERS=eight` silently runs with 4 workers.
- **Worker FTP pool** is built from `pftp.PoolConfig` fields: `IMGSYNC_FTP_MAX_PER_HOST` (4), `IMGSYNC_FTP_IDLE_TTL_SEC` (300), `IMGSYNC_FTP_NOOP_AFTER_SEC` (60) — the last two are seconds multiplied into `time.Duration`. `IMGSYNC_FTP_HOST_CAP` (8) feeds `hostcap.Wrap(pool, ftpRaw, hostcap.Config{Cap: ...})` (a cluster-wide advisory-lock cap, distinct from the per-pod `MaxPerHost`). `IMGSYNC_FTP_USER`/`IMGSYNC_FTP_PASSWORD` are optional plain strings (→ `AuthUser`/`AuthPassword`).
- **DB pool sizing is derived, not configured:** `MaxConns = int32(2 + workers)` in worker; hardcoded `MaxConns: 4` in enqueue. There is no env var for pool size.
- **Pod identity:** `IMGSYNC_POD_NAME` defaults to `os.Hostname()` when empty; it becomes the lease `locked_by` / runner `PodName` identifier.
- **Health/metrics bind:** both worker (`IMGSYNC_HEALTH_ADDR`) and sniffer (`SNIFFER_HEALTH_ADDR`) default to `:8080`; `health.NewServer` registers `/livez`, `/readyz`, `/healthz` unconditionally and mounts `/metrics` whenever a metrics handler is passed (`health.WithMetrics`) — which both worker and sniffer do. So **all four endpoints are live on both daemons** on that one port. The only difference: the worker updates the shared `health.Status` (lease attempts via `OnLeaseAttempt`, sweep cycles via `OnSweepCycle`), while the sniffer constructs `health.NewStatus()` and never updates it — so the sniffer's `/healthz` JSON returns 200 with zero-valued `last_lease_attempt_ts`/`last_lease_success_ts`/`last_sweep_ts` (pool stats are still real).
- **Sniffer required set (10 vars, all fail-fast):** `SNIFFER_SOURCE_ID`, `SNIFFER_SOURCE_DSN`, `SNIFFER_IMGSYNC_DSN`, `SNIFFER_TABLE`, `SNIFFER_PK_COLUMN`, `SNIFFER_TS_COLUMN`, `SNIFFER_DST_PATTERN`, `SNIFFER_SRC_PATTERN`, `SNIFFER_SRC_PROTOCOL`, `SNIFFER_DST_PROTOCOL` → `fmt.Errorf("required env %s missing")`. Plus range guards: `SNIFFER_INTERVAL_SEC > 0` and `SNIFFER_BATCH_SIZE > 0` (both error if `<= 0`, e.g. `"SNIFFER_INTERVAL_SEC must be > 0, got %d"`).
- **Sniffer optional/defaulted:** `SNIFFER_SHADOW` (bool, default `true` — audit-only, no enqueue), `SNIFFER_BATCH_SIZE` (500), `SNIFFER_BIAS_SEC` (5, → `BiasDuration` seconds), `SNIFFER_INTERVAL_SEC` (60), `SNIFFER_EXTRA_COLUMNS` (CSV, split on `,` + `TrimSpace`d, empty entries dropped, into `[]string`).
- **`envBool` accepts only** `"1"` or case-insensitive `"true"` (`v == "1" || strings.EqualFold(v, "true")`) as truthy; anything else (e.g. `"yes"`, `"TRUE "` with trailing space) → false.

### Agent notes (gotchas, conventions, constraints)
- **Two separate `envInt` helpers exist** (`cmd/imgsync/worker.go:155` and `internal/cli/sniffer.go:182`) plus `envBool` in the cli one. They are NOT shared. If you add a worker var, edit the worker file; sniffer vars go through the cli file. The worker variant warns on bad parse; the cli variant does not.
- **No config file, no flags for worker/sniffer.** Do not add a `--flag` to worker or sniffer expecting parity with enqueue/migrate — the established convention is env-only for the long-running daemons, flags only for the one-shot `enqueue`/`migrate` commands.
- **Doc/code cross-check — verified consistent**, with these nuances an agent should not "fix" blindly:
  - `environment-variables.md` (line 18) lists `IMGSYNC_HEALTH_ADDR` as exposing `/livez,/readyz,/healthz,/metrics`, but the same table (line 35) and `sniffer.md` list only `/livez,/readyz,/metrics` for `SNIFFER_HEALTH_ADDR`. This is a **doc editorial choice, not a code difference**: `health.NewServer` registers `/healthz` on both. The sniffer's `/healthz` is just not documented because its status is never updated (it returns zero timestamps), so it carries no operational signal. Do not "fix" the code to gate `/healthz` on a flag — it isn't gated, and do not add `/healthz` to the sniffer doc row unless you intend to document the zero-status behavior.
  - **`SNIFFER_BIAS_SEC`/`SNIFFER_INTERVAL_SEC`/`SNIFFER_BATCH_SIZE`/`*_TTL_SEC`/`*_NOOP_AFTER_SEC` are plain integers** (the `_SEC` ones are *seconds* the code multiplies by `time.Second`). Do not pass Go duration strings like `"5s"` — `strconv.Atoi("5s")` fails and silently falls back to the default.
- **`sourcedb` query timeout is hardcoded** at `QueryTimeoutMs: 30000` in `cli.RunSniffer` (the `sourcedb.Config` zero-value default is also 30000) — there is no env var for it despite being a plausible knob. Don't document one that doesn't exist.
- **`IMGSYNC_MIGRATIONS_DIR` only sets the flag default.** The real input is `--dir` (default `/etc/imgsync/migrations`). A passed `--dir` overrides the env var.
- **DB pool max-conns is not env-configurable** (`2 + IMGSYNC_WORKERS` for worker, fixed `4` for enqueue). To raise the worker pool size you raise `IMGSYNC_WORKERS`; there's no independent override.
- **PR guard convention** (from `environment-variables.md`): when adding/removing an env var, update the master table in that file; reviewers diff it against `grep -rn 'os.Getenv\|envInt\|envBool' cmd/ internal/cli/`. Keep that grep clean — env reads live only in `cmd/imgsync/*.go` and `internal/cli/sniffer.go`; no other `internal/` package reads env (verified: sweeper/sourcedb/health/db/sniffer/metrics/jobs/hostcap/worker packages have zero `os.Getenv` calls — they take typed config structs).

## Build, Packaging & Deployment

imgsync ships as a single static Go binary built into a distroless image, deployed via a single Helm chart that renders worker + sniffer Deployments plus a pre-install migration Job. One image, four cobra subcommands (`migrate`, `enqueue`, `worker`, `sniffer`) selected by container `args`. CI gates on a streaming-buffering guard, lint, and race tests; an opt-in (`e2e` PR label) job spins up kind. Agents editing here must preserve the Dockerfile contract (nonroot, <50MB, subcommands) and the immutable Helm selector labels, both of which are guarded by tests.

### Key files & symbols
- `Dockerfile` — 2-stage: `golang:${GO_VERSION}-alpine` builder (default `GO_VERSION=1.25`; `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, `-trimpath -ldflags="-s -w -X main.version=${VERSION}"`) → `gcr.io/distroless/static-debian12:nonroot`. `ENTRYPOINT ["/app/imgsync"]`, `CMD ["--help"]`, `USER nonroot:nonroot`, `ENV IMGSYNC_MIGRATIONS_DIR=/app/migrations` (migrations baked in at `/app/migrations`). Build-args `GO_VERSION` and `VERSION` are both declared (`VERSION` re-declared inside the builder stage).
- `cmd/imgsync/main.go:main` — cobra root `imgsync`; registers `newMigrateCmd/newEnqueueCmd/newWorkerCmd/newSnifferCmd` (defined in `cmd/imgsync/{migrate,enqueue,worker,sniffer}.go`). `var version = "dev"` is the ldflags injection target, wired to the root's `Version:` field. `cmd/imgsync/migrate.go` reads `os.Getenv("IMGSYNC_MIGRATIONS_DIR")` for the migrations path.
- `Makefile` — targets below; `VERSION ?= git describe --tags --always --dirty` (falls back to `dev`), `IMAGE ?= imgsync:$(VERSION)`.
- `docker-compose.yml` — dev stack: `postgres:16-alpine`, `delfer/alpine-ftp-server:latest` (service name `ftpd`), `imgsync-migrate` (runs `migrate up`, gated on pg `service_healthy`), `imgsync-worker` (`command: ["worker"]`, gated on `service_completed_successfully` of migrate and `service_started` of ftpd, exposes 8080 via `IMGSYNC_HEALTH_ADDR=":8080"`). `imgsync-migrate`/`imgsync-worker` use `image: imgsync:dev`.
- `deploy/helm/imgsync/templates/deployment.yaml` — worker Deployment, `args: ["worker"]`, `RollingUpdate maxSurge:1 maxUnavailable:0`, `terminationGracePeriodSeconds: 60`.
- `deploy/helm/imgsync/templates/migrate-job.yaml` — `args: ["migrate", "up"]`, `helm.sh/hook: pre-install,pre-upgrade` (no rollback), `hook-delete-policy: before-hook-creation,hook-succeeded`, name suffixed with `.Release.Revision`. Wrapped in `if .Values.migrationJob.enabled`.
- `deploy/helm/imgsync/templates/sniffer-deployment.yaml` — sniffer Deployment, `args: ["sniffer"]`, `strategy.type: Recreate`, hard `{{ fail }}` if `sniffer.replicas > 1`.
- `deploy/helm/imgsync/templates/_helpers.tpl` — `imgsync.image` (tag = `.Values.image.tag` else `.Chart.AppVersion`), `imgsync.name`, `imgsync.fullname`, `imgsync.labels`, `imgsync.selectorLabels`, `imgsync.serviceAccountName`.
- `deploy/helm/imgsync/templates/{configmap,sniffer-configmap}.yaml` — non-secret env (`IMGSYNC_*` and `SNIFFER_*` resp.), consumed via `envFrom.configMapRef`.
- `deploy/helm/imgsync/templates/{service,sniffer-service,pdb,serviceaccount,servicemonitor}.yaml` — see flow below.
- `deploy/helm/imgsync/tests/template_test.sh` — 12 structural assertions (bash, runs under `make helm-test`). The authority on chart invariants.
- `scripts/check-streaming.sh` — CI guard forbidding `io.ReadAll`/`ioutil.ReadAll`/`bytes.NewBuffer(...body...)` in `internal/{sources,transports,transfer}`.
- `scripts/test-docker-build.sh` — Dockerfile contract checks (run via `make docker-test`).
- `.github/workflows/{ci.yml,docs.yml}` — CI and mkdocs/GitHub Pages.

### How it works / flow
- **Image build**: BuildKit cache mounts for `/root/.cache/go-build` and `/go/pkg/mod`. `VERSION` build-arg flows into `main.version`. Migrations are `cp -r`-copied to `/out/migrations` and re-copied into the runtime layer at `/app/migrations`; the binary reads them from `IMGSYNC_MIGRATIONS_DIR=/app/migrations` at runtime (filesystem, not embedded).
- **Subcommand dispatch**: same image everywhere; behavior is chosen by `args` (`["worker"]`, `["migrate","up"]`, `["sniffer"]`) or compose `command`. `--help` is the default `CMD`.
- **Makefile targets**: `build` (→`bin/imgsync`), `test` (`go test ./... -race -count=1`), `lint` (`golangci-lint run`), `streaming-check`, `tidy` (`go mod tidy`), `ci` = `lint streaming-check test`. Docker: `docker-build` (tags `$(IMAGE)` + `imgsync:dev`, `DOCKER_BUILDKIT=1`, `--build-arg VERSION`), `docker-test`, `docker-run-help` (depends on `docker-build`, runs `imgsync:dev --help`). Dev: `dev-up` (`docker-build` + `docker compose up -d`), `dev-down` (`down -v`), `dev-seed`, `dev-smoke`. Helm (`HELM_CHART=deploy/helm/imgsync`): `helm-lint`, `helm-template`, `helm-test`. E2E (env `IMGSYNC_E2E=1`, build-tag `e2e`): `e2e-up/down`, `e2e-throughput` (`TestC7_ThroughputScaleOut`, `-timeout 35m`), `e2e-dirty-state` (`TestF5_DirtyStateRecovery`, `-timeout 30m`), `e2e-sniffer` (`TestC5Prime_`, `-timeout 20m`), real-cluster `e2e-up-real/down-real/seed-real/push-real`. Integration (build-tag `integration`): `test-integration-sniffer` (`TestS[0-3]_` in `./internal/sniffer/`), `test-integration-metrics` (`./internal/metrics/`). Docs: `docs-install/serve/build/clean` (mkdocs `--strict`).
- **Helm install order**: `migrate-job.yaml` runs as a `pre-install,pre-upgrade` hook (`hook-weight: "0"`) before worker/sniffer roll out; job name carries `.Release.Revision` and prior jobs are reaped by `before-hook-creation`. Migration uses `restartPolicy: Never`, `backoffLimit` from `migrationJob.backoffLimit` (default 2), `ttlSecondsAfterFinished` from `migrationJob.ttlSecondsAfterFinished` (default 600), and a 16Mi `emptyDir` at `/tmp`.
- **Conditional rendering**: PDB renders only when `replicaCount >= 2` (`pdb.yaml`); ServiceMonitor renders only when `monitoring.serviceMonitor.enabled` AND cluster has `monitoring.coreos.com/v1` (`--api-versions` in tests). Sniffer Deployment/Service/ConfigMap render only when `sniffer.enabled` (default true).
- **Metrics/monitoring**: both worker Service (`service.yaml`, selector `component: worker`, `targetPort: health`) and sniffer Service (`component: sniffer`, `targetPort: http`) expose port name `http-metrics` → container port 8080. The health server (`internal/health/server.go`) serves `/livez`, `/readyz`, `/healthz`, and `/metrics` on that port. A single ServiceMonitor endpoint fans out across both via `matchExpressions` (`app.kubernetes.io/name In [<name>]` AND `app.kubernetes.io/component In [worker, sniffer]`) scraping `/metrics`.
- **Secrets**: DSN comes from `dsnSecretRef` (default Secret name `imgsync-dsn`, key `dsn`) via `secretKeyRef` in worker + migrate-job. FTP creds optional from `ftpSecretRef` (e.g. `imgsync-ftp`, keys from `userKey`/`passwordKey`, default `user`/`password`) — only injected when `ftpSecretRef.name` set. Sniffer reads `SNIFFER_SOURCE_DSN`/`SNIFFER_IMGSYNC_DSN` from `sniffer.secrets.{sourceDSNSecretRef,imgsyncDSNSecretRef}` (default `imgsync-source-dsn`/`imgsync-db-dsn`), each keyed by the env-var name itself. **Operators must pre-create these Secrets**; the chart never creates them.
- **ConfigMap-change rollout**: worker pod template carries `checksum/config` and sniffer carries `checksum/sniffer-config` (`sha256sum` of the rendered ConfigMap) so `helm upgrade` restarts pods when env changes (envFrom reads env only at pod start).
- **Security context** (worker + migrate-job): both use `.Values.podSecurityContext` (`runAsNonRoot: true`, `runAsUser: 65532`, `fsGroup: 65532`, `seccompProfile: RuntimeDefault`) and `.Values.securityContext` (`allowPrivilegeEscalation: false`, `cap drop ALL`, `readOnlyRootFilesystem: true`, `runAsNonRoot: true`, `runAsUser: 65532`). `readOnlyRootFilesystem` forces a writable `emptyDir` at `/tmp` (worker 64Mi, migrate-job 16Mi).
- **CI** (`ci.yml`): `lint-and-test` on push-to-main + all PRs — runs `bash scripts/check-streaming.sh`, `golangci/golangci-lint-action@v7` (`version: v2.4.0`), `go test ./... -race -count=1` on Go 1.25. `e2e` job is gated on a PR label named `e2e` (`needs: [lint-and-test]`), installs kind (`helm/kind-action@v1`, `version: v0.31.0`, `install_only`) + helm (`azure/setup-helm@v4`, `version: v3.14.0`), runs `make e2e-throughput` then `make e2e-dirty-state` (`if: success() || failure()` — F5 runs even if C7 fails), cleanup `make e2e-down` (`if: always()`).
- **docs CI** (`docs.yml`): builds `mkdocs build --strict` (Python 3.12, `actions/setup-python@v5`) on `docs/**`, `mkdocs.yml`, `requirements-docs.txt` (and the workflow file on push) changes; uploads pages artifact and deploys to GitHub Pages (`deploy` job) only on `refs/heads/main`.

### Agent notes (gotchas, conventions, constraints)
- **Deployment selector labels are immutable.** Both worker and sniffer set `app.kubernetes.io/component` in `spec.selector.matchLabels`. Changing component labels breaks `helm upgrade` ("field is immutable") — `NOTES.txt` documents the delete-then-upgrade workaround (`kubectl delete deploy <fullname> <fullname>-sniffer`). Do not touch `selectorLabels` or component keys casually.
- **Sniffer is single-pod by contract.** `sniffer-deployment.yaml` hard-`fail`s when `sniffer.replicas > 1` and uses `strategy: Recreate` — v1 has no advisory lock on `sniffer_state`, so concurrent sniffers race the watermark and re-enqueue rows. Do not add a sniffer PDB or RollingUpdate.
- **Migration Job must stay nonroot + `args: ["migrate", "up"]`.** `template_test.sh` Test 6 isolates the migrate-job manifest with `awk` and asserts `runAsNonRoot/runAsUser 65532/readOnlyRootFilesystem` and the exact `args: ["migrate", "up"]` array — a regression flipping it to root or changing args fails CI. The hook is forward-only (no rollback hook); don't add `post-rollback`.
- **Dockerfile contract is enforced** by `scripts/test-docker-build.sh`: image user must be `nonroot:nonroot`/`65532:65532`, size **<50MB**, and subcommands `migrate`/`enqueue`/`worker` must each respond to `--help` (note: `sniffer` is NOT in the script's subcommand loop). Adding a CGO dep, switching off distroless, or bloating the binary will break this.
- **Streaming guard is a hard CI gate.** Never introduce `io.ReadAll`, `ioutil.ReadAll`, or `bytes.NewBuffer(...body...)` in `internal/sources`, `internal/transports`, or `internal/transfer` (non-`_test.go`). The regex (`grep -vE '^[^:]+:[0-9]+:[[:space:]]*//'`) skips full-line comments only; inline `// io.ReadAll` after code mid-line still trips it. There's a self-test at `scripts/check-streaming.sh.test.sh`.
- **`migrations/` is filesystem-loaded, not embedded.** If you change where migrations live or `IMGSYNC_MIGRATIONS_DIR`, update both Dockerfile `COPY`/build lines and the env default. `.dockerignore` does NOT exclude `migrations/`.
- **`.dockerignore` excludes `deploy/`, `scripts/`, `e2e/`, `docs/`, `.git/`, `.github/`, `README.md`, `PRD.txt`, `testdata/`.** The build context is source + `migrations/` only; don't expect chart/script files inside the image.
- **values.yaml sniffer patterns are raw Go `text/template`, not Helm-templated.** `dstPattern: "/incoming/{{.file_path}}"`, `srcPattern: "src://images/{{.id}}"` etc. are passed through verbatim to the sniffer's runtime template engine (rendered per-row). Do not Helm-escape them.
- **`extraVolumes`/`extraVolumeMounts`** append to BOTH worker and sniffer (used by e2e to mount shared NFS/hostPath); worker always also mounts a 64Mi `emptyDir` at `/tmp`. Production installs leave them empty.
- **Image tag resolution**: `image.tag` defaults to `.Chart.AppVersion` (currently `0.1.0`), not the Makefile `VERSION`/git-describe. Chart `version` and `appVersion` (both `0.1.0`) live in `Chart.yaml`.
- **Dashboard JSON is an unwired scaffold.** `deploy/helm/imgsync/dashboards/imgsync-overview.json` (30 lines, empty `panels: []`) is NOT referenced by any template/ConfigMap — it's a standalone Grafana stub. There is no dashboards ConfigMap template.
- **`make helm-test` runs `tests/template_test.sh`**, which is excluded by `.helmignore` (`tests/` won't ship in a packaged chart). It resolves the chart dir relative to itself (`SCRIPT_DIR/..`), so cwd-independent.
- E2E secrets (`imgsync-dsn`, `imgsync-db-dsn`, `imgsync-source-dsn`) are created imperatively by `scripts/e2e-up.sh` via `kubectl create secret ... --dry-run=client -o yaml | kubectl apply`; the chart itself assumes they pre-exist. `e2e-up.sh` also pre-creates the `imgsync` ServiceAccount (with Helm adoption labels/annotations) so the pre-install migrate-job hook can reference it.

## Testing Strategy, Build Tags & E2E

A three-layer test pyramid gated by Go build tags and env vars: **unit** (default, always in CI), **integration** (`integration` tag, Docker/testcontainers Postgres), and **E2E** (`e2e` tag + `IMGSYNC_E2E=1`, live kind cluster). A separate shell-based **streaming guard** (`scripts/check-streaming.sh`) is a hard CI gate that greps source for buffering anti-patterns. An agent editing tests must know which tag/env gates each layer or tests silently won't compile/run.

### Key files & symbols
- `Makefile` — single source of truth for invocation. `test:` = `go test ./... -race -count=1`; `ci: lint streaming-check test`; `streaming-check:` runs the guard. E2E targets `e2e-throughput`/`e2e-dirty-state`/`e2e-sniffer` each prepend `IMGSYNC_E2E=1 go test -tags e2e -timeout {35m,30m,20m}`. Integration targets `test-integration-sniffer` (`-tags integration -timeout 5m -run "TestS[0-3]_" ./internal/sniffer/`) and `test-integration-metrics` (`-tags integration -timeout 5m ./internal/metrics/`).
- `.github/workflows/ci.yml` — `lint-and-test` job runs streaming guard → golangci-lint → `go test ./... -race -count=1` (no tags, so integration/e2e excluded). Separate `e2e` job runs only `if: contains(...labels...'e2e')`, spins kind (`helm/kind-action`) + helm (`azure/setup-helm`), runs `make e2e-throughput` then `make e2e-dirty-state` (the latter `if: success() || failure()`), and `make e2e-down` (`if: always()`). **C5' sniffer E2E is NOT wired into CI** — only the throughput + dirty-state targets are.
- `e2e/helpers.go:kindEnv` — live-cluster harness struct (pgxpool + port-forward cmd + `tearingDown atomic.Bool` watchdog). `bootstrapKindEnv`/`bootstrapKindEnvSized` run `./scripts/e2e-up.sh`, port-forward `svc/postgres` to `127.0.0.1:5433`, seed fixtures. Helpers: `helmUpgrade`/`helmRollback`/`helmUninstall`, `waitReplicasReady`, `enqueueLocalFSJobs` (bulk INSERT, not the CLI), `waitAllSucceeded`, `countByStatus`, `truncateJobs`, `truncateSnifferState`, `killOnePod`, `waitForLeasedJob`, `inspectSweeperRecovery`.
- `e2e/throughput_test.go:TestC7_ThroughputScaleOut` — C7 linearity.
- `e2e/dirty_state_test.go:TestF5_DirtyStateRecovery` — F5 with 3 subtests.
- `e2e/sniffer_test.go:TestC5Prime_SnifferSelfAudit` — C5' enqueue correctness; helpers `openSourcePool` (port-forward `svc/source-postgres`→`5434`), `snifferCounts`, `countDistinctTraceIDs`.
- `e2e/kind_config.yaml` — 1 control-plane + 3 workers, all `extraMounts` map host `/tmp/imgsync-e2e-localfs` → node `/srv/imgsync` (shared LocalFS volume backing hostPath PV).
- `internal/sniffer/integration_test.go:TestS0..TestS3` — `//go:build integration`. Helpers `setupSourceDB` (in `query_test.go`) and `setupImgsyncDBWithTransferJobs` (in `state_test.go`) are **untagged** — they compile in the default build but only run when an integration test func calls them.
- `internal/metrics/integration_test.go:setupPG` — `//go:build integration`; testcontainers `postgres:16-alpine` + `db.ApplyMigrations(ctx, dsn, "../../migrations")`. Tests `TestQueueDepthCollector_ReflectsPerStatusCount`, `TestLeaseLockAge_IsZeroWhenNoLeasedRows`, `TestDBPoolCollector_ExposesInUseIdleMax`.
- `internal/db/migrate_integration_test.go:TestMigrate_0003_StatusIndex` — `//go:build integration`; asserts `transfer_jobs_status_idx` exists after migrations. **Not in any Makefile target** — runs only via raw `go test -tags integration ./internal/db/`.
- `scripts/check-streaming.sh` — the streaming guard. `scripts/check-streaming.sh.test.sh` — self-test for the guard.

### How it works / flow
- **Unit (default):** `go test ./... -race -count=1`. `-count=1` disables the test cache; `-race` is mandatory. No tags ⇒ files with `//go:build integration` or `//go:build e2e` are excluded from compilation.
- **Integration (`integration` tag, needs Docker):** Each `setupPG`/`setupSourceDB` spins a fresh `postgres:16-alpine` testcontainer per test and applies real migrations from `../../migrations`. **Sniffer S0-S3** exercise the queue invariants against live PG: **S0** `TestS0_PollingOverlapNoDuplicate` — rewinds `sniffer_state.last_run_ts` by 25m (also nulls `last_run_pk`) to force an overlapping window; asserts `ON CONFLICT (trace_id, dst) DO NOTHING` yields exactly 1 row (RowsAffected==1 guards that run #1 actually wrote `sniffer_state`). **S1** `TestS1_CrashRecoveryNoLossNoDup` — 100 rows, BatchSize=50, two fresh `Sniffer` instances back-to-back (only the persisted watermark carries over); asserts `COUNT==COUNT(DISTINCT)==100`. **S2** `TestS2_TieBreakBatchCorrectness` — 10 rows with identical `updated_at`, BatchSize=3, RunOnce called 5× ⇒ drains in 4 batches; asserts all 10 enqueued once and `sniffer_state.last_run_pk=="10"` (PK tie-break, via `sniffer.NewStateRepo(imgPool).Load`). **S3** `TestS3_QueryTimeoutLeavesWatermarkUnchanged` — a `pg_sleep(2)` view (`images_slow`) + 100ms ctx timeout; asserts `RunOnce` errors, watermark stays zero (`LastRunTS.IsZero()`), 0 rows enqueued (fetch-then-commit ordering).
- **E2E (`e2e` tag + `IMGSYNC_E2E=1`):** Every E2E test first checks `os.Getenv("IMGSYNC_E2E") != "1"` and `t.Skip`s otherwise — so even compiled-in (`-tags e2e`) they no-op without the env var. `bootstrapKindEnv` runs `e2e-up.sh`, deploys via `helm upgrade --install` with `image.repository=imgsync, image.tag=e2e, image.pullPolicy=IfNotPresent`.
  - **C7** (`TestC7_ThroughputScaleOut`, 30m ctx / 35m timeout): Phase A = 2 replicas / 1000 jobs, Phase B = 8 replicas / 1000 fresh jobs. `truncateJobs` (`TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE` + wipes the dst dir) between phases so both pay equal block-allocation cost (rename-over-existing would skew). Wall-clock measured by `waitAllSucceeded` (1s poll). Asserts **`tputB/tputA >= 3.2`** (`const minRatio`; linearity target for a 4× scale-out), scale 2→8 ready `<= 5min`, and `dead==0`.
  - **F5** (`TestF5_DirtyStateRecovery`, 25m ctx): **F5a** kills a pod mid-flight (`killOnePod` force-deletes `pods[0]` with `--grace-period=0 --force`), snapshots `status='leased'` ids *before* the kill (race-safe), fast-forwards their `locked_at = NOW() - INTERVAL '6 minutes'` so the sweeper (5m default threshold) recovers them; asserts sweeper-recovered jobs ended `succeeded` with `attempts==0` and an `'expire'` event (`inspectSweeperRecovery`), and `dead==0`, `leased==0`. **F5b** (50 jobs) pushes a bad image tag (`does-not-exist`→ImagePullBackOff) then `helmRollback`; asserts all 50 drain, `dead==0`, `leased==0`. **F5c** (30 jobs) uninstalls mid-drain, asserts all 30 rows survive across `pending+leased+succeeded` (helm uninstall never touches DB), fast-forwards orphaned leased rows, reinstalls (pre-install hook re-runs `migrate up` — must be idempotent), asserts drain.
  - **C5'** (`TestC5Prime_SnifferSelfAudit`, 20m ctx): uses `bootstrapKindEnvSized(..., 1024)` (1KB fixtures). Upgrades chart with `sniffer.enabled=true, sniffer.config.intervalSec=5, sniffer.config.shadow=true, sniffer.config.srcProtocol/dstProtocol=localfs` (+ `srcPattern`/`dstPattern` using `{{.file_path}}`) **before** seeding source rows (avoids the old sniffer racing with default `fs` protocol → would dead==1000), then `truncateJobs` + `truncateSnifferState`. Inserts 1000 source `images` rows with `updated_at = NOW()-INTERVAL '10 seconds'` (clears the `biasSec=5` window), polls until `enqueued>=1000 && pending==0`; asserts `enqueued==1000`, `distinct(trace_id)==1000`, `dead==0`. **shadow=true appends `.imgsync_shadow_v1`** (`sniffer.ShadowSuffix`) to each dst.

- **Streaming guard (`check-streaming.sh`):** Scans `internal/sources`, `internal/transports`, `internal/transfer` (`.go`, excluding `*_test.go`) with regex `\b(io|ioutil)\.ReadAll\b|bytes\.NewBuffer\b.*\bbody\b`, then filters out lines whose content starts with `//` (single-line comment filter via `grep -vE '^[^:]+:[0-9]+:[[:space:]]*//'`). Any match ⇒ `exit 1`. The companion `.test.sh` plants an `io.ReadAll` fixture under a temp `internal/sources/bad/` and asserts the guard exits non-zero.

### Agent notes (gotchas, conventions, constraints)
- **Build-tag gating is exact:** `//go:build integration` and `//go:build e2e`. Without `-tags`, those files don't compile and tests silently don't exist. E2E additionally requires `IMGSYNC_E2E=1` at runtime or every test `t.Skip`s — do not "fix" a passing E2E run that's actually skipping.
- **`-count=1` is mandatory** for `make test`/CI (cache off); `-race` is always on. Keep both when adding unit-test invocations.
- **Sniffer integration helpers are untagged.** `setupSourceDB` (`query_test.go`) and `setupImgsyncDBWithTransferJobs` (`state_test.go`) have NO `//go:build integration` line — they live in `sniffer_test` package files that compile in the default build, while only `integration_test.go` (the S0-S3 funcs) is tagged. If you move/rename these helpers, keep them compilable without the tag or the whole `sniffer_test` package breaks. They use testcontainers (`postgres.Run("postgres:16-alpine")`) but only touch Docker at call-time.
- **Status enum has NO `'processing'`.** Valid statuses: `pending | leased | succeeded | skipped | dead`. The E2E helpers carry explicit comments about this — never reintroduce `processing`. Sweeper resets `leased` → `pending`. (Separately, `transfer_events.status` has its own CHECK set: `enqueue|lease|success|skip|fail|expire|dead`.)
- **Queue invariant under test:** dedup is `ON CONFLICT (trace_id, dst) DO NOTHING` against the `transfer_jobs_trace_id_dst_key UNIQUE(trace_id, dst)` constraint. S0/S1 assert no-dup via `COUNT==COUNT(DISTINCT)`. Don't change dst-uniqueness without updating these.
- **Migrations must be idempotent** — F5c reinstall re-runs the pre-install `migrate up` hook on a populated DB; a non-idempotent migration breaks F5c.
- **Streaming guard known regex gap:** the pattern `bytes\.NewBuffer\b.*\bbody\b` only catches `bytes.NewBuffer` and `body` on the *same line* and only matches the identifier substring `body` (e.g. won't catch `resp.Body`, multi-line buffering, or `bytes.Buffer{}` accumulation). It also matches inside block comments (`/* */`) since the comment filter only strips lines starting with `//`. Treat it as a tripwire, not a proof — adding buffering on separate lines or via other helpers will pass the guard.
- **Streaming guard is a hard CI gate** (first step of `lint-and-test`, also `make ci` / `make streaming-check`) and scans only `internal/{sources,transports,transfer}`. New streaming hot-path packages are NOT covered unless added to the `DIRS` array in the script.
- **C7 ratio threshold is 3.2** (`const minRatio = 3.2`). It is not exactly 4.0 by design (polling jitter + scale overhead). Don't tighten without re-validating on real kind hardware.
- **C5' E2E is not in CI** (no Makefile→CI wiring); run manually via `make e2e-sniffer`. `TestMigrate_0003_StatusIndex` has no Makefile target either — invoke with `go test -tags integration ./internal/db/`.
- **kind shared volume:** all nodes mount host `/tmp/imgsync-e2e-localfs`→`/srv/imgsync`; `enqueueLocalFSJobs`/fixtures use raw FS paths (no `file://` scheme) because LocalFS Source/Transport call `os.Open`/`os.Create` directly. E2E DB access is via port-forward (`5433` imgsync `imgsync:imgsync`, `5434` source `source:source`) with a watchdog goroutine that `t.Errorf`s if the forward dies before teardown.

## Repository & Docs Map (where to find more)

imgsync is a Go + PostgreSQL file-transfer work queue that replaces an in-house NiFi pipeline. The authoritative product intent lives in `PRD.txt`; the human-facing guide is a Korean MkDocs site under `docs/` (served from `mkdocs.yml`); the deepest engineering ground-truth lives in the **external design doc** (not in-repo) plus the in-repo `docs/superpowers/{plans,specs}` and `docs/test-reports`. An AI agent should grep this index first to locate which file holds authoritative detail before reading code.

### Key files & symbols
- `PRD.txt` — product intent (Korean). Roles: 대량 파일 전송, 파일 단위 traceability, FTP 세션 재사용, worker scale-out. Transfer modes: `remote→local` (default), plus `local→local` / `local→remote` / `remote→remote` (추후/future). Protocols: FTP (default), S3 (PRD intent). Components: Client (Bubble Tea TUI), Connector (DB/File, daemon|bulk), Backend Server, DB (PostgreSQL queue+audit), worker. Dev=Docker, prod=K8s. **Note:** S3 + Client TUI + Backend Server are PRD-level roadmap, not yet in code — only FTP + LocalFS sources/transports exist (`internal/{sources,transports}/{ftp,localfs}`).
- `README.md` — English quickstart: `make docker-build/dev-up/dev-seed/dev-smoke` (compose: postgres+ftpd+1 worker, 10 LocalFS jobs) and Helm install (DSN secret `imgsync-dsn`, FTP secret `imgsync-ftp`, chart at `deploy/helm/imgsync`, pre-install migration hook). Full `make` target table is here.
- `mkdocs.yml` — site config + `nav`. `strict: true`. ReadTheDocs theme, Korean (`language: ko`). `exclude_docs` hides `superpowers/`, `test-reports/`, `e2e-real-cluster-guide.md`, and `README.md` from the public site (they remain in-repo for agents).
- `migrations/000{1,2,3}_*.{up,down}.sql` — forward-only SQL: `0001_initial` (`transfer_jobs`/`transfer_events`, `job_status` ENUM = `pending|leased|succeeded|skipped|dead`, UNIQUE `(trace_id, dst)`), `0002_sniffer_state`, `0003_jobs_status_index` (creates `transfer_jobs_status_idx`). Applied in lexical order (`sort.Strings`) by `internal/db.ApplyMigrations`; `schema_migrations` tracks applied versions. Note `0001` lacks a `.down.sql`.
- **External design doc (NOT in repo):** `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` (currently **revision 5 APPROVED**; in-repo plans cite "rev 4", and the test plan header was generated against "rev 3") and test plan `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md`. Every plan/spec cross-references these — they are the canonical architecture/test source.

### docs/ site map (mkdocs nav, one line per page)
- `docs/index.md` — landing; what imgsync solves, who uses it.
- **getting-started/** — `index.md` (5-min vs 15-min path table); `quickstart-docker-compose.md` (laptop smoke); `quickstart-kind.md` (kind+Helm topology).
- **concepts/** — `index.md` (reading order); `architecture.md` (component diagram + data flow, "의도적으로 하지 않은 것"/intentional non-goals); `job-queue-model.md` (Two-Table Minimal: `transfer_jobs`+`transfer_events`, 상태 전이도/state transitions); `components.md` (Worker/Sniffer/Sweeper responsibilities + per-component metrics emission); `sources-and-transports.md` (Source/Transport interfaces + impls); `glossary.md` (Job, trace_id, …).
- **installation/** — `index.md`; `helm.md` (cluster deploy); `secrets.md` (DSN/FTP secret prep); `values-reference.md` (full values.yaml reference).
- **configuration/** — `index.md` (env vars vs Helm values vs Config structs); `environment-variables.md`; `worker.md`; `sniffer.md`; `sweeper.md`; `protocols.md` (`localfs` + `ftp` supported; `s3` documented as 예정/planned, not implemented).
- **cli/** — `index.md` (single cobra binary, subcommands `migrate`/`enqueue`/`worker`/`sniffer`); `migrate.md` (lists migrations 0001/0002/0003 + intent); `worker.md`; `sniffer.md`; `enqueue.md` (idempotent via `(trace_id, dst)` UNIQUE + `ON CONFLICT DO NOTHING`).
- **operating/** — `index.md`; `runbook.md` (증상→SQL→조치 incident procedures — start here for ops); `monitoring.md` (Prometheus `/metrics`, alarms); `dashboards.md` (Grafana); `scaling.md` (replicaCount throughput, SQL+PromQL scale signals); `upgrades-and-rollback.md`; `troubleshooting.md` (metrics-first diagnostic table).
- **developer/** — `index.md` (`make ci`); `build-and-test.md`; `architecture-deep-dive.md` (`internal/metrics` architecture etc.); `e2e-manual.md` (kind+helm manual verify); `contributing.md` (PR/style); `release-process.md`.
- `docs/faq.md` — design Q&A (왜 Kafka/RabbitMQ 가 아니라 Postgres; `FOR UPDATE SKIP LOCKED` lease guarantee; `(trace_id, dst)` idempotency), each links into concept pages.
- `docs/e2e-real-cluster-guide.md` — real Talos homelab E2E guide (excluded from public site).

### docs/superpowers/plans/ — phased implementation plans (chronological)
- `2026-04-27-imgsync-v1-week1-foundation.md` — Go module, schema migration, pgx pool, streaming Source/Transport interfaces, LocalFS impls, CI streaming guard, `enqueue` CLI. No FTP/worker yet.
- `2026-04-27-imgsync-v1-week2a-ftp-worker-core.md` — FTP Source/Transport (`jlaffaye/ftp` v0.2.0, per-host pool, NOOP ping), worker dispatch SQL (`FOR UPDATE SKIP LOCKED` lease/return, `internal/worker/job.go`), per-job processor (sha256 + byte-count + size-mismatch verify, D6/F4).
- `2026-04-27-imgsync-v1-week2b-sweeper-eval.md` — sweeper (`pg_try_advisory_xact_lock(hashtext('imgsync_sweeper'))` single-writer, 30s cycle, recovers leases older than 5 min), shared jittered idle backoff (F2), FTP host concurrency cap (F1, session-scoped `pg_advisory_lock` semaphore on a pinned conn), `/livez /readyz /healthz`, EVAL invariants C0/C1/C2/C3/C6.
- `2026-04-27-imgsync-v1-week3-helm-cutover.md` — distroless Dockerfile (`gcr.io/distroless/static-debian12:nonroot`), Helm chart (worker Deployment+Service+PDB, `migrate up` pre-install/pre-upgrade Job hook), cutover gates C7 (≥3.2× scale-out 2→8 replicas) and F5 (dirty-state recovery).
- `2026-04-27-imgsync-shadow-sniffer.md` — `imgsync sniffer` subcommand: polls source Postgres, deterministic trace_ids, idempotent enqueue, watermark in `sniffer_state` (one row per source_id, `(timestamp, pk)` tie-break, migration 0002). Doubles as v2 DB Connector. Spec: shadow-sniffer-design.md.
- `2026-05-03-imgsync-e2e-real-cluster.md` — reproduce 5 E2E scenarios (C5'/F5a/F5b/F5c/C7) on real Talos homelab k8s via `e2e/manifests/real/values-real.yaml` + `scripts/e2e-*-real.sh`; NFS RWX PVCs; ghcr.io image. Go suite stays kind-only.
- `2026-05-05-imgsync-public-docs-site.md` — build the MkDocs site (this `docs/` tree), GitHub Pages deploy, `make docs-*` targets.
- `2026-05-05-monitoring-phase-1.md` — `internal/metrics` package (own `prometheus.NewRegistry`, `Metrics` struct callbacks via existing `OnXxx` pattern: `OnLeaseAttempt`/`OnJobFinished`/`OnSweepCycle`/`OnSnifferEnqueue`/`OnSnifferError`/`OnFTPPoolChange`), `/metrics` on worker + sniffer (sniffer via `health.WithMetrics`), Grafana dashboard. Metric names are `imgsync_*` (`jobs_in_status`, `jobs_processed_total`, `lease_attempts_total`, `job_duration_seconds`, `sweep_cycles_total`, `ftp_pool_size`, `sniffer_enqueue_total`, `sniffer_run_errors_total`, `workers_active`, `db_pool_conns`, `lease_lock_age_seconds`). Spec: monitoring-stack-integration-design.md.
- `2026-05-05-monitoring-phase-1-5-status-index.md` — migration `0003_jobs_status_index` (`transfer_jobs_status_idx`) so `imgsync_jobs_in_status` scrape GROUP BY drops to index-only scan; migration-only, no Go/Helm change.
- `2026-05-05-monitoring-phase-1-5-explain.md` — local `EXPLAIN ANALYZE` verification of the 0003 index against a 1,050,305-row synthetic distribution (control vs indexed).
- `2026-05-06-monitoring-phase-1-guide-docs.md` — docs-only PR closing the gap where PR #13 (status index, commit `6df49a08`) / PR #14 (monitoring stack, commit `2e23aa49`) touched code/Helm but not the public guide pages; verified via `make docs-build` (mkdocs `--strict`). **This is the current branch's lineage** (`docs/monitoring-phase-1-guide-2026-05-06`).

### docs/superpowers/specs/ — design specs (precede plans)
- `2026-04-27-imgsync-shadow-sniffer-design.md` — resolves shadow-mode trigger: polling sniffer independent of NiFi; basis for the sniffer plan and v2 Connector.
- `2026-05-05-monitoring-stack-integration-design.md` — standard observability stack; defines Phase 1 (`/metrics` + Grafana), Phase 1.5 (status index, scrape cost), the source for both monitoring plans.

### docs/test-reports/ — verification records
- `2026-05-01-imgsync-a69bcb0.md` — full test run at commit `a69bcb0` (main, post PR #6): go vet, gofmt, streaming guard, Helm lint/structure, unit (17 pkgs, race+count=1), integration (`-tags integration`, sniffer 22/22), prod binary build (18MB, 5 subcommands incl. `--help`). All PASS; E2E gated on `IMGSYNC_E2E=1`.
- `2026-05-03-imgsync-real-cluster-0612277.md` — real Talos homelab k8s (k8s v1.36.0, Talos v1.13.0, 3 CP + 2 worker, NFS RWX), 5/5 scenarios PASS: C5' (sniffer self-audit), F5a (mid-flight worker kill → sweeper recovery, attempts=0), F5b (bad helm upgrade → rollback), F5c (uninstall→reinstall idempotent migration), C7 (throughput scale-out 2/4/6/8 replicas). Image-pull, migration hook, NFS PVC perms also exercised.

### Agent notes (gotchas, conventions, constraints)
- **The external design doc + eng-review test plan (`~/.gstack/projects/nineking424-imgsync/...`) are ground truth, not the in-repo docs.** Invariant IDs are referenced across plans (`C0`–`C7`, `D6`, `F4`, plus cutover gates `C7`/`F5`/`F5a–c`); the external test plan defines `C0`–`C9` + `C5'` as EVAL/test invariants and uses `F1`–`F4` for open-questions/follow-ups (different semantics), with `D7` as a locked decision (no `D6` there). When in doubt, read both before changing queue/lease/sweeper/streaming semantics. The current design doc on disk is **revision 5 APPROVED**.
- `mkdocs.yml` has `strict: true` and `exclude_docs` covering `superpowers/`, `test-reports/`, `e2e-real-cluster-guide.md`, `README.md`. Any nav/link break fails `make docs-build` (= `mkdocs build --strict`). Adding a page under a nav'd dir without updating `nav` will break the build. Docs venv is `.venv-docs/` (`requirements-docs.txt`); the public docs site is Korean prose — code/commands stay English.
- Plans use checkbox (`- [ ]`) task syntax and declare a REQUIRED SUB-SKILL (`superpowers:subagent-driven-development` / `executing-plans`). Treat plans as already-executed history unless told otherwise — they describe how the code was built, file-by-file.
- Migrations are **forward-only**, applied in lexical order by `internal/db.ApplyMigrations` (Helm pre-install/pre-upgrade hook auto-runs them). Add the next `000N_*.up.sql`+`.down.sql` pair; do not edit/reorder existing ones (`0001` has no down).
- Commit convention seen in-tree: `docs(<area>): <subject>` with a `Co-Authored-By: Claude ...` trailer; monitoring docs PRs are docs-only (no code/chart changes) — keep that boundary.
- Do not treat `docs/operating/{monitoring,dashboards,runbook}.md` as stale: the 2026-05-06 plan explicitly leaves them untouched because PR #14 already updated them.

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
