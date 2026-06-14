package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestOnSnifferRun_SetsLastRunTimestamp is the issue #27 regression guard for
// sniffer freshness. A single-pod sniffer that polls but falls behind looks
// healthy under enqueue_total/run_errors_total alone; imgsync_sniffer_last_run_-
// timestamp{source} records the wall-clock time of the last successful RunOnce
// so an alert can fire when it stops advancing.
//
// On current code there is no OnSnifferRun callback and no
// imgsync_sniffer_last_run_timestamp gauge, so this fails to compile / the
// series is absent. GREEN once the gauge + callback are added.
func TestOnSnifferRun_SetsLastRunTimestamp(t *testing.T) {
	m := New()

	before := float64(time.Now().Unix())
	m.OnSnifferRun("orders")
	after := float64(time.Now().Unix()) + 1

	got := testutil.ToFloat64(m.snifferLastRun.WithLabelValues("orders"))
	if got < before || got > after {
		t.Fatalf("sniffer_last_run_timestamp = %v, want within [%v, %v]", got, before, after)
	}
}

// TestSnifferWatermarkLag_ExposedAndNonNegative asserts the lag gauge is
// registered and tracks NOW()-last_run_ts for a source after a run. Immediately
// after OnSnifferRun the lag must be ~0 (never negative). Absent on current
// code.
func TestSnifferWatermarkLag_ExposedAndNonNegative(t *testing.T) {
	m := New()
	m.OnSnifferRun("orders")

	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() != "imgsync_sniffer_watermark_lag_seconds" {
			continue
		}
		found = true
		for _, mm := range mf.GetMetric() {
			if v := mm.GetGauge().GetValue(); v < 0 {
				t.Fatalf("watermark_lag = %v, must not be negative", v)
			}
		}
	}
	if !found {
		t.Fatalf("imgsync_sniffer_watermark_lag_seconds not exposed")
	}
}
