package localfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/stretchr/testify/require"
)

func TestTransport_Send_WritesFileAndReportsBytesAndSha(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.txt")
	body := strings.NewReader("hello world")
	want := sha256.Sum256([]byte("hello world"))

	tr := localfs.NewTransport()
	written, shaHex, err := tr.Send(context.Background(), dst, body, 11)
	require.NoError(t, err)

	require.Equal(t, int64(11), written)
	require.Equal(t, hex.EncodeToString(want[:]), shaHex)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestTransport_Send_AtomicRename_NoPartialAtDst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "atomic.bin")
	tr := localfs.NewTransport()

	// 1 MiB body to make any partial write observable
	payload := bytes.Repeat([]byte{'A'}, 1<<20)
	_, _, err := tr.Send(context.Background(), dst, bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)

	got, _ := os.ReadFile(dst)
	require.Equal(t, len(payload), len(got))

	// no temp leftovers
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover temp file: %s", e.Name())
	}
}

func TestTransport_Send_DstDirMissing_ReturnsError(t *testing.T) {
	tr := localfs.NewTransport()
	_, _, err := tr.Send(context.Background(), "/no/such/dir/out.txt", strings.NewReader("x"), 1)
	require.Error(t, err)
}

func TestTransport_Send_BodyError_DoesNotCreateDst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fail.bin")
	tr := localfs.NewTransport()

	_, _, err := tr.Send(context.Background(), dst, &errReader{}, -1)
	require.Error(t, err)

	_, statErr := os.Stat(dst)
	require.ErrorIs(t, statErr, os.ErrNotExist, "dst created despite body error")
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
