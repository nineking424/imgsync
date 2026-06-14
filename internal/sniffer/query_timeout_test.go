package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
)

// Regression for issue #30: sourcedb.QueryTimeout is computed but never applied
// in Query.Fetch. A slow/hung source query must be bounded by the per-query
// timeout even when the caller ctx has NO deadline (the real sniffer loop ctx,
// from signal.NotifyContext, is deadline-less). Without the fix, Fetch runs the
// query under the raw long-lived ctx and blocks until the query finishes — the
// poll loop wedges and the watermark stalls silently.
//
// This deliberately uses context.Background() (no deadline) so the ONLY thing
// that can abort the slow query is Query.QueryTimeout. The existing integration
// test S3 passes a 100ms ctx deadline, so it does not exercise this field — it
// would stay green even with the timeout ignored.
func TestQuery_QueryTimeoutBoundsSlowQuery(t *testing.T) {
	pool := setupSourceDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	_, err := pool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p')`, ts)
	require.NoError(t, err)

	// A view that sleeps 3s per scan, far longer than the 200ms QueryTimeout
	// below. Mirrors the pg_sleep pattern used by integration test S3.
	_, err = pool.Exec(ctx, `
		CREATE OR REPLACE VIEW images_slow AS
		SELECT i.id, i.updated_at, i.file_path
		FROM images i CROSS JOIN (SELECT pg_sleep(3)) AS _delay`)
	require.NoError(t, err)

	q := sniffer.Query{
		Table:        "images_slow",
		PKColumn:     "id",
		TSColumn:     "updated_at",
		ExtraColumns: []string{"file_path"},
		BatchSize:    10,
		QueryTimeout: 200 * time.Millisecond,
	}

	start := time.Now()
	_, err = q.Fetch(ctx, pool, sniffer.State{LastRunTS: ts.Add(-time.Hour)})
	elapsed := time.Since(start)

	require.Error(t, err, "Fetch must return a deadline error when the source query exceeds QueryTimeout")
	require.Less(t, elapsed, 2*time.Second,
		"Fetch must abort at ~QueryTimeout (200ms), not run the full 3s query; took %s", elapsed)
}
