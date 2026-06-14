// Package hostcap enforces a cluster-wide concurrent-transfer cap per FTP host
// using session-scoped pg_advisory_lock pinned to a dedicated pgx connection
// for the entire transfer. Mandated by Outside Voice F1.
package hostcap

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

type Config struct {
	Cap            int
	Host           string
	AcquireBackoff time.Duration
}

func Wrap(pool *pgxpool.Pool, inner transfer.Transport, cfg Config) *CapTransport {
	if cfg.Cap <= 0 {
		cfg.Cap = 8
	}
	if cfg.AcquireBackoff <= 0 {
		cfg.AcquireBackoff = 100 * time.Millisecond
	}
	return &CapTransport{pool: pool, inner: inner, cfg: cfg}
}

type CapTransport struct {
	pool  *pgxpool.Pool
	inner transfer.Transport
	cfg   Config
}

func (c *CapTransport) Send(ctx context.Context, dst string, body io.Reader, size int64) (int64, string, error) {
	host := c.cfg.Host
	if host == "" {
		u, err := url.Parse(dst)
		if err == nil {
			host = u.Host
		}
	}
	if host == "" {
		return 0, "", errors.New("hostcap: cannot derive host from dst")
	}

	pgConn, err := c.pool.Acquire(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("hostcap: acquire dedicated conn: %w", err)
	}
	defer pgConn.Release()

	slot, err := acquireSlot(ctx, pgConn.Conn(), host, dst, c.cfg.Cap, c.cfg.AcquireBackoff)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_, _ = pgConn.Conn().Exec(context.Background(),
			`SELECT pg_advisory_unlock(hashtext($1))`, slotKey(host, slot))
	}()

	return c.inner.Send(ctx, dst, body, size)
}

func slotKey(host string, slot int) string {
	return fmt.Sprintf("ftp_host_%s_%d", host, slot)
}

// slotHash derives a deterministic 0..cap-1 starting slot from the per-host
// destination so distinct dsts spread across slots instead of every acquire
// starting its scan at slot 0 (issue #33). The hash also seeds backoff jitter so
// no wall-clock time is involved.
func slotHash(host, dst string, cap int) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(dst))
	return h.Sum32() % uint32(cap)
}

// acquireSlot acquires one of cap per-host advisory-lock slots. It starts probing
// at a hash-derived offset (slotHash) and wraps around all cap slots, so a single
// uncontended acquire is one round-trip while preserving the EXACT cap: under
// contention it still scans every slot before backing off, and identical dsts
// still fan out into distinct slots via the wrap-around fallback. On full
// contention it waits with exponential backoff plus hash-seeded jitter (never
// wall-clock-derived) until a slot frees or ctx cancels.
func acquireSlot(ctx context.Context, conn *pgx.Conn, host, dst string, cap int, backoff time.Duration) (int, error) {
	start := int(slotHash(host, dst, cap))
	// seed is a deterministic per-(host,dst) value used to perturb the backoff so
	// concurrent waiters on the same dst desynchronize without reading the clock.
	seed := slotHash(host, dst+"#jitter", cap*7+13)
	wait := backoff
	for {
		for i := 0; i < cap; i++ {
			slot := (start + i) % cap
			var got bool
			if err := conn.QueryRow(ctx,
				`SELECT pg_try_advisory_lock(hashtext($1))`, slotKey(host, slot),
			).Scan(&got); err != nil {
				return 0, fmt.Errorf("hostcap: try lock: %w", err)
			}
			if got {
				return slot, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(jittered(wait, seed)):
		}
		wait = nextBackoff(wait, backoff)
		seed = seed*1664525 + 1013904223 // LCG step: evolve jitter deterministically
	}
}

// maxBackoff caps exponential growth so a long-blocked acquire still re-probes
// at a bounded interval.
const maxBackoff = 2 * time.Second

// nextBackoff doubles the current wait up to maxBackoff. The base floor guards
// against a zero/negative configured backoff degenerating the schedule.
func nextBackoff(cur, base time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if cur < base {
		cur = base
	}
	next := cur * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return next
}

// jittered perturbs d by ±25% using seed (a hash-derived value, never the wall
// clock) so concurrent waiters spread their retries.
func jittered(d time.Duration, seed uint32) time.Duration {
	if d <= 0 {
		return d
	}
	// frac in [0,1): fraction of the ±25% span selected by the seed.
	frac := float64(seed%1000) / 1000.0
	delta := (frac*2 - 1) * 0.25 * float64(d) // [-25%, +25%)
	return d + time.Duration(delta)
}
