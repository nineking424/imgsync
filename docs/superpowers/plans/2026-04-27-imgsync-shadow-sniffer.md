# imgsync Shadow Sniffer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a polling sniffer subcommand to imgsync that periodically reads a source PostgreSQL DB, deterministically generates trace_ids, and idempotently enqueues file-transfer jobs into `transfer_jobs` — independent of NiFi.

**Architecture:** New `imgsync sniffer` subcommand on the existing single binary, runs as a separate Helm-deployed pod (replicas=1), maintains watermark in a single-row `sniffer_state` table with `(updated_at, pk)` tie-break. Reconcile is imgsync-self-audit only — no NiFi observation.

**Tech Stack:** Go (existing imgsync v1), pgx (PostgreSQL driver), testcontainers-go (integration), kind (E2E), Helm. Uses two pgx pools at runtime (source DB + imgsync DB).

**Spec:** `docs/superpowers/specs/2026-04-27-imgsync-shadow-sniffer-design.md`

**Pre-condition:** imgsync v1 base must be implemented first — `transfer_jobs` / `transfer_events` schema, pgx pool wiring, `cmd/imgsync` entrypoint, Helm chart, migration runner. This plan covers only the sniffer addition.

---

## File Structure

**New files:**
- `migrations/0002_sniffer_state.up.sql` — sniffer_state table
- `migrations/0002_sniffer_state.down.sql` — rollback
- `internal/sniffer/state.go` — sniffer_state repository (read/upsert)
- `internal/sniffer/state_test.go` — unit tests
- `internal/sniffer/traceid.go` — `${source_table}-${pk}` generator + dst path mapping
- `internal/sniffer/traceid_test.go` — unit tests
- `internal/sniffer/query.go` — source DB query builder (window + tie-break SQL)
- `internal/sniffer/query_test.go` — unit tests
- `internal/sniffer/enqueue.go` — idempotent insert into transfer_jobs
- `internal/sniffer/sniffer.go` — main poll loop (compose state + query + enqueue)
- `internal/sniffer/sniffer_integration_test.go` — testcontainers tests S0~S3
- `internal/cli/sniffer.go` — `imgsync sniffer` subcommand handler
- `internal/sourcedb/pool.go` — separate pgx pool for source DB (read-only credential)
- `deploy/helm/templates/sniffer-deployment.yaml` — Helm sniffer pod
- `deploy/helm/templates/sniffer-configmap.yaml` — sniffer config
- `test/e2e/sniffer_test.go` — kind cluster E2E (S-E1 = C5')

**Modified files:**
- `cmd/imgsync/main.go` — register `sniffer` subcommand
- `deploy/helm/values.yaml` — add `sniffer:` section (replicas, source DB DSN secret ref, polling cron)
- `Makefile` — add `test-integration-sniffer`, `test-e2e-sniffer` targets

**Documentation updates (Task 13):**
- `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` — rev 5 (Sniffer section, schema, SC#1 replacement, The Assignment Week 4-5)
- `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md` — add sniffer rows + C5'/C8~C11

---

## Task 0: Pre-flight check (verify v1 base)

This plan assumes the imgsync v1 base is implemented. Verify before starting.

**Files:** none (read-only checks)

- [ ] **Step 1: Verify migration runner exists**

```bash
ls migrations/0001_*.up.sql
```

Expected: file exists with transfer_jobs + transfer_events schema. If not: STOP, implement v1 base first per `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` "The Assignment" Week 1-3.

- [ ] **Step 2: Verify imgsync DB pool wiring**

```bash
grep -r "pgxpool" internal/db/ cmd/imgsync/
```

Expected: pgxpool.Pool initialized somewhere reachable from subcommands. If not: STOP, v1 base incomplete.

- [ ] **Step 3: Verify subcommand router**

```bash
grep -A5 "func main" cmd/imgsync/main.go
```

Expected: subcommand dispatch (e.g. `os.Args[1]` switch on `enqueue`/`worker`). If only one subcommand exists: extend the switch in Task 8.

- [ ] **Step 4: Verify Helm chart skeleton**

```bash
ls deploy/helm/templates/
```

Expected: at minimum `worker-deployment.yaml`, `migrations-job.yaml`. If absent: STOP.

- [ ] **Step 5: Verify test runners**

```bash
grep "test-integration\|test-e2e" Makefile
```

Expected: existing targets we extend in Task 9 and Task 12.

If all 5 checks pass, proceed to Task 1. If any fail, document the gap and pause this plan until v1 base catches up.

---

## Task 1: Migration — sniffer_state table

**Files:**
- Create: `migrations/0002_sniffer_state.up.sql`
- Create: `migrations/0002_sniffer_state.down.sql`
- Test: `migrations/migrate_test.go` (modify if exists; create if not)

- [ ] **Step 1: Write the failing migration test**

Create or extend `migrations/migrate_test.go`:

```go
package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestSnifferStateMigration(t *testing.T) {
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine")
	if err != nil {
		t.Fatal(err)
	}
	defer pgC.Terminate(ctx)

	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// Apply 0001 then 0002. ApplyAll is the v1 helper; substitute the actual name.
	if err := ApplyAll(ctx, conn, "./"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var name string
	err = conn.QueryRow(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_name='sniffer_state'`).Scan(&name)
	if err != nil {
		t.Fatalf("sniffer_state table not found: %v", err)
	}
	if name != "sniffer_state" {
		t.Fatalf("got %q", name)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./migrations/ -run TestSnifferStateMigration -v
```

Expected: FAIL — migration file 0002 does not exist.

- [ ] **Step 3: Write migrations/0002_sniffer_state.up.sql**

```sql
CREATE TABLE sniffer_state (
  source_id   TEXT PRIMARY KEY,
  last_run_ts TIMESTAMPTZ NOT NULL,
  last_run_pk TEXT,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE sniffer_state IS
  'Watermark + tie-break key per polled source. One row per source_id. v1 single sniffer pod, no advisory lock.';
```

- [ ] **Step 4: Write migrations/0002_sniffer_state.down.sql**

```sql
DROP TABLE IF EXISTS sniffer_state;
```

- [ ] **Step 5: Run test to verify it passes**

```
go test ./migrations/ -run TestSnifferStateMigration -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add migrations/0002_sniffer_state.up.sql migrations/0002_sniffer_state.down.sql migrations/migrate_test.go
git commit -m "feat(sniffer): add sniffer_state table migration"
```

---

## Task 2: Source DB pool wiring

The sniffer reads a separate database (the NiFi-shared source). It needs its own pgx pool, distinct from the imgsync DB pool, with read-only credentials.

**Files:**
- Create: `internal/sourcedb/pool.go`
- Create: `internal/sourcedb/pool_test.go`

- [ ] **Step 1: Write the failing test**

`internal/sourcedb/pool_test.go`:

```go
package sourcedb_test

import (
	"context"
	"testing"

	"github.com/nineking424/imgsync/internal/sourcedb"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestNewPool_Connects(t *testing.T) {
	ctx := context.Background()
	pgC, _ := postgres.Run(ctx, "postgres:16-alpine")
	defer pgC.Terminate(ctx)
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")

	pool, err := sourcedb.NewPool(ctx, sourcedb.Config{
		DSN:            dsn,
		MaxConns:       4,
		QueryTimeoutMs: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	if one != 1 {
		t.Fatalf("got %d", one)
	}
}

func TestNewPool_BadDSN(t *testing.T) {
	_, err := sourcedb.NewPool(context.Background(), sourcedb.Config{
		DSN: "postgres://nope:nope@127.0.0.1:1/none",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sourcedb/ -v
```

Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement minimal pool**

`internal/sourcedb/pool.go`:

```go
package sourcedb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DSN            string
	MaxConns       int32
	QueryTimeoutMs int
}

type Pool struct {
	*pgxpool.Pool
	QueryTimeout time.Duration
}

func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 4
	}
	if cfg.QueryTimeoutMs == 0 {
		cfg.QueryTimeoutMs = 30000
	}

	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{
		Pool:         pool,
		QueryTimeout: time.Duration(cfg.QueryTimeoutMs) * time.Millisecond,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sourcedb/ -v
```

Expected: both tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sourcedb/
git commit -m "feat(sniffer): add source DB pgx pool with read-only credential support"
```

---

## Task 3: sniffer_state repository

Read/upsert the single watermark row per source_id.

**Files:**
- Create: `internal/sniffer/state.go`
- Create: `internal/sniffer/state_test.go`

- [ ] **Step 1: Write the failing test**

`internal/sniffer/state_test.go`:

```go
package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupImgsyncDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE sniffer_state (
		  source_id   TEXT PRIMARY KEY,
		  last_run_ts TIMESTAMPTZ NOT NULL,
		  last_run_pk TEXT,
		  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestStateRepo_LoadMissingReturnsZeroValue(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)

	st, err := repo.Load(context.Background(), "main-source-db.images")
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastRunTS.IsZero() {
		t.Fatalf("expected zero ts, got %v", st.LastRunTS)
	}
	if st.LastRunPK != "" {
		t.Fatalf("expected empty pk, got %q", st.LastRunPK)
	}
}

func TestStateRepo_UpsertThenLoad(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)
	ctx := context.Background()

	want := sniffer.State{
		SourceID:  "main-source-db.images",
		LastRunTS: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
		LastRunPK: "100",
	}
	if err := repo.Upsert(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Load(ctx, want.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastRunTS.Equal(want.LastRunTS) {
		t.Fatalf("ts: got %v want %v", got.LastRunTS, want.LastRunTS)
	}
	if got.LastRunPK != want.LastRunPK {
		t.Fatalf("pk: got %q want %q", got.LastRunPK, want.LastRunPK)
	}
}

func TestStateRepo_UpsertOverwritesExisting(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)
	ctx := context.Background()

	first := sniffer.State{
		SourceID: "src", LastRunTS: time.Unix(1000, 0).UTC(), LastRunPK: "1",
	}
	second := sniffer.State{
		SourceID: "src", LastRunTS: time.Unix(2000, 0).UTC(), LastRunPK: "2",
	}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Load(ctx, "src")
	if got.LastRunPK != "2" {
		t.Fatalf("got %q", got.LastRunPK)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sniffer/ -v
```

Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement state.go**

`internal/sniffer/state.go`:

```go
package sniffer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type State struct {
	SourceID  string
	LastRunTS time.Time
	LastRunPK string
}

type StateRepo struct {
	pool *pgxpool.Pool
}

func NewStateRepo(pool *pgxpool.Pool) *StateRepo {
	return &StateRepo{pool: pool}
}

// Load returns the watermark for source_id. If no row exists, returns
// State{SourceID: id} with zero LastRunTS — caller treats zero as "first run".
func (r *StateRepo) Load(ctx context.Context, sourceID string) (State, error) {
	var st State
	st.SourceID = sourceID
	err := r.pool.QueryRow(ctx, `
		SELECT last_run_ts, COALESCE(last_run_pk, '')
		  FROM sniffer_state
		 WHERE source_id = $1`, sourceID).Scan(&st.LastRunTS, &st.LastRunPK)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("load %q: %w", sourceID, err)
	}
	return st, nil
}

// Upsert writes the new watermark. last_run_pk is stored as nullable empty -> NULL.
func (r *StateRepo) Upsert(ctx context.Context, s State) error {
	var pk any = s.LastRunPK
	if s.LastRunPK == "" {
		pk = nil
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO sniffer_state (source_id, last_run_ts, last_run_pk, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (source_id) DO UPDATE
		   SET last_run_ts = EXCLUDED.last_run_ts,
		       last_run_pk = EXCLUDED.last_run_pk,
		       updated_at  = NOW()`,
		s.SourceID, s.LastRunTS, pk)
	if err != nil {
		return fmt.Errorf("upsert %q: %w", s.SourceID, err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sniffer/ -run TestStateRepo -v
```

Expected: 3 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/state.go internal/sniffer/state_test.go
git commit -m "feat(sniffer): add sniffer_state repository (Load/Upsert)"
```

---

## Task 4: trace_id + dst path generator

Deterministic generation per spec Section 2. trace_id = `${source_table}-${pk}`. dst path mirrors NiFi's 1:1 row mapping.

**Files:**
- Create: `internal/sniffer/traceid.go`
- Create: `internal/sniffer/traceid_test.go`

- [ ] **Step 1: Write the failing test**

`internal/sniffer/traceid_test.go`:

```go
package sniffer_test

import (
	"testing"

	"github.com/nineking424/imgsync/internal/sniffer"
)

func TestTraceID(t *testing.T) {
	tests := []struct {
		table string
		pk    string
		want  string
	}{
		{"images", "12345", "images-12345"},
		{"documents", "uuid-abc-def", "documents-uuid-abc-def"},
		{"main-db.images", "1", "main-db.images-1"},
	}
	for _, tt := range tests {
		got := sniffer.TraceID(tt.table, tt.pk)
		if got != tt.want {
			t.Errorf("TraceID(%q,%q)=%q want %q", tt.table, tt.pk, got, tt.want)
		}
	}
}

func TestDstPath_ShadowSuffixApplied(t *testing.T) {
	tmpl := sniffer.DstTemplate{
		Pattern: "/incoming/{{.FilePath}}",
		Shadow:  true,
	}
	got, err := tmpl.Render(map[string]string{"FilePath": "2026/04/img.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	want := "/incoming/2026/04/img.jpg.imgsync_shadow_v1"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDstPath_ShadowOff(t *testing.T) {
	tmpl := sniffer.DstTemplate{
		Pattern: "/incoming/{{.FilePath}}",
		Shadow:  false,
	}
	got, _ := tmpl.Render(map[string]string{"FilePath": "a/b.jpg"})
	if got != "/incoming/a/b.jpg" {
		t.Errorf("got %q", got)
	}
}

func TestDstPath_MissingKey(t *testing.T) {
	tmpl := sniffer.DstTemplate{Pattern: "/x/{{.Missing}}"}
	_, err := tmpl.Render(map[string]string{})
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sniffer/ -run "TestTraceID|TestDstPath" -v
```

Expected: FAIL.

- [ ] **Step 3: Implement traceid.go**

`internal/sniffer/traceid.go`:

```go
package sniffer

import (
	"bytes"
	"fmt"
	"text/template"
)

// TraceID composes the deterministic identifier per spec Section 2:
//   trace_id = "<source_table>-<pk>"
// Same source row always yields same trace_id; idempotency relies on this.
func TraceID(sourceTable, pk string) string {
	return sourceTable + "-" + pk
}

// DstTemplate renders the destination path for a source row. The exact
// pattern mirrors NiFi's 1:1 mapping (verified Week 4 — see spec OQ1).
// Shadow=true appends ".imgsync_shadow_v1" to avoid colliding with NiFi
// production output (operational safety, NOT for cross-system reconcile).
type DstTemplate struct {
	Pattern string // text/template body, fields = source row columns
	Shadow  bool
}

const ShadowSuffix = ".imgsync_shadow_v1"

func (t DstTemplate) Render(fields map[string]string) (string, error) {
	tmpl, err := template.New("dst").Option("missingkey=error").Parse(t.Pattern)
	if err != nil {
		return "", fmt.Errorf("parse pattern: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, fields); err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	out := buf.String()
	if t.Shadow {
		out += ShadowSuffix
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sniffer/ -run "TestTraceID|TestDstPath" -v
```

Expected: 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/traceid.go internal/sniffer/traceid_test.go
git commit -m "feat(sniffer): add deterministic trace_id and shadow-aware dst path generator"
```

---

## Task 5: Source DB query builder (window + tie-break)

Builds the SQL per spec Section 3 with `(updated_at, pk::TEXT) > (last_run_ts, last_run_pk)` predicate, bias subtraction, ORDER BY for batch advancing.

**Files:**
- Create: `internal/sniffer/query.go`
- Create: `internal/sniffer/query_test.go`

- [ ] **Step 1: Write the failing test**

`internal/sniffer/query_test.go`:

```go
package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupSourceDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	pool, _ := pgxpool.New(ctx, dsn)
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE images (
		  id BIGINT PRIMARY KEY,
		  updated_at TIMESTAMPTZ NOT NULL,
		  file_path TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestQuery_WindowAdvancesAcrossSameTS(t *testing.T) {
	ctx := context.Background()
	pool := setupSourceDB(t)
	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		_, _ = pool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, ts, "p")
	}

	q := sniffer.Query{
		Table:        "images",
		PKColumn:     "id",
		TSColumn:     "updated_at",
		BatchSize:    2,
		BiasDuration: 0,
	}
	from := sniffer.State{LastRunTS: ts.Add(-time.Hour), LastRunPK: ""}

	rows, err := q.Fetch(ctx, pool, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("first batch len=%d", len(rows))
	}
	if rows[0].PK != "1" || rows[1].PK != "2" {
		t.Fatalf("first batch pks: %v", rows)
	}

	from = sniffer.State{LastRunTS: rows[1].TS, LastRunPK: rows[1].PK}
	rows2, _ := q.Fetch(ctx, pool, from)
	if len(rows2) != 2 || rows2[0].PK != "3" || rows2[1].PK != "4" {
		t.Fatalf("second batch: %v", rows2)
	}

	from = sniffer.State{LastRunTS: rows2[1].TS, LastRunPK: rows2[1].PK}
	rows3, _ := q.Fetch(ctx, pool, from)
	if len(rows3) != 1 || rows3[0].PK != "5" {
		t.Fatalf("third batch: %v", rows3)
	}
}

func TestQuery_BiasExcludesRecentRows(t *testing.T) {
	ctx := context.Background()
	pool := setupSourceDB(t)
	now := time.Now().UTC()
	_, _ = pool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p')`,
		now.Add(-2*time.Second))

	q := sniffer.Query{
		Table: "images", PKColumn: "id", TSColumn: "updated_at",
		BatchSize: 10, BiasDuration: 5 * time.Second,
	}
	rows, _ := q.Fetch(ctx, pool, sniffer.State{LastRunTS: time.Unix(0, 0).UTC()})
	if len(rows) != 0 {
		t.Fatalf("bias should exclude row newer than NOW()-bias, got %d", len(rows))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sniffer/ -run TestQuery -v
```

Expected: FAIL.

- [ ] **Step 3: Implement query.go**

`internal/sniffer/query.go`:

```go
package sniffer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Row struct {
	PK     string            // serialized as TEXT regardless of source pk type
	TS     time.Time         // updated_at
	Fields map[string]string // additional columns for dst-path templating
}

type Query struct {
	Table        string        // source table, e.g. "images"
	PKColumn     string        // primary key column, e.g. "id"
	TSColumn     string        // watermark column, e.g. "updated_at"
	ExtraColumns []string      // additional columns to SELECT for dst rendering
	BatchSize    int           // LIMIT
	BiasDuration time.Duration // exclude rows newer than NOW()-bias
}

// Fetch runs the windowed query against the source DB. The predicate uses
// (TSColumn, PKColumn::TEXT) > (last_run_ts, last_run_pk) so that batches of
// rows sharing the same TS are split correctly across calls.
func (q Query) Fetch(ctx context.Context, pool *pgxpool.Pool, from State) ([]Row, error) {
	if q.BatchSize <= 0 {
		return nil, fmt.Errorf("batch_size must be > 0")
	}
	cols := append([]string{q.PKColumn, q.TSColumn}, q.ExtraColumns...)
	colList := ""
	for i, c := range cols {
		if i > 0 {
			colList += ", "
		}
		colList += c
	}
	biasSec := int(q.BiasDuration.Seconds())
	pk := from.LastRunPK
	sql := fmt.Sprintf(`
		SELECT %s FROM %s
		WHERE (%s, %s::TEXT) > ($1, $2)
		  AND %s <= NOW() - ($3::INT || ' seconds')::INTERVAL
		ORDER BY %s, %s
		LIMIT %d`,
		colList, q.Table,
		q.TSColumn, q.PKColumn,
		q.TSColumn,
		q.TSColumn, q.PKColumn,
		q.BatchSize)

	rows, err := pool.Query(ctx, sql, from.LastRunTS, pk, biasSec)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := Row{Fields: map[string]string{}}
		for i, c := range cols {
			r.Fields[c] = fmt.Sprintf("%v", vals[i])
		}
		r.PK = r.Fields[q.PKColumn]
		switch v := vals[1].(type) {
		case time.Time:
			r.TS = v
		default:
			return nil, fmt.Errorf("unexpected ts type %T", vals[1])
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sniffer/ -run TestQuery -v
```

Expected: 2 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/query.go internal/sniffer/query_test.go
git commit -m "feat(sniffer): add source DB query builder with window + tie-break"
```

---

## Task 6: Idempotent enqueue

Insert a `transfer_jobs` row from a source `Row` with `ON CONFLICT (trace_id, dst) DO NOTHING`, returning whether the row was actually inserted.

**Files:**
- Create: `internal/sniffer/enqueue.go`
- Create: `internal/sniffer/enqueue_test.go`

This task assumes the v1 `transfer_jobs` schema includes columns `(id, trace_id, src, dst, status, attempts, max_attempts, created_at, updated_at)` and a `UNIQUE(trace_id, dst)` constraint. If column names differ in v1, adapt the INSERT below to match.

- [ ] **Step 1: Write the failing test**

`internal/sniffer/enqueue_test.go`:

```go
package sniffer_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupTransferJobs(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, _ := postgres.Run(ctx, "postgres:16-alpine")
	t.Cleanup(func() { pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	pool, _ := pgxpool.New(ctx, dsn)
	t.Cleanup(pool.Close)
	// Minimal schema matching v1 contract.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE transfer_jobs (
		  id BIGSERIAL PRIMARY KEY,
		  trace_id TEXT NOT NULL,
		  src TEXT NOT NULL,
		  dst TEXT NOT NULL,
		  status TEXT NOT NULL DEFAULT 'pending',
		  attempts INT NOT NULL DEFAULT 0,
		  max_attempts INT NOT NULL DEFAULT 5,
		  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		  UNIQUE (trace_id, dst)
		)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestEnqueue_InsertsNewRow(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)

	inserted, err := enq.Enqueue(context.Background(), sniffer.JobSpec{
		TraceID: "images-1",
		Src:     "src://images/1",
		Dst:     "/incoming/a.jpg.imgsync_shadow_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("expected inserted=true")
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='images-1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("got %d", n)
	}
}

func TestEnqueue_SecondCallIsNoop(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)
	spec := sniffer.JobSpec{TraceID: "images-1", Src: "s", Dst: "d"}

	_, _ = enq.Enqueue(context.Background(), spec)
	inserted, err := enq.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("expected inserted=false on duplicate")
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='images-1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 row, got %d", n)
	}
}

func TestEnqueue_DifferentDstSameTraceIDInsertsBoth(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)
	ctx := context.Background()
	_, _ = enq.Enqueue(ctx, sniffer.JobSpec{TraceID: "x-1", Src: "s", Dst: "/a"})
	_, _ = enq.Enqueue(ctx, sniffer.JobSpec{TraceID: "x-1", Src: "s", Dst: "/b"})

	var n int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='x-1'`).Scan(&n)
	if n != 2 {
		t.Fatalf("got %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sniffer/ -run TestEnqueue -v
```

Expected: FAIL.

- [ ] **Step 3: Implement enqueue.go**

`internal/sniffer/enqueue.go`:

```go
package sniffer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type JobSpec struct {
	TraceID string
	Src     string
	Dst     string
}

type Enqueuer struct {
	pool *pgxpool.Pool
}

func NewEnqueuer(pool *pgxpool.Pool) *Enqueuer {
	return &Enqueuer{pool: pool}
}

// Enqueue inserts one transfer_jobs row if (trace_id, dst) is novel.
// Returns inserted=true when a new row was created, false on UNIQUE conflict.
func (e *Enqueuer) Enqueue(ctx context.Context, j JobSpec) (bool, error) {
	tag, err := e.pool.Exec(ctx, `
		INSERT INTO transfer_jobs (trace_id, src, dst)
		VALUES ($1, $2, $3)
		ON CONFLICT (trace_id, dst) DO NOTHING`,
		j.TraceID, j.Src, j.Dst)
	if err != nil {
		return false, fmt.Errorf("enqueue %s->%s: %w", j.TraceID, j.Dst, err)
	}
	return tag.RowsAffected() == 1, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sniffer/ -run TestEnqueue -v
```

Expected: 3 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/enqueue.go internal/sniffer/enqueue_test.go
git commit -m "feat(sniffer): add idempotent enqueuer with ON CONFLICT DO NOTHING"
```

---

## Task 7: Sniffer main loop

Compose state + query + traceid + enqueue into a single `Run` call. One iteration: load state → fetch batch → for each row enqueue → if any rows, upsert state to last row's `(ts, pk)`.

**Files:**
- Create: `internal/sniffer/sniffer.go`
- Create: `internal/sniffer/sniffer_test.go` (test for `RunOnce`)

- [ ] **Step 1: Write the failing test**

Append to `internal/sniffer/sniffer_test.go`:

```go
package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
)

func TestRunOnce_EnqueuesAllAndAdvancesWatermark(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		_, _ = srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, ts.Add(time.Duration(i)*time.Second), "row.jpg")
	}

	s := sniffer.New(sniffer.Config{
		SourceID:     "main.images",
		Query:        sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:          sniffer.DstTemplate{Pattern: "/in/{{.file_path}}", Shadow: true},
		SrcPattern:   "src://images/{{.id}}",
		ImgsyncPool:  imgPool,
		SourcePool:   srcPool,
	})

	n, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("enqueued=%d", n)
	}

	var jobs int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&jobs)
	if jobs != 3 {
		t.Fatalf("transfer_jobs=%d", jobs)
	}

	// Watermark advanced to last row.
	st, _ := sniffer.NewStateRepo(imgPool).Load(ctx, "main.images")
	if st.LastRunPK != "3" {
		t.Fatalf("last_run_pk=%q", st.LastRunPK)
	}
}

func TestRunOnce_NoRowsLeavesWatermarkUnchanged(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://images/{{.id}}",
		ImgsyncPool: imgPool, SourcePool: srcPool,
	})

	n, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
	st, _ := sniffer.NewStateRepo(imgPool).Load(ctx, "main.images")
	if !st.LastRunTS.IsZero() {
		t.Fatalf("watermark should remain zero, got %v", st.LastRunTS)
	}
}
```

Add helper `setupImgsyncDBWithTransferJobs` to `state_test.go` (extends `setupImgsyncDB` with the transfer_jobs schema from Task 6's helper). Concretely: combine the two CREATE TABLE blocks into one helper.

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/sniffer/ -run TestRunOnce -v
```

Expected: FAIL.

- [ ] **Step 3: Implement sniffer.go**

`internal/sniffer/sniffer.go`:

```go
package sniffer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	SourceID    string
	Query       Query
	Dst         DstTemplate
	SrcPattern  string // text/template for src URL, fields = source row columns
	ImgsyncPool *pgxpool.Pool
	SourcePool  *pgxpool.Pool
}

type Sniffer struct {
	cfg   Config
	state *StateRepo
	enq   *Enqueuer
	src   DstTemplate // reuse template renderer for src pattern
}

func New(cfg Config) *Sniffer {
	return &Sniffer{
		cfg:   cfg,
		state: NewStateRepo(cfg.ImgsyncPool),
		enq:   NewEnqueuer(cfg.ImgsyncPool),
		src:   DstTemplate{Pattern: cfg.SrcPattern},
	}
}

// RunOnce executes a single poll iteration. Returns the count of rows
// successfully inserted (excludes UNIQUE conflicts). Watermark is advanced
// only after enqueue completes for every row in the batch.
func (s *Sniffer) RunOnce(ctx context.Context) (int, error) {
	st, err := s.state.Load(ctx, s.cfg.SourceID)
	if err != nil {
		return 0, fmt.Errorf("load state: %w", err)
	}

	rows, err := s.cfg.Query.Fetch(ctx, s.cfg.SourcePool, st)
	if err != nil {
		return 0, fmt.Errorf("fetch: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	inserted := 0
	for _, r := range rows {
		dst, err := s.cfg.Dst.Render(r.Fields)
		if err != nil {
			return inserted, fmt.Errorf("render dst pk=%s: %w", r.PK, err)
		}
		src, err := s.src.Render(r.Fields)
		if err != nil {
			return inserted, fmt.Errorf("render src pk=%s: %w", r.PK, err)
		}
		ok, err := s.enq.Enqueue(ctx, JobSpec{
			TraceID: TraceID(s.cfg.Query.Table, r.PK),
			Src:     src,
			Dst:     dst,
		})
		if err != nil {
			return inserted, fmt.Errorf("enqueue pk=%s: %w", r.PK, err)
		}
		if ok {
			inserted++
		}
	}

	last := rows[len(rows)-1]
	if err := s.state.Upsert(ctx, State{
		SourceID:  s.cfg.SourceID,
		LastRunTS: last.TS,
		LastRunPK: last.PK,
	}); err != nil {
		return inserted, fmt.Errorf("upsert state: %w", err)
	}
	return inserted, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/sniffer/ -run TestRunOnce -v
```

Expected: 2 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/sniffer.go internal/sniffer/sniffer_test.go internal/sniffer/state_test.go
git commit -m "feat(sniffer): add main poll loop composing state+query+enqueue"
```

---

## Task 8: Sniffer subcommand wiring

Register `imgsync sniffer` on the existing CLI router. Reads config from env, runs the loop on a cron, exits cleanly on SIGTERM.

**Files:**
- Create: `internal/cli/sniffer.go`
- Modify: `cmd/imgsync/main.go`

- [ ] **Step 1: Write the failing test (config parsing only — full loop covered in Task 9-10 integration)**

Create `internal/cli/sniffer_test.go`:

```go
package cli_test

import (
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/cli"
)

func TestParseConfig_DefaultsApplied(t *testing.T) {
	t.Setenv("SNIFFER_SOURCE_ID", "main.images")
	t.Setenv("SNIFFER_SOURCE_DSN", "postgres://x/x")
	t.Setenv("SNIFFER_IMGSYNC_DSN", "postgres://y/y")
	t.Setenv("SNIFFER_TABLE", "images")
	t.Setenv("SNIFFER_PK_COLUMN", "id")
	t.Setenv("SNIFFER_TS_COLUMN", "updated_at")
	t.Setenv("SNIFFER_DST_PATTERN", "/in/{{.file_path}}")
	t.Setenv("SNIFFER_SRC_PATTERN", "src://images/{{.id}}")

	cfg, err := cli.ParseSnifferConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IntervalSec != 60 {
		t.Errorf("default interval=%d", cfg.IntervalSec)
	}
	if cfg.BatchSize != 500 {
		t.Errorf("default batch=%d", cfg.BatchSize)
	}
	if cfg.BiasDuration != 5*time.Second {
		t.Errorf("default bias=%v", cfg.BiasDuration)
	}
	if cfg.Shadow != true {
		t.Errorf("default shadow should be true")
	}
}

func TestParseConfig_RequiredMissing(t *testing.T) {
	t.Setenv("SNIFFER_SOURCE_ID", "")
	_, err := cli.ParseSnifferConfig()
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/cli/ -run TestParseConfig -v
```

Expected: FAIL.

- [ ] **Step 3: Implement internal/cli/sniffer.go**

```go
package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/nineking424/imgsync/internal/sourcedb"
)

type SnifferConfig struct {
	SourceID     string
	SourceDSN    string
	ImgsyncDSN   string
	Table        string
	PKColumn     string
	TSColumn     string
	ExtraColumns []string
	DstPattern   string
	SrcPattern   string
	Shadow       bool
	BatchSize    int
	BiasDuration time.Duration
	IntervalSec  int
}

func ParseSnifferConfig() (SnifferConfig, error) {
	c := SnifferConfig{
		Shadow:       envBool("SNIFFER_SHADOW", true),
		BatchSize:    envInt("SNIFFER_BATCH_SIZE", 500),
		BiasDuration: time.Duration(envInt("SNIFFER_BIAS_SEC", 5)) * time.Second,
		IntervalSec:  envInt("SNIFFER_INTERVAL_SEC", 60),
		SourceID:     os.Getenv("SNIFFER_SOURCE_ID"),
		SourceDSN:    os.Getenv("SNIFFER_SOURCE_DSN"),
		ImgsyncDSN:   os.Getenv("SNIFFER_IMGSYNC_DSN"),
		Table:        os.Getenv("SNIFFER_TABLE"),
		PKColumn:     os.Getenv("SNIFFER_PK_COLUMN"),
		TSColumn:     os.Getenv("SNIFFER_TS_COLUMN"),
		DstPattern:   os.Getenv("SNIFFER_DST_PATTERN"),
		SrcPattern:   os.Getenv("SNIFFER_SRC_PATTERN"),
	}
	if extra := os.Getenv("SNIFFER_EXTRA_COLUMNS"); extra != "" {
		c.ExtraColumns = strings.Split(extra, ",")
	}
	required := map[string]string{
		"SNIFFER_SOURCE_ID":   c.SourceID,
		"SNIFFER_SOURCE_DSN":  c.SourceDSN,
		"SNIFFER_IMGSYNC_DSN": c.ImgsyncDSN,
		"SNIFFER_TABLE":       c.Table,
		"SNIFFER_PK_COLUMN":   c.PKColumn,
		"SNIFFER_TS_COLUMN":   c.TSColumn,
		"SNIFFER_DST_PATTERN": c.DstPattern,
		"SNIFFER_SRC_PATTERN": c.SrcPattern,
	}
	for k, v := range required {
		if v == "" {
			return c, fmt.Errorf("required env %s missing", k)
		}
	}
	return c, nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return def
}

func RunSniffer(ctx context.Context, cfg SnifferConfig) error {
	srcPool, err := sourcedb.NewPool(ctx, sourcedb.Config{
		DSN:            cfg.SourceDSN,
		QueryTimeoutMs: 30000,
	})
	if err != nil {
		return fmt.Errorf("source pool: %w", err)
	}
	defer srcPool.Close()

	imgPool, err := pgxpool.New(ctx, cfg.ImgsyncDSN)
	if err != nil {
		return fmt.Errorf("imgsync pool: %w", err)
	}
	defer imgPool.Close()

	s := sniffer.New(sniffer.Config{
		SourceID: cfg.SourceID,
		Query: sniffer.Query{
			Table:        cfg.Table,
			PKColumn:     cfg.PKColumn,
			TSColumn:     cfg.TSColumn,
			ExtraColumns: cfg.ExtraColumns,
			BatchSize:    cfg.BatchSize,
			BiasDuration: cfg.BiasDuration,
		},
		Dst:         sniffer.DstTemplate{Pattern: cfg.DstPattern, Shadow: cfg.Shadow},
		SrcPattern:  cfg.SrcPattern,
		ImgsyncPool: imgPool,
		SourcePool:  srcPool.Pool,
	})

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	tick := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	defer tick.Stop()

	// First run immediately so failures surface fast.
	if n, err := s.RunOnce(ctx); err != nil {
		log.Printf("sniffer run error: %v", err)
	} else {
		log.Printf("sniffer enqueued %d new jobs", n)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			n, err := s.RunOnce(ctx)
			if err != nil {
				log.Printf("sniffer run error: %v", err)
				continue
			}
			log.Printf("sniffer enqueued %d new jobs", n)
		}
	}
}
```

- [ ] **Step 4: Run config parsing test**

```
go test ./internal/cli/ -run TestParseConfig -v
```

Expected: 2 tests PASS.

- [ ] **Step 5: Wire into cmd/imgsync/main.go**

Add a `sniffer` case to the existing subcommand switch. The exact patch depends on v1 main.go shape; the additive change is:

```go
case "sniffer":
    cfg, err := cli.ParseSnifferConfig()
    if err != nil {
        log.Fatal(err)
    }
    if err := cli.RunSniffer(context.Background(), cfg); err != nil {
        log.Fatal(err)
    }
```

If `cmd/imgsync/main.go` does not yet have a switch (only `enqueue` exists), introduce the switch and preserve existing behavior.

- [ ] **Step 6: Verify build**

```
go build ./...
```

Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/sniffer.go internal/cli/sniffer_test.go cmd/imgsync/main.go
git commit -m "feat(sniffer): add 'imgsync sniffer' subcommand with cron loop"
```

---

## Task 9: Integration test S0 (polling overlap) + S1 (crash recovery)

testcontainers-based integration tests. Two postgres containers — source + imgsync.

**Files:**
- Create: `internal/sniffer/integration_test.go` (build tag `//go:build integration`)
- Modify: `Makefile` — add `test-integration-sniffer` target

- [ ] **Step 1: Write the failing tests**

`internal/sniffer/integration_test.go`:

```go
//go:build integration

package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
)

// S0: polling overlap correctness
//   Run #1 sniffs window. 5 minutes later, run #2 sniffs an overlapping window.
//   Same source row must NOT produce a duplicate transfer_jobs row.
func TestS0_PollingOverlapNoDuplicate(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := srcPool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p.jpg')`, t0); err != nil {
		t.Fatal(err)
	}

	makeS := func(bias time.Duration) *sniffer.Sniffer {
		return sniffer.New(sniffer.Config{
			SourceID:    "src",
			Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 100, BiasDuration: bias},
			Dst:         sniffer.DstTemplate{Pattern: "/in/{{.file_path}}", Shadow: true},
			SrcPattern:  "src://images/{{.id}}",
			ImgsyncPool: imgPool, SourcePool: srcPool,
		})
	}

	if _, err := makeS(0).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// "5 minutes later" — re-run with overlap (we just call RunOnce again from the same state row, which already advanced).
	// Force overlap by resetting watermark backwards.
	_, _ = imgPool.Exec(ctx, `UPDATE sniffer_state SET last_run_ts = last_run_ts - INTERVAL '25 minutes', last_run_pk = NULL`)
	if _, err := makeS(0).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 row after overlapping sniff, got %d", n)
	}
}

// S1: crash recovery — kill -9 after 50/100 rows enqueued, restart, no loss.
// Simulate by running with batch_size=50 and wiping in-memory state between calls
// (the persistent watermark table is the only state).
func TestS1_CrashRecoveryNoLossNoDup(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 100; i++ {
		_, _ = srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, t0.Add(time.Duration(i)*time.Second), "f.jpg")
	}

	make := func() *sniffer.Sniffer {
		return sniffer.New(sniffer.Config{
			SourceID:    "src",
			Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 50},
			Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}", Shadow: true},
			SrcPattern:  "src://images/{{.id}}",
			ImgsyncPool: imgPool, SourcePool: srcPool,
		})
	}

	if _, err := make().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Drop in-memory state by constructing a fresh Sniffer; persistent watermark guides us.
	if _, err := make().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(DISTINCT trace_id) FROM transfer_jobs`).Scan(&n)
	if n != 100 {
		t.Fatalf("expected 100 distinct trace_ids, got %d", n)
	}
}

// helper deduplicated from earlier files; if already exported, remove this stub.
func setupImgsyncDBWithTransferJobs(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := setupImgsyncDB(t)
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE transfer_jobs (
		  id BIGSERIAL PRIMARY KEY,
		  trace_id TEXT NOT NULL,
		  src TEXT NOT NULL,
		  dst TEXT NOT NULL,
		  status TEXT NOT NULL DEFAULT 'pending',
		  attempts INT NOT NULL DEFAULT 0,
		  max_attempts INT NOT NULL DEFAULT 5,
		  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		  UNIQUE (trace_id, dst)
		)`); err != nil {
		t.Fatal(err)
	}
	return pool
}
```

If `setupImgsyncDBWithTransferJobs` already exists in another test file (Task 6 or Task 7), delete the duplicate from this file.

- [ ] **Step 2: Add Makefile target**

Add to `Makefile`:

```makefile
test-integration-sniffer:
	go test -tags integration -run "TestS[01]_" -v ./internal/sniffer/
```

- [ ] **Step 3: Run tests**

```
make test-integration-sniffer
```

Expected: both tests PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/sniffer/integration_test.go Makefile
git commit -m "test(sniffer): add S0 polling overlap + S1 crash recovery integration tests"
```

---

## Task 10: Integration test S2 (tie-break) + S3 (query timeout)

**Files:**
- Modify: `internal/sniffer/integration_test.go`

- [ ] **Step 1: Add S2 — tie-break correctness**

Append to `internal/sniffer/integration_test.go`:

```go
// S2: 10 rows with identical updated_at, batch_size=3 forces 4 batches.
// All 10 enqueued exactly once; sniffer_state.last_run_pk == "10".
func TestS2_TieBreakBatchCorrectness(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 10; i++ {
		_, _ = srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, 'p')`, i, ts)
	}

	s := sniffer.New(sniffer.Config{
		SourceID:    "src",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 3},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://{{.id}}",
		ImgsyncPool: imgPool, SourcePool: srcPool,
	})

	for i := 0; i < 5; i++ {
		if _, err := s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n)
	if n != 10 {
		t.Fatalf("expected 10 rows, got %d", n)
	}
	st, _ := sniffer.NewStateRepo(imgPool).Load(ctx, "src")
	if st.LastRunPK != "10" {
		t.Fatalf("last_run_pk=%q, want 10", st.LastRunPK)
	}
}
```

- [ ] **Step 2: Add S3 — query timeout isolation**

Append:

```go
// S3: source DB query takes longer than the per-query timeout.
// Sniffer's RunOnce returns an error (timeout) and watermark stays unchanged.
func TestS3_QueryTimeoutLeavesWatermarkUnchanged(t *testing.T) {
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	// Insert one row so a successful run would advance the watermark.
	ts := time.Now().UTC().Add(-time.Hour)
	_, _ = srcPool.Exec(context.Background(), `INSERT INTO images VALUES (1, $1, 'p')`, ts)

	// Build a tiny context with a 100ms deadline; the query path will exceed it
	// because the planner+round-trip on a fresh container exceeds 100ms.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Slow the source DB by injecting a pg_sleep via lock_timeout setting.
	// Simpler: wrap the table in a view that calls pg_sleep.
	_, _ = srcPool.Exec(context.Background(), `
		CREATE OR REPLACE VIEW images_slow AS
		SELECT id, updated_at, file_path FROM images
		WHERE pg_sleep(2) IS NOT NULL OR TRUE`)

	s := sniffer.New(sniffer.Config{
		SourceID:    "src",
		Query:       sniffer.Query{Table: "images_slow", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://{{.id}}",
		ImgsyncPool: imgPool, SourcePool: srcPool,
	})

	if _, err := s.RunOnce(ctx); err == nil {
		t.Fatal("expected timeout error")
	}

	st, _ := sniffer.NewStateRepo(imgPool).Load(context.Background(), "src")
	if !st.LastRunTS.IsZero() {
		t.Fatalf("watermark advanced despite timeout: %v", st.LastRunTS)
	}
}
```

- [ ] **Step 3: Update Makefile target to include S2/S3**

```makefile
test-integration-sniffer:
	go test -tags integration -run "TestS[0-3]_" -v ./internal/sniffer/
```

- [ ] **Step 4: Run all sniffer integration tests**

```
make test-integration-sniffer
```

Expected: 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sniffer/integration_test.go Makefile
git commit -m "test(sniffer): add S2 tie-break + S3 query timeout integration tests"
```

---

## Task 11: Helm sniffer deployment

Add a Helm `Deployment` for the sniffer pod (replicas=1) plus its ConfigMap. Reuses the existing imgsync image; differs only in the entrypoint command.

**Files:**
- Create: `deploy/helm/templates/sniffer-deployment.yaml`
- Create: `deploy/helm/templates/sniffer-configmap.yaml`
- Modify: `deploy/helm/values.yaml`

- [ ] **Step 1: values.yaml — add sniffer section**

Append to `deploy/helm/values.yaml`:

```yaml
sniffer:
  enabled: true
  replicas: 1   # v1: single sniffer pod, no advisory lock
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 50m
      memory: 64Mi
  config:
    sourceID: "main-source-db.images"
    table: "images"
    pkColumn: "id"
    tsColumn: "updated_at"
    extraColumns: "file_path"
    dstPattern: "/incoming/{{ '{{' }}.file_path{{ '}}' }}"
    srcPattern: "src://images/{{ '{{' }}.id{{ '}}' }}"
    shadow: true
    batchSize: "500"
    biasSec: "5"
    intervalSec: "60"
  secrets:
    sourceDSNSecretRef: "imgsync-source-dsn"      # contains key SNIFFER_SOURCE_DSN
    imgsyncDSNSecretRef: "imgsync-db-dsn"         # contains key SNIFFER_IMGSYNC_DSN
```

- [ ] **Step 2: Create sniffer-configmap.yaml**

`deploy/helm/templates/sniffer-configmap.yaml`:

```yaml
{{- if .Values.sniffer.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "imgsync.fullname" . }}-sniffer
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
    component: sniffer
data:
  SNIFFER_SOURCE_ID:    {{ .Values.sniffer.config.sourceID | quote }}
  SNIFFER_TABLE:        {{ .Values.sniffer.config.table | quote }}
  SNIFFER_PK_COLUMN:    {{ .Values.sniffer.config.pkColumn | quote }}
  SNIFFER_TS_COLUMN:    {{ .Values.sniffer.config.tsColumn | quote }}
  SNIFFER_EXTRA_COLUMNS: {{ .Values.sniffer.config.extraColumns | quote }}
  SNIFFER_DST_PATTERN:  {{ .Values.sniffer.config.dstPattern | quote }}
  SNIFFER_SRC_PATTERN:  {{ .Values.sniffer.config.srcPattern | quote }}
  SNIFFER_SHADOW:       {{ .Values.sniffer.config.shadow | quote }}
  SNIFFER_BATCH_SIZE:   {{ .Values.sniffer.config.batchSize | quote }}
  SNIFFER_BIAS_SEC:     {{ .Values.sniffer.config.biasSec | quote }}
  SNIFFER_INTERVAL_SEC: {{ .Values.sniffer.config.intervalSec | quote }}
{{- end }}
```

- [ ] **Step 3: Create sniffer-deployment.yaml**

`deploy/helm/templates/sniffer-deployment.yaml`:

```yaml
{{- if .Values.sniffer.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "imgsync.fullname" . }}-sniffer
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
    component: sniffer
spec:
  replicas: {{ .Values.sniffer.replicas }}
  strategy:
    type: Recreate   # never run two sniffers in parallel — v1 has no advisory lock on sniffer_state
  selector:
    matchLabels:
      {{- include "imgsync.selectorLabels" . | nindent 6 }}
      component: sniffer
  template:
    metadata:
      labels:
        {{- include "imgsync.selectorLabels" . | nindent 8 }}
        component: sniffer
    spec:
      containers:
        - name: sniffer
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args: ["sniffer"]
          envFrom:
            - configMapRef:
                name: {{ include "imgsync.fullname" . }}-sniffer
          env:
            - name: SNIFFER_SOURCE_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.sniffer.secrets.sourceDSNSecretRef }}
                  key: SNIFFER_SOURCE_DSN
            - name: SNIFFER_IMGSYNC_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.sniffer.secrets.imgsyncDSNSecretRef }}
                  key: SNIFFER_IMGSYNC_DSN
          resources:
            {{- toYaml .Values.sniffer.resources | nindent 12 }}
{{- end }}
```

`Recreate` strategy is critical: a rolling update would briefly run two sniffer pods in parallel, and v1 does not lock `sniffer_state`. With Recreate, the old pod is stopped before the new one starts. Out-of-scope sniffer horizontal scaling (spec Section 7) would later replace this with an advisory lock + RollingUpdate.

- [ ] **Step 4: Verify Helm template renders**

```
helm template deploy/helm --set sniffer.enabled=true | grep -A3 "kind: Deployment" | grep sniffer
```

Expected: a Deployment with name suffix `-sniffer` is emitted.

```
helm lint deploy/helm
```

Expected: 0 errors.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/values.yaml deploy/helm/templates/sniffer-deployment.yaml deploy/helm/templates/sniffer-configmap.yaml
git commit -m "feat(helm): add sniffer Deployment + ConfigMap with Recreate strategy"
```

---

## Task 12: E2E test — kind cluster, 1000 rows → 1000 jobs (S-E1 = C5')

Replaces the existing test plan's C5 (which compared imgsync vs NiFi). New C5' is imgsync self-audit only.

**Files:**
- Create: `test/e2e/sniffer_test.go`
- Modify: `Makefile` — add `test-e2e-sniffer` target

- [ ] **Step 1: Write the E2E test**

`test/e2e/sniffer_test.go`:

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// C5' (S-E1): kind cluster brings up sniffer + worker + ftpd + 2 postgres.
// Insert 1000 rows into source DB. Wait. Verify all reach a terminal status.
//
// Pre-requisites (one-time, set up by ./test/e2e/setup.sh):
//   - kind cluster named "imgsync-e2e"
//   - helm release with sniffer.enabled=true, worker.replicas=4
//   - source DB seeded with empty schema
//   - port-forwards exposing imgsync DB at localhost:5433, source DB at localhost:5434
func TestC5Prime_ShadowSelfAudit(t *testing.T) {
	if out, err := exec.Command("./setup.sh").CombinedOutput(); err != nil {
		t.Fatalf("setup.sh failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("./teardown.sh").Run() })

	ctx := context.Background()
	src, err := pgxpool.New(ctx, "postgres://test:test@localhost:5434/source?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	img, err := pgxpool.New(ctx, "postgres://test:test@localhost:5433/imgsync?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()

	if _, err := src.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS images (
		  id BIGINT PRIMARY KEY,
		  updated_at TIMESTAMPTZ NOT NULL,
		  file_path TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 1000; i++ {
		_, err := src.Exec(ctx,
			`INSERT INTO images VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			i, t0.Add(time.Duration(i)*time.Millisecond),
			fmt.Sprintf("2026/04/%04d.jpg", i))
		if err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var pending int
		_ = img.QueryRow(ctx,
			`SELECT COUNT(*) FROM transfer_jobs WHERE status NOT IN ('succeeded','skipped','dead')`).Scan(&pending)
		var enqueued int
		_ = img.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&enqueued)
		t.Logf("enqueued=%d pending=%d", enqueued, pending)
		if enqueued >= 1000 && pending == 0 {
			break
		}
		time.Sleep(15 * time.Second)
	}

	// C2: zero pending after deadline
	var pending int
	if err := img.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE status NOT IN ('succeeded','skipped','dead')`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("expected 0 pending, got %d", pending)
	}

	// C1: 1000 rows enqueued, idempotency held under retries
	var enqueued, distinct int
	_ = img.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&enqueued)
	_ = img.QueryRow(ctx, `SELECT COUNT(DISTINCT trace_id) FROM transfer_jobs`).Scan(&distinct)
	if enqueued != 1000 || distinct != 1000 {
		t.Fatalf("enqueued=%d distinct=%d, want 1000/1000", enqueued, distinct)
	}

	// C3: dead/skipped ratio bounded (test fixture has zero injected failures)
	var dead int
	_ = img.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs WHERE status='dead'`).Scan(&dead)
	if dead > 0 {
		t.Fatalf("dead=%d, expected 0 in clean fixture", dead)
	}
}
```

- [ ] **Step 2: Add setup.sh and teardown.sh**

`test/e2e/setup.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

CLUSTER=imgsync-e2e
if ! kind get clusters | grep -q "^${CLUSTER}$"; then
  kind create cluster --name "${CLUSTER}"
fi

kubectl config use-context "kind-${CLUSTER}"

# Build + load image
docker build -t imgsync:e2e ../..
kind load docker-image imgsync:e2e --name "${CLUSTER}"

# Two postgres instances + ftpd via Helm dependencies (assumed present in v1 chart)
helm upgrade --install imgsync ../../deploy/helm \
  --set image.repository=imgsync --set image.tag=e2e --set image.pullPolicy=Never \
  --set sniffer.enabled=true --set sniffer.config.intervalSec=5 \
  --set worker.replicas=4 \
  --wait --timeout 5m

# Port-forwards
kubectl port-forward svc/imgsync-db 5433:5432 >/dev/null 2>&1 &
echo $! > .pf-imgsync.pid
kubectl port-forward svc/imgsync-source-db 5434:5432 >/dev/null 2>&1 &
echo $! > .pf-source.pid
sleep 3
```

`test/e2e/teardown.sh`:

```bash
#!/usr/bin/env bash
set +e
cd "$(dirname "$0")"
[ -f .pf-imgsync.pid ] && kill "$(cat .pf-imgsync.pid)" 2>/dev/null
[ -f .pf-source.pid ]  && kill "$(cat .pf-source.pid)"  2>/dev/null
rm -f .pf-imgsync.pid .pf-source.pid
kind delete cluster --name imgsync-e2e
```

```
chmod +x test/e2e/setup.sh test/e2e/teardown.sh
```

- [ ] **Step 3: Add Makefile target**

```makefile
test-e2e-sniffer:
	go test -tags e2e -run TestC5Prime_ -timeout 20m -v ./test/e2e/
```

- [ ] **Step 4: Run E2E**

```
make test-e2e-sniffer
```

Expected: PASS within 10 minutes (deadline) + cluster spin-up + tear-down. If `kind` isn't installed: `brew install kind` (macOS) or follow upstream install. If the v1 Helm chart lacks an `imgsync-source-db` service, add a postgres dependency in Chart.yaml or document the gap and pause this task.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/sniffer_test.go test/e2e/setup.sh test/e2e/teardown.sh Makefile
git commit -m "test(sniffer): add C5' kind-cluster E2E for shadow self-audit"
```

---

## Task 13: Documentation updates

Apply the design changes the spec promised: design doc rev 5 + test plan sniffer section.

**Files (outside repo, in gstack project home):**
- Modify: `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md`
- Modify: `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md`

- [ ] **Step 1: design doc — change status line to rev 5**

Replace:

```
**APPROVED** (revision 4, 2026-04-27 — 2 adversarial + 1 plan-eng-review + 1 outside voice cold read all PASS-WITH-FIXES; all fixes applied; ready for Week 1 The Assignment)
```

With:

```
**APPROVED** (revision 5, 2026-04-27 — adds Shadow Sniffer per `/superpowers:brainstorming` design (`docs/superpowers/specs/2026-04-27-imgsync-shadow-sniffer-design.md`); SC#1 replaced with C1~C4 imgsync-self-audit)
```

- [ ] **Step 2: design doc — add Sniffer section**

Insert a new top-level section "Sniffer (Polling, v1 + v2 Connector first iteration)" after the Worker section. Body summarizes:
- Single subcommand `imgsync sniffer`, separate pod (replicas=1, Recreate strategy)
- `sniffer_state(source_id, last_run_ts, last_run_pk)` schema
- trace_id = `${source_table}-${pk}`
- Window query with `(ts, pk::TEXT) > (last, last_pk)` tie-break + `NOW() - bias` exclusion
- `.imgsync_shadow_v1` suffix (operational safety, NOT reconcile)
- Reference: full spec at `docs/superpowers/specs/2026-04-27-imgsync-shadow-sniffer-design.md`

- [ ] **Step 3: design doc — replace SC#1**

Replace SC#1 ("NiFi vs imgsync sha256 set-equality 24h+24h") with C1~C4 verbatim from the spec Section 4.

- [ ] **Step 4: design doc — extend The Assignment**

Add Week 4 (sniffer body) and Week 5 (cutover gate kind test) entries:

```
- **Week 4** Sniffer body: Tasks 1-11 from `docs/superpowers/plans/2026-04-27-imgsync-shadow-sniffer.md`
  - OQ1~OQ4 resolved before start (see spec Section 7)
  - Deliverable: sniffer subcommand passes S0~S3 integration tests
- **Week 5** Cutover gate: Task 12 (kind E2E C5')
  - Pass = 1000 source rows → 1000 terminal jobs in clean fixture
  - Block: shadow start in production until C5' passes locally
```

- [ ] **Step 5: test plan — add sniffer Coverage Map rows**

Insert after row 16 of the existing Coverage Map:

```
[SNIFFER]
17. Sniffer extract_range window calc                ✓       —          —          —    —
18. Sniffer tie-break (last_run_pk)                  ✓       —          —          —    —
19. Sniffer trace_id deterministic                   ✓       —          —          —    —
20. Sniffer source DB query (testcontainers ×2)      —       ✓★         ✓          —    —
21. Sniffer idempotency (UNIQUE conflict path)       —       ✓★         ✓          —    —
22. Sniffer crash recovery (S1)                      —       —          ✓★         —    —
23. Sniffer query timeout isolation (S3)             —       —          ✓          —    —
24. Sniffer tie-break batch correctness (S2)         —       —          ✓★         —    —
25. Shadow self-audit invariant (C5' = S-E1)         —       —          —          ✓★   ✓
```

Renumber the user-flow section accordingly.

- [ ] **Step 6: test plan — add Critical test cases C8~C11 + C5' replacement**

Append to "Critical Test Cases" section:

```
### C5'. Shadow self-audit invariant (replaces C5; locked 2026-04-27)
- E2E + EVAL. NiFi 비교 제거. imgsync 단독으로 1000 row 처리 성공 검증.
- pass: enqueued==1000 ∧ distinct trace_id==1000 ∧ pending==0 ∧ dead==0
- 측정: kind cluster, 10분 deadline, full plan Task 12

### C8. Polling overlap correctness (S0)
- 윈도우 overlap 으로 같은 source row 가 두 번 sniff 되어도 transfer_jobs 1 row 유지

### C9. Crash recovery (S1)
- Sniffer pod kill -9 + restart, 100 row 손실 0 / 중복 0

### C10. Tie-break batch correctness (S2)
- 동일 ts 10 row + batch_size=3 → 4 batch 후 모두 한 번씩 enqueue

### C11. Query timeout isolation (S3)
- Source DB query 가 timeout → watermark 진행 안 함, 다음 run 에서 재 sweep
```

- [ ] **Step 7: test plan — mark old C5 as REPLACED**

Edit the existing "C5. Shadow mode reconcile" entry's first line to:

```
### C5. ~~Shadow mode reconcile~~ — **REPLACED 2026-04-27** by C5' (NiFi observation removed; see spec Section 4 + brainstorming pushback record)
```

- [ ] **Step 8: Verify both docs render cleanly**

```
grep -c "^## " ~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md
```

Expected: section count went up by 1 (new Sniffer section).

```
grep "^### C" ~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md | wc -l
```

Expected: 8+ entries (C0..C7 + C5' + C8..C11).

- [ ] **Step 9: Commit (in repo)**

The two updated docs live outside the repo (`~/.gstack/projects/...`). They are tracked separately by gstack — no repo commit needed for them.

Confirm no repo files were modified in this task:

```
git status
```

Expected: clean working tree.

---

## Self-Review Checklist (run after writing this plan, fix inline)

**1. Spec coverage:**

- [x] Section 1 (Architecture, single binary subcommand) → Task 8
- [x] Section 2 (trace_id deterministic) → Task 4
- [x] Section 3 (sniffer_state schema + tie-break) → Task 1, Task 3, Task 5
- [x] Section 4 (Reconcile = self-audit; SC#1 replacement) → Task 13 (design doc) + Task 12 (C1/C2/C3 in C5')
- [x] Section 5 (Cutover phases, .imgsync_shadow_v1 suffix, F1-F5) → Task 4 (suffix), Task 11 (Helm Recreate), Task 13 (design doc Week 4-5)
- [x] Section 6 (S0~S3 + C5') → Task 9, Task 10, Task 12
- [x] Section 7 (OQ1~OQ4 resolved Week 4 start gate) → Task 13 (design doc Week 4 entry)

**Out-of-scope confirmed v1.1:**
- F5 `imgsync replay` — not in plan (correct)
- Multi-source sniffer — not in plan (correct)
- Sniffer horizontal scaling — Recreate strategy in Task 11 + comment (correct)

**2. Placeholder scan:** none. All "TBD: verify NiFi DSL Week 4" references are correctly tied to spec OQ1, not plan gaps.

**3. Type consistency:**
- `sniffer.State{SourceID, LastRunTS, LastRunPK}` — used same way in Task 3, 5, 7
- `sniffer.Row{PK, TS, Fields}` — Task 5 defines, Task 7 consumes
- `sniffer.Query{Table, PKColumn, TSColumn, ExtraColumns, BatchSize, BiasDuration}` — defined Task 5, used Task 7, 8, 9, 10
- `sniffer.JobSpec{TraceID, Src, Dst}` — Task 6 defines, Task 7 consumes
- `sniffer.Config{...}` — Task 7 defines (uses ImgsyncPool, SourcePool fields), Task 8 wires to env
- `cli.SnifferConfig` (different from sniffer.Config) — Task 8 only

No name collisions, no signature drift.

**4. Pre-conditions verified by Task 0** before any work begins.

---

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 0 | — | — |
| Codex Review | `/codex review` | Independent 2nd opinion | 0 | — | — |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 0 | — | — |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — | — |

**VERDICT:** NO REVIEWS YET — run `/autoplan` for full review pipeline, or `/plan-eng-review` (required) before execution.
