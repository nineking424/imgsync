// Package sweeper recovers expired leases. Single-writer enforced by
// pg_try_advisory_xact_lock so multiple pods can run a sweeper without
// duplicating expire events. The lock is transaction-scoped: COMMIT or
// ROLLBACK releases it automatically, so no cleanup is needed on error paths.
package sweeper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config controls sweeper timing.
type Config struct {
	Threshold time.Duration // lease age beyond which to recover; default 5m
	Interval  time.Duration // loop interval; default 30s
}

const sweeperLockKey = "imgsync_sweeper"

// Sweep runs one sweeper cycle in a single transaction. Returns the number
// of rows recovered. If the advisory lock cannot be acquired (another sweeper
// is in flight), returns 0 with no error.
func Sweep(ctx context.Context, pool *pgxpool.Pool, cfg Config) (int, error) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5 * time.Minute
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweeper: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked bool
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtext($1))`, sweeperLockKey,
	).Scan(&locked); err != nil {
		return 0, fmt.Errorf("sweeper: try advisory lock: %w", err)
	}
	if !locked {
		return 0, nil
	}

	threshold := fmt.Sprintf("%d seconds", int(cfg.Threshold.Seconds()))
	rows, err := tx.Query(ctx, `
UPDATE transfer_jobs
SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE status='leased' AND locked_at < NOW() - $1::INTERVAL
RETURNING id, trace_id`, threshold)
	if err != nil {
		return 0, fmt.Errorf("sweeper: update: %w", err)
	}
	type recovered struct {
		id      int64
		traceID string
	}
	var recoveredRows []recovered
	for rows.Next() {
		var r recovered
		if err := rows.Scan(&r.id, &r.traceID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sweeper: scan: %w", err)
		}
		recoveredRows = append(recoveredRows, r)
	}
	rows.Close()

	for _, r := range recoveredRows {
		if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail)
VALUES ($1,$2,'expire','{"reason":"lease_expired"}'::JSONB)`,
			r.traceID, r.id,
		); err != nil {
			return 0, fmt.Errorf("sweeper: insert event for %d: %w", r.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sweeper: commit: %w", err)
	}
	return len(recoveredRows), nil
}

// Run loops Sweep on cfg.Interval ticks until ctx is cancelled.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if _, err := Sweep(ctx, pool, cfg); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			fmt.Fprintf(os.Stderr, "sweeper: cycle error: %v\n", err)
		}
	}
}
