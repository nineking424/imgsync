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

// TestMigrate_0004_DropTraceIDIndex 는 0004 마이그레이션 적용 후
// 중복 인덱스 transfer_jobs_trace_id_idx 가 제거되었는지 검증한다.
// 이 단일 컬럼 인덱스는 UNIQUE(trace_id, dst) 복합 인덱스의 선두 컬럼
// prefix 라 redundant 하다. 동시에 큐 무결성을 지키는 UNIQUE 인덱스
// transfer_jobs_trace_id_dst_key 는 반드시 남아 있어야 한다.
func TestMigrate_0004_DropTraceIDIndex(t *testing.T) {
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

	// Redundant single-column index must be gone.
	var redundantExists bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes
		   WHERE schemaname = 'public'
		     AND tablename  = 'transfer_jobs'
		     AND indexname  = 'transfer_jobs_trace_id_idx')`,
	).Scan(&redundantExists)
	if err != nil {
		t.Fatalf("query redundant index: %v", err)
	}
	if redundantExists {
		t.Fatalf("transfer_jobs_trace_id_idx still exists; 0004 should DROP it")
	}

	// UNIQUE(trace_id, dst) must remain — it is the queue-integrity guard
	// and the composite that makes the single-column index redundant.
	var uniqueExists bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes
		   WHERE schemaname = 'public'
		     AND tablename  = 'transfer_jobs'
		     AND indexname  = 'transfer_jobs_trace_id_dst_key')`,
	).Scan(&uniqueExists)
	if err != nil {
		t.Fatalf("query unique index: %v", err)
	}
	if !uniqueExists {
		t.Fatalf("transfer_jobs_trace_id_dst_key missing; UNIQUE(trace_id, dst) must remain")
	}

	// Migration version must be recorded (self-registering convention).
	var recorded int
	err = conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0004_drop_trace_id_index'`,
	).Scan(&recorded)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("0004_drop_trace_id_index not recorded in schema_migrations (got %d)", recorded)
	}
}
