package eval_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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
  FROM transfer_jobs j LEFT JOIN transfer_events e ON j.id = e.job_id
  WHERE j.trace_id=$1 AND j.dst=$2 ORDER BY e.ts, e.id`

type expectation struct {
	jobStatus     string
	jobAttempts   int
	eventStates   []string // ordered transfer_events.status sequence
	requireDetail string   // a substring that must appear in last event's detail JSON; empty = skip
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
		_, _ = worker.ProcessJob(ctx, worker.Deps{
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
		_, _ = worker.ProcessJob(ctx, worker.Deps{
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
		process(traceID, fmt.Sprintf("/no/such/file/skip-%d", i), dstP, 5, localfs.NewSource(), tlocalfs.NewTransport())
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
		job, err := worker.LeaseJob(ctx, pool, "lost-pod")
		require.NoError(t, err)
		require.NotNil(t, job)
		_, err = pool.Exec(ctx,
			`UPDATE transfer_jobs SET locked_at = NOW() - INTERVAL '6 minutes' WHERE id=$1`, job.ID)
		require.NoError(t, err)
		// Sweeper recovers.
		_, err = sweeper.Sweep(ctx, pool, sweeper.Config{Threshold: 5 * time.Minute})
		require.NoError(t, err)
		// Re-lease + process.
		job2, err := worker.LeaseJob(ctx, pool, "rescue-pod")
		require.NoError(t, err)
		require.NotNil(t, job2)
		_, _ = worker.ProcessJob(ctx, worker.Deps{
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

		// Negative case: USING (trace_id) only would fan out. Verify ON j.id = e.job_id is correct.
		var nA int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT COUNT(*) FROM transfer_jobs j LEFT JOIN transfer_events e ON j.id = e.job_id
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
	return 0, "", fmt.Errorf("synthetic transient")
}
