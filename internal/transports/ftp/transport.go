package ftp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/textproto"
	"net/url"
	"path"
	"strings"

	"github.com/jlaffaye/ftp"
	"github.com/nineking424/imgsync/internal/transfer"
)

// Transport streams bodies to FTP destinations using temp-file + RNFR/RNTO.
type Transport struct {
	pool *Pool
}

// NewTransport binds a Transport to a pool.
func NewTransport(pool *Pool) *Transport { return &Transport{pool: pool} }

// Send streams body to dst (ftp://host/path). The wire path is dst+".imgsync.tmp"
// during STOR and is renamed to the final path on ACK. Returns bytes ACK'd by
// the FTP server and sha256 hex of the streamed bytes.
func (t *Transport) Send(
	ctx context.Context,
	dst string,
	body io.Reader,
	_ int64,
) (int64, string, error) {
	u, err := url.Parse(dst)
	if err != nil || u.Scheme != "ftp" || u.Host == "" || u.Path == "" {
		return 0, "", fmt.Errorf("ftp transport: invalid dst %q", dst)
	}
	host := u.Host
	finalPath := u.Path
	tmpPath := finalPath + ".imgsync.tmp"

	pc, err := t.pool.Acquire(ctx, host)
	if err != nil {
		return 0, "", fmt.Errorf("ftp transport: acquire: %w", err)
	}
	conn := pc.Conn()

	if dir := path.Dir(finalPath); dir != "/" && dir != "." && dir != "" {
		// Best-effort, single-level only. Pre-existing dirs return an error
		// we ignore. Multi-level paths require all parents to already exist;
		// recursive MakeDir would need walking path components and is not
		// worth it for v1 (operators provision target dirs out-of-band).
		_ = conn.MakeDir(dir)
	}

	hasher := sha256.New()
	cw := &countingHashWriter{h: hasher}
	// Wrap with a ctx-aware reader so a cancelled ctx aborts the in-flight STOR
	// promptly: jlaffaye's Stor runs io.Copy(dataConn, body) with no ctx, so the
	// only way to unblock it without a library patch is to make Read return
	// ctx.Err(). Streams (no full-body buffering).
	tee := io.TeeReader(transfer.NewCtxReader(ctx, body), cw)

	if err := conn.Stor(strings.TrimPrefix(tmpPath, "/"), tee); err != nil {
		// Best-effort: clean up partial tmp before releasing the broken conn.
		_ = conn.Delete(strings.TrimPrefix(tmpPath, "/"))
		pc.Release(true)
		return 0, "", fmt.Errorf("ftp transport: stor %s: %w", tmpPath, classify(err))
	}
	// STOR ACK reached. Rename tmp → final.
	if err := conn.Rename(strings.TrimPrefix(tmpPath, "/"), strings.TrimPrefix(finalPath, "/")); err != nil {
		// Best-effort cleanup; if delete fails too, leave a tmp for ops.
		_ = conn.Delete(strings.TrimPrefix(tmpPath, "/"))
		pc.Release(true)
		return 0, "", fmt.Errorf("ftp transport: rename %s -> %s: %w", tmpPath, finalPath, classify(err))
	}
	pc.Release(false)
	return cw.n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// classify maps an FTP STOR/Rename error to a retry disposition. Permanent FTP
// reply codes on a write/rename (550 file unavailable, 552 exceeded storage,
// 553 bad file name — e.g. missing parent dir, dst-is-a-directory) wrap
// transfer.ErrPermanent so the worker marks the job dead instead of burning its
// retry budget. Everything else — transient 4xx replies, dial/connection
// failures, non-protocol errors — is returned unchanged so the worker retries
// with backoff. Matching is on the jlaffaye *textproto.Error reply code, not on
// substrings, to avoid misclassifying message text.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		switch te.Code {
		case ftp.StatusFileUnavailable, ftp.StatusExceededStorage, ftp.StatusBadFileName:
			return fmt.Errorf("%w: %w", err, transfer.ErrPermanent)
		}
	}
	return err
}

type countingHashWriter struct {
	h hash.Hash
	n int64
}

func (w *countingHashWriter) Write(p []byte) (int, error) {
	n, err := w.h.Write(p)
	w.n += int64(n)
	return n, err
}
