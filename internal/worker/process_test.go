package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

func enqueueLocal(t *testing.T, pool *pgxpool.Pool, traceID, src, dst string, max int) int64 {
	t.Helper()
	id, _, err := jobs.Enqueue(context.Background(), pool, jobs.EnqueueArgs{
		TraceID: traceID, Src: src, Dst: dst,
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: max,
	})
	require.NoError(t, err)
	return id
}

func mustEvent(t *testing.T, pool *pgxpool.Pool, jobID int64, status string) {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status=$2`, jobID, status,
	).Scan(&n))
	require.Equal(t, 1, n, "expected one transfer_events row with status=%q for job %d", status, jobID)
}

func TestProcessJob_Success_TransitionsToSucceededWithEvent(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "ok-1", srcPath, dstPath, 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)
	require.NotNil(t, job)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status))
	require.Equal(t, "succeeded", status)

	mustEvent(t, pool, job.ID, "success")

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))

	want := sha256.Sum256([]byte("hello world"))
	var detail []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT detail FROM transfer_events WHERE job_id=$1 AND status='success'`, job.ID,
	).Scan(&detail))
	require.Contains(t, string(detail), hex.EncodeToString(want[:]))
}

func TestProcessJob_SourceMissing_TransitionsToSkippedNoAttemptsBump(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	enqueueLocal(t, pool, "skip-1", "/no/such/src", "/tmp/imgsync-skip-out", 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "skipped", status)
	require.Equal(t, 0, attempts, "skipped must not bump attempts")
	mustEvent(t, pool, job.ID, "skip")
}

func TestProcessJob_PermanentSrcError_TransitionsToDeadAttemptsBumped(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	// Permanent: source path is a directory.
	enqueueLocal(t, pool, "perm-1", t.TempDir(), "/tmp/imgsync-perm", 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "dead", status)
	require.Equal(t, 1, attempts, "permanent err must bump attempts once on the dead transition")
	mustEvent(t, pool, job.ID, "dead")
}

func TestProcessJob_RetryableTransportError_BackoffPending(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	enqueueLocal(t, pool, "retry-1", srcPath, filepath.Join(dir, "out"), 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: retryableTransport{},
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "pending", status)
	require.Equal(t, 1, attempts)
	mustEvent(t, pool, job.ID, "fail")

	var nextRunAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT next_run_at FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&nextRunAt))
	require.True(t, nextRunAt.After(time.Now()), "next_run_at must be bumped into the future for retryable failures, got %v", nextRunAt)
}

func TestProcessJob_RetryableHitsMaxAttempts_TransitionsToDead(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	enqueueLocal(t, pool, "exhaust-1", srcPath, filepath.Join(dir, "out"), 1) // max_attempts=1
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: retryableTransport{},
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "dead", status)
	require.Equal(t, 1, attempts)
	mustEvent(t, pool, job.ID, "dead")
}

func TestProcessJob_TruncatedTransfer_PermanentDead(t *testing.T) {
	// Verifies F4: writtenBytes != srcSize must demote to ErrPermanent.
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "trunc-1", srcPath, dstPath, 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: &truncatingTransport{actual: 5}, // claims 5 bytes ACK'd vs srcSize=11
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status))
	require.Equal(t, "dead", status, "truncated transfer must be classified ErrPermanent (dead)")
	mustEvent(t, pool, job.ID, "dead")
}

type truncatingTransport struct{ actual int64 }

func (t *truncatingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body) // consume reader so callers don't deadlock
	return t.actual, "deadbeef", nil
}

// retryableTransport returns a plain (non-sentinel) error so the worker treats
// it as retryable. Used to exercise the backoff/retry-exhaustion paths without
// relying on a specific transport's error classification.
type retryableTransport struct{}

func (retryableTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body) // consume reader so callers don't deadlock
	return 0, "", errors.New("transient transport failure")
}
