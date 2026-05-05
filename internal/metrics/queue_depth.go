package metrics

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// queueDepthCollector emits imgsync_jobs_in_status{status} by running
// SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status at scrape time.
// 2-second timeout per Collect. Failures emit 0 metrics + warn log; never
// panics, never blocks. Phase 1.5 adds an index that makes this an index-only
// scan.
type queueDepthCollector struct {
	pool *pgxpool.Pool
	desc *prometheus.Desc
}

func newQueueDepthCollector(pool *pgxpool.Pool) *queueDepthCollector {
	return &queueDepthCollector{
		pool: pool,
		desc: prometheus.NewDesc(
			"imgsync_jobs_in_status",
			"Number of transfer_jobs rows per status.",
			[]string{"status"}, nil,
		),
	}
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rows, err := c.pool.Query(ctx,
		`SELECT status::text, COUNT(*)::bigint FROM transfer_jobs GROUP BY status`)
	if err != nil {
		log.Printf("metrics: queue_depth scrape failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			log.Printf("metrics: queue_depth scan failed: %v", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), status)
	}
	if err := rows.Err(); err != nil {
		log.Printf("metrics: queue_depth iter failed: %v", err)
	}
}
