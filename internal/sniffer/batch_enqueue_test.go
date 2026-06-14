package sniffer_test

import (
	"context"
	"testing"

	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
)

// TestEnqueueBatch_InsertsAllRowsInOneCall is the #31 regression: the sniffer
// must enqueue a whole poll's worth of rows through a single batched insert
// (one round-trip) instead of one INSERT per row. The batch entry point
// (EnqueueBatch) is the observable contract for that change.
//
// RED on current code: EnqueueBatch does not exist, so this file fails to
// compile — the package's test binary will not build. GREEN once the #31 fix
// adds the batched insert path. The semantic assertions below pin the
// inserted-count and idempotency invariants the refactor must preserve.
func TestEnqueueBatch_InsertsAllRowsInOneCall(t *testing.T) {
	ctx := context.Background()
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)

	specs := []sniffer.JobSpec{
		{TraceID: "images-1", Src: "src://images/1", Dst: "/in/1", SrcProtocol: "postgres", DstProtocol: "ftp"},
		{TraceID: "images-2", Src: "src://images/2", Dst: "/in/2", SrcProtocol: "postgres", DstProtocol: "ftp"},
		{TraceID: "images-3", Src: "src://images/3", Dst: "/in/3", SrcProtocol: "postgres", DstProtocol: "ftp"},
	}

	inserted, err := enq.EnqueueBatch(ctx, specs)
	require.NoError(t, err)
	require.Equal(t, 3, inserted, "all 3 novel rows must report as newly inserted")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n))
	require.Equal(t, 3, n, "all 3 rows must land in transfer_jobs")
}

// TestEnqueueBatch_PreservesInsertedCountSemantics pins the inserted-count
// contract under ON CONFLICT (trace_id, dst) DO NOTHING: the per-row Enqueue
// counts a UNIQUE conflict as 0, and the batched path must report the SAME
// newly-inserted total (NOT the input length) so RunOnce's returned count and
// the OnEnqueue callback stay correct across the refactor.
func TestEnqueueBatch_PreservesInsertedCountSemantics(t *testing.T) {
	ctx := context.Background()
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)

	// Pre-insert one row so the next batch overlaps it.
	pre, err := enq.Enqueue(ctx, sniffer.JobSpec{TraceID: "images-1", Src: "s", Dst: "/in/1", SrcProtocol: "postgres", DstProtocol: "ftp"})
	require.NoError(t, err)
	require.True(t, pre)

	// Batch of 3, one of which (images-1 -> /in/1) already exists.
	specs := []sniffer.JobSpec{
		{TraceID: "images-1", Src: "s", Dst: "/in/1", SrcProtocol: "postgres", DstProtocol: "ftp"}, // conflict
		{TraceID: "images-2", Src: "s", Dst: "/in/2", SrcProtocol: "postgres", DstProtocol: "ftp"}, // new
		{TraceID: "images-3", Src: "s", Dst: "/in/3", SrcProtocol: "postgres", DstProtocol: "ftp"}, // new
	}
	inserted, err := enq.EnqueueBatch(ctx, specs)
	require.NoError(t, err)
	require.Equal(t, 2, inserted, "only the 2 novel rows count; the conflicting row must not be counted")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n))
	require.Equal(t, 3, n, "no duplicate row from the conflicting spec")
}

// TestEnqueueBatch_EmptyIsNoop guards the degenerate input the batched path
// must tolerate without emitting a malformed VALUES () clause.
func TestEnqueueBatch_EmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	pool := setupTransferJobs(t)
	enq := sniffer.NewEnqueuer(pool)

	inserted, err := enq.EnqueueBatch(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 0, inserted)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n))
	require.Equal(t, 0, n)
}
