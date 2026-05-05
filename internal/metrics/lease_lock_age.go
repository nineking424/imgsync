package metrics

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// newLeaseLockAge returns a GaugeFunc that runs the lease-lock-age SQL each
// scrape. Cheap thanks to transfer_jobs_leased_idx (locked_at). Returns 0 if
// no rows are leased (NULL MIN). Errors emit a warn log + 0.
func newLeaseLockAge(pool *pgxpool.Pool) prometheus.Collector {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "imgsync_lease_lock_age_seconds",
			Help: "Age in seconds of the oldest currently-leased job (0 if none).",
		},
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var ageSec sql.NullFloat64
			err := pool.QueryRow(ctx,
				`SELECT EXTRACT(EPOCH FROM NOW() - MIN(locked_at))::double precision
				   FROM transfer_jobs WHERE status='leased'`,
			).Scan(&ageSec)
			if err != nil {
				log.Printf("metrics: lease_lock_age scrape failed: %v", err)
				return 0
			}
			if !ageSec.Valid {
				return 0
			}
			return ageSec.Float64
		},
	)
}
