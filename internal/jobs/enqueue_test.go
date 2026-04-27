package jobs_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
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
