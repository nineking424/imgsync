package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// dbPoolCollector wraps pgxpool.Pool.Stat() into imgsync_db_pool_conns{state}.
// Cost: zero — pgxpool keeps these counters in-process.
type dbPoolCollector struct {
	pool *pgxpool.Pool
	desc *prometheus.Desc
}

func newDBPoolCollector(pool *pgxpool.Pool) *dbPoolCollector {
	return &dbPoolCollector{
		pool: pool,
		desc: prometheus.NewDesc(
			"imgsync_db_pool_conns",
			"pgxpool connection counts by state.",
			[]string{"state"}, nil,
		),
	}
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.AcquiredConns()), "in_use")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.MaxConns()), "max")
}
