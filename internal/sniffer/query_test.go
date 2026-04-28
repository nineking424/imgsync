package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func setupSourceDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:16-alpine",
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
		CREATE TABLE images (
		  id BIGINT PRIMARY KEY,
		  updated_at TIMESTAMPTZ NOT NULL,
		  file_path TEXT NOT NULL
		)`)
	require.NoError(t, err)

	return pool
}

func TestQuery_WindowAdvancesAcrossSameTS(t *testing.T) {
	ctx := context.Background()
	pool := setupSourceDB(t)
	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		_, _ = pool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, ts, "p")
	}

	q := sniffer.Query{
		Table:        "images",
		PKColumn:     "id",
		TSColumn:     "updated_at",
		BatchSize:    2,
		BiasDuration: 0,
	}
	from := sniffer.State{LastRunTS: ts.Add(-time.Hour), LastRunPK: ""}

	rows, err := q.Fetch(ctx, pool, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("first batch len=%d", len(rows))
	}
	if rows[0].PK != "1" || rows[1].PK != "2" {
		t.Fatalf("first batch pks: %v", rows)
	}

	from = sniffer.State{LastRunTS: rows[1].TS, LastRunPK: rows[1].PK}
	rows2, _ := q.Fetch(ctx, pool, from)
	if len(rows2) != 2 || rows2[0].PK != "3" || rows2[1].PK != "4" {
		t.Fatalf("second batch: %v", rows2)
	}

	from = sniffer.State{LastRunTS: rows2[1].TS, LastRunPK: rows2[1].PK}
	rows3, _ := q.Fetch(ctx, pool, from)
	if len(rows3) != 1 || rows3[0].PK != "5" {
		t.Fatalf("third batch: %v", rows3)
	}
}

func TestQuery_ExtraColumnsRenderNullAsEmpty(t *testing.T) {
	ctx := context.Background()
	pool := setupSourceDB(t)

	_, err := pool.Exec(ctx, `ALTER TABLE images ADD COLUMN category TEXT`)
	require.NoError(t, err)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `INSERT INTO images (id, updated_at, file_path, category) VALUES (1, $1, 'p', NULL)`, ts)
	require.NoError(t, err)

	q := sniffer.Query{
		Table:        "images",
		PKColumn:     "id",
		TSColumn:     "updated_at",
		ExtraColumns: []string{"file_path", "category"},
		BatchSize:    10,
	}
	rows, err := q.Fetch(ctx, pool, sniffer.State{LastRunTS: ts.Add(-time.Hour)})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "p", rows[0].Fields["file_path"])
	require.Equal(t, "", rows[0].Fields["category"], "NULL column must render as empty string")
}

func TestQuery_BatchSizeZeroErrors(t *testing.T) {
	q := sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", BatchSize: 0}
	_, err := q.Fetch(context.Background(), nil, sniffer.State{})
	require.Error(t, err)
}

func TestQuery_BiasExcludesRecentRows(t *testing.T) {
	ctx := context.Background()
	pool := setupSourceDB(t)
	now := time.Now().UTC()
	_, _ = pool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p')`,
		now.Add(-2*time.Second))

	q := sniffer.Query{
		Table: "images", PKColumn: "id", TSColumn: "updated_at",
		BatchSize: 10, BiasDuration: 5 * time.Second,
	}
	rows, _ := q.Fetch(ctx, pool, sniffer.State{LastRunTS: time.Unix(0, 0).UTC()})
	if len(rows) != 0 {
		t.Fatalf("bias should exclude row newer than NOW()-bias, got %d", len(rows))
	}
}
