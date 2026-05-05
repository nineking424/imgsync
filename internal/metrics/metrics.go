// Package metrics owns the Prometheus collectors imgsync exposes via /metrics.
// One Metrics instance per process. Domain packages (worker/sweeper/sniffer/
// ftp pool) call methods on Metrics via the existing OnXxx callback pattern,
// so they do not import this package.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every imgsync_* collector. Each instance has its own
// prometheus.Registry so tests can run in parallel.
type Metrics struct {
	reg *prometheus.Registry

	leaseAttempts *prometheus.CounterVec
	jobsProcessed *prometheus.CounterVec
	jobDuration   *prometheus.HistogramVec
	sweepCycles   prometheus.Counter
	ftpPoolSize   *prometheus.GaugeVec
	snifferEnq    *prometheus.CounterVec
	snifferErr    *prometheus.CounterVec
	workersActive *prometheus.GaugeVec
}

// New constructs a Metrics with a fresh registry and registers all collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		leaseAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_lease_attempts_total",
			Help: "Number of LeaseJob attempts, labeled by outcome.",
		}, []string{"result"}),
		jobsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_jobs_processed_total",
			Help: "Number of jobs the worker has finished, labeled by terminal status.",
		}, []string{"src", "dst", "result"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "imgsync_job_duration_seconds",
			Help:    "End-to-end duration from lease to terminal status.",
			Buckets: defaultDurationBuckets,
		}, []string{"src", "dst", "result"}),
		sweepCycles: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "imgsync_sweep_cycles_total",
			Help: "Number of sweeper cycles that completed (regardless of work done).",
		}),
		ftpPoolSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imgsync_ftp_pool_size",
			Help: "FTP connection pool size per host, labeled by state.",
		}, []string{"host", "state"}),
		snifferEnq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_sniffer_enqueue_total",
			Help: "Rows the sniffer has inserted into transfer_jobs.",
		}, []string{"source"}),
		snifferErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_sniffer_run_errors_total",
			Help: "RunOnce invocations that returned an error.",
		}, []string{"source"}),
		workersActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imgsync_workers_active",
			Help: "Worker goroutines currently running, per pod.",
		}, []string{"pod"}),
	}
	reg.MustRegister(
		m.leaseAttempts, m.jobsProcessed, m.jobDuration, m.sweepCycles,
		m.ftpPoolSize, m.snifferEnq, m.snifferErr, m.workersActive,
	)
	return m
}

// --- callback methods (bind to existing OnXxx hooks in domain packages) ----

func (m *Metrics) OnLeaseAttempt(success bool, err error) {
	switch {
	case err != nil:
		m.leaseAttempts.WithLabelValues("error").Inc()
	case success:
		m.leaseAttempts.WithLabelValues("success").Inc()
	default:
		m.leaseAttempts.WithLabelValues("empty").Inc()
	}
}

func (m *Metrics) OnJobFinished(src, dst, result string, dur time.Duration) {
	if src == "" {
		src = "unknown"
	}
	if dst == "" {
		dst = "unknown"
	}
	if result == "" {
		result = "unknown"
	}
	m.jobsProcessed.WithLabelValues(src, dst, result).Inc()
	m.jobDuration.WithLabelValues(src, dst, result).Observe(dur.Seconds())
}

func (m *Metrics) OnSweepCycle()                               { m.sweepCycles.Inc() }
func (m *Metrics) OnSnifferEnqueue(source string, n int)       { m.snifferEnq.WithLabelValues(source).Add(float64(n)) }
func (m *Metrics) OnSnifferError(source string)                { m.snifferErr.WithLabelValues(source).Inc() }
func (m *Metrics) OnFTPPoolChange(host string, inUse, idle int) {
	m.ftpPoolSize.WithLabelValues(host, "in_use").Set(float64(inUse))
	m.ftpPoolSize.WithLabelValues(host, "idle").Set(float64(idle))
}

func (m *Metrics) SetWorkersActive(pod string, n int) {
	m.workersActive.WithLabelValues(pod).Set(float64(n))
}

// Handler returns the HTTP handler that serves the metrics in this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
