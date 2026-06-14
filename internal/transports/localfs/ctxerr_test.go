package localfs_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/stretchr/testify/require"
)

// Issue #24: a missing parent directory (os.CreateTemp returns os.ErrNotExist)
// is a permanent misconfiguration and must classify as transfer.ErrPermanent so
// the worker marks the job dead instead of burning the full retry budget.
func TestTransport_Send_DstDirMissing_ReturnsErrPermanent(t *testing.T) {
	tr := localfs.NewTransport()
	_, _, err := tr.Send(context.Background(), "/no/such/dir/out.txt", blockingReader(nil), 1)
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent,
		"missing parent dir must be ErrPermanent (dead), got %v", err)
}

// Issue #22: a cancelled ctx must abort an in-flight copy PROMPTLY. The current
// transport hands the body to io.Copy without consulting ctx, so cancellation
// has zero effect and Send only returns once the body reader unblocks. This
// reader blocks forever after the first chunk, so on unfixed code Send hangs
// until the test deadline; on fixed code Read returns ctx.Err() right away.
func TestTransport_Send_CtxCancel_AbortsInFlightPromptly(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "slow.bin")
	tr := localfs.NewTransport()

	ctx, cancel := context.WithCancel(context.Background())
	body := newCtxBlockingReader()

	done := make(chan error, 1)
	go func() {
		_, _, err := tr.Send(ctx, dst, body, -1)
		done <- err
	}()

	// Let the copy start and consume the first chunk, then cancel.
	<-body.started
	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "cancelled transfer must return an error, not succeed")
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not abort within 3s of ctx cancel: ctx is not propagated into the copy")
	}
}

// blockingReader returns an io.Reader that yields the given bytes once then EOF.
func blockingReader(b []byte) io.Reader { return &simpleReader{b: b} }

type simpleReader struct {
	b    []byte
	done bool
}

func (r *simpleReader) Read(p []byte) (int, error) {
	if r.done || len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	if len(r.b) == 0 {
		r.done = true
	}
	return n, nil
}

// ctxBlockingReader yields one chunk, signals started, then blocks forever. Only
// a ctx-aware copy can unblock it; an unfixed copy loop hangs.
type ctxBlockingReader struct {
	started   chan struct{}
	block     chan struct{}
	firstDone bool
}

func newCtxBlockingReader() *ctxBlockingReader {
	return &ctxBlockingReader{
		started: make(chan struct{}),
		block:   make(chan struct{}),
	}
}

func (r *ctxBlockingReader) Read(p []byte) (int, error) {
	if !r.firstDone {
		r.firstDone = true
		n := copy(p, []byte("first-chunk"))
		close(r.started)
		return n, nil
	}
	<-r.block // blocks forever; never unblocks on its own
	return 0, io.EOF
}
