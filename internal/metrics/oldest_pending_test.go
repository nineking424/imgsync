package metrics_test

import (
	"context"
	"testing"

	"github.com/nineking424/imgsync/internal/metrics"
)

// gaugeValue reads a single-series gauge by name from the registry, failing the
// test if the series is absent.
func gaugeValue(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		mm := mf.GetMetric()
		if len(mm) == 0 {
			t.Fatalf("%s exposed but has no series", name)
		}
		return mm[0].GetGauge().GetValue()
	}
	t.Fatalf("%s not exposed", name)
	return 0
}

// TestOldestPendingAge_ReflectsDuePendingRow is the issue #27 regression guard
// for the canonical "are we keeping up" SLI. jobs_in_status{pending} cannot tell
// a healthy churning backlog from jobs stuck for an hour; imgsync_oldest_-
// pending_age_seconds is a scrape GaugeFunc over MIN(next_run_at) for due
// pending rows, mirroring lease_lock_age. A row whose next_run_at is one hour in
// the past must report an age of roughly one hour.
//
// On current code there is no AttachOldestPending method and no
// imgsync_oldest_pending_age_seconds collector, so this fails to compile / the
// series is absent. GREEN once the collector + Attach method are added.
func TestOldestPendingAge_ReflectsDuePendingRow(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	// A pending row that came due an hour ago.
	_, err := pool.Exec(ctx, `INSERT INTO transfer_jobs
		(trace_id, src, dst, src_protocol, dst_protocol, status, next_run_at)
		VALUES ('old-pending', 'src://x', 'dst://y', 'localfs', 'localfs', 'pending', NOW() - INTERVAL '1 hour')`)
	if err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	m := metrics.New()
	m.AttachOldestPending(pool)

	got := gaugeValue(t, m, "imgsync_oldest_pending_age_seconds")
	// ~3600s; allow generous slack for clock + scrape latency.
	if got < 3500 || got > 3700 {
		t.Fatalf("oldest_pending_age = %v, want ~3600 (row due 1h ago)", got)
	}
}

// TestOldestPendingAge_ZeroWhenNoDuePending asserts the gauge is 0 when there is
// no due pending work: a future-scheduled retry (next_run_at in the future) and
// a terminal row must not count. This proves the WHERE next_run_at<=NOW() filter
// is applied, not just status='pending'.
func TestOldestPendingAge_ZeroWhenNoDuePending(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	// Pending but not yet due (backoff in the future) -> must not count.
	_, err := pool.Exec(ctx, `INSERT INTO transfer_jobs
		(trace_id, src, dst, src_protocol, dst_protocol, status, next_run_at)
		VALUES ('future-pending', 'src://x', 'dst://y', 'localfs', 'localfs', 'pending', NOW() + INTERVAL '1 hour')`)
	if err != nil {
		t.Fatalf("insert future pending: %v", err)
	}
	// A succeeded terminal row -> must not count.
	_, err = pool.Exec(ctx, `INSERT INTO transfer_jobs
		(trace_id, src, dst, src_protocol, dst_protocol, status)
		VALUES ('done', 'src://x', 'dst://z', 'localfs', 'localfs', 'succeeded')`)
	if err != nil {
		t.Fatalf("insert succeeded: %v", err)
	}

	m := metrics.New()
	m.AttachOldestPending(pool)

	if got := gaugeValue(t, m, "imgsync_oldest_pending_age_seconds"); got != 0 {
		t.Fatalf("oldest_pending_age = %v, want 0 (no due pending rows)", got)
	}
}
