package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
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

func TestLeaseJob_EmptyQueue_ReturnsNil(t *testing.T) {
	pool := mustDB(t)
	j, err := worker.LeaseJob(context.Background(), pool, "worker-1")
	require.NoError(t, err)
	require.Nil(t, j)
}

func TestLeaseJob_PendingRow_TransitionsToLeased(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-1", Src: "localfs:///in/a", Dst: "localfs:///out/a",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
	})
	require.NoError(t, err)

	j, err := worker.LeaseJob(ctx, pool, "worker-1")
	require.NoError(t, err)
	require.NotNil(t, j)
	require.Equal(t, id, j.ID)
	require.Equal(t, "leased", j.Status)
	require.Equal(t, "worker-1", j.LockedBy)
	require.NotNil(t, j.LockedAt)

	// Second lease must return nil (no pending rows left).
	j2, err := worker.LeaseJob(ctx, pool, "worker-2")
	require.NoError(t, err)
	require.Nil(t, j2)
}

func TestLeaseJob_FutureNextRunAt_NotLeased(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-future", Src: "x", Dst: "y",
		SrcProtocol: "localfs", DstProtocol: "localfs",
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`UPDATE transfer_jobs SET next_run_at = NOW() + INTERVAL '1 hour' WHERE trace_id='t-future'`)
	require.NoError(t, err)

	j, err := worker.LeaseJob(ctx, pool, "worker-1")
	require.NoError(t, err)
	require.Nil(t, j, "future next_run_at must not be leased")
}

// TestLeaseJob_RowLockedByOtherTx_IsSkipped exercises the SKIP LOCKED contract
// directly: a separate transaction holds id1 with FOR UPDATE; LeaseJob must
// skip id1 and lease id2 instead. Without SKIP LOCKED the worker would block
// on the held row and the bounded ctx would surface the regression.
func TestLeaseJob_RowLockedByOtherTx_IsSkipped(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id1, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-locked", Src: "x", Dst: "y1",
		SrcProtocol: "localfs", DstProtocol: "localfs",
	})
	require.NoError(t, err)
	id2, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-free", Src: "x", Dst: "y2",
		SrcProtocol: "localfs", DstProtocol: "localfs",
	})
	require.NoError(t, err)

	// Hold id1 in a separate tx with FOR UPDATE so SKIP LOCKED must skip it.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	var heldID int64
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT id FROM transfer_jobs WHERE id=$1 FOR UPDATE`, id1).Scan(&heldID))
	require.Equal(t, id1, heldID)

	// Bounded ctx so a regression (missing SKIP LOCKED → blocked on id1) fails
	// loudly instead of hanging the suite.
	leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	j, err := worker.LeaseJob(leaseCtx, pool, "worker-1")
	require.NoError(t, err)
	require.NotNil(t, j, "must lease the unlocked row, not block on the held one")
	require.Equal(t, id2, j.ID, "must skip locked id1 and pick id2 — SKIP LOCKED contract violated")
}

func TestLeaseJob_ConcurrentLeases_DoNotCollide(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: string(rune('a' + i)), Src: "x", Dst: "y" + string(rune('a'+i)),
			SrcProtocol: "localfs", DstProtocol: "localfs",
		})
		require.NoError(t, err)
	}

	type result struct {
		id  int64
		err error
	}
	const N = 4
	out := make(chan result, N*5)
	for w := 0; w < N; w++ {
		go func(idx int) {
			for k := 0; k < 5; k++ {
				j, err := worker.LeaseJob(ctx, pool, "worker-x")
				if j == nil {
					out <- result{0, err}
					continue
				}
				out <- result{j.ID, err}
			}
		}(w)
	}
	seen := map[int64]int{}
	for i := 0; i < N*5; i++ {
		r := <-out
		require.NoError(t, r.err)
		if r.id != 0 {
			seen[r.id]++
		}
	}
	for id, cnt := range seen {
		require.Equal(t, 1, cnt, "job %d leased %d times — SKIP LOCKED contract violated", id, cnt)
	}
}
