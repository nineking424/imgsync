package metrics

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// newOldestPendingAge returns a GaugeFunc that runs the oldest-due-pending SQL
// each scrape. Served by transfer_jobs_pending_idx (next_run_at, id) WHERE
// status='pending'. Returns 0 when no pending row is due (NULL MIN), so a
// future-scheduled retry or a terminal row never inflates the gauge. Errors emit
// a warn log + 0.
func newOldestPendingAge(pool *pgxpool.Pool) prometheus.Collector {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "imgsync_oldest_pending_age_seconds",
			Help: "Age in seconds of the oldest due pending job (0 if none due).",
		},
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var ageSec sql.NullFloat64
			err := pool.QueryRow(ctx,
				`SELECT EXTRACT(EPOCH FROM NOW() - MIN(next_run_at))::double precision
				   FROM transfer_jobs WHERE status='pending' AND next_run_at<=NOW()`,
			).Scan(&ageSec)
			if err != nil {
				log.Printf("metrics: oldest_pending_age scrape failed: %v", err)
				return 0
			}
			if !ageSec.Valid {
				return 0
			}
			return ageSec.Float64
		},
	)
}
