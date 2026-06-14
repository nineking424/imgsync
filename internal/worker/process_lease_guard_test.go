package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nineking424/imgsync/internal/sources/localfs"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// TestProcessJob_LeaseLost_TerminalWriteIsNoOp reproduces issue #19: a terminal
// status write must carry an optimistic lease guard. When a slow transfer
// overruns the sweeper threshold, the sweeper resets the row leased->pending and
// another worker re-leases it; the original worker must NOT clobber that
// re-leased state when it finishes late.
//
// Scenario: worker A leases the job, then the row is re-leased by worker B
// (simulating sweep + re-lease while A's transfer was still in flight). A then
// finishes its (successful) transfer and writes the terminal status with
// Deps.LockedBy=A. Because A no longer holds the lease, the terminal UPDATE must
// affect zero rows and no success event may be written: the row must stay
// status='leased', locked_by='B'. On current code (WHERE id=$1 only) the write
// clobbers B's lease to 'succeeded' and inserts a 'success' event, so this test
// FAILS until the guard is added.
func TestProcessJob_LeaseLost_TerminalWriteIsNoOp(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "lease-lost-1", srcPath, dstPath, 3)

	// Worker A leases the job.
	job, err := worker.LeaseJob(ctx, pool, "A")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "A", job.LockedBy)

	// Simulate sweeper reclaim + re-lease by worker B while A's transfer is in
	// flight: the row is now owned by B, not A.
	_, err = pool.Exec(ctx,
		`UPDATE transfer_jobs SET status='leased', locked_by='B', locked_at=NOW() WHERE id=$1`,
		job.ID)
	require.NoError(t, err)

	// Worker A finishes late and writes its terminal status holding the stale
	// lease identity A (job snapshot still carries locked_by='A').
	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "A",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	// A lost-lease outcome must be a silent no-op, not an error (the worker loop
	// ignores ProcessJob's job-level outcome anyway).
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	// The row must NOT be clobbered: B still owns the lease. locked_by is
	// nullable (terminal writes set it to NULL), so scan into a pointer; on the
	// buggy code the clobber sets status='succeeded' and locked_by=NULL.
	var status string
	var lockedBy *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, locked_by FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &lockedBy))
	require.Equal(t, "leased", status, "lost-lease terminal write must not overwrite the re-leased row")
	require.NotNil(t, lockedBy, "lost-lease terminal write must not clear the re-leased row's locked_by")
	require.Equal(t, "B", *lockedBy, "lost-lease terminal write must not steal the row from worker B")

	// No success event may be written for a job A no longer owns.
	var nSuccess int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status='success'`, job.ID,
	).Scan(&nSuccess))
	require.Equal(t, 0, nSuccess, "no success event may be written when the lease was lost")
}

// TestProcessJob_LeaseHeld_TerminalWriteSucceeds is the positive control: when
// Deps.LockedBy matches the row's current locked_by, the terminal write commits
// as before (status='succeeded' + 'success' event). This guards against a fix
// that over-rejects and breaks the normal path.
func TestProcessJob_LeaseHeld_TerminalWriteSucceeds(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "lease-held-1", srcPath, dstPath, 3)

	job, err := worker.LeaseJob(ctx, pool, "A")
	require.NoError(t, err)
	require.NotNil(t, job)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "A",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status))
	require.Equal(t, "succeeded", status, "lease-held terminal write must commit as before")
	mustEvent(t, pool, job.ID, "success")
}
