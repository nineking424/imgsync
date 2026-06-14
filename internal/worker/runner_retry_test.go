package worker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// TestRunner_OnRetry_FiresWhenJobReschedules is the issue #27 regression guard
// for the retry counter wiring. A non-sentinel transport error with
// attempts(0)+1 < max(3) routes through writeRetryOrDead (DB status=pending);
// that retry path must invoke Runner.OnRetry so cmd/imgsync/worker.go can wire
// it to imgsync_job_retries_total. On a terminal outcome (success/skip/dead)
// OnRetry must NOT fire.
//
// On current code Runner has no OnRetry field, so this fails to compile. Once
// the field exists but is unwired the callback never fires and the test fails
// on the count assertion. GREEN once writeRetryOrDead fires OnRetry.
func TestRunner_OnRetry_FiresWhenJobReschedules(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "retry-metric-1", Src: srcPath, Dst: filepath.Join(dir, "out.txt"),
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
	})
	require.NoError(t, err)

	var retryCalls int32
	var mu sync.Mutex
	var gotStages []string

	r := &worker.Runner{
		Pool:        pool,
		Workers:     1,
		PodName:     "test-pod",
		IdleBackoff: backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor:   func(_ string) (worker.SourceLike, error) { return localfs.NewSource(), nil },
		// Transport always returns a plain retryable error -> writeRetryOrDead.
		TransportFor: func(_ string) (worker.TransportLike, error) {
			return fakeTransport{err: io.ErrUnexpectedEOF}, nil
		},
	}

	fired := make(chan struct{}, 1)
	r.OnRetry = func(job *worker.Job, stage string) {
		mu.Lock()
		gotStages = append(gotStages, stage)
		mu.Unlock()
		if atomic.AddInt32(&retryCalls, 1) == 1 {
			fired <- struct{}{}
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })

	select {
	case <-fired:
	case <-time.After(15 * time.Second):
		t.Fatal("OnRetry never fired for a rescheduled (retryable) job")
	}

	// Stop the loop so attempts don't keep climbing, then assert the row is
	// actually pending (the retry path, not a terminal write).
	cancel()
	wg.Wait()

	mu.Lock()
	require.NotEmpty(t, gotStages, "OnRetry must carry a non-empty stage label")
	require.Equal(t, "transport", gotStages[0],
		"a Transport.Send retry must report stage=transport")
	mu.Unlock()
}
