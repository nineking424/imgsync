package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/hostcap"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// blockingTransport reports when each Send begins and then blocks until hold is
// closed, so the test can hold Cap transfers in flight while it probes the
// worker pool. No sleeps — fully channel-driven for determinism.
type blockingTransport struct {
	enter chan struct{}
	hold  chan struct{}
}

func (bt *blockingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	bt.enter <- struct{}{}
	<-bt.hold
	return 0, "deadbeef", nil
}

// startTestPostgres spins a throwaway Postgres, applies migrations, and returns
// its DSN. The container is terminated on test cleanup.
func startTestPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	return dsn
}

// TestHostcapDoesNotStarveWorkerPool asserts the worker pool stays usable while
// Cap FTP transfers are in flight.
//
// Issue #18: newHostcapTransport currently passes the worker pool into
// hostcap.Wrap, and hostcap.Send pins one worker-pool connection for the ENTIRE
// transfer (it only holds a session advisory lock). With Cap concurrent
// transfers, all Cap worker-pool connections are parked in hostcap, leaving
// zero for LeaseJob / commit / sweeper / scrape — a plain "SELECT 1" via the
// worker pool blocks until a transfer finishes.
//
// We size the worker pool to exactly Cap so Cap in-flight Sends consume every
// connection it would have if it shared. The fix gives hostcap its OWN
// dedicated pool, so the worker pool is left entirely free and the probe
// succeeds immediately. This test is RED on the shared-pool wiring and GREEN
// once newHostcapTransport hands hostcap a separate pool.
func TestHostcapDoesNotStarveWorkerPool(t *testing.T) {
	dsn := startTestPostgres(t)

	const slotCap = 3
	// Mirror production sizing shape: worker pool holds the conns lease/commit/
	// sweep/scrape need. Sized to Cap so the starvation is crisp and
	// deterministic — Cap pinned conns drain it to empty.
	ctx := context.Background()
	workerPool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: slotCap})
	require.NoError(t, err)
	t.Cleanup(workerPool.Close)

	bt := &blockingTransport{enter: make(chan struct{}, slotCap), hold: make(chan struct{})}

	ftpTr, closeHostcap, err := newHostcapTransport(ctx, dsn, workerPool, bt,
		hostcap.Config{Cap: slotCap, Host: "ftp.test.local"})
	require.NoError(t, err)
	defer closeHostcap()

	// Launch Cap concurrent transfers; each pins a hostcap conn for its whole
	// duration. They block in Send until we close hold.
	var wg sync.WaitGroup
	for i := 0; i < slotCap; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = ftpTr.Send(context.Background(),
				"ftp://ftp.test.local/x", strings.NewReader("y"), 1)
		}()
	}
	// Always release the in-flight transfers, even if an assertion below aborts
	// the test goroutine — otherwise the blocked Sends leak and stall teardown.
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(bt.hold) })
		wg.Wait()
	})

	// Wait until all Cap transfers are actually in flight (conns pinned).
	for i := 0; i < slotCap; i++ {
		select {
		case <-bt.enter:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d transfers entered transport", i, slotCap)
		}
	}

	// The bug: with the shared worker pool, all Cap conns are now pinned by
	// hostcap, so this acquire/probe blocks until a transfer releases. With a
	// dedicated hostcap pool, the worker pool is untouched and this returns at
	// once. A 2s budget is far beyond a healthy "SELECT 1".
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := workerPool.Acquire(probeCtx)
	require.NoError(t, err,
		"#18 starvation: worker pool exhausted by in-flight hostcap transfers — "+
			"hostcap must use its own dedicated pool, not the worker pool")
	var one int
	require.NoError(t, conn.QueryRow(probeCtx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
	conn.Release()

	// Release the transfers and let the goroutines finish (cleanup also covers
	// the failure path).
	release.Do(func() { close(bt.hold) })
	wg.Wait()
}
