# imgsync v1 — Week 2B: Sweeper, Backoff, Host Cap, Health, EVAL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the production-essential pieces that Week 2A intentionally deferred: sweeper (xact_lock single-writer), per-pod shared jittered idle backoff (F2), FTP host cluster concurrency cap (F1 conn pin), `/livez /readyz /healthz` endpoints, and the EVAL invariants C0/C1/C2/C3/C6 from the eng-review test plan.

**Architecture:** New packages: `internal/sweeper` (single goroutine + xact_lock), `internal/health` (HTTP server with three endpoints), `internal/hostcap` (advisory-lock semaphore wrapping any Transport). Existing `internal/worker.Runner` is amended to use a shared `IdleBackoff` instead of fixed sleep, and `cmd/imgsync/worker.go` wires sweeper + health + host-cap.

**Tech Stack:** stdlib `net/http` for health, `crypto/rand` for jitter, pgx/v5 for advisory locks, testcontainers postgres + in-process FTP server (Week 2A) for tests.

**Series:** This is plan 2B of 4 for v1 base. Predecessors: Week 1 foundation, Week 2A FTP+worker-core. Successor: Week 3 (Helm + cutover).

**Spec references:**
- Design: `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` rev 4 — sections "Sweeper", "Empty-queue idle backoff", "Cluster-wide FTP host concurrency cap", "Health & Metrics"
- Test Plan: `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md` — C0 (size-unknown), C1 (RSS<250MB), C2 (sweeper attempts==0), C3 (skip audit), C6 (52-fixture suite)

---

## File Structure

After Week 2B completes, the new tree under `internal/` is:

```
internal/
├── sweeper/
│   ├── sweeper.go                # xact_lock + 30s loop + expire events
│   └── sweeper_test.go
├── backoff/
│   ├── backoff.go                # per-pod shared 50ms→1s exp + ±25% jitter
│   └── backoff_test.go
├── hostcap/
│   ├── hostcap.go                # advisory_lock + conn pin Transport wrapper
│   └── hostcap_test.go
├── health/
│   ├── server.go                 # /livez /readyz /healthz
│   └── server_test.go
└── eval/
    ├── rss_contract_test.go      # C1: 2GB sparse fixture, HeapInuse<250MB
    ├── audit_invariants_test.go  # C0 + C3 invariants
    └── fixture_suite_test.go     # C6: 52-fixture audit SQL matrix
```

`internal/worker/runner.go` and `cmd/imgsync/worker.go` are amended (not new) to wire these.

---

## Task 1: Sweeper

**Files:**
- Create: `internal/sweeper/sweeper.go`, `internal/sweeper/sweeper_test.go`

Spec: single goroutine per pod, 30s cycle, all work happens inside one transaction with `pg_try_advisory_xact_lock(hashtext('imgsync_sweeper'))`. Lock fails → ROLLBACK (no-op). Lock succeeds → UPDATE expired leases (`status='leased' AND locked_at < NOW() - INTERVAL '5 minutes'`) back to `pending` with `RETURNING id, trace_id`, then INSERT one `expire` event per recovered row. COMMIT releases the lock automatically.

The sweeper never bumps `attempts`. That's the contract C2 enforces.

- [ ] **Step 1: Write the failing test**

Create `internal/sweeper/sweeper_test.go`:

```go
package sweeper_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sweeper"
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
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// helper: stamp a job into 'leased' with a stale locked_at to simulate expiry.
func stampStale(t *testing.T, pool *pgxpool.Pool, traceID string, ageMins int) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: traceID, Src: "x", Dst: traceID + "-dst",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
UPDATE transfer_jobs SET status='leased', locked_by='dead-pod',
       locked_at = NOW() - ($2 * INTERVAL '1 minute')
WHERE id=$1`, id, ageMins)
	require.NoError(t, err)
	return id
}

func TestSweep_StaleLease_RecoversToPending(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	id := stampStale(t, pool, "stale-1", 6) // 6 min > 5 min threshold

	n, err := sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var status string
	var lockedBy *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, locked_by FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status, &lockedBy))
	require.Equal(t, "pending", status)
	require.Nil(t, lockedBy)

	var events int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status='expire'`, id,
	).Scan(&events))
	require.Equal(t, 1, events)
}

func TestSweep_FreshLease_NotRecovered(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	id := stampStale(t, pool, "fresh-1", 1) // 1 min < threshold

	n, err := sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
	require.NoError(t, err)
	require.Equal(t, 0, n)

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status))
	require.Equal(t, "leased", status, "fresh lease must not be swept")
}

func TestSweep_AdvisoryLock_OnlyOneSweeperRunsAtATime(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		stampStale(t, pool, "concur-"+string(rune('a'+i)), 6)
	}

	type result struct {
		n   int
		err error
	}
	out := make(chan result, 4)
	for w := 0; w < 4; w++ {
		go func() {
			n, err := sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
			out <- result{n, err}
		}()
	}
	totalRecovered := 0
	for i := 0; i < 4; i++ {
		r := <-out
		require.NoError(t, r.err)
		totalRecovered += r.n
	}
	require.Equal(t, 5, totalRecovered, "exactly 5 recoveries across all sweepers (not duplicated)")

	var events int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE status='expire'`,
	).Scan(&events))
	require.Equal(t, 5, events, "no duplicate expire events under contention")
}

func TestSweep_DoesNotBumpAttempts(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	id := stampStale(t, pool, "attempts-0", 6)

	_, err := sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
	require.NoError(t, err)

	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT attempts FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&attempts))
	require.Equal(t, 0, attempts, "sweeper must not bump attempts (C2 invariant)")
}

func TestRun_LoopsUntilContextCancelled(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	stampStale(t, pool, "looping-1", 6)
	err := sweeper.Run(ctx, pool, sweeper.Config{
		Threshold: 5 * time.Minute,
		Interval:  60 * time.Millisecond,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	var status string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM transfer_jobs WHERE trace_id='looping-1'`).Scan(&status))
	require.Equal(t, "pending", status)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sweeper/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/sweeper/sweeper.go`**

```go
// Package sweeper recovers expired leases. Single-writer enforced by
// pg_try_advisory_xact_lock so multiple pods can run a sweeper without
// duplicating expire events.
package sweeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config controls sweeper timing.
type Config struct {
	Threshold time.Duration // lease age beyond which to recover; default 5m
	Interval  time.Duration // loop interval; default 30s
}

const sweeperLockKey = "imgsync_sweeper"

// Sweep runs one sweeper cycle in a single transaction. Returns the number
// of rows recovered. If the advisory lock cannot be acquired (another sweeper
// is in flight), returns 0 with no error.
func Sweep(ctx context.Context, pool *pgxpool.Pool, cfg Config) (int, error) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5 * time.Minute
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweeper: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked bool
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtext($1))`, sweeperLockKey,
	).Scan(&locked); err != nil {
		return 0, fmt.Errorf("sweeper: try advisory lock: %w", err)
	}
	if !locked {
		return 0, nil
	}

	threshold := fmt.Sprintf("%d seconds", int(cfg.Threshold.Seconds()))
	rows, err := tx.Query(ctx, `
UPDATE transfer_jobs
SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE status='leased' AND locked_at < NOW() - $1::INTERVAL
RETURNING id, trace_id`, threshold)
	if err != nil {
		return 0, fmt.Errorf("sweeper: update: %w", err)
	}
	type recovered struct {
		id      int64
		traceID string
	}
	var recoveredRows []recovered
	for rows.Next() {
		var r recovered
		if err := rows.Scan(&r.id, &r.traceID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sweeper: scan: %w", err)
		}
		recoveredRows = append(recoveredRows, r)
	}
	rows.Close()

	for _, r := range recoveredRows {
		if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail)
VALUES ($1,$2,'expire','{"reason":"lease_expired"}'::JSONB)`,
			r.traceID, r.id,
		); err != nil {
			return 0, fmt.Errorf("sweeper: insert event for %d: %w", r.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sweeper: commit: %w", err)
	}
	return len(recoveredRows), nil
}

// Run loops Sweep on cfg.Interval ticks until ctx is cancelled. Errors from
// individual cycles are swallowed (sweeper is best-effort); only ctx.Err is
// returned.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if _, err := Sweep(ctx, pool, cfg); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Log-and-continue. Production logger is wired by the runner.
			fmt.Printf("sweeper: cycle error: %v\n", err)
		}
	}
}

// silence unused import in some build matrices
var _ = pgx.ErrNoRows
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sweeper/... -race -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sweeper/
git commit -m "feat(sweeper): xact_lock single-writer + 30s loop + expire events"
```

---

## Task 2: C2 sweeper recovery audit invariant — attempts == 0

**Files:**
- Create: `internal/eval/sweeper_audit_test.go`

The C2 invariant: a job that is leased, abandoned (worker SIGKILL or pgx conn drop), recovered by the sweeper, and then re-leased + processed to success MUST have `attempts == 0`. The reason: `attempts` represents user-visible failure count for SLO triage. Crashes are infrastructure problems, not retries.

This test exercises the full chain: enqueue → simulate-leased-then-abandoned → sweeper recovers → re-lease → ProcessJob success → assert attempts==0.

- [ ] **Step 1: Write the failing test**

Create `internal/eval/sweeper_audit_test.go`:

```go
package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/sweeper"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
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
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestC2_SweeperRecoveredJob_HasAttemptsZero(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	dst := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(src, []byte("c2"), 0o644))

	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "c2-1", Src: src, Dst: dst,
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
	})
	require.NoError(t, err)

	// Simulate worker A leases the row.
	job, err := worker.LeaseJob(ctx, pool, "worker-A")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, id, job.ID)

	// Simulate worker A SIGKILL: lease persists with stale locked_at.
	_, err = pool.Exec(ctx,
		`UPDATE transfer_jobs SET locked_at = NOW() - INTERVAL '6 minutes' WHERE id=$1`, id)
	require.NoError(t, err)

	// Sweeper recovers.
	n, err := sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Worker B re-leases and processes successfully.
	job2, err := worker.LeaseJob(ctx, pool, "worker-B")
	require.NoError(t, err)
	require.NotNil(t, job2)
	require.Equal(t, id, job2.ID)

	deps := worker.Deps{
		Pool: pool, LockedBy: "worker-B",
		Source: localfs.NewSource(), Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job2))

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status, &attempts))
	require.Equal(t, "succeeded", status)
	require.Equal(t, 0, attempts, "C2: sweeper-recovered then succeeded job MUST have attempts==0")

	// Audit trail: enqueue + expire + success in that order.
	rows, err := pool.Query(ctx,
		`SELECT status FROM transfer_events WHERE job_id=$1 ORDER BY ts`, id)
	require.NoError(t, err)
	defer rows.Close()
	var sequence []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		sequence = append(sequence, s)
	}
	require.Equal(t, []string{"enqueue", "expire", "success"}, sequence)

	// Sanity: no time travel.
	_ = time.Now()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/... -run TestC2 -v`
Expected: FAIL until Task 1 sweeper exists.

If Task 1 was committed first (it should be), this test should already PASS. If it doesn't, the bug is in sweeper or ProcessJob — fix the implementation, do not relax the test.

- [ ] **Step 3: Run the test to verify it passes**

Run: `go test ./internal/eval/... -run TestC2 -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/eval/sweeper_audit_test.go
git commit -m "test(eval): add C2 sweeper recovery audit invariant (attempts==0)"
```

---

## Task 3: Per-pod shared jittered idle backoff

**Files:**
- Create: `internal/backoff/backoff.go`, `internal/backoff/backoff_test.go`
- Modify: `internal/worker/runner.go` (replace fixed `IdleSleep` with `*backoff.Idle`)
- Modify: `cmd/imgsync/worker.go` (construct one shared backoff)

Spec: per-pod shared backoff state. 4 worker goroutines share one timer. Schedule: 50ms → 200ms → 500ms → 1s (cap). On each scheduled wait, apply ±25% uniform jitter independently per goroutine. When ANY goroutine successfully leases a job, the entire backoff resets to 50ms and all sleeping siblings wake immediately.

- [ ] **Step 1: Write the failing test**

Create `internal/backoff/backoff_test.go`:

```go
package backoff_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/stretchr/testify/require"
)

func TestIdle_FirstWait_NearBaseDelay(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 50 * time.Millisecond,
		MaxDelay:  1 * time.Second,
	})
	start := time.Now()
	b.WaitOnce(context.Background())
	d := time.Since(start)
	// 50ms ±25% = [37.5ms, 62.5ms]
	require.GreaterOrEqual(t, d, 30*time.Millisecond)
	require.LessOrEqual(t, d, 80*time.Millisecond)
}

func TestIdle_DelayClimbsToCap(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 10 * time.Millisecond,
		MaxDelay:  40 * time.Millisecond,
	})
	for i := 0; i < 5; i++ {
		b.WaitOnce(context.Background())
	}
	require.Equal(t, 40*time.Millisecond, b.CurrentNominalDelay(),
		"after several waits without wakes, delay must reach MaxDelay cap")
}

func TestIdle_WakeAll_ResetsAndUnblocksSiblings(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 200 * time.Millisecond,
		MaxDelay:  1 * time.Second,
	})

	var wg sync.WaitGroup
	woke := make([]time.Duration, 4)
	start := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.WaitOnce(context.Background())
			woke[idx] = time.Since(start)
		}(i)
	}

	// Let goroutines arm their timers, then wake.
	time.Sleep(20 * time.Millisecond)
	b.WakeAll()

	wg.Wait()

	for i, d := range woke {
		require.Less(t, d, 100*time.Millisecond,
			"goroutine %d woke at %v — WakeAll must unblock sleeping siblings", i, d)
	}
	require.Equal(t, 200*time.Millisecond, b.CurrentNominalDelay(),
		"WakeAll must reset nominal delay back to BaseDelay")
}

func TestIdle_ContextCancel_UnblocksWait(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 1 * time.Second,
		MaxDelay:  10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.WaitOnce(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitOnce did not return on ctx cancel")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/backoff/... -v`
Expected: FAIL.

- [ ] **Step 3: Write `internal/backoff/backoff.go`**

```go
// Package backoff implements the per-pod shared idle backoff used by worker
// goroutines when the queue is empty. Schedule is 50ms→200ms→500ms→1s with
// ±25% jitter applied per goroutine. WakeAll resets the schedule and unblocks
// every sleeping goroutine immediately.
package backoff

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Config controls the schedule.
type Config struct {
	BaseDelay time.Duration // first wait; default 50ms
	MaxDelay  time.Duration // cap; default 1s
}

// Idle is a shared backoff state for one pod's worker goroutines.
type Idle struct {
	cfg Config

	mu       sync.Mutex
	nominal  time.Duration   // current scheduled delay (no jitter)
	wakers   []chan struct{} // one per parked goroutine
	rng      *rand.Rand
}

// NewIdle constructs an Idle backoff. Goroutine-safe.
func NewIdle(cfg Config) *Idle {
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 50 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 1 * time.Second
	}
	return &Idle{
		cfg:     cfg,
		nominal: cfg.BaseDelay,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// WaitOnce blocks for the current jittered delay, then advances the nominal
// schedule one step toward MaxDelay. Returns early if ctx is cancelled or
// WakeAll is called.
func (i *Idle) WaitOnce(ctx context.Context) {
	i.mu.Lock()
	delay := i.jitter(i.nominal)
	i.advance()
	wake := make(chan struct{}, 1)
	i.wakers = append(i.wakers, wake)
	i.mu.Unlock()

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-wake:
	case <-ctx.Done():
	}

	// Best-effort waker eviction.
	i.mu.Lock()
	for k, w := range i.wakers {
		if w == wake {
			i.wakers = append(i.wakers[:k], i.wakers[k+1:]...)
			break
		}
	}
	i.mu.Unlock()
}

// WakeAll resets the nominal schedule to BaseDelay and unblocks every sleeping
// goroutine. Call this when a goroutine successfully leases a job.
func (i *Idle) WakeAll() {
	i.mu.Lock()
	i.nominal = i.cfg.BaseDelay
	wakers := i.wakers
	i.wakers = nil
	i.mu.Unlock()
	for _, w := range wakers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// CurrentNominalDelay returns the current pre-jitter delay. Test helper.
func (i *Idle) CurrentNominalDelay() time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.nominal
}

// advance must be called with mu held.
func (i *Idle) advance() {
	switch i.nominal {
	case 50 * time.Millisecond:
		i.nominal = 200 * time.Millisecond
	case 200 * time.Millisecond:
		i.nominal = 500 * time.Millisecond
	case 500 * time.Millisecond:
		i.nominal = 1 * time.Second
	default:
		// generic doubling for non-default base configs (used in tests)
		next := i.nominal * 2
		if next > i.cfg.MaxDelay {
			next = i.cfg.MaxDelay
		}
		i.nominal = next
	}
	if i.nominal > i.cfg.MaxDelay {
		i.nominal = i.cfg.MaxDelay
	}
}

// jitter applies ±25% uniform jitter. mu must be held.
func (i *Idle) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := float64(d) * 0.5 // total +/-25% range = 50% span
	offset := time.Duration((i.rng.Float64() - 0.5) * span)
	return d + offset
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/backoff/... -race -v`
Expected: all PASS.

- [ ] **Step 5: Wire backoff into the runner**

Edit `internal/worker/runner.go`:

Replace the `IdleSleep` field and its usage with a `*backoff.Idle`. Specifically:

```go
// In imports, add:
//   "github.com/nineking424/imgsync/internal/backoff"

// Replace the IdleSleep field on Runner with:
type Runner struct {
	Pool         *pgxpool.Pool
	Workers      int
	PodName      string
	IdleBackoff  *backoff.Idle  // NEW: shared per-pod
	SourceFor    func(protocol string) (SourceLike, error)
	TransportFor func(protocol string) (TransportLike, error)
	OnFinish     func(*Job)
}

// In Run(), replace the IdleSleep default:
if r.IdleBackoff == nil {
	r.IdleBackoff = backoff.NewIdle(backoff.Config{})
}

// In loop(), replace:
//   case <-time.After(r.IdleSleep):
// with:
//   r.IdleBackoff.WaitOnce(ctx)
//   continue
//
// And after a successful LeaseJob (job != nil), call:
//   r.IdleBackoff.WakeAll()
```

Concretely the new `loop` body looks like:

```go
func (r *Runner) loop(ctx context.Context, idx int) {
	lockedBy := fmt.Sprintf("%s-w%d", r.PodName, idx)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := LeaseJob(ctx, r.Pool, lockedBy)
		if err != nil || job == nil {
			r.IdleBackoff.WaitOnce(ctx)
			continue
		}
		r.IdleBackoff.WakeAll()

		src, err := r.SourceFor(job.SrcProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "source-factory"}, true)
			r.fire(job)
			continue
		}
		tr, err := r.TransportFor(job.DstProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "transport-factory"}, true)
			r.fire(job)
			continue
		}

		_ = ProcessJob(ctx, Deps{
			Pool: r.Pool, LockedBy: lockedBy, Source: src, Transport: tr,
		}, job)
		r.fire(job)
	}
}
```

- [ ] **Step 6: Update `cmd/imgsync/worker.go`** to construct the shared backoff

Replace the `IdleSleep: 1 * time.Second` field with:

```go
import (
	// ... existing
	"github.com/nineking424/imgsync/internal/backoff"
)

// In RunE:
idle := backoff.NewIdle(backoff.Config{
	BaseDelay: 50 * time.Millisecond,
	MaxDelay:  1 * time.Second,
})

r := &worker.Runner{
	Pool:        pool,
	Workers:     workers,
	PodName:     podName,
	IdleBackoff: idle,                         // CHANGED
	SourceFor:   /* ... */,
	TransportFor: /* ... */,
}
```

- [ ] **Step 7: Update `internal/worker/runner_test.go`** to use the new field

Find the `Runner{...}` literal in `TestRunner_DrainsQueue` and `TestRunner_UnknownProtocol_RetriesUntilDead`. Replace:

```go
IdleSleep:   50 * time.Millisecond,
```

with:

```go
IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
```

and add `"github.com/nineking424/imgsync/internal/backoff"` to the test imports.

- [ ] **Step 8: Run all tests**

Run: `go test ./... -race -count=1`
Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/backoff/ internal/worker/runner.go internal/worker/runner_test.go cmd/imgsync/worker.go
git commit -m "feat(backoff,worker): shared per-pod jittered idle backoff (F2)"
```

---

## Task 4: FTP host cluster cap with conn pin (F1 regression)

**Files:**
- Create: `internal/hostcap/hostcap.go`, `internal/hostcap/hostcap_test.go`
- Modify: `cmd/imgsync/worker.go` (wrap FTPTransport with HostCap)

Spec: cluster-wide cap on concurrent transfers per FTP host. Implementation: per-host slot semaphore using `pg_advisory_lock(hashtext('ftp_host_${host}_${slot}'))` for slots 0..cap-1. Slot must be released via `pg_advisory_unlock` on the same connection that acquired it (session-scoped lock). Implementation MUST `pool.Acquire()` a dedicated pgx conn, pin it for the entire `Send`, then unlock + release.

The F1 regression test: with `pool_size=2, slot_cap=4`, 4 goroutines acquire host slots concurrently and check `pg_locks WHERE locktype='advisory'` — must see exactly 4 distinct backend pids holding 4 distinct lock keys. If pgx conn reuse leaks a lock or duplicates a slot, the test fails.

- [ ] **Step 1: Write the failing test**

Create `internal/hostcap/hostcap_test.go`:

```go
package hostcap_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/hostcap"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
)

func mustDB(t *testing.T, maxConns int32) *pgxpool.Pool {
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
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: maxConns})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// recordingTransport reports when Send begins, holds until release is signaled,
// and reports when Send ends. Used to observe cap+pin behavior.
type recordingTransport struct {
	enter chan struct{}
	hold  chan struct{}
}

func (rt *recordingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	rt.enter <- struct{}{}
	<-rt.hold
	return 0, "deadbeef", nil
}

func TestHostCap_RespectsSlotLimit_AndPinsConn(t *testing.T) {
	// pool_size=8 ensures we have headroom; slot_cap=4 is the actual ceiling.
	pool := mustDB(t, 8)
	rt := &recordingTransport{enter: make(chan struct{}, 8), hold: make(chan struct{})}
	hc := hostcap.Wrap(pool, rt, hostcap.Config{Cap: 4, Host: "ftp.test.local"})

	// Launch 4 concurrent Sends — all should acquire slots quickly.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = hc.Send(context.Background(), "ftp://ftp.test.local/x", strings.NewReader("y"), 1)
		}()
	}

	for i := 0; i < 4; i++ {
		select {
		case <-rt.enter:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d Sends entered transport — expected 4", i)
		}
	}

	// Inspect pg_locks: exactly 4 advisory locks held by 4 distinct backend pids.
	type lockRow struct {
		pid       int
		classID   uint32
		objID     uint32
	}
	rows, err := pool.Query(context.Background(),
		`SELECT pid, classid, objid FROM pg_locks WHERE locktype='advisory' AND granted=true`)
	require.NoError(t, err)
	defer rows.Close()
	var locks []lockRow
	for rows.Next() {
		var lr lockRow
		require.NoError(t, rows.Scan(&lr.pid, &lr.classID, &lr.objID))
		locks = append(locks, lr)
	}

	require.Len(t, locks, 4, "F1 regression: must see exactly 4 advisory locks for 4 in-flight Sends")
	pids := map[int]bool{}
	keys := map[uint64]bool{}
	for _, lr := range locks {
		pids[lr.pid] = true
		keys[uint64(lr.classID)<<32|uint64(lr.objID)] = true
	}
	require.Equal(t, 4, len(pids), "F1: each lock must be held by a distinct backend pid (no pgx conn reuse)")
	require.Equal(t, 4, len(keys), "F1: 4 distinct slot keys must be held (no duplicates)")

	// Release transports.
	close(rt.hold)
	wg.Wait()

	// After release, advisory locks must be gone.
	require.Eventually(t, func() bool {
		var n int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND granted=true`).Scan(&n)
		return n == 0
	}, 2*time.Second, 50*time.Millisecond, "advisory locks must be released after Send returns")
}

func TestHostCap_OverCapBlocksUntilRelease(t *testing.T) {
	pool := mustDB(t, 8)
	rt := &recordingTransport{enter: make(chan struct{}, 8), hold: make(chan struct{})}
	hc := hostcap.Wrap(pool, rt, hostcap.Config{
		Cap:        2,
		Host:       "ftp.test.local",
		AcquireBackoff: 30 * time.Millisecond,
	})

	for i := 0; i < 2; i++ {
		go func() {
			_, _, _ = hc.Send(context.Background(), "ftp://ftp.test.local/x", strings.NewReader("a"), 1)
		}()
	}
	<-rt.enter
	<-rt.enter

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := hc.Send(ctx, "ftp://ftp.test.local/x", strings.NewReader("a"), 1)
	require.ErrorIs(t, err, context.DeadlineExceeded, "third Send must block beyond cap")

	close(rt.hold)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/hostcap/... -v`
Expected: FAIL.

- [ ] **Step 3: Write `internal/hostcap/hostcap.go`**

```go
// Package hostcap enforces a cluster-wide concurrent-transfer cap per FTP host
// using session-scoped pg_advisory_lock pinned to a dedicated pgx connection
// for the entire transfer. Mandated by Outside Voice F1.
package hostcap

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

// Config controls cap behavior.
type Config struct {
	Cap            int           // 0..N-1 slot count per host; default 8
	Host           string        // optional override; if empty, derived from dst URI
	AcquireBackoff time.Duration // sleep between slot-scan retries; default 100ms
}

// Wrap returns a Transport that gates inner.Send via a Postgres advisory-lock
// semaphore. inner must accept io.Reader; pinning is invisible to it.
func Wrap(pool *pgxpool.Pool, inner transfer.Transport, cfg Config) *CapTransport {
	if cfg.Cap <= 0 {
		cfg.Cap = 8
	}
	if cfg.AcquireBackoff <= 0 {
		cfg.AcquireBackoff = 100 * time.Millisecond
	}
	return &CapTransport{pool: pool, inner: inner, cfg: cfg}
}

// CapTransport is the cap-enforcing wrapper.
type CapTransport struct {
	pool  *pgxpool.Pool
	inner transfer.Transport
	cfg   Config
}

// Send acquires a slot, calls inner.Send while pinned, then releases the slot.
func (c *CapTransport) Send(ctx context.Context, dst string, body io.Reader, size int64) (int64, string, error) {
	host := c.cfg.Host
	if host == "" {
		u, err := url.Parse(dst)
		if err == nil {
			host = u.Host
		}
	}
	if host == "" {
		return 0, "", errors.New("hostcap: cannot derive host from dst")
	}

	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("hostcap: acquire dedicated conn: %w", err)
	}
	defer conn.Release()

	slot, err := acquireSlot(ctx, conn.Conn(), host, c.cfg.Cap, c.cfg.AcquireBackoff)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_, _ = conn.Conn().Exec(context.Background(),
			`SELECT pg_advisory_unlock(hashtext($1))`, slotKey(host, slot))
	}()

	return c.inner.Send(ctx, dst, body, size)
}

func slotKey(host string, slot int) string {
	return fmt.Sprintf("ftp_host_%s_%d", host, slot)
}

func acquireSlot(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgxRow
}, host string, cap int, backoff time.Duration) (int, error) {
	// Use pgx.Conn QueryRow directly via small adapter.
	return 0, nil // placeholder, replaced below
}

// silence unused
var _ = crc32.ChecksumIEEE
```

The `pgxRow` interface adapter is awkward. Use the concrete `*pgx.Conn`:

```go
import (
	"github.com/jackc/pgx/v5"
)

func acquireSlot(ctx context.Context, conn *pgx.Conn, host string, cap int, backoff time.Duration) (int, error) {
	for {
		for slot := 0; slot < cap; slot++ {
			var got bool
			if err := conn.QueryRow(ctx,
				`SELECT pg_try_advisory_lock(hashtext($1))`, slotKey(host, slot),
			).Scan(&got); err != nil {
				return 0, fmt.Errorf("hostcap: try lock: %w", err)
			}
			if got {
				return slot, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff):
		}
	}
}
```

Re-write the file in one shot to avoid the awkward placeholder:

```go
// Package hostcap enforces a cluster-wide concurrent-transfer cap per FTP host
// using session-scoped pg_advisory_lock pinned to a dedicated pgx connection
// for the entire transfer. Mandated by Outside Voice F1.
package hostcap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

type Config struct {
	Cap            int
	Host           string
	AcquireBackoff time.Duration
}

func Wrap(pool *pgxpool.Pool, inner transfer.Transport, cfg Config) *CapTransport {
	if cfg.Cap <= 0 {
		cfg.Cap = 8
	}
	if cfg.AcquireBackoff <= 0 {
		cfg.AcquireBackoff = 100 * time.Millisecond
	}
	return &CapTransport{pool: pool, inner: inner, cfg: cfg}
}

type CapTransport struct {
	pool  *pgxpool.Pool
	inner transfer.Transport
	cfg   Config
}

func (c *CapTransport) Send(ctx context.Context, dst string, body io.Reader, size int64) (int64, string, error) {
	host := c.cfg.Host
	if host == "" {
		u, err := url.Parse(dst)
		if err == nil {
			host = u.Host
		}
	}
	if host == "" {
		return 0, "", errors.New("hostcap: cannot derive host from dst")
	}

	pgConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("hostcap: acquire dedicated conn: %w", err)
	}
	defer pgConn.Release()

	slot, err := acquireSlot(ctx, pgConn.Conn(), host, c.cfg.Cap, c.cfg.AcquireBackoff)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_, _ = pgConn.Conn().Exec(context.Background(),
			`SELECT pg_advisory_unlock(hashtext($1))`, slotKey(host, slot))
	}()

	return c.inner.Send(ctx, dst, body, size)
}

func slotKey(host string, slot int) string {
	return fmt.Sprintf("ftp_host_%s_%d", host, slot)
}

func acquireSlot(ctx context.Context, conn *pgx.Conn, host string, cap int, backoff time.Duration) (int, error) {
	for {
		for slot := 0; slot < cap; slot++ {
			var got bool
			if err := conn.QueryRow(ctx,
				`SELECT pg_try_advisory_lock(hashtext($1))`, slotKey(host, slot),
			).Scan(&got); err != nil {
				return 0, fmt.Errorf("hostcap: try lock: %w", err)
			}
			if got {
				return slot, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff):
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/hostcap/... -race -v`
Expected: all PASS, including the F1 regression test.

- [ ] **Step 5: Wire HostCap into `cmd/imgsync/worker.go`**

Wrap the FTPTransport before passing it to the runner:

```go
// imports:
//   "github.com/nineking424/imgsync/internal/hostcap"

ftpRaw := pftp.NewTransport(ftpPool)
hostCap := envInt("IMGSYNC_FTP_HOST_CAP", 8)
ftpTr := hostcap.Wrap(pool, ftpRaw, hostcap.Config{Cap: hostCap})

// In TransportFor:
case "ftp":
	return ftpTr, nil
```

- [ ] **Step 6: Run full test suite and CLI smoke test**

Run: `go test ./... -race -count=1` and `go build -o bin/imgsync ./cmd/imgsync`
Expected: all PASS, build succeeds.

- [ ] **Step 7: Commit**

```bash
git add internal/hostcap/ cmd/imgsync/worker.go
git commit -m "feat(hostcap): cluster-wide FTP cap via advisory_lock pinned to pgx conn (F1)"
```

---

## Task 5: Health endpoints

**Files:**
- Create: `internal/health/server.go`, `internal/health/server_test.go`
- Modify: `cmd/imgsync/worker.go` (start health server alongside the runner)

Spec endpoints:
- `/livez` — `200 OK` always (process responsiveness only).
- `/readyz` — `200 OK` if last DB ping within 10s; else `503`. Returns when worker is ready to accept traffic.
- `/healthz` — JSON body summarizing pool stats + last-sweep timestamp + last-lease timestamp.

The state is updated by callers (worker loop / sweeper loop) via `Status.OnLeaseAttempt(success bool)` and `Status.OnSweepCycle()`.

- [ ] **Step 1: Write the failing test**

Create `internal/health/server_test.go`:

```go
package health_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/health"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
)

func mustPool(t *testing.T) *pgxpool.Pool {
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

func startServer(t *testing.T, pool *pgxpool.Pool, st *health.Status) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := health.NewServer(pool, st)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + listener.Addr().String()
}

func TestLivez_AlwaysOK(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/livez")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestReadyz_DBOK_Returns200(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestReadyz_DBDown_Returns503(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)
	pool.Close() // simulate DB outage

	resp, err := http.Get(addr + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 503, resp.StatusCode)
}

func TestHealthz_ReportsStatusJSON(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	st.OnLeaseAttempt(true)
	st.OnSweepCycle()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var body map[string]any
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &body))

	require.Contains(t, body, "last_lease_success_ts")
	require.Contains(t, body, "last_sweep_ts")
	require.Contains(t, body, "pool_in_use")
	require.Contains(t, body, "pool_idle")
	require.Contains(t, body, "pool_max")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/health/... -v`
Expected: FAIL.

- [ ] **Step 3: Write `internal/health/server.go`**

```go
// Package health exposes /livez, /readyz, /healthz HTTP endpoints.
package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is updated by the worker loop + sweeper. It is goroutine-safe.
type Status struct {
	mu                  sync.Mutex
	LastLeaseAttemptTS  time.Time
	LastLeaseSuccessTS  time.Time
	LastSweepTS         time.Time
}

func NewStatus() *Status { return &Status{} }

func (s *Status) OnLeaseAttempt(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastLeaseAttemptTS = time.Now()
	if success {
		s.LastLeaseSuccessTS = time.Now()
	}
}

func (s *Status) OnSweepCycle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastSweepTS = time.Now()
}

// Server is a lightweight HTTP server. Use Serve(listener) to bind, Close to stop.
type Server struct {
	pool   *pgxpool.Pool
	status *Status
	hs     *http.Server
}

func NewServer(pool *pgxpool.Pool, st *Status) *Server {
	mux := http.NewServeMux()
	s := &Server{pool: pool, status: st}
	mux.HandleFunc("/livez", s.livez)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/healthz", s.healthz)
	s.hs = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) Serve(l net.Listener) error { return s.hs.Serve(l) }
func (s *Server) Close() error               { return s.hs.Close() }

func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	stat := s.pool.Stat()
	s.status.mu.Lock()
	body := map[string]any{
		"last_lease_attempt_ts": s.status.LastLeaseAttemptTS,
		"last_lease_success_ts": s.status.LastLeaseSuccessTS,
		"last_sweep_ts":         s.status.LastSweepTS,
		"pool_in_use":           stat.AcquiredConns(),
		"pool_idle":             stat.IdleConns(),
		"pool_max":              stat.MaxConns(),
	}
	s.status.mu.Unlock()

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/health/... -race -v`
Expected: all PASS.

- [ ] **Step 5: Wire health server into the worker subcommand**

Edit `cmd/imgsync/worker.go` to start the health server alongside the runner:

```go
import (
	// ... existing
	"net"
	"github.com/nineking424/imgsync/internal/health"
	"github.com/nineking424/imgsync/internal/sweeper"
)

// In RunE, before r.Run(ctx):
status := health.NewStatus()
healthAddr := os.Getenv("IMGSYNC_HEALTH_ADDR")
if healthAddr == "" {
	healthAddr = ":8080"
}
ln, err := net.Listen("tcp", healthAddr)
if err != nil {
	return err
}
hs := health.NewServer(pool, status)
go func() { _ = hs.Serve(ln) }()
defer hs.Close()

// Sweeper goroutine
go func() {
	_ = sweeper.Run(ctx, pool, sweeper.Config{
		Threshold: 5 * time.Minute,
		Interval:  30 * time.Second,
	})
	status.OnSweepCycle()
}()

// OnFinish hook to update lease success counter:
r.OnFinish = func(_ *worker.Job) { status.OnLeaseAttempt(true) }
```

- [ ] **Step 6: Run full test suite**

Run: `go test ./... -race -count=1`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/health/ cmd/imgsync/worker.go
git commit -m "feat(health): add /livez /readyz /healthz endpoints + wire sweeper into worker cmd"
```

---

## Task 6: C1 streaming RSS<250MB contract test

**Files:**
- Create: `internal/eval/rss_contract_test.go`

Spec C1: 2GB sparse fixture; LocalFS→LocalFS and FTP→FTP both pass with `runtime.MemStats.HeapInuse` peak < 250MB. Sample at 100ms via a goroutine started before the transfer and stopped after.

- [ ] **Step 1: Write the failing test**

Create `internal/eval/rss_contract_test.go`:

```go
package eval_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/stretchr/testify/require"
)

const fixtureSize = 2 << 30 // 2 GiB

func make2GBSparseFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	// Sparse: seek to last byte, write one byte. POSIX sparse file.
	_, err = f.Seek(int64(fixtureSize)-1, io.SeekStart)
	require.NoError(t, err)
	_, err = f.Write([]byte{0})
	require.NoError(t, err)
	return path
}

// startRSSWatcher samples HeapInuse every 100ms until ctx is done. Returns a
// pointer that holds peak in bytes.
func startRSSWatcher(ctx context.Context) *uint64 {
	var peak uint64
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runtime.ReadMemStats(&ms)
				for {
					old := atomic.LoadUint64(&peak)
					if ms.HeapInuse <= old {
						break
					}
					if atomic.CompareAndSwapUint64(&peak, old, ms.HeapInuse) {
						break
					}
				}
			}
		}
	}()
	return &peak
}

func TestC1_LocalFS_StreamingRSSUnder250MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 2GB streaming RSS test in -short mode")
	}
	srcPath := make2GBSparseFile(t)
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "out.bin")

	src := localfs.NewSource()
	tr := tlocalfs.NewTransport()

	body, srcSize, err := src.Open(context.Background(), srcPath)
	require.NoError(t, err)
	require.Equal(t, int64(fixtureSize), srcSize)
	defer func() { _ = body.Close() }()

	wctx, wcancel := context.WithCancel(context.Background())
	peak := startRSSWatcher(wctx)

	written, _, err := tr.Send(context.Background(), dstPath, body, srcSize)
	wcancel()
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, int64(fixtureSize), written)

	got := atomic.LoadUint64(peak)
	require.LessOrEqual(t, got, uint64(250<<20),
		"C1: HeapInuse peak %d MiB exceeds 250 MiB cap", got>>20)
}

func TestC1_FTP_StreamingRSSUnder250MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 2GB streaming RSS test in -short mode")
	}
	srv := ftpserver.Start(t)
	srcLocalPath := make2GBSparseFile(t)
	srcCopy := filepath.Join(srv.RootDir, "big.bin")
	require.NoError(t, hardLinkOrCopy(srcLocalPath, srcCopy))

	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost: 4, IdleTTL: 5 * time.Minute, NoopAfter: 60 * time.Second,
		AuthUser: srv.User, AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	src := srcftp.NewSource(pool)
	tr := pftp.NewTransport(pool)

	srcURI := fmt.Sprintf("ftp://%s/big.bin", srv.Addr)
	dstURI := fmt.Sprintf("ftp://%s/out.bin", srv.Addr)

	body, srcSize, err := src.Open(context.Background(), srcURI)
	require.NoError(t, err)
	defer func() { _ = body.Close() }()

	wctx, wcancel := context.WithCancel(context.Background())
	peak := startRSSWatcher(wctx)

	_, _, err = tr.Send(context.Background(), dstURI, body, srcSize)
	wcancel()
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, err)

	got := atomic.LoadUint64(peak)
	require.LessOrEqual(t, got, uint64(250<<20),
		"C1: HeapInuse peak %d MiB exceeds 250 MiB cap (FTP path)", got>>20)
}

// hardLinkOrCopy: try hard link first (no IO), fall back to copy.
func hardLinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// silence unused
var _ = sha256.New
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/eval/... -run TestC1 -v -timeout 5m`
Expected: PASS for both LocalFS and FTP paths. The 2GB sparse files mean disk usage stays small, but the test still spends real time copying 2GB through io.Copy buffers — expect 30-90s per test on a fast box.

If a test fails with HeapInuse > 250MB, the regression is in the streaming chain. Find where buffering was introduced (look for `bytes.Buffer`, `io.ReadAll`, or oversized `make([]byte, N)`) and fix it. Do not relax the threshold.

- [ ] **Step 3: Commit**

```bash
git add internal/eval/rss_contract_test.go
git commit -m "test(eval): add C1 streaming RSS<250MB contract test (LocalFS + FTP)"
```

---

## Task 7: C0 + C3 + C6 — audit invariants and 52-fixture matrix

**Files:**
- Create: `internal/eval/audit_invariants_test.go` (C0 + C3)
- Create: `internal/eval/fixture_suite_test.go` (C6 — 52 scenarios)

C0 verifies size-unknown handling: when `srcSize == -1` and the streamed bytes count differs from the Transport's reported writtenBytes, the job MUST land in `dead` (truncated transfer = ErrPermanent). C3 verifies the ErrSkippable single-event audit invariant. C6 is the 52-scenario fixture matrix from the test plan, validating the operator's one-line SQL audit query.

The C6 SQL under test (from test plan, F3 fix applied):

```sql
SELECT j.id, j.status, j.attempts, e.status, e.ts, e.detail
  FROM transfer_jobs j LEFT JOIN transfer_events e USING (trace_id, job_id)
  WHERE j.trace_id=$1 AND j.dst=$2 ORDER BY e.ts;
```

- [ ] **Step 1: Write the failing C0 + C3 test**

Create `internal/eval/audit_invariants_test.go`:

```go
package eval_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/transfer"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// fakeUnknownSizeSource always returns srcSize=-1.
type fakeUnknownSizeSource struct{ payload string }

func (f *fakeUnknownSizeSource) Open(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader(f.payload)), -1, nil
}

// truncatingTransport claims a different writtenBytes than what was read.
type truncatingTransport struct{ actual int64 }

func (t *truncatingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	return t.actual, "deadbeef", nil
}

func TestC0_SizeUnknownMismatch_TransitionsToDead(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "c0-1", Src: "ftp://fake/x", Dst: "ftp://fake/y",
		SrcProtocol: "ftp", DstProtocol: "ftp", MaxAttempts: 5,
	})
	require.NoError(t, err)

	job, err := worker.LeaseJob(ctx, pool, "w-c0")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, id, job.ID)

	deps := worker.Deps{
		Pool: pool, LockedBy: "w-c0",
		Source:    &fakeUnknownSizeSource{payload: "hello world"}, // 11 bytes read
		Transport: &truncatingTransport{actual: 5},                // claims 5 ACK'd
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status))
	require.Equal(t, "dead", status, "C0: srcSize=-1 with bytesRead != writtenBytes must be ErrPermanent")
}

func TestC3_SkippedJob_ExactlyOneSkipEventWithReason(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "c3-1", Src: "/no/such/file/c3", Dst: "/tmp/c3-out",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
	})
	require.NoError(t, err)

	job, err := worker.LeaseJob(ctx, pool, "w-c3")
	require.NoError(t, err)
	deps := worker.Deps{
		Pool: pool, LockedBy: "w-c3",
		Source: localfs.NewSource(), Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	// (a) status='skipped', (b) attempts==0
	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, id,
	).Scan(&status, &attempts))
	require.Equal(t, "skipped", status)
	require.Equal(t, 0, attempts)

	// (c) exactly one transfer_events row with status='skip' AND detail.reason non-empty
	var n int
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `
SELECT COUNT(*), COALESCE(MIN(detail->>'reason'),'')
FROM transfer_events WHERE job_id=$1 AND status='skip'`, id,
	).Scan(&n, &reason))
	require.Equal(t, 1, n, "C3: must have exactly 1 skip event")
	require.NotEmpty(t, reason, "C3: detail.reason MUST be non-empty for skip events")

	// Re-enqueue same (trace_id, dst) — must not produce a new event.
	_, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "c3-1", Src: "/no/such/file/c3", Dst: "/tmp/c3-out",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
	})
	require.NoError(t, err)
	require.False(t, inserted, "duplicate enqueue must be no-op")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1`, id,
	).Scan(&n))
	require.Equal(t, 2, n, "C3: only enqueue + skip events; re-enqueue MUST NOT add a row")

	// silence unused
	_ = transfer.ErrSkippable
}

// silence unused imports for tests
var _ = filepath.Join
var _ = os.WriteFile
```

- [ ] **Step 2: Run C0 + C3 to verify pass**

Run: `go test ./internal/eval/... -run "TestC0|TestC3" -v`
Expected: PASS.

- [ ] **Step 3: Write the C6 fixture suite**

Create `internal/eval/fixture_suite_test.go`:

```go
package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/sweeper"
	"github.com/nineking424/imgsync/internal/transfer"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// auditQuery is THE one-line SQL the SRE will type. F3 JOIN fix applied.
const auditQuery = `
SELECT j.id, j.status, j.attempts, e.status, e.ts, e.detail
  FROM transfer_jobs j LEFT JOIN transfer_events e USING (trace_id, job_id)
  WHERE j.trace_id=$1 AND j.dst=$2 ORDER BY e.ts`

type expectation struct {
	jobStatus    string
	jobAttempts  int
	eventStates  []string // ordered transfer_events.status sequence
	requireDetail string  // a substring that must appear in last event's detail JSON; empty = skip
}

func runAudit(t *testing.T, pool *pgxpool.Pool, traceID, dst string, want expectation) {
	t.Helper()
	rows, err := pool.Query(context.Background(), auditQuery, traceID, dst)
	require.NoError(t, err)
	defer rows.Close()

	type r struct {
		jobStatus, eventStatus string
		attempts               int
		detail                 []byte
	}
	var got []r
	for rows.Next() {
		var rr r
		var es *string
		var det []byte
		var jobID int64
		_, _ = jobID, det
		if err := rows.Scan(new(int64), &rr.jobStatus, &rr.attempts, &es, new(any), &det); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if es != nil {
			rr.eventStatus = *es
		}
		rr.detail = det
		got = append(got, rr)
	}
	require.NotEmpty(t, got, "audit returned 0 rows for trace_id=%s dst=%s", traceID, dst)

	require.Equal(t, want.jobStatus, got[0].jobStatus, "job status mismatch (%s)", traceID)
	require.Equal(t, want.jobAttempts, got[0].attempts, "attempts mismatch (%s)", traceID)

	var seq []string
	for _, rr := range got {
		if rr.eventStatus != "" {
			seq = append(seq, rr.eventStatus)
		}
	}
	require.Equal(t, want.eventStates, seq, "event sequence mismatch (%s)", traceID)

	if want.requireDetail != "" {
		require.Contains(t, string(got[len(got)-1].detail), want.requireDetail,
			"last event detail must contain %q (%s)", want.requireDetail, traceID)
	}
}

func TestC6_FixtureSuite(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	dst := func(s string) string { return filepath.Join(dir, "dst", s) }

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dst"), 0o755))

	plainSrc := func(name string) string {
		p := filepath.Join(dir, "src", name)
		require.NoError(t, os.WriteFile(p, []byte("data-"+name), 0o644))
		return p
	}

	// Helper to enqueue + lease + process with given Source/Transport.
	process := func(traceID, src, dstPath string, maxAttempts int, sourceImpl transfer.Source, transportImpl transfer.Transport) {
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: traceID, Src: src, Dst: dstPath,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: maxAttempts,
		})
		require.NoError(t, err)
		job, err := worker.LeaseJob(ctx, pool, "w-c6")
		require.NoError(t, err)
		require.NotNil(t, job)
		_ = worker.ProcessJob(ctx, worker.Deps{
			Pool: pool, LockedBy: "w-c6",
			Source: sourceImpl, Transport: transportImpl,
		}, job)
	}

	// --- 10 plain success ---
	for i := 0; i < 10; i++ {
		traceID := fmt.Sprintf("plain-%d", i)
		dstP := dst(fmt.Sprintf("plain-%d", i))
		process(traceID, plainSrc(fmt.Sprintf("plain-%d", i)), dstP, 5, localfs.NewSource(), tlocalfs.NewTransport())
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "succeeded", jobAttempts: 0,
			eventStates: []string{"enqueue", "success"}, requireDetail: "sha256",
		})
	}

	// --- 10 retry-then-success ---
	for i := 0; i < 10; i++ {
		traceID := fmt.Sprintf("retry-%d", i)
		srcP := plainSrc(fmt.Sprintf("retry-%d", i))
		dstP := dst(fmt.Sprintf("retry-%d", i))
		// First attempt: failing transport.
		process(traceID, srcP, dstP, 5, localfs.NewSource(), &alwaysFailTransport{})
		// Reset next_run_at so we can re-lease immediately.
		_, err := pool.Exec(ctx, `UPDATE transfer_jobs SET next_run_at=NOW() WHERE trace_id=$1`, traceID)
		require.NoError(t, err)
		// Second attempt: real transport.
		job, err := worker.LeaseJob(ctx, pool, "w-c6")
		require.NoError(t, err)
		require.NotNil(t, job)
		_ = worker.ProcessJob(ctx, worker.Deps{
			Pool: pool, LockedBy: "w-c6",
			Source: localfs.NewSource(), Transport: tlocalfs.NewTransport(),
		}, job)
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "succeeded", jobAttempts: 1,
			eventStates: []string{"enqueue", "fail", "success"},
		})
	}

	// --- 10 ErrSkippable terminal ---
	for i := 0; i < 10; i++ {
		traceID := fmt.Sprintf("skip-%d", i)
		dstP := dst(fmt.Sprintf("skip-%d", i))
		process(traceID, "/no/such/file/skip-"+fmt.Sprint(i), dstP, 5, localfs.NewSource(), tlocalfs.NewTransport())
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "skipped", jobAttempts: 0,
			eventStates: []string{"enqueue", "skip"}, requireDetail: "source_not_found",
		})
	}

	// --- 10 ErrPermanent + max_attempts dead ---
	for i := 0; i < 10; i++ {
		traceID := fmt.Sprintf("dead-%d", i)
		srcP := plainSrc(fmt.Sprintf("dead-%d", i))
		dstP := dst(fmt.Sprintf("dead-%d", i))
		process(traceID, srcP, dstP, 1, localfs.NewSource(), &alwaysFailTransport{}) // maxAttempts=1
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "dead", jobAttempts: 1,
			eventStates: []string{"enqueue", "dead"},
		})
	}

	// --- 5 duplicate enqueue same (trace_id, dst) ---
	for i := 0; i < 5; i++ {
		traceID := fmt.Sprintf("dup-%d", i)
		srcP := plainSrc(fmt.Sprintf("dup-%d", i))
		dstP := dst(fmt.Sprintf("dup-%d", i))
		// First enqueue + process to success.
		process(traceID, srcP, dstP, 5, localfs.NewSource(), tlocalfs.NewTransport())
		// Second enqueue must be no-op.
		_, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: traceID, Src: srcP, Dst: dstP,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
		})
		require.NoError(t, err)
		require.False(t, inserted, "duplicate (trace_id,dst) must be no-op")
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "succeeded", jobAttempts: 0,
			eventStates: []string{"enqueue", "success"},
		})
	}

	// --- 5 sweeper-recovered (cross-check with C2) ---
	for i := 0; i < 5; i++ {
		traceID := fmt.Sprintf("recov-%d", i)
		srcP := plainSrc(fmt.Sprintf("recov-%d", i))
		dstP := dst(fmt.Sprintf("recov-%d", i))
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: traceID, Src: srcP, Dst: dstP,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
		})
		require.NoError(t, err)
		// Lease and abandon.
		job, _ := worker.LeaseJob(ctx, pool, "lost-pod")
		_, err = pool.Exec(ctx,
			`UPDATE transfer_jobs SET locked_at = NOW() - INTERVAL '6 minutes' WHERE id=$1`, job.ID)
		require.NoError(t, err)
		// Sweeper recovers.
		_, err = sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * 60 * 1e9})
		require.NoError(t, err)
		// Re-lease + process.
		job2, _ := worker.LeaseJob(ctx, pool, "rescue-pod")
		_ = worker.ProcessJob(ctx, worker.Deps{
			Pool: pool, LockedBy: "rescue-pod",
			Source: localfs.NewSource(), Transport: tlocalfs.NewTransport(),
		}, job2)
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "succeeded", jobAttempts: 0, // C2 invariant
			eventStates: []string{"enqueue", "expire", "success"},
		})
	}

	// --- 1 duplicate trace_id with DIFFERENT dst (F3 fix) ---
	{
		traceID := "f3-fanout"
		srcP := plainSrc("f3-src")
		dstA := dst("f3-A")
		dstB := dst("f3-B")
		process(traceID, srcP, dstA, 5, localfs.NewSource(), tlocalfs.NewTransport())
		process(traceID, srcP, dstB, 5, localfs.NewSource(), tlocalfs.NewTransport())

		runAudit(t, pool, traceID, dstA, expectation{
			jobStatus: "succeeded", jobAttempts: 0, eventStates: []string{"enqueue", "success"},
		})
		runAudit(t, pool, traceID, dstB, expectation{
			jobStatus: "succeeded", jobAttempts: 0, eventStates: []string{"enqueue", "success"},
		})

		// Negative case: USING (trace_id) only would fan out. Verify USING (trace_id, job_id) is correct.
		var nA int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT COUNT(*) FROM transfer_jobs j LEFT JOIN transfer_events e USING (trace_id, job_id)
WHERE j.trace_id=$1 AND j.dst=$2`, traceID, dstA).Scan(&nA))
		require.Equal(t, 2, nA, "F3: scoped audit returns ONLY events for dstA's job (not dstB's)")
	}

	// --- 1 re-enqueue same (trace_id, dst) after success (F3 fix) ---
	{
		traceID := "f3-reenqueue"
		srcP := plainSrc("f3-rq-src")
		dstP := dst("f3-rq")
		process(traceID, srcP, dstP, 5, localfs.NewSource(), tlocalfs.NewTransport())
		_, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: traceID, Src: srcP, Dst: dstP,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
		})
		require.NoError(t, err)
		require.False(t, inserted)
		runAudit(t, pool, traceID, dstP, expectation{
			jobStatus: "succeeded", jobAttempts: 0,
			eventStates: []string{"enqueue", "success"},
		})
	}
}

// alwaysFailTransport returns a retryable error on Send.
type alwaysFailTransport struct{}

func (alwaysFailTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	return 0, "", fmt.Errorf("synthetic transient: %s", "io")
}

// silence unused
var _ = json.Marshal
```

> **Note on the recov fixture sweeper threshold:** the test passes `5 * 60 * 1e9` nanoseconds = 5 minutes. If your IDE flags it, replace with `5 * time.Minute` and add a `time` import.

- [ ] **Step 4: Run the C6 suite**

Run: `go test ./internal/eval/... -run TestC6 -v`
Expected: PASS. The suite executes 52 scenarios; if any fails, the failure message names the trace_id so triage is one grep away.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./... -race -count=1`
Expected: all PASS. Skip the 2GB C1 tests with `-short` for normal dev iteration; they run on CI.

- [ ] **Step 6: Commit**

```bash
git add internal/eval/audit_invariants_test.go internal/eval/fixture_suite_test.go
git commit -m "test(eval): add C0 size-unknown, C3 skip-audit, C6 52-fixture matrix"
```

---

## Week 2B Exit Criteria

After Task 7 commits cleanly, the repo state is:

- `make ci` is green (use `-short` to skip 2GB tests in dev; CI runs the full suite).
- Sweeper runs in the worker process, recovers stale leases, never bumps `attempts`. C2 audit invariant proven.
- Idle backoff is per-pod shared with ±25% jitter; thundering-herd risk neutralized.
- FTP host cluster cap enforces N concurrent transfers per host with conn-pin verified by F1 regression test.
- `/livez /readyz /healthz` respond correctly under DB-up and DB-down.
- C0/C1/C3/C6 invariants pass.
- The operator's one-line audit SQL works for every fixture scenario including F3 fan-out.

After this plan is executed, v1 base code is feature-complete. Week 3 is packaging only: Dockerfile, Helm chart with pre-install migration init Job, throughput E2E (C7), and the F5 dirty-state recovery gate.
