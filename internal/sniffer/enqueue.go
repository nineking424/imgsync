package sniffer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// JobSpec describes a single enqueue request. All fields are required;
// SrcProtocol/DstProtocol must match v1 protocol identifiers consumed by the worker.
type JobSpec struct {
	TraceID     string
	Src         string
	Dst         string
	SrcProtocol string
	DstProtocol string
}

// Enqueuer inserts deduplicated transfer_jobs rows. Pool lifecycle is caller-owned.
type Enqueuer struct {
	pool *pgxpool.Pool
}

// NewEnqueuer wraps an existing pool; it does not take ownership.
func NewEnqueuer(pool *pgxpool.Pool) *Enqueuer {
	return &Enqueuer{pool: pool}
}

// Enqueue inserts one transfer_jobs row if (trace_id, dst) is novel.
// Returns inserted=true when a new row was created, false on UNIQUE conflict.
func (e *Enqueuer) Enqueue(ctx context.Context, j JobSpec) (bool, error) {
	tag, err := e.pool.Exec(ctx, `
		INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (trace_id, dst) DO NOTHING`,
		j.TraceID, j.Src, j.Dst, j.SrcProtocol, j.DstProtocol)
	if err != nil {
		return false, fmt.Errorf("enqueue %s->%s: %w", j.TraceID, j.Dst, err)
	}
	return tag.RowsAffected() == 1, nil
}
