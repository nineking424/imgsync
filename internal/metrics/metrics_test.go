package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNew_ExposesMetricsHandlerWithIsolatedRegistry(t *testing.T) {
	m := New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)

	m.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200 got %d", rec.Code)
	}
	body := rec.Body.String()
	// 빈 registry 라도 promhttp 가 빈 본문 + 200 을 반환한다.
	// global default registry 와 달리 go_* / process_* 가 절대 안 보여야 한다.
	if strings.Contains(body, "go_goroutines") || strings.Contains(body, "process_cpu_seconds_total") {
		t.Fatalf("isolated registry leaked global metrics: %s", body)
	}
}

func TestOnJobFinished_IncrementsCounterAndObservesHistogram(t *testing.T) {
	m := New()
	m.OnJobFinished("ftp", "localfs", "success", 1500*time.Millisecond)

	got := testutil.ToFloat64(m.jobsProcessed.WithLabelValues("ftp", "localfs", "success"))
	if got != 1 {
		t.Fatalf("jobs_processed = %v, want 1", got)
	}
	count := testutil.CollectAndCount(m.jobDuration, "imgsync_job_duration_seconds")
	if count != 1 {
		t.Fatalf("job_duration series = %d, want 1", count)
	}
}

func TestOnLeaseAttempt_LabelsResult(t *testing.T) {
	m := New()
	m.OnLeaseAttempt(true, nil)
	m.OnLeaseAttempt(false, nil)
	m.OnLeaseAttempt(false, errors.New("boom"))

	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("success")); v != 1 {
		t.Fatalf("success = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("empty")); v != 1 {
		t.Fatalf("empty = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("error")); v != 1 {
		t.Fatalf("error = %v, want 1", v)
	}
}

func TestOnFTPPoolChange_GaugeReflectsInUseAndIdle(t *testing.T) {
	m := New()
	m.OnFTPPoolChange("ftp.example.com:21", 3, 1)

	if v := testutil.ToFloat64(m.ftpPoolSize.WithLabelValues("ftp.example.com:21", "in_use")); v != 3 {
		t.Fatalf("in_use = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.ftpPoolSize.WithLabelValues("ftp.example.com:21", "idle")); v != 1 {
		t.Fatalf("idle = %v, want 1", v)
	}
}

func TestOnSweepCycle_IncrementsCounter(t *testing.T) {
	m := New()
	m.OnSweepCycle()
	m.OnSweepCycle()
	if v := testutil.ToFloat64(m.sweepCycles); v != 2 {
		t.Fatalf("sweep_cycles = %v, want 2", v)
	}
}

func TestOnSnifferEnqueue_AddsN(t *testing.T) {
	m := New()
	m.OnSnifferEnqueue("orders", 7)
	m.OnSnifferEnqueue("orders", 3)
	if v := testutil.ToFloat64(m.snifferEnq.WithLabelValues("orders")); v != 10 {
		t.Fatalf("enqueue = %v, want 10", v)
	}
}

func TestOnSnifferError_IncrementsCounter(t *testing.T) {
	m := New()
	m.OnSnifferError("orders")
	if v := testutil.ToFloat64(m.snifferErr.WithLabelValues("orders")); v != 1 {
		t.Fatalf("err = %v, want 1", v)
	}
}
