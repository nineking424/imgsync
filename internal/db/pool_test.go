package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestNewPool_AppliesConfig(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := db.NewPool(ctx, db.PoolConfig{
		DSN:               dsn,
		MaxConns:          4,
		MinConns:          1,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.Equal(t, int32(4), pool.Config().MaxConns)
	require.Equal(t, int32(1), pool.Config().MinConns)

	var one int
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
	require.Equal(t, 1, one)
}

func TestNewPool_BadDSN_ReturnsError(t *testing.T) {
	_, err := db.NewPool(context.Background(), db.PoolConfig{
		DSN:      "postgres://nope:nope@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
		MaxConns: 1,
	})
	require.Error(t, err)
}
