package sniffer_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupTransferJobs(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func TestEnqueue_InsertsNewRow(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)

	inserted, err := enq.Enqueue(context.Background(), sniffer.JobSpec{
		TraceID:     "images-1",
		Src:         "src://images/1",
		Dst:         "/incoming/a.jpg.imgsync_shadow_v1",
		SrcProtocol: "postgres",
		DstProtocol: "ftp",
	})
	require.NoError(t, err)
	require.True(t, inserted, "expected inserted=true")

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='images-1'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestEnqueue_SecondCallIsNoop(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)
	spec := sniffer.JobSpec{TraceID: "images-1", Src: "s", Dst: "d", SrcProtocol: "postgres", DstProtocol: "ftp"}

	_, err := enq.Enqueue(context.Background(), spec)
	require.NoError(t, err)
	inserted, err := enq.Enqueue(context.Background(), spec)
	require.NoError(t, err)
	require.False(t, inserted, "expected inserted=false on duplicate")

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='images-1'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestEnqueue_DifferentDstSameTraceIDInsertsBoth(t *testing.T) {
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)
	ctx := context.Background()
	_, err := enq.Enqueue(ctx, sniffer.JobSpec{TraceID: "x-1", Src: "s", Dst: "/a", SrcProtocol: "postgres", DstProtocol: "ftp"})
	require.NoError(t, err)
	_, err = enq.Enqueue(ctx, sniffer.JobSpec{TraceID: "x-1", Src: "s", Dst: "/b", SrcProtocol: "postgres", DstProtocol: "ftp"})
	require.NoError(t, err)

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id='x-1'`).Scan(&n))
	require.Equal(t, 2, n)
}
