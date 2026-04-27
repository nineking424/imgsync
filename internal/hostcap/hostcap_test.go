package hostcap_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/hostcap"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func mustDB(t *testing.T, maxConns int32) *pgxpool.Pool {
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
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: maxConns})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// recordingTransport reports when Send begins, holds until release is signaled,
// and reports when Send ends. Used to observe cap+pin behavior.
type recordingTransport struct {
	enter chan struct{}
	hold  chan struct{}
}

func (rt *recordingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	rt.enter <- struct{}{}
	<-rt.hold
	return 0, "deadbeef", nil
}

func TestHostCap_RespectsSlotLimit_AndPinsConn(t *testing.T) {
	// pool_size=8 ensures we have headroom; slot_cap=4 is the actual ceiling.
	pool := mustDB(t, 8)
	rt := &recordingTransport{enter: make(chan struct{}, 8), hold: make(chan struct{})}
	hc := hostcap.Wrap(pool, rt, hostcap.Config{Cap: 4, Host: "ftp.test.local"})

	// Launch 4 concurrent Sends — all should acquire slots quickly.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = hc.Send(context.Background(), "ftp://ftp.test.local/x", strings.NewReader("y"), 1)
		}()
	}

	for i := 0; i < 4; i++ {
		select {
		case <-rt.enter:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d Sends entered transport — expected 4", i)
		}
	}

	// Inspect pg_locks: exactly 4 advisory locks held by 4 distinct backend pids.
	type lockRow struct {
		pid     int
		classID uint32
		objID   uint32
	}
	rows, err := pool.Query(context.Background(),
		`SELECT pid, classid, objid FROM pg_locks WHERE locktype='advisory' AND granted=true`)
	require.NoError(t, err)
	defer rows.Close()
	var locks []lockRow
	for rows.Next() {
		var lr lockRow
		require.NoError(t, rows.Scan(&lr.pid, &lr.classID, &lr.objID))
		locks = append(locks, lr)
	}

	require.Len(t, locks, 4, "F1 regression: must see exactly 4 advisory locks for 4 in-flight Sends")
	pids := map[int]bool{}
	keys := map[uint64]bool{}
	for _, lr := range locks {
		pids[lr.pid] = true
		keys[uint64(lr.classID)<<32|uint64(lr.objID)] = true
	}
	require.Equal(t, 4, len(pids), "F1: each lock must be held by a distinct backend pid (no pgx conn reuse)")
	require.Equal(t, 4, len(keys), "F1: 4 distinct slot keys must be held (no duplicates)")

	// Release transports.
	close(rt.hold)
	wg.Wait()

	// After release, advisory locks must be gone.
	require.Eventually(t, func() bool {
		var n int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND granted=true`).Scan(&n)
		return n == 0
	}, 2*time.Second, 50*time.Millisecond, "advisory locks must be released after Send returns")
}

func TestHostCap_OverCapBlocksUntilRelease(t *testing.T) {
	pool := mustDB(t, 8)
	rt := &recordingTransport{enter: make(chan struct{}, 8), hold: make(chan struct{})}
	hc := hostcap.Wrap(pool, rt, hostcap.Config{
		Cap:            2,
		Host:           "ftp.test.local",
		AcquireBackoff: 30 * time.Millisecond,
	})

	for i := 0; i < 2; i++ {
		go func() {
			_, _, _ = hc.Send(context.Background(), "ftp://ftp.test.local/x", strings.NewReader("a"), 1)
		}()
	}
	<-rt.enter
	<-rt.enter

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := hc.Send(ctx, "ftp://ftp.test.local/x", strings.NewReader("a"), 1)
	require.ErrorIs(t, err, context.DeadlineExceeded, "third Send must block beyond cap")

	close(rt.hold)
}
