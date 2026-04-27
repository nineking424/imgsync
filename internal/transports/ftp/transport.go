package ftp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"path"
	"strings"
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
	tee := io.TeeReader(body, cw)

	if err := conn.Stor(strings.TrimPrefix(tmpPath, "/"), tee); err != nil {
		// Best-effort: clean up partial tmp before releasing the broken conn.
		_ = conn.Delete(strings.TrimPrefix(tmpPath, "/"))
		pc.Release(true)
		return 0, "", fmt.Errorf("ftp transport: stor %s: %w", tmpPath, err)
	}
	// STOR ACK reached. Rename tmp → final.
	if err := conn.Rename(strings.TrimPrefix(tmpPath, "/"), strings.TrimPrefix(finalPath, "/")); err != nil {
		// Best-effort cleanup; if delete fails too, leave a tmp for ops.
		_ = conn.Delete(strings.TrimPrefix(tmpPath, "/"))
		pc.Release(true)
		return 0, "", fmt.Errorf("ftp transport: rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	pc.Release(false)
	return cw.n, hex.EncodeToString(hasher.Sum(nil)), nil
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
