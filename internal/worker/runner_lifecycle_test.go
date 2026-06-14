package worker_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// panickingSource panics from Open, simulating a poison job whose Source
// blows up (e.g. a nil-deref deep inside a protocol adapter). The panic
// originates inside ProcessJob, which runs in the worker-loop goroutine.
type panickingSource struct {
	hits *int64
}

func (s panickingSource) Open(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	if s.hits != nil {
		atomic.AddInt64(s.hits, 1)
	}
	panic("boom: poison source")
}

// TestRunner_PanicInProcessJob_WorkerSurvivesAndJobAdvances reproduces issue #23.
//
// On current code the recover() lives at loop() scope, so a panic raised inside
// ProcessJob unwinds the whole worker goroutine permanently: with Workers=1 the
// single worker dies and never respawns. The poison row stays leased forever
// (the sweeper only reclaims it leased->pending and never bumps attempts, so it
// is re-leased and re-panics ad infinitum), AND every other pending job stops
// draining because the only worker is gone.
//
// After the fix the worker must SURVIVE a panic: it records a retry/terminal
// outcome for the poison job (advancing attempts so a poison job eventually caps
// to dead) and keeps draining the rest of the queue.
//
// To keep this fast and sweeper-independent we give the poison job
// MaxAttempts=1, so the very first panic-as-failure must drive it terminal
// (dead). We also enqueue good jobs the surviving worker must still drain.
func TestRunner_PanicInProcessJob_WorkerSurvivesAndJobAdvances(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()

	// Poison job: SrcProtocol "poison" -> panicking source. MaxAttempts=1 so the
	// first failure must terminate it dead (no infinite poison loop).
	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "poison-1", Src: "ignored", Dst: filepath.Join(dir, "poison-out"),
		SrcProtocol: "poison", DstProtocol: "localfs", MaxAttempts: 1,
	})
	require.NoError(t, err)

	// Good jobs the surviving worker must still drain after the panic.
	const good = 4
	for i := 0; i < good; i++ {
		src := filepath.Join(dir, "src", "g"+string(rune('0'+i)))
		dst := filepath.Join(dir, "dst", "g"+string(rune('0'+i)))
		require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
		require.NoError(t, os.WriteFile(src, []byte("ok-"+string(rune('0'+i))), 0o644))
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: "good-" + string(rune('0'+i)), Src: src, Dst: dst,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
		})
		require.NoError(t, err)
	}

	var panicHits int64
	r := &worker.Runner{
		Pool:        pool,
		Workers:     1, // single worker: if the panic kills it, nothing else drains
		PodName:     "panic-pod",
		IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor: func(p string) (worker.SourceLike, error) {
			if p == "poison" {
				return panickingSource{hits: &panicHits}, nil
			}
			return localfs.NewSource(), nil
		},
		TransportFor: func(_ string) (worker.TransportLike, error) { return tlocalfs.NewTransport(), nil },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// The surviving worker must drain ALL good jobs to succeeded despite the panic.
	require.Eventually(t, func() bool {
		var succeeded int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM transfer_jobs WHERE status='succeeded'`).Scan(&succeeded)
		return succeeded == good
	}, 20*time.Second, 100*time.Millisecond,
		"worker did not survive the panic: good jobs never drained")

	// The poison job must advance to a terminal state (dead) — not loop forever
	// as leased/pending. attempts must have been bumped past 0.
	require.Eventually(t, func() bool {
		var status string
		_ = pool.QueryRow(ctx,
			`SELECT status FROM transfer_jobs WHERE trace_id='poison-1'`).Scan(&status)
		return status == "dead"
	}, 20*time.Second, 100*time.Millisecond,
		"poison job never reached a terminal (dead) state — infinite poison loop")

	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT attempts FROM transfer_jobs WHERE trace_id='poison-1'`).Scan(&attempts))
	require.GreaterOrEqual(t, attempts, 1,
		"a panicking job must advance attempts so poison eventually caps to dead")
}

// blockingSource opens, signals it is in flight, then blocks until ctx is
// cancelled. This pins the leased row in transfer_jobs while the worker is
// mid-transfer, so ctx cancel (SIGTERM) catches the worker holding a lease.
type blockingSource struct {
	opened chan struct{}
	once   sync.Once
}

func (s *blockingSource) Open(ctx context.Context, _ string) (io.ReadCloser, int64, error) {
	s.once.Do(func() { close(s.opened) })
	<-ctx.Done()
	return nil, 0, errors.New("source aborted: context cancelled")
}

// TestRunner_GracefulDrain_OnCtxCancel_ResetsHeldLease reproduces issue #21.
//
// When the worker pod receives SIGTERM, ctx is cancelled while a job is still in
// flight and held as leased by this worker. On current code the loop simply
// exits and the row is abandoned in status='leased' until the sweeper reclaims
// it ~5 min later — the job does not requeue promptly on a clean shutdown.
//
// After the fix, Run/loop must best-effort drain: reset THIS worker's still-held
// leased rows back to pending (locked_at/locked_by cleared) so they requeue
// immediately. This test leases a row, cancels ctx mid-flight, waits for the
// runner to exit, and asserts the row is back to pending — not stranded leased.
func TestRunner_GracefulDrain_OnCtxCancel_ResetsHeldLease(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	dst := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello drain"), 0o644))

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "drain-1", Src: src, Dst: dst,
		SrcProtocol: "block", DstProtocol: "localfs", MaxAttempts: 3,
	})
	require.NoError(t, err)

	bs := &blockingSource{opened: make(chan struct{})}
	r := &worker.Runner{
		Pool:        pool,
		Workers:     1,
		PodName:     "drain-pod",
		IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor: func(p string) (worker.SourceLike, error) {
			if p == "block" {
				return bs, nil
			}
			return localfs.NewSource(), nil
		},
		TransportFor: func(_ string) (worker.TransportLike, error) { return tlocalfs.NewTransport(), nil },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = r.Run(ctx)
	}()

	// Wait until the worker has leased the row and is blocked inside the transfer.
	select {
	case <-bs.opened:
	case <-time.After(15 * time.Second):
		cancel()
		wg.Wait()
		t.Fatal("worker never leased + opened the blocking job")
	}

	// Confirm the row is genuinely held as leased by this worker before cancel.
	assertLeasedBy(t, pool, "drain-1", "drain-pod")

	// SIGTERM equivalent: cancel ctx while the job is in flight, then wait for the
	// runner to fully exit (drain must complete during shutdown).
	cancel()
	wg.Wait()

	// The held lease must be drained back to pending so it requeues immediately,
	// NOT left stranded as leased for the sweeper.
	var status string
	var lockedBy *string
	var lockedAt *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, locked_by, locked_at FROM transfer_jobs WHERE trace_id='drain-1'`,
	).Scan(&status, &lockedBy, &lockedAt))
	require.Equal(t, "pending", status,
		"graceful drain must reset this worker's held lease to pending, not abandon it leased")
	require.Nil(t, lockedBy, "drain must clear locked_by")
	require.Nil(t, lockedAt, "drain must clear locked_at")
}

// assertLeasedBy fails unless the named job is currently leased by a worker of
// the given pod (locked_by has the "<pod>-w<idx>" prefix).
func assertLeasedBy(t *testing.T, pool *pgxpool.Pool, traceID, pod string) {
	t.Helper()
	var status string
	var lockedBy *string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, locked_by FROM transfer_jobs WHERE trace_id=$1`, traceID,
	).Scan(&status, &lockedBy))
	require.Equal(t, "leased", status, "precondition: job must be leased before cancel")
	require.NotNil(t, lockedBy, "precondition: leased job must have locked_by set")
	require.Contains(t, *lockedBy, pod, "precondition: job must be leased by this pod's worker")
}
