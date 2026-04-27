package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnqueueArgs is the input to Enqueue.
type EnqueueArgs struct {
	TraceID     string
	Src         string
	Dst         string
	SrcProtocol string
	DstProtocol string
	Payload     []byte // raw JSON; pass nil for empty object
	MaxAttempts int
}

// Enqueue inserts a new transfer_jobs row idempotently. inserted=false means
// the (trace_id, dst) tuple already existed; in that case id is the existing row.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, a EnqueueArgs) (int64, bool, error) {
	if a.TraceID == "" || a.Src == "" || a.Dst == "" {
		return 0, false, errors.New("enqueue: trace_id, src, dst are required")
	}
	if a.SrcProtocol == "" || a.DstProtocol == "" {
		return 0, false, errors.New("enqueue: src_protocol and dst_protocol are required")
	}
	if a.MaxAttempts <= 0 {
		a.MaxAttempts = 5
	}
	payload := a.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	scanErr := tx.QueryRow(ctx, `
INSERT INTO transfer_jobs
  (trace_id, src, dst, src_protocol, dst_protocol, payload, max_attempts)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (trace_id, dst) DO NOTHING
RETURNING id`,
		a.TraceID, a.Src, a.Dst, a.SrcProtocol, a.DstProtocol, payload, a.MaxAttempts,
	).Scan(&id)
	if scanErr == nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail)
VALUES ($1,$2,'enqueue',$3)`,
			a.TraceID, id, payload,
		); err != nil {
			return 0, false, fmt.Errorf("emit enqueue event: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, false, fmt.Errorf("commit: %w", err)
		}
		return id, true, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("insert: %w", scanErr)
	}

	// Conflict path: look up the existing row id.
	if err := tx.QueryRow(ctx,
		`SELECT id FROM transfer_jobs WHERE trace_id=$1 AND dst=$2`,
		a.TraceID, a.Dst,
	).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("lookup existing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit: %w", err)
	}
	return id, false, nil
}
