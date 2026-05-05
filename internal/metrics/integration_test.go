//go:build integration

package metrics_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/metrics"
)

func setupPG(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
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
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}
	if err := db.ApplyMigrations(ctx, dsn, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return pool, func() {
		pool.Close()
		_ = testcontainers.TerminateContainer(pg)
	}
}

func TestQueueDepthCollector_ReflectsPerStatusCount(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	seq := 0
	insert := func(status string) {
		t.Helper()
		seq++
		_, err := pool.Exec(ctx, `INSERT INTO transfer_jobs
		   (trace_id, src, dst, src_protocol, dst_protocol, status)
		   VALUES ($1, 'src://x', $2, 'localfs', 'localfs', $3)`,
			fmt.Sprintf("%s-trace-%d", status, seq),
			fmt.Sprintf("dst://y/%d", seq),
			status)
		if err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}
	for i := 0; i < 3; i++ {
		insert("pending")
	}
	insert("succeeded")

	m := metrics.New()
	m.AttachQueueDepth(pool)

	want := strings.NewReader(`
# HELP imgsync_jobs_in_status Number of transfer_jobs rows per status.
# TYPE imgsync_jobs_in_status gauge
imgsync_jobs_in_status{status="pending"} 3
imgsync_jobs_in_status{status="succeeded"} 1
`)
	if err := testutil.GatherAndCompare(m.RegistryForTest(), want,
		"imgsync_jobs_in_status"); err != nil {
		t.Fatalf("gather: %v", err)
	}
}

func TestLeaseLockAge_IsZeroWhenNoLeasedRows(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	m := metrics.New()
	m.AttachLeaseLockAge(pool)

	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "imgsync_lease_lock_age_seconds" {
			found = true
			if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 0 {
				t.Fatalf("lease_lock_age = %v, want 0 with no leased rows", got)
			}
		}
	}
	if !found {
		t.Fatalf("lease_lock_age_seconds not exposed")
	}
	_ = prometheus.NewRegistry // keep import live in case future tests need it
}

func TestDBPoolCollector_ExposesInUseIdleMax(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	m := metrics.New()
	m.AttachDBPool(pool)

	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	states := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "imgsync_db_pool_conns" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "state" {
					states[l.GetValue()] = true
				}
			}
		}
	}
	for _, s := range []string{"in_use", "idle", "max"} {
		if !states[s] {
			t.Fatalf("state %q missing from imgsync_db_pool_conns", s)
		}
	}
}
