// Package retention bounds unbounded growth of terminal transfer_jobs rows.
//
// transfer_jobs terminal rows (succeeded/skipped/dead) and their cascaded
// transfer_events are never deleted by the normal queue lifecycle, so they
// grow without bound. Retention is a SAFE, OPT-IN periodic batched DELETE of
// terminal-status rows older than a configurable window; their events
// cascade-delete via the existing FK ON DELETE CASCADE. It is DISABLED by
// default (Window<=0) and never touches non-terminal (pending/leased) rows.
//
// Single-writer is enforced by pg_try_advisory_xact_lock on a retention-only
// key so multiple pods cooperate without double-deleting. The lock is
// transaction-scoped: COMMIT or ROLLBACK releases it automatically.
package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config controls retention timing and batching.
type Config struct {
	Window    time.Duration // delete terminal rows older than this; <=0 disables retention
	BatchSize int           // rows deleted per DELETE statement; default 1000
	Interval  time.Duration // Run loop interval; default 1h

	// OnCycle is called after each successful Sweep cycle with the number of
	// rows deleted in that cycle. Used to emit the rows-deleted metric.
	OnCycle func(deleted int)
}

// retentionLockKey is a retention-only advisory-lock key. Advisory locks share
// one global namespace, so this must NOT collide with the sweeper, migration,
// or hostcap keys.
const retentionLockKey = "imgsync_retention"

// Sweep deletes terminal-status rows (succeeded/skipped/dead) whose updated_at
// is older than cfg.Window, in batches of cfg.BatchSize, within a single
// transaction guarded by an advisory xact lock. Returns the number of rows
// deleted. If cfg.Window <= 0 retention is disabled and Sweep deletes nothing.
// If the advisory lock cannot be acquired (another retention run is in flight),
// returns 0 with no error.
func Sweep(ctx context.Context, pool *pgxpool.Pool, cfg Config) (int, error) {
	if cfg.Window <= 0 {
		return 0, nil
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("retention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked bool
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtext($1))`, retentionLockKey,
	).Scan(&locked); err != nil {
		return 0, fmt.Errorf("retention: try advisory lock: %w", err)
	}
	if !locked {
		return 0, nil
	}

	window := fmt.Sprintf("%d seconds", int(cfg.Window.Seconds()))
	deleted := 0
	for {
		tag, err := tx.Exec(ctx, `
DELETE FROM transfer_jobs
WHERE id IN (
    SELECT id FROM transfer_jobs
    WHERE status IN ('succeeded','skipped','dead')
      AND updated_at < NOW() - $1::INTERVAL
    LIMIT $2
)`, window, cfg.BatchSize)
		if err != nil {
			return 0, fmt.Errorf("retention: delete: %w", err)
		}
		n := int(tag.RowsAffected())
		deleted += n
		if n < cfg.BatchSize {
			break
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("retention: commit: %w", err)
	}
	return deleted, nil
}

// Run loops Sweep on cfg.Interval ticks until ctx is cancelled. Each cycle gets
// its own derived timeout (2*Interval) so a wedged pgx connection cannot hold
// pg_try_advisory_xact_lock indefinitely and jam every other pod's retention.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr,
				"imgsync retention: panic in Run: %v\n%s\n",
				rec, debug.Stack())
		}
	}()
	if cfg.Interval <= 0 {
		cfg.Interval = 1 * time.Hour
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
		deleted, err := Sweep(sweepCtx, pool, cfg)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Disambiguate parent ctx cancel vs per-cycle timeout: parent
				// done → return; cycle deadline only → log and try next tick.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Fprintf(os.Stderr, "retention: cycle timeout (%s): %v\n", cycleTimeout, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "retention: cycle error: %v\n", err)
		} else if cfg.OnCycle != nil {
			cfg.OnCycle(deleted)
		}
	}
}
