// Package hostcap enforces a cluster-wide concurrent-transfer cap per FTP host
// using session-scoped pg_advisory_lock pinned to a dedicated pgx connection
// for the entire transfer. Mandated by Outside Voice F1.
package hostcap

import (
	"context"
	"errors"
	"fmt"
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

	slot, err := acquireSlot(ctx, pgConn.Conn(), host, c.cfg.Cap, c.cfg.AcquireBackoff)
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

func acquireSlot(ctx context.Context, conn *pgx.Conn, host string, cap int, backoff time.Duration) (int, error) {
	for {
		for slot := 0; slot < cap; slot++ {
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
		case <-time.After(backoff):
		}
	}
}
