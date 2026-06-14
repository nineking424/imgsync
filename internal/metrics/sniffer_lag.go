package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// snifferLagCollector emits imgsync_sniffer_watermark_lag_seconds{source} =
// NOW() - last successful RunOnce timestamp, per source. It reads the
// per-source last-run wall-clock recorded by OnSnifferRun and computes the lag
// at scrape time, so a sniffer that stops advancing shows a steadily climbing
// lag. Non-negative by construction (clamped at 0).
type snifferLagCollector struct {
	desc *prometheus.Desc
	mu   *sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newSnifferLagCollector(mu *sync.Mutex, last map[string]time.Time, now func() time.Time) *snifferLagCollector {
	return &snifferLagCollector{
		desc: prometheus.NewDesc(
			"imgsync_sniffer_watermark_lag_seconds",
			"Seconds since the sniffer's last successful RunOnce, per source.",
			[]string{"source"}, nil,
		),
		mu:   mu,
		last: last,
		now:  now,
	}
}

func (c *snifferLagCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *snifferLagCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	c.mu.Lock()
	snapshot := make(map[string]time.Time, len(c.last))
	for src, ts := range c.last {
		snapshot[src] = ts
	}
	c.mu.Unlock()
	for src, ts := range snapshot {
		lag := now.Sub(ts).Seconds()
		if lag < 0 {
			lag = 0
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, lag, src)
	}
}
