//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestF5_DirtyStateRecovery(t *testing.T) {
	if os.Getenv("IMGSYNC_E2E") != "1" {
		t.Skip("set IMGSYNC_E2E=1 to run E2E tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	env := bootstrapKindEnv(t, ctx)
	defer env.teardown()

	// Use a small replica count and tighter sweeper interval indirectly via
	// the default 30s loop / 5min threshold. We'll force the sweeper threshold
	// down to something testable by editing locked_at directly (cluster-level
	// equivalent of C2's setup).
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
	env.waitReplicasReady(t, ctx, 2, 3*time.Minute)

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5a_mid_flight_kill", func(t *testing.T) {
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5a-", 100)

		// Wait until a worker leases at least one job
		env.waitForLeasedJob(t, ctx, 30*time.Second)

		// Snapshot leased row IDs BEFORE the kill. Two races make
		// "filter by killed pod's locked_by" unreliable: (a) killOnePod picks
		// pods[0] which may not own any leased row, and (b) an in-flight job
		// can flip to 'succeeded' between waitForLeasedJob and the UPDATE
		// (kubectl force-delete adds 1-2s; LocalFS Send is sub-second). Capturing
		// IDs upfront lets the UPDATE target them by id, idempotent and race-free.
		rows, err := env.pool.Query(ctx, `SELECT id FROM transfer_jobs WHERE status='leased'`)
		if err != nil {
			t.Fatalf("snapshot leased ids: %v", err)
		}
		var leasedIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("scan leased id: %v", err)
			}
			leasedIDs = append(leasedIDs, id)
		}
		rows.Close()
		if len(leasedIDs) == 0 {
			t.Fatal("F5a: snapshot found 0 leased rows but waitForLeasedJob just returned; tighten the race")
		}
		t.Logf("snapshotted %d leased rows before kill", len(leasedIDs))

		// Kill one pod hard
		killed := env.killOnePod(t, ctx)
		t.Logf("killed pod %s mid-flight", killed)

		// Fast-forward locked_at on the snapshotted rows that are still leased.
		// Rows that drained on surviving pods between snapshot and UPDATE are no-op
		// skipped by the status='leased' filter. Sweeper threshold is 5m default.
		// Note: status enum has no 'processing'; use 'leased' (sweeper sets back to 'pending').
		_, err = env.pool.Exec(ctx, `
UPDATE transfer_jobs
   SET locked_at = NOW() - INTERVAL '6 minutes'
 WHERE id = ANY($1) AND status='leased'
`, leasedIDs)
		if err != nil {
			t.Fatalf("force expire: %v", err)
		}

		// Wait for full drain
		env.waitAllSucceeded(t, ctx, 100, 5*time.Minute)

		// Assert sweeper-recovered jobs ended with attempts==0 and have an 'expire' event
		recovered := env.inspectSweeperRecovery(t, ctx)
		if recovered == 0 {
			t.Fatalf("expected at least 1 sweeper-recovered job with attempts==0; got 0")
		}
		t.Logf("sweeper-recovered jobs: %d", recovered)

		// And: zero dead, zero stuck leased
		if d := env.countByStatus(t, ctx, "dead"); d != 0 {
			t.Errorf("expected 0 dead, got %d", d)
		}
		// Note: status enum has no 'processing'; use 'leased' (sweeper sets back to 'pending').
		if p := env.countByStatus(t, ctx, "leased"); p != 0 {
			t.Errorf("expected 0 leased, got %d", p)
		}
	})

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5b_bad_upgrade_then_rollback", func(t *testing.T) {
		// Snapshot DB state before bad upgrade
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5b-", 50)

		// Wait until at least 10 jobs succeed under the good config
		startCheck := time.Now()
		for env.countByStatus(t, ctx, "succeeded") < 10 {
			if time.Since(startCheck) > 2*time.Minute {
				t.Fatal("warm-up: 10 succeeded never reached")
			}
			time.Sleep(500 * time.Millisecond)
		}
		preBad := env.countByStatus(t, ctx, "succeeded")
		t.Logf("pre-bad-upgrade succeeded count: %d", preBad)

		// Push a bad upgrade (image tag does not exist → ImagePullBackOff)
		err := runCmd(ctx, repoRoot(t), "helm",
			"-n", namespace, "upgrade", releaseName, chartPath,
			"--set", "image.repository=imgsync",
			"--set", "image.tag=does-not-exist",
			"--set", "image.pullPolicy=IfNotPresent",
			"--set", "replicaCount=2",
			// Bad upgrade will hang waiting for ready; we cap and don't block.
			"--timeout", "30s",
			"--wait")
		if err == nil {
			t.Log("bad upgrade unexpectedly succeeded (kind may have cached the tag); continuing")
		} else {
			t.Logf("bad upgrade failed as expected: %v", err)
		}

		// Verify pods are crashlooping or not running
		// (We don't assert this strictly — only that rollback recovers.)

		// Rollback
		env.helmRollback(t, ctx)
		env.waitReplicasReady(t, ctx, 2, 2*time.Minute)

		// All 50 jobs should drain
		env.waitAllSucceeded(t, ctx, 50, 5*time.Minute)

		// No dead, no stuck leased
		if d := env.countByStatus(t, ctx, "dead"); d != 0 {
			t.Errorf("F5b: expected 0 dead after rollback, got %d", d)
		}
		// Note: status enum has no 'processing'; use 'leased' (sweeper sets back to 'pending').
		if p := env.countByStatus(t, ctx, "leased"); p != 0 {
			t.Errorf("F5b: expected 0 leased after rollback, got %d", p)
		}
	})

	// ─────────────────────────────────────────────────────────────────────
	t.Run("F5c_uninstall_reinstall_idempotent_migration", func(t *testing.T) {
		// Enqueue 30 jobs but stop the workers before they finish
		env.truncateJobs(t, ctx)
		env.enqueueLocalFSJobs(t, ctx, "f5c-", 30)

		// Uninstall (this also deletes the migrate Job since hook-succeeded reaped it)
		env.helmUninstall(t, ctx)

		// helm uninstall does NOT touch the DB. The invariant is "all 30 rows
		// survive uninstall" — not "all 30 are still pending". With 16 workers
		// (8 goroutines × 2 replicas) some rows will have leased or succeeded
		// before SIGTERM landed, so assert the sum across pending/leased/succeeded.
		var pending, leased, succeeded int
		err := env.pool.QueryRow(ctx, `
SELECT
  count(*) FILTER (WHERE status='pending'),
  count(*) FILTER (WHERE status='leased'),
  count(*) FILTER (WHERE status='succeeded')
FROM transfer_jobs
`).Scan(&pending, &leased, &succeeded)
		if err != nil {
			t.Fatalf("count by status: %v", err)
		}
		if total := pending + leased + succeeded; total != 30 {
			t.Fatalf("F5c: expected 30 jobs to survive uninstall, got pending=%d leased=%d succeeded=%d (total=%d)",
				pending, leased, succeeded, total)
		}
		t.Logf("F5c post-uninstall: pending=%d leased=%d succeeded=%d (DB survived)", pending, leased, succeeded)

		// Fast-forward any rows the uninstalled workers left in 'leased' so the
		// new install's sweeper recovers them within one tick (30s) instead of
		// the 5-min default threshold. Without this, waitAllSucceeded races
		// against the threshold within its own 5-min budget.
		if _, err := env.pool.Exec(ctx, `
UPDATE transfer_jobs
   SET locked_at = NOW() - INTERVAL '6 minutes'
 WHERE status='leased'
`); err != nil {
			t.Fatalf("fast-forward orphaned leased rows: %v", err)
		}

		// Reinstall — pre-install hook re-runs migrate up (must be idempotent)
		env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
		env.waitReplicasReady(t, ctx, 2, 3*time.Minute)

		// Workers drain the existing 30 jobs
		env.waitAllSucceeded(t, ctx, 30, 5*time.Minute)
	})
}
