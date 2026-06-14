package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// TestProcessJob_LeaseLost_RetryWriteIsNoOp guards the retry-path lease guard
// (#19) under the single-statement writable-CTE collapse (#32). The success
// path is already covered by TestProcessJob_LeaseLost_TerminalWriteIsNoOp; this
// pins the SAME no-op invariant for writeRetryOrDead, which is the riskiest
// function to collapse into a CTE: its UPDATE is the only one that also sets
// status='pending', attempts, and next_run_at=NOW()+INTERVAL, and it emits a
// 'fail' event. When the lease is lost the UPDATE matches 0 rows, the CTE is
// empty, no 'fail' event may be inserted, and the row must stay untouched.
//
// Scenario: worker A leases the job. The row is then re-leased by worker B
// (simulating sweep + re-lease while A's transfer was still in flight). A's
// transfer then fails with a plain (retryable) error, so A drives the retry
// write holding the stale lease identity A. Because A no longer holds the
// lease, the retry UPDATE must affect zero rows: no 'fail' event may be
// written and the row must stay status='leased', locked_by='B' (NOT clobbered
// to 'pending'). The function must still return (result, nil) — a silent no-op.
func TestProcessJob_LeaseLost_RetryWriteIsNoOp(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	// max_attempts=3 so attempts(0)+1 < max routes through writeRetryOrDead
	// (the scheduled-retry branch), not the exhausted->dead branch.
	enqueueLocal(t, pool, "retry-lease-lost-1", srcPath, filepath.Join(dir, "out"), 3)

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

	// Worker A's transfer fails with a plain retryable error and A drives the
	// retry write holding the stale lease identity A.
	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "A",
		Source:    localfs.NewSource(),
		Transport: retryableTransport{}, // defined in process_test.go
	}
	// A lost-lease outcome must be a silent no-op, not an error.
	_, err = worker.ProcessJob(ctx, deps, job)
	require.NoError(t, err)

	// The row must NOT be clobbered: B still owns the lease, status stays
	// 'leased'. On a broken CTE collapse the retry UPDATE would set
	// status='pending' and clear locked_by.
	var status string
	var lockedBy *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, locked_by FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &lockedBy))
	require.Equal(t, "leased", status, "lost-lease retry write must not overwrite the re-leased row")
	require.NotNil(t, lockedBy, "lost-lease retry write must not clear the re-leased row's locked_by")
	require.Equal(t, "B", *lockedBy, "lost-lease retry write must not steal the row from worker B")

	// No 'fail' event may be written for a job A no longer owns: with the CTE
	// form an empty UPDATE result means the INSERT ... SELECT FROM u inserts
	// nothing, reproducing the current RowsAffected()==0 silent no-op.
	var nFail int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status='fail'`, job.ID,
	).Scan(&nFail))
	require.Equal(t, 0, nFail, "no fail event may be written when the lease was lost")
}
