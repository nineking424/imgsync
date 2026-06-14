package health_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/health"
)

// liveResult drives /livez synchronously through the mux (no TCP, no DB) so the
// injected Status field is read in the same goroutine that wrote it — no race.
func liveResult(t *testing.T, st *health.Status, opts ...health.Option) int {
	t.Helper()
	srv := health.NewServer(nil, st, opts...) // nil pool: /livez must not touch the DB
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/livez", nil)
	srv.MuxForTest().ServeHTTP(w, r)
	return w.Code
}

// Behavioral demonstration of the bug on the CURRENT public API (no new symbols):
// even with an ancient LastLeaseAttemptTS, /livez returns 200, so a wedged worker
// stays Live. This compiles today and fails RED on the bug; GREEN makes it 503.
// (Mirrors TestLivez_StaleLeaseAttempt_Returns503 but without the threshold option,
// relying on the fix's sane default kicking in once an attempt is recorded.)
func TestLivez_WedgedWorker_CurrentlyStaysLive_IsTheBug(t *testing.T) {
	st := health.NewStatus()
	// An hour with no lease attempt = unambiguously wedged, far beyond any sane default.
	st.LastLeaseAttemptTS = time.Now().Add(-1 * time.Hour)

	code := liveResult(t, st)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("wedged worker (lease attempt 1h stale): livez = %d, want 503", code)
	}
}

// A worker whose lease loop wedged: LastLeaseAttemptTS is non-zero but older than
// the configured liveness threshold. /livez must 503 so kubelet restarts the pod.
func TestLivez_StaleLeaseAttempt_Returns503(t *testing.T) {
	st := health.NewStatus()
	st.LastLeaseAttemptTS = time.Now().Add(-30 * time.Second)

	code := liveResult(t, st, health.WithLivenessThreshold(10*time.Second))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("stale lease attempt: livez = %d, want 503", code)
	}
}

// A healthy worker: lease attempt recorded within the threshold. /livez stays 200.
func TestLivez_RecentLeaseAttempt_Returns200(t *testing.T) {
	st := health.NewStatus()
	st.LastLeaseAttemptTS = time.Now().Add(-1 * time.Second)

	code := liveResult(t, st, health.WithLivenessThreshold(10*time.Second))
	if code != http.StatusOK {
		t.Fatalf("recent lease attempt: livez = %d, want 200", code)
	}
}

// CRITICAL SUBTLETY: the sniffer builds health.NewStatus() and never calls
// OnLeaseAttempt, so LastLeaseAttemptTS stays the zero value. The staleness
// check must be gated on a recorded attempt — a never-set timestamp must NOT
// trip 503, or the sniffer livez would falsely fail and crashloop the pod.
func TestLivez_NeverLeased_SnifferCase_Returns200(t *testing.T) {
	st := health.NewStatus() // LastLeaseAttemptTS is the zero value

	code := liveResult(t, st, health.WithLivenessThreshold(10*time.Second))
	if code != http.StatusOK {
		t.Fatalf("never-leased (sniffer) with zero TS: livez = %d, want 200 (must not 503)", code)
	}
}
