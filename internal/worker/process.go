package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

// Deps is what ProcessJob needs from the outside world.
type Deps struct {
	Pool      *pgxpool.Pool
	LockedBy  string
	Source    transfer.Source
	Transport transfer.Transport
	// OnRetry fires when writeRetryOrDead schedules a retry (status=pending,
	// attempts<max). stage is the error-category that triggered it. nil-safe.
	OnRetry func(stage string)
}

// ProcessJob drives a single leased job to a terminal status. It never returns
// the worker-loop error from a job-level outcome — only DB write failures
// propagate. Terminal status writes use a tx so events match status.
//
// The first return value is the terminal metric result label
// (succeeded / skipped / dead / fail) describing the job outcome. Note that the
// retry path (DB status=pending) reports result="fail" for the metric even
// though the transfer_jobs.status value is "pending".
func ProcessJob(ctx context.Context, d Deps, job *Job) (string, error) {
	start := time.Now()

	body, srcSize, openErr := d.Source.Open(ctx, job.Src)
	if openErr != nil {
		return classifyAndWrite(ctx, d, job, openErr, openErrDetails(openErr), start)
	}
	// Ensure body is closed exactly once: explicit close on success before
	// committing 'succeeded'; deferred close on every other path.
	closed := false
	defer func() {
		if !closed {
			_ = body.Close()
		}
	}()

	cw := &counter{r: body}
	written, shaHex, sendErr := d.Transport.Send(ctx, job.Dst, cw, srcSize)
	if sendErr != nil {
		return classifyAndWrite(ctx, d, job, sendErr, transportErrDetails(sendErr), start)
	}

	// F4 size verification.
	if srcSize >= 0 && written != srcSize {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: src=%d written=%d: %w", srcSize, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch", "stage": "verify", "src_size": srcSize, "written": written},
			start)
	}
	if srcSize < 0 && cw.n != written {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: read=%d written=%d: %w", cw.n, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch_unknown_src", "stage": "verify", "read": cw.n, "written": written},
			start)
	}

	// Source close error after a successful send is a retryable transport-class
	// failure (e.g., FTP 226 final-response did not arrive cleanly). Treat it
	// the same as Transport.Send returning a retryable error.
	if closeErr := body.Close(); closeErr != nil {
		closed = true
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("source close after send: %w", closeErr),
			map[string]any{"error": closeErr.Error(), "stage": "source_close"},
			start)
	}
	closed = true

	if err := writeSuccess(ctx, d, job, written, shaHex, start); err != nil {
		return "", err
	}
	return "succeeded", nil
}

// counter wraps an io.Reader to record bytes read.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func writeSuccess(ctx context.Context, d Deps, job *Job, written int64, shaHex string, start time.Time) error {
	detail, _ := json.Marshal(map[string]any{
		"size":        written,
		"sha256":      shaHex,
		"duration_ms": time.Since(start).Milliseconds(),
	})
	// Single writable-CTE statement (implicit tx via pool.Exec): the UPDATE
	// carries the #19 lease guard, and the event INSERT selects from the CTE so
	// it only fires when the UPDATE matched the leased row. When the lease was
	// lost the UPDATE matches 0 rows, the CTE is empty, and no event is inserted
	// — a silent no-op, identical to the previous RowsAffected()==0 behavior.
	if _, err := d.Pool.Exec(ctx, `
WITH u AS (
  UPDATE transfer_jobs SET status='succeeded', locked_at=NULL, locked_by=NULL, updated_at=NOW()
  WHERE id=$1 AND status='leased' AND locked_by=$2
  RETURNING trace_id
)
INSERT INTO transfer_events (trace_id, job_id, status, detail)
SELECT trace_id, $1, 'success', $3 FROM u`, job.ID, d.LockedBy, detail); err != nil {
		return err
	}
	return nil
}

// classifyAndWrite maps a job-level error to its terminal write and returns the
// metric result label for that outcome (skipped / dead / fail).
func classifyAndWrite(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any, _ time.Time) (string, error) {
	switch {
	case errors.Is(jobErr, transfer.ErrSkippable):
		if err := writeTerminal(ctx, d, job, "skipped", "skip", detail, false); err != nil {
			return "", err
		}
		return "skipped", nil
	case errors.Is(jobErr, transfer.ErrPermanent):
		if err := writeTerminal(ctx, d, job, "dead", "dead", detail, true); err != nil {
			return "", err
		}
		return "dead", nil
	default:
		return writeRetryOrDead(ctx, d, job, jobErr, detail)
	}
}

// writeRetryOrDead schedules a retry (DB status=pending) or, when attempts are
// exhausted, writes a terminal dead row. The metric result is "dead" when the
// job is exhausted, otherwise "fail" for the scheduled retry.
func writeRetryOrDead(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any) (string, error) {
	nextAttempts := job.Attempts + 1
	if nextAttempts >= job.MaxAttempts {
		if err := writeTerminalWithAttempts(ctx, d, job, "dead", "dead", detail, nextAttempts); err != nil {
			return "", err
		}
		return "dead", nil
	}
	backoff := time.Duration(1<<nextAttempts) * time.Second // 2,4,8,16,32...
	detailJSON, _ := json.Marshal(detail)
	// Single writable-CTE statement (implicit tx via pool.Exec): the UPDATE
	// carries the #19 lease guard, and the 'fail' event INSERT selects from the
	// CTE. When the lease was lost the UPDATE matches 0 rows, the CTE is empty,
	// and no event is inserted; ct.RowsAffected() (the INSERT count) is then 0,
	// reproducing the previous silent no-op and skipping OnRetry.
	ct, err := d.Pool.Exec(ctx, `
WITH u AS (
  UPDATE transfer_jobs
  SET status='pending', attempts=$2, next_run_at=NOW()+$3::INTERVAL,
      locked_at=NULL, locked_by=NULL, updated_at=NOW()
  WHERE id=$1 AND status='leased' AND locked_by=$4
  RETURNING trace_id
)
INSERT INTO transfer_events (trace_id, job_id, status, detail)
SELECT trace_id, $1, 'fail', $5 FROM u`,
		job.ID, nextAttempts, fmt.Sprintf("%d seconds", int(backoff.Seconds())), d.LockedBy, detailJSON)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() == 0 {
		// Lease was lost (swept + re-leased to another worker): silent no-op.
		return "", nil
	}
	if d.OnRetry != nil {
		stage, _ := detail["stage"].(string)
		d.OnRetry(stage)
	}
	return "fail", nil
}

func writeTerminal(ctx context.Context, d Deps, job *Job, jobStatus, eventStatus string, detail map[string]any, bumpAttempts bool) error {
	attempts := job.Attempts
	if bumpAttempts {
		attempts++
	}
	return writeTerminalWithAttempts(ctx, d, job, jobStatus, eventStatus, detail, attempts)
}

func writeTerminalWithAttempts(ctx context.Context, d Deps, job *Job, jobStatus, eventStatus string, detail map[string]any, attempts int) error {
	detailJSON, _ := json.Marshal(detail)
	// Single writable-CTE statement (implicit tx via pool.Exec): the UPDATE
	// carries the #19 lease guard, and the event INSERT selects from the CTE.
	// When the lease was lost the UPDATE matches 0 rows, the CTE is empty, and
	// no event is inserted — a silent no-op, identical to the previous
	// RowsAffected()==0 behavior.
	if _, err := d.Pool.Exec(ctx, `
WITH u AS (
  UPDATE transfer_jobs
  SET status=$2, attempts=$3, locked_at=NULL, locked_by=NULL, updated_at=NOW()
  WHERE id=$1 AND status='leased' AND locked_by=$4
  RETURNING trace_id
)
INSERT INTO transfer_events (trace_id, job_id, status, detail)
SELECT trace_id, $1, $5, $6 FROM u`, job.ID, jobStatus, attempts, d.LockedBy, eventStatus, detailJSON); err != nil {
		return err
	}
	return nil
}

func openErrDetails(err error) map[string]any {
	d := map[string]any{"error": err.Error(), "stage": "open"}
	if errors.Is(err, transfer.ErrSkippable) {
		d["reason"] = "source_not_found"
	}
	return d
}

func transportErrDetails(err error) map[string]any {
	return map[string]any{"error": err.Error(), "stage": "transport"}
}
