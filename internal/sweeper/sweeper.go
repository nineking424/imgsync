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

	// OnCycle is called after each successful Sweep cycle, regardless of how
	// many rows were recovered. Used by /healthz to report last_sweep_ts.
	OnCycle func()
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
	// rows.Next() returning false is ambiguous (EOF vs mid-stream error).
	// Without rows.Err() a network/pgx failure would silently commit a
	// partial recovery. Mirrors the pattern in internal/db/migrate.go.
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sweeper: rows iteration: %w", err)
	}

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

// Run loops Sweep on cfg.Interval ticks until ctx is cancelled. Each cycle
// gets its own derived timeout (2*Interval) so a wedged pgx connection
// cannot hold pg_try_advisory_xact_lock indefinitely and jam every other
// pod's sweeper.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	cycleTimeout := 2 * cfg.Interval
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		sweepCtx, cancel := context.WithTimeout(ctx, cycleTimeout)
		_, err := Sweep(sweepCtx, pool, cfg)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Disambiguate parent ctx cancel vs per-cycle timeout: parent
				// done → return; cycle deadline only → log and try next tick.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Fprintf(os.Stderr, "sweeper: cycle timeout (%s): %v\n", cycleTimeout, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "sweeper: cycle error: %v\n", err)
		} else if cfg.OnCycle != nil {
			cfg.OnCycle()
		}
	}
}
