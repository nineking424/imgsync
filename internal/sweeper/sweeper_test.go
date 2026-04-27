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
	// Generous budget vs Interval: gives ~20 tick windows so a slow CI
	// runner (testcontainers cold-start, GC pause) can't push the first
	// tick past the deadline and flake the final 'pending' assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	stampStale(t, pool, "looping-1", 6)
	err := sweeper.Run(ctx, pool, sweeper.Config{
		Threshold: 5 * time.Minute,
		Interval:  50 * time.Millisecond,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	var status string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM transfer_jobs WHERE trace_id='looping-1'`).Scan(&status))
	require.Equal(t, "pending", status)
}
