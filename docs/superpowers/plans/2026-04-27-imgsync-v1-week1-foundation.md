# imgsync v1 — Week 1 Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the structural foundation of imgsync v1 — Go module, schema migration, pgx pool, streaming Source/Transport interfaces, LocalFS reference implementations, CI streaming guard, and the `enqueue` CLI command. No FTP, no worker yet.

**Architecture:** Single Go binary (`imgsync`) with pgx/v5 connecting to PostgreSQL. Streaming I/O contract enforced via `io.Reader` everywhere (no `io.ReadAll` in src/). Migrations are forward-only SQL files run from `migrations/`. LocalFS Source/Transport serve as reference impls and as test harness for the worker (Week 2).

**Tech Stack:** Go 1.22, pgx/v5, testcontainers-go (postgres), golang-migrate, golangci-lint, GitHub Actions.

**Series:** This is plan 1 of 3 for v1 base. Follow-ups: `2026-04-27-imgsync-v1-week2-worker-ftp.md` (worker, FTP, sweeper, retry, idle backoff, EVAL tests), `2026-04-27-imgsync-v1-week3-helm-cutover.md` (Dockerfile, Helm chart, init Job hook, F5 dirty-state recovery E2E).

**Spec reference:** `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` (rev 4 APPROVED). Section "The Assignment" items 5-6.

---

## File Structure

After Week 1 completes, the repo layout is:

```
imgsync/
├── cmd/imgsync/
│   ├── main.go              # cobra root + subcommand wiring
│   ├── enqueue.go           # `imgsync enqueue` subcommand
│   └── migrate.go           # `imgsync migrate` subcommand
├── internal/
│   ├── db/
│   │   ├── pool.go          # pgxpool wrapper, env-driven config
│   │   ├── pool_test.go
│   │   ├── migrate.go       # forward-only SQL migration runner
│   │   └── migrate_test.go
│   ├── transfer/
│   │   ├── interfaces.go    # Source, Transport interfaces
│   │   ├── errors.go        # ErrSkippable, ErrPermanent sentinels
│   │   └── errors_test.go
│   ├── jobs/
│   │   ├── enqueue.go       # idempotent INSERT + enqueue event
│   │   └── enqueue_test.go
│   ├── sources/localfs/
│   │   ├── source.go        # LocalFS Source impl
│   │   └── source_test.go
│   └── transports/localfs/
│       ├── transport.go     # LocalFS Transport impl (atomic rename)
│       └── transport_test.go
├── migrations/
│   └── 0001_initial.up.sql  # transfer_jobs, transfer_events, indexes
├── scripts/
│   └── check-streaming.sh   # CI guard: forbid io.ReadAll under src/
├── .github/workflows/
│   └── ci.yml               # lint + test + streaming guard
├── .golangci.yml
├── .gitignore
├── Makefile
├── go.mod
├── go.sum
├── LICENSE
├── PRD.txt
└── README.md
```

Each file has one responsibility. `internal/db` owns connection + migration. `internal/transfer` owns the streaming contract. Source/Transport adapters live in their own subpackages so adding FTP in Week 2 is additive (no edits to interfaces).

---

## Task 1: Go module scaffold

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `.golangci.yml`
- Create: `cmd/imgsync/main.go` (skeleton only)

This task lays the module skeleton. It is not TDD — there's no behavior to test yet — but every following task is.

- [ ] **Step 1: Initialize Go module**

Run from repo root:
```bash
go mod init github.com/nineking424/imgsync
```

- [ ] **Step 2: Add baseline dependencies**

```bash
go get github.com/jackc/pgx/v5@latest
go get github.com/jackc/pgx/v5/pgxpool@latest
go get github.com/spf13/cobra@latest
go get github.com/testcontainers/testcontainers-go@latest
go get github.com/testcontainers/testcontainers-go/modules/postgres@latest
go get github.com/stretchr/testify@latest
```

- [ ] **Step 3: Write `.gitignore`**

```gitignore
/imgsync
/bin/
/dist/
*.test
*.out
.env
.envrc
.idea/
.vscode/
.DS_Store
```

- [ ] **Step 4: Write `Makefile`**

```makefile
.PHONY: build test lint streaming-check tidy

build:
	go build -o bin/imgsync ./cmd/imgsync

test:
	go test ./... -race -count=1

lint:
	golangci-lint run

streaming-check:
	./scripts/check-streaming.sh

tidy:
	go mod tidy

ci: lint streaming-check test
```

- [ ] **Step 5: Write `.golangci.yml`**

```yaml
run:
  timeout: 3m
  go: "1.22"

linters:
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gofmt
    - goimports
    - revive
    - bodyclose
    - misspell

linters-settings:
  revive:
    rules:
      - name: exported
      - name: var-naming
      - name: error-return
      - name: error-strings

issues:
  exclude-dirs:
    - bin
    - dist
```

- [ ] **Step 6: Write `cmd/imgsync/main.go` skeleton**

```go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "imgsync",
		Short: "imgsync: file transfer queue (Go + PostgreSQL)",
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 7: Verify build**

Run:
```bash
go mod tidy
go build ./...
```
Expected: succeeds with no output. `bin/` is empty (we only built; we didn't install).

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum .gitignore Makefile .golangci.yml cmd/imgsync/main.go
git commit -m "chore: scaffold Go module and CLI skeleton"
```

---

## Task 2: Migration 0001 — initial schema

**Files:**
- Create: `migrations/0001_initial.up.sql`
- Create: `internal/db/migrate.go`, `internal/db/migrate_test.go`

The schema is locked by the design doc rev 4. Reproduce it exactly. Do not introduce columns or indexes the spec does not mention — those belong in later migrations.

- [ ] **Step 1: Write the failing migration test**

Create `internal/db/migrate_test.go`:

```go
package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
	"time"
)

func TestApplyMigrations_FreshDB_CreatesSchema(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		tc.WithWaitStrategy(postgres.DefaultWaitStrategy(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })

	var jobsExists bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='transfer_jobs')`,
	).Scan(&jobsExists))
	require.True(t, jobsExists, "transfer_jobs table missing")

	var eventsExists bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='transfer_events')`,
	).Scan(&eventsExists))
	require.True(t, eventsExists, "transfer_events table missing")

	var uniqueIdx bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname='transfer_jobs_trace_id_dst_key')`,
	).Scan(&uniqueIdx))
	require.True(t, uniqueIdx, "UNIQUE(trace_id, dst) index missing")

	var pendingIdx bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname='transfer_jobs_pending_idx')`,
	).Scan(&pendingIdx))
	require.True(t, pendingIdx, "partial pending index missing")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/... -run TestApplyMigrations_FreshDB_CreatesSchema -v`
Expected: FAIL — `db.ApplyMigrations` undefined and `migrations/0001_initial.up.sql` does not exist.

- [ ] **Step 3: Write `migrations/0001_initial.up.sql`**

```sql
-- imgsync v1 initial schema
-- Spec: design doc rev 4, section "Database Schema"

BEGIN;

CREATE TYPE job_status AS ENUM (
    'pending',
    'leased',
    'succeeded',
    'skipped',
    'dead'
);

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
    CONSTRAINT transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)
);

CREATE INDEX transfer_jobs_pending_idx
    ON transfer_jobs (next_run_at, id)
    WHERE status = 'pending';

CREATE INDEX transfer_jobs_leased_idx
    ON transfer_jobs (locked_at)
    WHERE status = 'leased';

CREATE INDEX transfer_jobs_trace_id_idx
    ON transfer_jobs (trace_id);

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

CREATE TABLE schema_migrations (
    version    TEXT        PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version) VALUES ('0001_initial');

COMMIT;
```

- [ ] **Step 4: Write `internal/db/migrate.go`**

```go
package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ApplyMigrations runs every *.up.sql under dir in lexical order, skipping
// versions already recorded in schema_migrations. The first migration creates
// schema_migrations itself, so a fresh DB starts empty.
func ApplyMigrations(ctx context.Context, dsn, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	applied := map[string]bool{}
	if hasTable(ctx, conn, "schema_migrations") {
		rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			applied[v] = true
		}
		rows.Close()
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".up.sql")
		if applied[version] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

func hasTable(ctx context.Context, conn *pgx.Conn, name string) bool {
	var exists bool
	_ = conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name=$1)`,
		name,
	).Scan(&exists)
	return exists
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/db/... -run TestApplyMigrations_FreshDB_CreatesSchema -v`
Expected: PASS. Test takes ~10-20s due to postgres container startup.

- [ ] **Step 6: Add idempotency test**

Append to `internal/db/migrate_test.go`:

```go
func TestApplyMigrations_RunTwice_NoOp(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		tc.WithWaitStrategy(postgres.DefaultWaitStrategy(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	conn, _ := pgx.Connect(ctx, dsn)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	var n int
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n))
	require.Equal(t, 1, n, "migration recorded twice")
}
```

Run: `go test ./internal/db/... -v`
Expected: both tests PASS.

- [ ] **Step 7: Commit**

```bash
git add migrations/ internal/db/migrate.go internal/db/migrate_test.go
git commit -m "feat(db): add migration runner and 0001 initial schema"
```

---

## Task 3: pgxpool wrapper

**Files:**
- Create: `internal/db/pool.go`, `internal/db/pool_test.go`

Design doc requires `MaxConns` per pod and `QueryTimeout` per statement. The wrapper accepts an env-driven config so the worker, sniffer, and CLI all share one bootstrap path.

- [ ] **Step 1: Write the failing test**

Create `internal/db/pool_test.go`:

```go
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
)

func TestNewPool_AppliesConfig(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		tc.WithWaitStrategy(postgres.DefaultWaitStrategy(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")

	pool, err := db.NewPool(ctx, db.PoolConfig{
		DSN:               dsn,
		MaxConns:          4,
		MinConns:          1,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.Equal(t, int32(4), pool.Config().MaxConns)
	require.Equal(t, int32(1), pool.Config().MinConns)

	var one int
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
	require.Equal(t, 1, one)
}

func TestNewPool_BadDSN_ReturnsError(t *testing.T) {
	_, err := db.NewPool(context.Background(), db.PoolConfig{
		DSN:      "postgres://nope:nope@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
		MaxConns: 1,
	})
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/... -run TestNewPool -v`
Expected: FAIL — `db.NewPool` and `db.PoolConfig` undefined.

- [ ] **Step 3: Write `internal/db/pool.go`**

```go
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is the env-derived configuration for a pgxpool.Pool.
type PoolConfig struct {
	DSN               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// NewPool builds and pings a pgxpool.Pool. The caller owns Close().
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/pool.go internal/db/pool_test.go
git commit -m "feat(db): add pgxpool wrapper with env-driven config"
```

---

## Task 4: Source/Transport interfaces and error sentinels

**Files:**
- Create: `internal/transfer/interfaces.go`, `internal/transfer/errors.go`, `internal/transfer/errors_test.go`

The streaming contract is the load-bearing invariant of v1. Every Source returns `io.ReadCloser`. Every Transport accepts an `io.Reader`. No `[]byte` body parameters.

- [ ] **Step 1: Write the failing test**

Create `internal/transfer/errors_test.go`:

```go
package transfer_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/stretchr/testify/require"
)

func TestErrSkippable_WrapsAndUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("size mismatch: %w", transfer.ErrSkippable)
	require.True(t, errors.Is(wrapped, transfer.ErrSkippable))
	require.False(t, errors.Is(wrapped, transfer.ErrPermanent))
}

func TestErrPermanent_WrapsAndUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("auth failed: %w", transfer.ErrPermanent)
	require.True(t, errors.Is(wrapped, transfer.ErrPermanent))
	require.False(t, errors.Is(wrapped, transfer.ErrSkippable))
}

func TestErrSentinels_AreDistinct(t *testing.T) {
	require.NotEqual(t, transfer.ErrSkippable, transfer.ErrPermanent)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transfer/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/transfer/errors.go`**

```go
package transfer

import "errors"

// ErrSkippable signals the worker to mark the job 'skipped' (terminal, audit-only).
// Use for D6 size mismatch, dst-already-exists with identical sha256, etc.
var ErrSkippable = errors.New("skippable: job intentionally not transferred, audit only")

// ErrPermanent signals the worker to mark the job 'dead' immediately, bypassing
// the retry budget. Use for malformed src URI, auth failures, or any error that
// will not change with another attempt.
var ErrPermanent = errors.New("permanent: do not retry, mark dead")
```

- [ ] **Step 4: Write `internal/transfer/interfaces.go`**

```go
package transfer

import (
	"context"
	"io"
)

// Source opens a streaming reader for src. The caller MUST Close the returned
// ReadCloser. Implementations MUST NOT buffer the body in memory.
//
// Returned size is the source's reported byte count, or -1 if unknown.
type Source interface {
	Open(ctx context.Context, src string) (body io.ReadCloser, size int64, err error)
}

// Transport streams body to dst. expectedSize is the source's reported byte count
// (-1 if unknown). Implementations MUST count bytes actually written and compute
// sha256 over the streamed bytes; the worker uses these for D6 size verification.
//
// Implementations MUST NOT buffer the entire body in memory.
type Transport interface {
	Send(
		ctx context.Context,
		dst string,
		body io.Reader,
		expectedSize int64,
	) (writtenBytes int64, sha256Hex string, err error)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/transfer/... -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/transfer/
git commit -m "feat(transfer): define streaming Source/Transport interfaces and error sentinels"
```

---

## Task 5: LocalFS Source

**Files:**
- Create: `internal/sources/localfs/source.go`, `internal/sources/localfs/source_test.go`

LocalFS is the reference Source. It validates the streaming contract and serves as the test harness for the worker (Week 2). It also gives us a smoke path that doesn't require an FTP server.

- [ ] **Step 1: Write the failing test**

Create `internal/sources/localfs/source_test.go`:

```go
package localfs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/stretchr/testify/require"
)

func TestSource_Open_StreamsFileAndReportsSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))

	s := localfs.NewSource()
	body, size, err := s.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = body.Close() })

	require.Equal(t, int64(11), size)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestSource_Open_Missing_ReturnsErrPermanent(t *testing.T) {
	s := localfs.NewSource()
	_, _, err := s.Open(context.Background(), "/no/such/path/xyzzy")
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent)
}

func TestSource_Open_Directory_ReturnsErrPermanent(t *testing.T) {
	s := localfs.NewSource()
	_, _, err := s.Open(context.Background(), t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sources/localfs/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/sources/localfs/source.go`**

```go
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nineking424/imgsync/internal/transfer"
)

// Source reads files from the local filesystem. Suitable for tests and as a
// reference implementation of the streaming Source contract.
type Source struct{}

// NewSource constructs a LocalFS Source.
func NewSource() *Source { return &Source{} }

// Open returns an os.File handle. The caller is responsible for Close().
func (s *Source) Open(ctx context.Context, src string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	st, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("localfs: stat %s: %w", src, transfer.ErrPermanent)
		}
		return nil, 0, fmt.Errorf("localfs: stat %s: %w", src, err)
	}
	if st.IsDir() {
		return nil, 0, fmt.Errorf("localfs: %s is a directory: %w", src, transfer.ErrPermanent)
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, 0, fmt.Errorf("localfs: open %s: %w", src, err)
	}
	return f, st.Size(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sources/localfs/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sources/localfs/
git commit -m "feat(sources/localfs): implement streaming LocalFS Source"
```

---

## Task 6: LocalFS Transport

**Files:**
- Create: `internal/transports/localfs/transport.go`, `internal/transports/localfs/transport_test.go`

LocalFS Transport writes via temp file + atomic rename so a partial write never appears at dst. It computes sha256 and counts bytes during the stream copy — the worker uses these for D6.

- [ ] **Step 1: Write the failing test**

Create `internal/transports/localfs/transport_test.go`:

```go
package localfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/stretchr/testify/require"
)

func TestTransport_Send_WritesFileAndReportsBytesAndSha(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.txt")
	body := strings.NewReader("hello world")
	want := sha256.Sum256([]byte("hello world"))

	tr := localfs.NewTransport()
	written, shaHex, err := tr.Send(context.Background(), dst, body, 11)
	require.NoError(t, err)

	require.Equal(t, int64(11), written)
	require.Equal(t, hex.EncodeToString(want[:]), shaHex)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestTransport_Send_AtomicRename_NoPartialAtDst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "atomic.bin")
	tr := localfs.NewTransport()

	// 1 MiB body to make any partial write observable
	payload := bytes.Repeat([]byte{'A'}, 1<<20)
	_, _, err := tr.Send(context.Background(), dst, bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)

	got, _ := os.ReadFile(dst)
	require.Equal(t, len(payload), len(got))

	// no temp leftovers
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover temp file: %s", e.Name())
	}
}

func TestTransport_Send_DstDirMissing_ReturnsError(t *testing.T) {
	tr := localfs.NewTransport()
	_, _, err := tr.Send(context.Background(), "/no/such/dir/out.txt", strings.NewReader("x"), 1)
	require.Error(t, err)
}

func TestTransport_Send_BodyError_DoesNotCreateDst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fail.bin")
	tr := localfs.NewTransport()

	_, _, err := tr.Send(context.Background(), dst, &errReader{}, -1)
	require.Error(t, err)

	_, statErr := os.Stat(dst)
	require.ErrorIs(t, statErr, os.ErrNotExist, "dst created despite body error")
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transports/localfs/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/transports/localfs/transport.go`**

```go
package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Transport writes streaming bodies to the local filesystem with atomic rename.
type Transport struct{}

// NewTransport constructs a LocalFS Transport.
func NewTransport() *Transport { return &Transport{} }

// Send streams body into a tempfile next to dst, fsyncs, and renames atomically.
// Returns bytes written and the sha256 of the streamed bytes (lowercase hex).
func (t *Transport) Send(
	ctx context.Context,
	dst string,
	body io.Reader,
	_ int64,
) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".imgsync-*.tmp")
	if err != nil {
		return 0, "", fmt.Errorf("localfs: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	written, copyErr := io.Copy(mw, body)
	if copyErr != nil {
		_ = tmp.Close()
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: copy: %w", copyErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: rename: %w", err)
	}
	return written, hex.EncodeToString(hasher.Sum(nil)), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transports/localfs/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transports/localfs/
git commit -m "feat(transports/localfs): implement streaming LocalFS Transport with atomic rename"
```

---

## Task 7: CI streaming guard

**Files:**
- Create: `scripts/check-streaming.sh`
- Create: `.github/workflows/ci.yml`

Design doc rev 4 mandates a CI rule that fails any PR introducing `io.ReadAll` or `ioutil.ReadAll` under `internal/sources/`, `internal/transports/`, or `internal/transfer/` (excluding `*_test.go`). This is the structural guard against future contributors silently buffering bodies in memory.

- [ ] **Step 1: Write the failing CI guard test**

There's no Go test for a shell script; the test is "running it on a known-bad fixture exits 1". Create `scripts/check-streaming.sh.test.sh`:

```bash
#!/usr/bin/env bash
# Test for check-streaming.sh: it must catch io.ReadAll under internal/sources/.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

mkdir -p "$TMPDIR/internal/sources/bad"
cat > "$TMPDIR/internal/sources/bad/source.go" <<'EOF'
package bad

import (
	"context"
	"io"
	"os"
)

func Open(_ context.Context, p string) ([]byte, error) {
	f, _ := os.Open(p)
	return io.ReadAll(f)
}
EOF

cd "$TMPDIR"
if "$REPO_ROOT/scripts/check-streaming.sh"; then
  echo "FAIL: streaming guard did not detect io.ReadAll" >&2
  exit 1
fi
echo "PASS: streaming guard rejected bad fixture"
```

Make it executable: `chmod +x scripts/check-streaming.sh.test.sh`

- [ ] **Step 2: Run test to verify it fails**

Run: `bash scripts/check-streaming.sh.test.sh`
Expected: FAIL — `scripts/check-streaming.sh` does not exist.

- [ ] **Step 3: Write `scripts/check-streaming.sh`**

```bash
#!/usr/bin/env bash
# CI guard: forbid io.ReadAll / ioutil.ReadAll inside streaming hot paths.
# Runs from repo root.
set -euo pipefail

DIRS=(
  "internal/sources"
  "internal/transports"
  "internal/transfer"
)

violations=0
for d in "${DIRS[@]}"; do
  if [[ ! -d "$d" ]]; then
    continue
  fi
  matches=$(grep -RnE '\b(io|ioutil)\.ReadAll\b' "$d" \
              --include='*.go' --exclude='*_test.go' || true)
  if [[ -n "$matches" ]]; then
    echo "$matches"
    violations=$((violations + 1))
  fi
done

if (( violations > 0 )); then
  echo ""
  echo "FAIL: io.ReadAll detected in streaming hot path. Use io.Copy or io.Reader chains instead." >&2
  exit 1
fi
echo "OK: no io.ReadAll in streaming hot paths"
```

Make it executable: `chmod +x scripts/check-streaming.sh`

- [ ] **Step 4: Run test to verify it passes**

Run: `bash scripts/check-streaming.sh.test.sh`
Expected: PASS.

Also run on the live tree:
```bash
bash scripts/check-streaming.sh
```
Expected: `OK: no io.ReadAll in streaming hot paths`

- [ ] **Step 5: Write `.github/workflows/ci.yml`**

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

jobs:
  lint-and-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
          cache: true

      - name: Streaming guard
        run: bash scripts/check-streaming.sh

      - name: Lint
        uses: golangci/golangci-lint-action@v6
        with:
          version: v1.59.1

      - name: Test
        run: go test ./... -race -count=1
```

- [ ] **Step 6: Commit**

```bash
git add scripts/ .github/workflows/ci.yml
git commit -m "ci: add streaming hot-path guard and workflow"
```

---

## Task 8: `imgsync enqueue` and `imgsync migrate` CLI commands

**Files:**
- Create: `internal/jobs/enqueue.go`, `internal/jobs/enqueue_test.go`
- Create: `cmd/imgsync/migrate.go`, `cmd/imgsync/enqueue.go`
- Modify: `cmd/imgsync/main.go` (wire subcommands)

`migrate` is the bootstrap command — the Helm init Job (Week 3) calls it. `enqueue` is what tests, ops, and the sniffer (next plan) use to insert jobs idempotently. The CLI commands read all config from env (`IMGSYNC_DSN`); the enqueue payload comes from flags.

Why a separate `internal/jobs` package: `cmd/imgsync` is `package main` and cannot be imported by tests. The actual SQL logic lives in `internal/jobs.Enqueue` and the CLI is a thin wrapper. The sniffer (next plan) imports `internal/jobs.Enqueue` directly.

Spec idempotency rule: `INSERT ... ON CONFLICT (trace_id, dst) DO NOTHING`. Re-enqueue is a no-op.

- [ ] **Step 1: Write the failing test**

Create `internal/jobs/enqueue_test.go`:

```go
package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
)

func mustDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		tc.WithWaitStrategy(postgres.DefaultWaitStrategy(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestEnqueue_InsertsJobWithEnqueueEvent(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID:     "trace-001",
		Src:         "localfs:///in/a.bin",
		Dst:         "localfs:///out/a.bin",
		SrcProtocol: "localfs",
		DstProtocol: "localfs",
		MaxAttempts: 5,
	})
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, id)

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status))
	require.Equal(t, "pending", status)

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status='enqueue'`, id,
	).Scan(&n))
	require.Equal(t, 1, n, "no enqueue event recorded")
}

func TestEnqueue_DuplicateTraceIDDst_IsNoOp(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	args := jobs.EnqueueArgs{
		TraceID:     "trace-dup",
		Src:         "localfs:///in/a.bin",
		Dst:         "localfs:///out/a.bin",
		SrcProtocol: "localfs",
		DstProtocol: "localfs",
		MaxAttempts: 5,
	}

	id1, inserted1, err := jobs.Enqueue(ctx, pool, args)
	require.NoError(t, err)
	require.True(t, inserted1)

	id2, inserted2, err := jobs.Enqueue(ctx, pool, args)
	require.NoError(t, err)
	require.False(t, inserted2, "duplicate enqueue must report inserted=false")
	require.Equal(t, id1, id2, "duplicate enqueue must return existing id")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1`, id1,
	).Scan(&n))
	require.Equal(t, 1, n, "duplicate enqueue should not emit a second event")
}

func TestEnqueue_MissingRequiredFields_ReturnsError(t *testing.T) {
	pool := mustDB(t)
	_, _, err := jobs.Enqueue(context.Background(), pool, jobs.EnqueueArgs{
		TraceID: "",
		Src:     "x",
		Dst:     "y",
	})
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/jobs/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/jobs/enqueue.go`**

```go
package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnqueueArgs is the input to Enqueue.
type EnqueueArgs struct {
	TraceID     string
	Src         string
	Dst         string
	SrcProtocol string
	DstProtocol string
	Payload     []byte // raw JSON; pass nil for empty object
	MaxAttempts int
}

// Enqueue inserts a new transfer_jobs row idempotently. inserted=false means
// the (trace_id, dst) tuple already existed; in that case id is the existing row.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, a EnqueueArgs) (int64, bool, error) {
	if a.TraceID == "" || a.Src == "" || a.Dst == "" {
		return 0, false, errors.New("enqueue: trace_id, src, dst are required")
	}
	if a.SrcProtocol == "" || a.DstProtocol == "" {
		return 0, false, errors.New("enqueue: src_protocol and dst_protocol are required")
	}
	if a.MaxAttempts <= 0 {
		a.MaxAttempts = 5
	}
	payload := a.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	scanErr := tx.QueryRow(ctx, `
INSERT INTO transfer_jobs
  (trace_id, src, dst, src_protocol, dst_protocol, payload, max_attempts)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (trace_id, dst) DO NOTHING
RETURNING id`,
		a.TraceID, a.Src, a.Dst, a.SrcProtocol, a.DstProtocol, payload, a.MaxAttempts,
	).Scan(&id)
	if scanErr == nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail)
VALUES ($1,$2,'enqueue',$3)`,
			a.TraceID, id, payload,
		); err != nil {
			return 0, false, fmt.Errorf("emit enqueue event: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, false, fmt.Errorf("commit: %w", err)
		}
		return id, true, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("insert: %w", scanErr)
	}

	// Conflict path: look up the existing row id.
	if err := tx.QueryRow(ctx,
		`SELECT id FROM transfer_jobs WHERE trace_id=$1 AND dst=$2`,
		a.TraceID, a.Dst,
	).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("lookup existing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit: %w", err)
	}
	return id, false, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/jobs/... -v`
Expected: all PASS.

- [ ] **Step 5: Write `cmd/imgsync/enqueue.go`**

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/spf13/cobra"
)

func newEnqueueCmd() *cobra.Command {
	var (
		traceID     string
		src         string
		dst         string
		srcProto    string
		dstProto    string
		maxAttempts int
	)
	cmd := &cobra.Command{
		Use:   "enqueue",
		Short: "Insert a transfer job (idempotent on trace_id, dst)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 4})
			if err != nil {
				return err
			}
			defer pool.Close()

			id, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
				TraceID:     traceID,
				Src:         src,
				Dst:         dst,
				SrcProtocol: srcProto,
				DstProtocol: dstProto,
				MaxAttempts: maxAttempts,
			})
			if err != nil {
				return err
			}
			if inserted {
				fmt.Fprintf(cmd.OutOrStdout(), "enqueued id=%d trace_id=%s\n", id, traceID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "exists id=%d trace_id=%s (no-op)\n", id, traceID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&traceID, "trace-id", "", "stable trace identifier (required)")
	cmd.Flags().StringVar(&src, "src", "", "source URI (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "destination URI (required)")
	cmd.Flags().StringVar(&srcProto, "src-protocol", "", "source protocol, e.g. localfs, ftp (required)")
	cmd.Flags().StringVar(&dstProto, "dst-protocol", "", "destination protocol (required)")
	cmd.Flags().IntVar(&maxAttempts, "max-attempts", 5, "retry budget")
	_ = cmd.MarkFlagRequired("trace-id")
	_ = cmd.MarkFlagRequired("src")
	_ = cmd.MarkFlagRequired("dst")
	_ = cmd.MarkFlagRequired("src-protocol")
	_ = cmd.MarkFlagRequired("dst-protocol")
	return cmd
}
```

- [ ] **Step 6: Write `cmd/imgsync/migrate.go`**

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply forward-only SQL migrations from a directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			if err := db.ApplyMigrations(ctx, dsn, dir); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "/etc/imgsync/migrations", "directory containing *.up.sql files")
	return cmd
}
```

- [ ] **Step 7: Wire subcommands in `cmd/imgsync/main.go`**

Replace the existing `cmd/imgsync/main.go` with:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root := &cobra.Command{
		Use:           "imgsync",
		Short:         "imgsync: file transfer queue (Go + PostgreSQL)",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newEnqueueCmd())

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./... -race -count=1`
Expected: all PASS.

- [ ] **Step 9: Smoke-test the CLI build**

Run:
```bash
go build -o bin/imgsync ./cmd/imgsync
./bin/imgsync --help
./bin/imgsync enqueue --help
./bin/imgsync migrate --help
```
Expected: each prints its own help text without error.

- [ ] **Step 10: Run full CI check locally**

Run: `make ci`
Expected: streaming-check OK, lint clean, all tests PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/jobs/ cmd/imgsync/
git commit -m "feat(cli,jobs): add Enqueue with idempotent semantics, wire CLI subcommands"
```

---

## Week 1 Exit Criteria

After Task 8 commits cleanly, the repo state is:

- `go build ./...` succeeds.
- `make ci` is green: streaming guard, golangci-lint, all unit + integration tests.
- `bin/imgsync migrate` applies `0001_initial.up.sql` against a fresh PostgreSQL.
- `bin/imgsync enqueue` inserts a `transfer_jobs` row idempotently and emits one `enqueue` event.
- LocalFS Source streams a file. LocalFS Transport writes via atomic rename.
- The streaming contract is enforced by both interfaces and CI.

This is the foundation Week 2 builds on. Week 2 adds the FTP pool, FTPSource/FTPTransport, the worker (dispatch + processing + sweeper + retry + idle backoff + FTP host cap), the `worker` and health-server subcommands, and EVAL invariants C0/C1/C2/C3/C6.
