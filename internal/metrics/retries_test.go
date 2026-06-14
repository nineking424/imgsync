package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestOnRetry_IncrementsRetriesTotalByStage is the issue #27 regression guard
// for the retry / error-category metric. The retry path (writeRetryOrDead) must
// be observable via imgsync_job_retries_total{src,dst,stage} so an operator can
// distinguish a transport-class retry storm ("FTP host down") from an open-class
// one ("source not found") without querying transfer_events.detail JSONB.
//
// On current code there is no OnRetry callback and no imgsync_job_retries_total
// collector, so this fails to compile / the series is absent. GREEN once the
// counter + OnRetry callback are added.
func TestOnRetry_IncrementsRetriesTotalByStage(t *testing.T) {
	m := New()

	m.OnRetry("localfs", "ftp", "transport")
	m.OnRetry("localfs", "ftp", "transport")
	m.OnRetry("ftp", "localfs", "open")

	if v := testutil.ToFloat64(m.jobRetries.WithLabelValues("localfs", "ftp", "transport")); v != 2 {
		t.Fatalf("retries{transport} = %v, want 2", v)
	}
	if v := testutil.ToFloat64(m.jobRetries.WithLabelValues("ftp", "localfs", "open")); v != 1 {
		t.Fatalf("retries{open} = %v, want 1", v)
	}
}

// TestOnRetry_DefaultsEmptyLabels mirrors OnJobFinished's empty-label handling
// so a retry with an unset stage still produces a usable series.
func TestOnRetry_DefaultsEmptyLabels(t *testing.T) {
	m := New()
	m.OnRetry("", "", "")
	if v := testutil.ToFloat64(m.jobRetries.WithLabelValues("unknown", "unknown", "unknown")); v != 1 {
		t.Fatalf("retries{unknown} = %v, want 1", v)
	}
}
