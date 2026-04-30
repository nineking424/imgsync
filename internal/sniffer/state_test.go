package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupImgsyncDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		CREATE TABLE sniffer_state (
		  source_id   TEXT PRIMARY KEY,
		  last_run_ts TIMESTAMPTZ NOT NULL,
		  last_run_pk TEXT,
		  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	require.NoError(t, err)

	return pool
}

// setupImgsyncDBWithTransferJobs spins up a Postgres container and applies both
// migrations (0001 transfer_jobs, 0002 sniffer_state) so tests that need both
// schemas can use a single pool.
func setupImgsyncDBWithTransferJobs(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func TestStateRepo_LoadMissingReturnsZeroValue(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)

	st, err := repo.Load(context.Background(), "main-source-db.images")
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastRunTS.IsZero() {
		t.Fatalf("expected zero ts, got %v", st.LastRunTS)
	}
	if st.LastRunPK != "" {
		t.Fatalf("expected empty pk, got %q", st.LastRunPK)
	}
}

func TestStateRepo_UpsertThenLoad(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)
	ctx := context.Background()

	want := sniffer.State{
		SourceID:  "main-source-db.images",
		LastRunTS: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
		LastRunPK: "100",
	}
	if err := repo.Upsert(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Load(ctx, want.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastRunTS.Equal(want.LastRunTS) {
		t.Fatalf("ts: got %v want %v", got.LastRunTS, want.LastRunTS)
	}
	if got.LastRunPK != want.LastRunPK {
		t.Fatalf("pk: got %q want %q", got.LastRunPK, want.LastRunPK)
	}
}

func TestStateRepo_UpsertOverwritesExisting(t *testing.T) {
	pool := setupImgsyncDB(t)
	repo := sniffer.NewStateRepo(pool)
	ctx := context.Background()

	first := sniffer.State{
		SourceID: "src", LastRunTS: time.Unix(1000, 0).UTC(), LastRunPK: "1",
	}
	second := sniffer.State{
		SourceID: "src", LastRunTS: time.Unix(2000, 0).UTC(), LastRunPK: "2",
	}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Load(ctx, "src")
	if got.LastRunPK != "2" {
		t.Fatalf("got %q", got.LastRunPK)
	}
}
