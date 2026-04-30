package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestApplyMigrations_FreshDB_CreatesSchema(t *testing.T) {
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

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })

	var jobsExists bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='transfer_jobs')`,
	).Scan(&jobsExists))
	require.True(t, jobsExists, "transfer_jobs table missing")

	var eventsExists bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='transfer_events')`,
	).Scan(&eventsExists))
	require.True(t, eventsExists, "transfer_events table missing")

	var uniqueIdx bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname='transfer_jobs_trace_id_dst_key')`,
	).Scan(&uniqueIdx))
	require.True(t, uniqueIdx, "UNIQUE(trace_id, dst) index missing")

	var pendingIdx bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname='transfer_jobs_pending_idx')`,
	).Scan(&pendingIdx))
	require.True(t, pendingIdx, "partial pending index missing")
}

func TestApplyMigrations_SnifferState(t *testing.T) {
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

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })

	// Assert sniffer_state table exists.
	var tableExists bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='sniffer_state')`,
	).Scan(&tableExists))
	require.True(t, tableExists, "sniffer_state table missing")

	// Assert source_id is the primary key (data_type TEXT, column present).
	var colDataType string
	require.NoError(t, conn.QueryRow(ctx, `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_name='sniffer_state' AND column_name='source_id'
	`).Scan(&colDataType))
	require.Equal(t, "text", colDataType, "source_id column should be TEXT")

	// Assert last_run_ts column exists with timestamp with time zone type.
	var tsDataType string
	require.NoError(t, conn.QueryRow(ctx, `
		SELECT data_type
		FROM information_schema.columns
		WHERE table_name='sniffer_state' AND column_name='last_run_ts'
	`).Scan(&tsDataType))
	require.Equal(t, "timestamp with time zone", tsDataType, "last_run_ts column should be TIMESTAMPTZ")

	// Assert migration version recorded.
	var migrCount int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version='0002_sniffer_state'`,
	).Scan(&migrCount))
	require.Equal(t, 1, migrCount, "0002_sniffer_state not recorded in schema_migrations")
}

func TestApplyMigrations_RunTwice_NoOp(t *testing.T) {
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

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	var n int
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n))
	require.Equal(t, 2, n, "expected exactly 2 migration versions, got duplicates or missing")
}
