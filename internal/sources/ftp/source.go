// Package ftp provides FTPSource for streaming reads from FTP servers.
package ftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/jlaffaye/ftp"
	"github.com/nineking424/imgsync/internal/transfer"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
)

// Source streams files from FTP. Concurrency-safe.
type Source struct {
	pool *pftp.Pool
}

// NewSource binds a Source to a pool.
func NewSource(pool *pftp.Pool) *Source { return &Source{pool: pool} }

// Open parses ftp://host[:port]/path, acquires a pooled conn, calls SIZE then RETR,
// and returns a ReadCloser that releases the conn on Close().
func (s *Source) Open(ctx context.Context, src string) (io.ReadCloser, int64, error) {
	u, err := url.Parse(src)
	if err != nil {
		return nil, 0, fmt.Errorf("ftp source: parse %q: %w", src, transfer.ErrPermanent)
	}
	if u.Scheme != "ftp" {
		return nil, 0, fmt.Errorf("ftp source: scheme %q not supported: %w", u.Scheme, transfer.ErrPermanent)
	}
	host := u.Host
	if host == "" {
		return nil, 0, fmt.Errorf("ftp source: empty host in %q: %w", src, transfer.ErrPermanent)
	}
	path := u.Path
	if path == "" {
		return nil, 0, fmt.Errorf("ftp source: empty path in %q: %w", src, transfer.ErrPermanent)
	}

	pc, err := s.pool.Acquire(ctx, host)
	if err != nil {
		return nil, 0, fmt.Errorf("ftp source: acquire conn: %w", err)
	}
	conn := pc.Conn()

	var size int64 = -1
	if sz, err := conn.FileSize(path); err == nil {
		size = sz
	}

	r, err := conn.Retr(path)
	if err != nil {
		pc.Release(true)
		if isNotFound(err) {
			return nil, 0, fmt.Errorf("ftp source: retr %s: %w", path, transfer.ErrSkippable)
		}
		return nil, 0, fmt.Errorf("ftp source: retr %s: %w", path, err)
	}

	return &retrReader{ReadCloser: r, pc: pc}, size, nil
}

// retrReader releases the pooled conn when the stream is closed.
type retrReader struct {
	io.ReadCloser
	pc       *pftp.PooledConn
	released bool
	ioErr    error
}

func (r *retrReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		r.ioErr = err
	}
	return n, err
}

func (r *retrReader) Close() error {
	closeErr := r.ReadCloser.Close()
	if !r.released {
		r.released = true
		broken := r.ioErr != nil || closeErr != nil
		r.pc.Release(broken)
	}
	return closeErr
}

// isNotFound reports whether err is an FTP "file unavailable" reply (code 550)
// that represents a genuinely missing source, which the worker treats as a
// skippable missing-source. Matching is primarily on the jlaffaye
// *textproto.Error reply code (not message substrings) to avoid misclassifying
// non-English messages. A 550 is generic, however, and also covers permission /
// access denials; those are operator misconfigurations that must surface
// (retry then dead), so they are carved out by message — the only signal the
// 550 code gives to distinguish them.
func isNotFound(err error) bool {
	var te *textproto.Error
	if !errors.As(err, &te) || te.Code != ftp.StatusFileUnavailable {
		return false
	}
	msg := strings.ToLower(te.Msg)
	return !strings.Contains(msg, "permission") && !strings.Contains(msg, "access denied")
}

// IsNotFoundForTest is exposed for unit testing the not-found classifier.
// Do not use in production paths.
func IsNotFoundForTest(err error) bool { return isNotFound(err) }
