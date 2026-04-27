// Package worker owns dispatch, per-job processing, and the worker loop.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is a snapshot of a transfer_jobs row at lease time.
type Job struct {
	ID          int64
	TraceID     string
	Src         string
	Dst         string
	SrcProtocol string
	DstProtocol string
	Payload     []byte
	Status      string
	Attempts    int
	MaxAttempts int
	LockedAt    *time.Time
	LockedBy    string
	NextRunAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LeaseJob runs the spec dispatch SQL: pick the oldest pending row whose
// next_run_at has come due, mark it leased, and return its full row.
// Returns (nil, nil) when the queue is empty.
func LeaseJob(ctx context.Context, pool *pgxpool.Pool, lockedBy string) (*Job, error) {
	var j Job
	err := pool.QueryRow(ctx, `
WITH next AS (
  SELECT id FROM transfer_jobs
  WHERE status='pending' AND next_run_at <= NOW()
  ORDER BY next_run_at, id
  FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE transfer_jobs j
SET status='leased', locked_at=NOW(), locked_by=$1, updated_at=NOW()
FROM next WHERE j.id = next.id
RETURNING j.id, j.trace_id, j.src, j.dst, j.src_protocol, j.dst_protocol,
          j.payload, j.status, j.attempts, j.max_attempts,
          j.locked_at, j.locked_by, j.next_run_at, j.created_at, j.updated_at`,
		lockedBy,
	).Scan(
		&j.ID, &j.TraceID, &j.Src, &j.Dst, &j.SrcProtocol, &j.DstProtocol,
		&j.Payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.LockedAt, &j.LockedBy, &j.NextRunAt, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lease: %w", err)
	}
	return &j, nil
}
