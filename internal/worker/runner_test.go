package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

func TestRunner_DrainsQueue(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		src := filepath.Join(dir, "src", "f"+string(rune('0'+i)))
		dst := filepath.Join(dir, "dst", "f"+string(rune('0'+i)))
		require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.WriteFile(src, []byte("hi-"+string(rune('0'+i))), 0o644))
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: "t-" + string(rune('0'+i)), Src: src, Dst: dst,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
		})
		require.NoError(t, err)
	}

	r := &worker.Runner{
		Pool:        pool,
		Workers:     2,
		PodName:     "test-pod",
		IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor:   func(_ string) (worker.SourceLike, error) { return localfs.NewSource(), nil },
		TransportFor: func(_ string) (worker.TransportLike, error) { return tlocalfs.NewTransport(), nil },
	}

	var processed int64
	r.OnFinish = func(_ *worker.Job) { atomic.AddInt64(&processed, 1) }

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

	require.Eventually(t, func() bool {
		var pending int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs WHERE status='pending'`).Scan(&pending)
		return pending == 0
	}, 10*time.Second, 100*time.Millisecond, "queue did not drain")

	require.GreaterOrEqual(t, atomic.LoadInt64(&processed), int64(8))

	var succeeded int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs WHERE status='succeeded'`).Scan(&succeeded))
	require.Equal(t, 8, succeeded)
}

func TestRunner_UnknownProtocol_RetriesUntilDead(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "bad-proto", Src: "x", Dst: "y",
		SrcProtocol: "made-up", DstProtocol: "localfs", MaxAttempts: 1,
	})
	require.NoError(t, err)

	r := &worker.Runner{
		Pool:        pool,
		Workers:     1,
		PodName:     "test-pod",
		IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor:   func(p string) (worker.SourceLike, error) {
			if p == "localfs" {
				return localfs.NewSource(), nil
			}
			return nil, worker.ErrUnknownProtocol
		},
		TransportFor: func(p string) (worker.TransportLike, error) {
			return tlocalfs.NewTransport(), nil
		},
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

	require.Eventually(t, func() bool {
		var status string
		_ = pool.QueryRow(ctx, `SELECT status FROM transfer_jobs WHERE trace_id='bad-proto'`).Scan(&status)
		return status == "dead"
	}, 10*time.Second, 100*time.Millisecond, "unknown protocol must terminate dead")
}
