package sniffer

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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

// EnqueueBatch inserts a whole poll's worth of rows in a single multi-row
// INSERT ... ON CONFLICT (trace_id, dst) DO NOTHING round-trip. It returns the
// number of rows that were NEWLY inserted (UNIQUE conflicts count as 0), which
// matches the per-row Enqueue inserted-count semantics RunOnce and OnEnqueue
// depend on. An empty (or nil) specs slice is a no-op that returns (0, nil)
// without issuing a query.
func (e *Enqueuer) EnqueueBatch(ctx context.Context, specs []JobSpec) (int, error) {
	if len(specs) == 0 {
		return 0, nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol) VALUES ")
	args := make([]any, 0, len(specs)*5)
	for i, j := range specs {
		if i > 0 {
			b.WriteByte(',')
		}
		n := i * 5
		b.WriteString("($" + strconv.Itoa(n+1) + ",$" + strconv.Itoa(n+2) + ",$" + strconv.Itoa(n+3) + ",$" + strconv.Itoa(n+4) + ",$" + strconv.Itoa(n+5) + ")")
		args = append(args, j.TraceID, j.Src, j.Dst, j.SrcProtocol, j.DstProtocol)
	}
	b.WriteString(" ON CONFLICT (trace_id, dst) DO NOTHING")

	tag, err := e.pool.Exec(ctx, b.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("enqueue batch of %d: %w", len(specs), err)
	}
	return int(tag.RowsAffected()), nil
}
