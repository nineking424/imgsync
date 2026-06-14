package ftp_test

import (
	"context"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	"github.com/nineking424/imgsync/internal/transfer"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	"github.com/stretchr/testify/require"
)

// Issue #24: a 550 on STOR (parent dir missing, non-best-effort multi-level) is a
// permanent misconfiguration and must classify as transfer.ErrPermanent. The
// transport currently wraps every STOR failure as a bare retryable error.
func TestFTPTransport_Send_Stor550_ReturnsErrPermanent(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))

	// Two missing path components: transport only best-effort MakeDir's a single
	// level, so STOR of the tmp file under <missing>/<missing>/ yields a 550.
	uri := fmt.Sprintf("ftp://%s/ghost/sub/out.txt", srv.Addr)
	_, _, err := tr.Send(context.Background(), uri, blockingReader([]byte("x")), 1)
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent,
		"550 STOR (missing parent) must be ErrPermanent (dead), got %v", err)
}

// Issue #24: a 550 on Rename when the destination is an existing directory
// ("dst is a directory") is permanent and must classify as ErrPermanent. STOR of
// the tmp file succeeds (root parent), but RNTO onto a directory fails 550.
func TestFTPTransport_Send_Rename550DstIsDir_ReturnsErrPermanent(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))

	// Pre-create a directory at the final dst path so Rename(tmp -> dst) 550s.
	require.NoError(t, os.Mkdir(filepath.Join(srv.RootDir, "isdir"), 0o755))

	uri := fmt.Sprintf("ftp://%s/isdir", srv.Addr)
	_, _, err := tr.Send(context.Background(), uri, blockingReader([]byte("payload")), 7)
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent,
		"550 Rename onto existing directory must be ErrPermanent (dead), got %v", err)
}

// Issue #24 (guard): a transient/connection error (dial refused) must stay
// non-sentinel so the worker retries with backoff. The classifier must NOT widen
// to mark dial/conn failures permanent.
func TestFTPTransport_Send_ConnError_StaysRetryable(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))

	// Port 1 is not listening: acquire/dial fails with a connection error.
	_, _, err := tr.Send(context.Background(), "ftp://127.0.0.1:1/x.bin", blockingReader([]byte("y")), 1)
	require.Error(t, err)
	require.NotErrorIs(t, err, transfer.ErrPermanent,
		"dial/conn error must stay retryable (non-sentinel), got %v", err)
	require.NotErrorIs(t, err, transfer.ErrSkippable,
		"dial/conn error must stay retryable (non-sentinel), got %v", err)
	var te *textproto.Error
	require.NotErrorAs(t, err, &te, "dial/conn error must not be a protocol 5xx error")
}

// Issue #22: a cancelled ctx must abort an in-flight STOR PROMPTLY. jlaffaye Stor
// runs io.Copy(dataConn, body); the transport never wires ctx into that copy, so
// cancellation has zero effect and Stor only returns once the body unblocks. This
// reader blocks forever after the first chunk, so on unfixed code Send hangs until
// the deadline; on fixed code Read returns ctx.Err() (or the data conn is closed).
func TestFTPTransport_Send_CtxCancel_AbortsInFlightPromptly(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))

	ctx, cancel := context.WithCancel(context.Background())
	body := newCtxBlockingReader()
	uri := fmt.Sprintf("ftp://%s/slow.bin", srv.Addr)

	done := make(chan error, 1)
	go func() {
		_, _, err := tr.Send(ctx, uri, body, -1)
		done <- err
	}()

	<-body.started
	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "cancelled FTP transfer must return an error, not succeed")
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not abort within 5s of ctx cancel: ctx is not propagated into Stor")
	}
}

// --- test helpers (scoped to this file) ---

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
	<-r.block
	return 0, io.EOF
}
