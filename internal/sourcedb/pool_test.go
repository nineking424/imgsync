package sourcedb_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sourcedb"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestNewPool_Connects(t *testing.T) {
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := sourcedb.NewPool(ctx, sourcedb.Config{
		DSN:            dsn,
		MaxConns:       4,
		QueryTimeoutMs: 30000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	require.Equal(t, 30*time.Second, pool.QueryTimeout)

	var one int
	require.NoError(t, pool.QueryRow(ctx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
}

func TestNewPool_BadDSN(t *testing.T) {
	_, err := sourcedb.NewPool(context.Background(), sourcedb.Config{
		DSN: "postgres://nope:nope@127.0.0.1:1/none",
	})
	require.Error(t, err)
}
