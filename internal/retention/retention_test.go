package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/retention"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func mustDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		postgres.BasicWaitStrategies(),
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

// stampJob enqueues a job (which also writes an 'enqueue' event), forces it to
// the given status, and back-dates updated_at by ageDays days so retention can
// see it as old/new. Returns the job id.
func stampJob(t *testing.T, pool *pgxpool.Pool, traceID, status string, ageDays int) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: traceID, Src: "x", Dst: traceID + "-dst",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 5,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
UPDATE transfer_jobs
SET status=$2::job_status, updated_at = NOW() - ($3 * INTERVAL '1 day')
WHERE id=$1`, id, status, ageDays)
	require.NoError(t, err)
	return id
}

func countJob(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_jobs WHERE id=$1`, id).Scan(&n))
	return n
}

func countEvents(t *testing.T, pool *pgxpool.Pool, jobID int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1`, jobID).Scan(&n))
	return n
}

// TestSweep_DeletesOldTerminalRows_CascadesEvents verifies the core behavior:
// terminal-status rows (succeeded/skipped/dead) older than the retention window
// are deleted and their transfer_events cascade-delete via the FK.
func TestSweep_DeletesOldTerminalRows_CascadesEvents(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	succ := stampJob(t, pool, "old-succeeded", "succeeded", 40)
	skip := stampJob(t, pool, "old-skipped", "skipped", 40)
	dead := stampJob(t, pool, "old-dead", "dead", 40)

	// Each enqueue wrote one 'enqueue' event; confirm they exist pre-sweep.
	require.Equal(t, 1, countEvents(t, pool, succ))
	require.Equal(t, 1, countEvents(t, pool, skip))
	require.Equal(t, 1, countEvents(t, pool, dead))

	n, err := retention.Sweep(ctx, pool, retention.Config{
		Window:    30 * 24 * time.Hour,
		BatchSize: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, 3, n, "all three old terminal rows must be deleted")

	require.Equal(t, 0, countJob(t, pool, succ), "old succeeded row must be deleted")
	require.Equal(t, 0, countJob(t, pool, skip), "old skipped row must be deleted")
	require.Equal(t, 0, countJob(t, pool, dead), "old dead row must be deleted")

	require.Equal(t, 0, countEvents(t, pool, succ), "events must cascade-delete with the job")
	require.Equal(t, 0, countEvents(t, pool, skip), "events must cascade-delete with the job")
	require.Equal(t, 0, countEvents(t, pool, dead), "events must cascade-delete with the job")
}

// TestSweep_PreservesRecentTerminalRows verifies that terminal rows updated
// more recently than the window are NOT deleted.
func TestSweep_PreservesRecentTerminalRows(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	recent := stampJob(t, pool, "recent-succeeded", "succeeded", 5) // 5 days < 30 day window

	n, err := retention.Sweep(ctx, pool, retention.Config{
		Window:    30 * 24 * time.Hour,
		BatchSize: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "no rows should be deleted")
	require.Equal(t, 1, countJob(t, pool, recent), "recent terminal row must be preserved")
	require.Equal(t, 1, countEvents(t, pool, recent), "recent terminal row events must be preserved")
}

// TestSweep_PreservesNonTerminalRows verifies that pending and leased rows are
// NEVER deleted regardless of age — only terminal rows are eligible.
func TestSweep_PreservesNonTerminalRows(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	pending := stampJob(t, pool, "old-pending", "pending", 90)
	leased := stampJob(t, pool, "old-leased", "leased", 90)

	n, err := retention.Sweep(ctx, pool, retention.Config{
		Window:    30 * 24 * time.Hour,
		BatchSize: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "non-terminal rows must never be deleted")
	require.Equal(t, 1, countJob(t, pool, pending), "old pending row must be preserved")
	require.Equal(t, 1, countJob(t, pool, leased), "old leased row must be preserved")
}

// TestSweep_DisabledByDefault verifies the conservative opt-in contract: a
// zero (or negative) Window means retention is disabled — nothing is ever
// deleted, even for very old terminal rows.
func TestSweep_DisabledByDefault(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	old := stampJob(t, pool, "ancient-dead", "dead", 365)

	n, err := retention.Sweep(ctx, pool, retention.Config{
		Window:    0, // disabled
		BatchSize: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "Window=0 disables retention; no deletes")
	require.Equal(t, 1, countJob(t, pool, old), "disabled retention must preserve all rows")
	require.Equal(t, 1, countEvents(t, pool, old))
}

// TestSweep_BatchesAcrossMultipleLoops verifies the DELETE is batched: with a
// BatchSize smaller than the number of eligible rows, a single Sweep still
// deletes them all (loops internally) to avoid long table locks.
func TestSweep_BatchesAcrossMultipleLoops(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	const total = 5
	for i := 0; i < total; i++ {
		stampJob(t, pool, "batch-"+string(rune('a'+i)), "succeeded", 40)
	}

	n, err := retention.Sweep(ctx, pool, retention.Config{
		Window:    30 * 24 * time.Hour,
		BatchSize: 2, // smaller than total → forces multiple internal batches
	})
	require.NoError(t, err)
	require.Equal(t, total, n, "Sweep must loop batches until all eligible rows are deleted")

	var remaining int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE status='succeeded'`).Scan(&remaining))
	require.Equal(t, 0, remaining, "no eligible terminal rows should remain after a batched sweep")
}

// TestSweep_AdvisoryLock_OnlyOneRetentionRunsAtATime verifies single-writer
// safety across pods: concurrent Sweeps cooperate via the advisory xact lock so
// each eligible row is deleted exactly once (total across all callers == count).
func TestSweep_AdvisoryLock_OnlyOneRetentionRunsAtATime(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	const total = 6
	for i := 0; i < total; i++ {
		stampJob(t, pool, "concur-"+string(rune('a'+i)), "succeeded", 40)
	}

	type result struct {
		n   int
		err error
	}
	out := make(chan result, 4)
	for w := 0; w < 4; w++ {
		go func() {
			n, err := retention.Sweep(ctx, pool, retention.Config{
				Window:    30 * 24 * time.Hour,
				BatchSize: 1000,
			})
			out <- result{n, err}
		}()
	}
	totalDeleted := 0
	for i := 0; i < 4; i++ {
		r := <-out
		require.NoError(t, r.err)
		totalDeleted += r.n
	}
	require.Equal(t, total, totalDeleted, "exactly N deletions across all sweepers (not duplicated)")
}

// TestRun_LoopsUntilContextCancelled verifies the periodic Run loop deletes
// eligible rows on its tick and exits cleanly on context cancellation,
// mirroring internal/sweeper.Run.
func TestRun_LoopsUntilContextCancelled(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	id := stampJob(t, pool, "run-old", "succeeded", 40)

	err := retention.Run(ctx, pool, retention.Config{
		Window:    30 * 24 * time.Hour,
		BatchSize: 1000,
		Interval:  50 * time.Millisecond,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	require.Equal(t, 0, countJob(t, pool, id), "Run loop must delete the old terminal row before cancellation")
}
