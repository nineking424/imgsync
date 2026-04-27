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

	// Worker A leases the row.
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
		require.NoError(t, rows.Scan(&s))
		sequence = append(sequence, s)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"enqueue", "expire", "success"}, sequence)
}
