package sourcedb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DSN            string
	MaxConns       int32
	QueryTimeoutMs int
}

type Pool struct {
	*pgxpool.Pool
	QueryTimeout time.Duration
}

func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 4
	}
	if cfg.QueryTimeoutMs == 0 {
		cfg.QueryTimeoutMs = 30000
	}

	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Pool{
		Pool:         pool,
		QueryTimeout: time.Duration(cfg.QueryTimeoutMs) * time.Millisecond,
	}, nil
}
