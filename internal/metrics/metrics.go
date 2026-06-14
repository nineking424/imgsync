// Package metrics owns the Prometheus collectors imgsync exposes via /metrics.
// One Metrics instance per process. Domain packages (worker/sweeper/sniffer/
// ftp pool) call methods on Metrics via the existing OnXxx callback pattern,
// so they do not import this package.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every imgsync_* collector. Each instance has its own
// prometheus.Registry so tests can run in parallel.
type Metrics struct {
	reg *prometheus.Registry

	leaseAttempts  *prometheus.CounterVec
	jobsProcessed  *prometheus.CounterVec
	jobRetries     *prometheus.CounterVec
	jobDuration    *prometheus.HistogramVec
	sweepCycles    prometheus.Counter
	ftpPoolSize    *prometheus.GaugeVec
	snifferEnq     *prometheus.CounterVec
	snifferErr     *prometheus.CounterVec
	snifferLastRun *prometheus.GaugeVec
	workersActive  *prometheus.GaugeVec

	// snifferMu guards snifferLast, the per-source last-successful-RunOnce
	// wall-clock the watermark-lag collector reads at scrape time.
	snifferMu   sync.Mutex
	snifferLast map[string]time.Time
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
		jobRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_job_retries_total",
			Help: "Number of jobs rescheduled for retry, labeled by error-category stage.",
		}, []string{"src", "dst", "stage"}),
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
		snifferLastRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imgsync_sniffer_last_run_timestamp",
			Help: "Unix timestamp of the sniffer's last successful RunOnce, per source.",
		}, []string{"source"}),
		workersActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imgsync_workers_active",
			Help: "Worker goroutines currently running, per pod.",
		}, []string{"pod"}),
		snifferLast: make(map[string]time.Time),
	}
	reg.MustRegister(
		m.leaseAttempts, m.jobsProcessed, m.jobRetries, m.jobDuration, m.sweepCycles,
		m.ftpPoolSize, m.snifferEnq, m.snifferErr, m.snifferLastRun, m.workersActive,
		newSnifferLagCollector(&m.snifferMu, m.snifferLast, time.Now),
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

// OnRetry records a job rescheduled for retry (DB status=pending), labeled by
// the error-category stage (e.g. "transport", "open"). Empty labels default to
// "unknown" so a retry with an unset stage still produces a usable series.
func (m *Metrics) OnRetry(src, dst, stage string) {
	if src == "" {
		src = "unknown"
	}
	if dst == "" {
		dst = "unknown"
	}
	if stage == "" {
		stage = "unknown"
	}
	m.jobRetries.WithLabelValues(src, dst, stage).Inc()
}

func (m *Metrics) OnSweepCycle() { m.sweepCycles.Inc() }
func (m *Metrics) OnSnifferEnqueue(source string, n int) {
	m.snifferEnq.WithLabelValues(source).Add(float64(n))
}
func (m *Metrics) OnSnifferError(source string) { m.snifferErr.WithLabelValues(source).Inc() }

// OnSnifferRun records a successful RunOnce: it sets
// imgsync_sniffer_last_run_timestamp{source} to now and refreshes the watermark
// the lag collector subtracts from NOW() at scrape time.
func (m *Metrics) OnSnifferRun(source string) {
	now := time.Now()
	m.snifferLastRun.WithLabelValues(source).Set(float64(now.Unix()))
	m.snifferMu.Lock()
	m.snifferLast[source] = now
	m.snifferMu.Unlock()
}
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

// AttachQueueDepth registers the jobs_in_status scrape collector. Idempotent:
// safe to call once per pool. Panics on duplicate registration (caller bug).
func (m *Metrics) AttachQueueDepth(pool *pgxpool.Pool) {
	m.reg.MustRegister(newQueueDepthCollector(pool))
}

// AttachDBPool registers the db_pool_conns collector.
func (m *Metrics) AttachDBPool(pool *pgxpool.Pool) {
	m.reg.MustRegister(newDBPoolCollector(pool))
}

// AttachLeaseLockAge registers the lease_lock_age_seconds gauge.
func (m *Metrics) AttachLeaseLockAge(pool *pgxpool.Pool) {
	m.reg.MustRegister(newLeaseLockAge(pool))
}

// AttachOldestPending registers the oldest_pending_age_seconds gauge.
func (m *Metrics) AttachOldestPending(pool *pgxpool.Pool) {
	m.reg.MustRegister(newOldestPendingAge(pool))
}

// RegistryForTest exposes the underlying registry for assertions in tests
// from external packages. Not for production code.
func (m *Metrics) RegistryForTest() *prometheus.Registry { return m.reg }
