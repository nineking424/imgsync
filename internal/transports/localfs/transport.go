package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Transport writes streaming bodies to the local filesystem with atomic rename.
type Transport struct{}

// NewTransport constructs a LocalFS Transport.
func NewTransport() *Transport { return &Transport{} }

// Send streams body into a tempfile next to dst, fsyncs, and renames atomically.
// Returns bytes written and the sha256 of the streamed bytes (lowercase hex).
func (t *Transport) Send(
	ctx context.Context,
	dst string,
	body io.Reader,
	_ int64,
) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".imgsync-*.tmp")
	if err != nil {
		return 0, "", fmt.Errorf("localfs: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	written, copyErr := io.Copy(mw, body)
	if copyErr != nil {
		_ = tmp.Close()
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: copy: %w", copyErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanupTmp()
		return 0, "", fmt.Errorf("localfs: rename: %w", err)
	}
	return written, hex.EncodeToString(hasher.Sum(nil)), nil
}
