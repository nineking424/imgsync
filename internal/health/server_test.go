package health_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/health"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func startServer(t *testing.T, pool *pgxpool.Pool, st *health.Status) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := health.NewServer(pool, st)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + listener.Addr().String()
}

func TestLivez_AlwaysOK(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/livez")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestReadyz_DBOK_Returns200(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestReadyz_DBDown_Returns503(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	addr := startServer(t, pool, st)
	pool.Close() // simulate DB outage

	resp, err := http.Get(addr + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 503, resp.StatusCode)
}

func TestHealthz_ReportsStatusJSON(t *testing.T) {
	pool := mustPool(t)
	st := health.NewStatus()
	st.OnLeaseAttempt(true)
	st.OnSweepCycle()
	addr := startServer(t, pool, st)

	resp, err := http.Get(addr + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var body map[string]any
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &body))

	require.Contains(t, body, "last_lease_success_ts")
	require.Contains(t, body, "last_sweep_ts")
	require.Contains(t, body, "pool_in_use")
	require.Contains(t, body, "pool_idle")
	require.Contains(t, body, "pool_max")
}
