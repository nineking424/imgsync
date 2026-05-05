// Package metrics owns the Prometheus collectors imgsync exposes via /metrics.
// One Metrics instance per process. Domain packages (worker/sweeper/sniffer/
// ftp pool) call methods on Metrics via the existing OnXxx callback pattern,
// so they do not import this package.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every imgsync_* collector. Each instance has its own
// prometheus.Registry so tests can run in parallel.
type Metrics struct {
	reg *prometheus.Registry
}

// New constructs an empty Metrics with a fresh registry.
func New() *Metrics {
	return &Metrics{reg: prometheus.NewRegistry()}
}

// Handler returns the HTTP handler that serves the metrics in this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
