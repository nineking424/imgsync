package eval_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
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
}
