//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

// Throughput methodology: each phase's wall-clock starts when waitAllSucceeded
// is called (right after enqueueLocalFSJobs returns) and ends when the
// "succeeded" count reaches the expected total. The 1s polling tick adds ±1s
// jitter symmetrically to both phases, so the 8/2 ratio assertion is robust
// to that noise. truncateJobs wipes the dst directory between phases so both
// phases pay the same per-job filesystem allocation cost (see fix in
// helpers.go truncateJobs).
func TestC7_ThroughputScaleOut(t *testing.T) {
	if os.Getenv("IMGSYNC_E2E") != "1" {
		t.Skip("set IMGSYNC_E2E=1 to run E2E tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	env := bootstrapKindEnv(t, ctx)
	defer env.teardown()

	// ─── Phase A: 2 replicas, 1000 jobs ────────────────────────────────────
	t.Log("==> Phase A: replicas=2")
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "2"})
	env.waitReplicasReady(t, ctx, 2, 5*time.Minute)

	jobsA := 1000
	env.truncateJobs(t, ctx)
	env.enqueueLocalFSJobs(t, ctx, "phaseA-", jobsA)
	durA := env.waitAllSucceeded(t, ctx, jobsA, 15*time.Minute)
	tputA := float64(jobsA) / durA.Seconds()
	t.Logf("Phase A: %d jobs in %v → %.2f jobs/sec", jobsA, durA, tputA)

	// ─── Phase B: scale to 8 replicas, 1000 fresh jobs ─────────────────────
	t.Log("==> Phase B: replicas=8")
	scaleStart := time.Now()
	env.helmUpgrade(t, ctx, map[string]string{"replicaCount": "8"})
	env.waitReplicasReady(t, ctx, 8, 5*time.Minute)
	scaleLatency := time.Since(scaleStart)
	t.Logf("Scale 2→8 ready in %v", scaleLatency)
	if scaleLatency > 5*time.Minute {
		t.Errorf("scale-out exceeded 5min budget: %v", scaleLatency)
	}

	jobsB := 1000
	env.truncateJobs(t, ctx)
	env.enqueueLocalFSJobs(t, ctx, "phaseB-", jobsB)
	durB := env.waitAllSucceeded(t, ctx, jobsB, 15*time.Minute)
	tputB := float64(jobsB) / durB.Seconds()
	t.Logf("Phase B: %d jobs in %v → %.2f jobs/sec", jobsB, durB, tputB)

	// ─── Assertion ─────────────────────────────────────────────────────────
	ratio := tputB / tputA
	t.Logf("Throughput ratio (8/2) = %.2f", ratio)
	const minRatio = 3.2
	if ratio < minRatio {
		t.Fatalf("throughput linearity FAIL: ratio %.2f < %.2f (tputA=%.2f, tputB=%.2f)",
			ratio, minRatio, tputA, tputB)
	}

	// Sanity: nothing dead
	dead := env.countByStatus(t, ctx, "dead")
	if dead > 0 {
		t.Fatalf("phase B left %d jobs in 'dead' state — unexpected", dead)
	}
}
