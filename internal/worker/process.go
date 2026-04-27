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
}

// ProcessJob drives a single leased job to a terminal status. It never returns
// the worker-loop error from a job-level outcome — only DB write failures
// propagate. Terminal status writes use a tx so events match status.
func ProcessJob(ctx context.Context, d Deps, job *Job) error {
	start := time.Now()

	body, srcSize, openErr := d.Source.Open(ctx, job.Src)
	if openErr != nil {
		return classifyAndWrite(ctx, d, job, openErr, openErrDetails(openErr), start)
	}
	defer func() { _ = body.Close() }()

	cw := &counter{r: body}
	written, shaHex, sendErr := d.Transport.Send(ctx, job.Dst, cw, srcSize)
	if sendErr != nil {
		return classifyAndWrite(ctx, d, job, sendErr, transportErrDetails(sendErr), start)
	}

	// F4 size verification.
	if srcSize >= 0 && written != srcSize {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: src=%d written=%d: %w", srcSize, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch", "src_size": srcSize, "written": written},
			start)
	}
	if srcSize < 0 && cw.n != written {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: read=%d written=%d: %w", cw.n, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch_unknown_src", "read": cw.n, "written": written},
			start)
	}

	return writeSuccess(ctx, d, job, written, shaHex, start)
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
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs SET status='succeeded', locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,'success',$3)`,
		job.TraceID, job.ID, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func classifyAndWrite(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any, _ time.Time) error {
	switch {
	case errors.Is(jobErr, transfer.ErrSkippable):
		return writeTerminal(ctx, d, job, "skipped", "skip", detail, false)
	case errors.Is(jobErr, transfer.ErrPermanent):
		return writeTerminal(ctx, d, job, "dead", "dead", detail, true)
	default:
		return writeRetryOrDead(ctx, d, job, jobErr, detail)
	}
}

func writeRetryOrDead(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any) error {
	nextAttempts := job.Attempts + 1
	if nextAttempts >= job.MaxAttempts {
		return writeTerminalWithAttempts(ctx, d, job, "dead", "dead", detail, nextAttempts)
	}
	backoff := time.Duration(1<<nextAttempts) * time.Second // 2,4,8,16,32...
	detailJSON, _ := json.Marshal(detail)
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs
SET status='pending', attempts=$2, next_run_at=NOW()+$3::INTERVAL,
    locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID, nextAttempts, fmt.Sprintf("%d seconds", int(backoff.Seconds()))); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,'fail',$3)`,
		job.TraceID, job.ID, detailJSON); err != nil {
		return err
	}
	_ = jobErr // consumed via detail map; retained for caller context
	return tx.Commit(ctx)
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
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs
SET status=$2, attempts=$3, locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID, jobStatus, attempts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,$3,$4)`,
		job.TraceID, job.ID, eventStatus, detailJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func openErrDetails(err error) map[string]any {
	d := map[string]any{"error": err.Error()}
	if errors.Is(err, transfer.ErrSkippable) {
		d["reason"] = "source_not_found"
	}
	return d
}

func transportErrDetails(err error) map[string]any {
	return map[string]any{"error": err.Error(), "stage": "transport"}
}
