package worker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/metrics"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/transfer"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

// fakeSource lets a test choose the open outcome deterministically.
type fakeSource struct {
	body io.ReadCloser
	size int64
	err  error
}

func (s fakeSource) Open(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return s.body, s.size, s.err
}

// fakeTransport returns a fixed Send outcome (used for the retryable path).
type fakeTransport struct{ err error }

func (t fakeTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body) // drain so the source reader is consumed
	return 0, "", t.err
}

// runOneJobAndScrape enqueues exactly one localfs job, drives the Runner with
// the given Source/Transport factories, wires OnFinish to a real metrics
// instance EXACTLY as cmd/imgsync/worker.go does, and returns the /metrics
// text once the job reaches a terminal state.
//
// Issue #17: the OnFinish wiring forwards job.Status into
// m.OnJobFinished(... result ...). job.Status is set to "leased" by LeaseJob and
// never updated by the terminal writes in process.go, so the result=... label is
// always "leased" and the succeeded/skipped/dead/fail series are never emitted.
func runOneJobAndScrape(
	t *testing.T,
	traceID, src, dst string,
	maxAttempts int,
	src0 worker.SourceLike,
	tr0 worker.TransportLike,
) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := mustDB(t)
	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: traceID, Src: src, Dst: dst,
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: maxAttempts,
	})
	require.NoError(t, err)

	m := metrics.New()

	r := &worker.Runner{
		Pool:         pool,
		Workers:      1,
		PodName:      "test-pod",
		IdleBackoff:  backoff.NewIdle(backoff.Config{BaseDelay: 30 * time.Millisecond, MaxDelay: 100 * time.Millisecond}),
		SourceFor:    func(_ string) (worker.SourceLike, error) { return src0, nil },
		TransportFor: func(_ string) (worker.TransportLike, error) { return tr0, nil },
	}

	// Mirror of cmd/imgsync/worker.go's OnFinish closure: the value handed to the
	// metric's result label is the per-job terminal outcome. The fix re-wires this
	// (and Runner.OnFinish) to carry the terminal result instead of j.Status.
	var finished sync.WaitGroup
	finished.Add(1)
	var once sync.Once
	r.OnFinish = func(j *worker.Job, result string) {
		m.OnJobFinished(j.SrcProtocol, j.DstProtocol, result, j.Duration())
		once.Do(finished.Done)
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

	done := make(chan struct{})
	go func() { finished.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("OnFinish never fired")
	}

	mfs, err := m.RegistryForTest().Gather()
	require.NoError(t, err)
	var sb strings.Builder
	for _, mf := range mfs {
		if mf.GetName() != "imgsync_jobs_processed_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, l := range mm.GetLabel() {
				sb.WriteString(l.GetName())
				sb.WriteString("=")
				sb.WriteString(l.GetValue())
				sb.WriteString(" ")
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// TestRunner_OnFinishResult_ReflectsTerminalOutcome is the issue #17 regression
// guard. It asserts the imgsync_jobs_processed_total{result=...} label reflects
// the TERMINAL job outcome. On current code the label is always "leased" (the
// lease-time job.Status that the terminal writes never update), so each case
// fails because the expected terminal-result series is absent.
func TestRunner_OnFinishResult_ReflectsTerminalOutcome(t *testing.T) {
	t.Run("success->succeeded", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "in.txt")
		dstPath := filepath.Join(dir, "out.txt")
		require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

		got := runOneJobAndScrape(t, "fin-ok", srcPath, dstPath, 3,
			localfs.NewSource(), tlocalfs.NewTransport())
		require.Contains(t, got, "result=succeeded",
			"successful transfer must emit result=succeeded; got series:\n%s", got)
		require.NotContains(t, got, "result=leased",
			"metric must not be labeled with the lease-time status")
	})

	t.Run("skippable->skipped", func(t *testing.T) {
		dir := t.TempDir()
		dstPath := filepath.Join(dir, "out.txt")
		// localfs Source.Open on a missing file wraps transfer.ErrSkippable.
		got := runOneJobAndScrape(t, "fin-skip", "/no/such/src", dstPath, 3,
			localfs.NewSource(), tlocalfs.NewTransport())
		require.Contains(t, got, "result=skipped",
			"ErrSkippable outcome must emit result=skipped; got series:\n%s", got)
	})

	t.Run("permanent->dead", func(t *testing.T) {
		dir := t.TempDir()
		dstPath := filepath.Join(dir, "out.txt")
		// Source.Open returns ErrPermanent -> job goes dead.
		permSrc := fakeSource{err: transfer.ErrPermanent, size: -1}
		got := runOneJobAndScrape(t, "fin-dead", "src-ignored", dstPath, 3,
			permSrc, tlocalfs.NewTransport())
		require.Contains(t, got, "result=dead",
			"ErrPermanent outcome must emit result=dead; got series:\n%s", got)
	})

	t.Run("retryable->fail", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "in.txt")
		dstPath := filepath.Join(dir, "out.txt")
		require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

		// A plain (non-sentinel) Send error with attempts(0)+1 < max(3) routes to
		// writeRetryOrDead (DB status=pending). The METRIC result must be "fail".
		got := runOneJobAndScrape(t, "fin-fail", srcPath, dstPath, 3,
			localfs.NewSource(), fakeTransport{err: io.ErrUnexpectedEOF})
		require.Contains(t, got, "result=fail",
			"a retryable error (attempts<max) must emit metric result=fail; got series:\n%s", got)
	})
}
