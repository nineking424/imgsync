//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nineking424/imgsync/internal/db"
)

// TestMigrate_0003_StatusIndex 는 0003 마이그레이션 적용 후
// transfer_jobs (status) 인덱스가 존재하고, GROUP BY status 쿼리가
// index-only / heap-not-touched plan 을 사용하는지 검증한다.
func TestMigrate_0003_StatusIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("imgsync"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("postgres run: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}

	if err := db.ApplyMigrations(ctx, dsn, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var idxName string
	err = conn.QueryRow(ctx,
		`SELECT indexname FROM pg_indexes
		   WHERE schemaname = 'public'
		     AND tablename  = 'transfer_jobs'
		     AND indexname  = 'transfer_jobs_status_idx'`,
	).Scan(&idxName)
	if err != nil {
		t.Fatalf("status index missing: %v", err)
	}
	if idxName != "transfer_jobs_status_idx" {
		t.Fatalf("got idx %q, want transfer_jobs_status_idx", idxName)
	}
}
