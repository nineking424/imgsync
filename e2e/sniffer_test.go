//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestC5Prime_SnifferSelfAudit inserts 1000 rows into the source DB, waits for
// the sniffer to enqueue them all into transfer_jobs, and asserts:
//   - enqueued == 1000
//   - distinct(trace_id) == 1000
//   - dead == 0
//
// Shadow semantics note: sniffer.config.shadow=true appends ".imgsync_shadow_v1"
// to each dst path. The worker still performs a real localfs copy to that shadow
// path, which keeps dead==0 achievable. Protocols are overridden to "localfs" so
// the worker can handle the jobs (default chart values use "fs" which is
// unregistered and would cause dead==1000).
func TestC5Prime_SnifferSelfAudit(t *testing.T) {
	if os.Getenv("IMGSYNC_E2E") != "1" {
		t.Skip("set IMGSYNC_E2E=1 to run E2E tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Use 1KB fixtures — the sniffer test measures enqueue correctness, not
	// transfer throughput, so tiny files keep disk usage under 2MB total.
	env := bootstrapKindEnvSized(t, ctx, 1024)
	defer env.teardown()

	// Upgrade helm first so the sniffer pod is already running with the correct
	// localfs protocols before any source rows exist. This prevents the old sniffer
	// pod (started by e2e-up.sh with default "fs" protocols) from racing the rows
	// during the helm upgrade's --wait period and enqueueing them with wrong protocols.
	// shadow=true means dst gets .imgsync_shadow_v1 suffix.
	// srcPattern/dstPattern use the file_path column which matches pre-seeded fixtures.
	env.helmUpgrade(t, ctx, map[string]string{
		"replicaCount":               "2",
		"sniffer.enabled":            "true",
		"sniffer.config.intervalSec": "5",
		"sniffer.config.shadow":      "true",
		"sniffer.config.srcProtocol": "localfs",
		"sniffer.config.dstProtocol": "localfs",
		"sniffer.config.srcPattern":  "/srv/imgsync/src/{{.file_path}}.bin",
		"sniffer.config.dstPattern":  "/srv/imgsync/dst/{{.file_path}}.bin",
	})
	env.waitReplicasReady(t, ctx, 2, 5*time.Minute)

	// Truncate any leftover jobs AND sniffer watermark from prior runs/the initial
	// sniffer boot. This ensures the sniffer will re-scan from the beginning.
	env.truncateJobs(t, ctx)
	env.truncateSnifferState(t, ctx)

	// Start port-forward to source-postgres on local port 5434.
	srcPool := env.openSourcePool(t, ctx)
	defer srcPool.Close()

	// Ensure the source schema exists and is clean.
	if _, err := srcPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS images (
			id         BIGSERIAL PRIMARY KEY,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			file_path  TEXT        NOT NULL
		)`); err != nil {
		t.Fatalf("create images table: %v", err)
	}
	if _, err := srcPool.Exec(ctx, "TRUNCATE images RESTART IDENTITY"); err != nil {
		t.Fatalf("truncate images: %v", err)
	}

	// Insert 1000 rows with updated_at far enough in the past to clear the
	// sniffer's bias window (default biasSec=5; we use 10s to be safe).
	// file_path matches the pre-seeded fixtures at /srv/imgsync/src/file-NNNNN.bin.
	const n = 1000
	t.Logf("Inserting %d rows into source DB...", n)
	tx, err := srcPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for i := 1; i <= n; i++ {
		filePath := fmt.Sprintf("file-%05d", i)
		if _, err := tx.Exec(ctx,
			`INSERT INTO images (updated_at, file_path) VALUES (NOW() - INTERVAL '10 seconds', $1)`,
			filePath,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Logf("Inserted %d rows into source DB", n)

	// Poll until sniffer has enqueued all 1000 rows and pending reaches zero.
	t.Log("Polling for sniffer to enqueue 1000 jobs and drain pending...")
	deadline := time.Now().Add(10 * time.Minute)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("context cancelled while waiting for sniffer to drain")
		case <-tick.C:
			enqueued, pending, dead := env.snifferCounts(t, ctx)
			t.Logf("  enqueued=%d pending=%d dead=%d", enqueued, pending, dead)
			if enqueued >= n && pending == 0 {
				t.Logf("Drain condition met: enqueued=%d pending=%d dead=%d", enqueued, pending, dead)
				goto done
			}
			if time.Now().After(deadline) {
				enqueued, pending, dead = env.snifferCounts(t, ctx)
				t.Fatalf("timeout: enqueued=%d pending=%d dead=%d after 10m", enqueued, pending, dead)
			}
		}
	}

done:
	// Final assertions.
	enqueued, _, dead := env.snifferCounts(t, ctx)
	if enqueued != n {
		t.Errorf("enqueued: got %d, want %d", enqueued, n)
	}
	if dead != 0 {
		t.Errorf("dead: got %d, want 0", dead)
	}

	distinctTraceIDs := env.countDistinctTraceIDs(t, ctx)
	if distinctTraceIDs != n {
		t.Errorf("distinct trace_ids: got %d, want %d", distinctTraceIDs, n)
	}

	t.Logf("C5' PASS: enqueued=%d distinct_trace_ids=%d dead=%d", enqueued, distinctTraceIDs, dead)
}

// openSourcePool starts a port-forward to svc/source-postgres on local port 5434
// and returns a connected pgxpool. The pool and port-forward are both cleaned up
// via t.Cleanup.
func (e *kindEnv) openSourcePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	pfCtx, pfCancel := context.WithCancel(context.Background())
	t.Cleanup(pfCancel)

	cmd := exec.CommandContext(pfCtx, "kubectl", "-n", namespace,
		"port-forward", "svc/source-postgres", "5434:5432")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("kubectl port-forward source-postgres: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Wait()
	})
	time.Sleep(2 * time.Second) // give port-forward time to establish

	dsn := "postgres://source:source@127.0.0.1:5434/source?sslmode=disable"
	deadline := time.Now().Add(30 * time.Second)
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("source-postgres port-forward never came up: %v", err)
		}
		time.Sleep(1 * time.Second)
	}
}

// snifferCounts returns (total_jobs, pending_jobs, dead_jobs) from transfer_jobs.
func (e *kindEnv) snifferCounts(t *testing.T, ctx context.Context) (enqueued, pending, dead int) {
	t.Helper()
	row := e.pool.QueryRow(ctx, `
		SELECT
		  count(*)                                    AS enqueued,
		  count(*) FILTER (WHERE status = 'pending')  AS pending,
		  count(*) FILTER (WHERE status = 'dead')     AS dead
		FROM transfer_jobs`)
	if err := row.Scan(&enqueued, &pending, &dead); err != nil {
		t.Fatalf("snifferCounts: %v", err)
	}
	return enqueued, pending, dead
}

// countDistinctTraceIDs returns the number of distinct trace_id values in transfer_jobs.
func (e *kindEnv) countDistinctTraceIDs(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	row := e.pool.QueryRow(ctx, "SELECT count(DISTINCT trace_id) FROM transfer_jobs")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countDistinctTraceIDs: %v", err)
	}
	return n
}
