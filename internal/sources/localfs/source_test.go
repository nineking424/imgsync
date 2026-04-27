package localfs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/stretchr/testify/require"
)

func TestSource_Open_StreamsFileAndReportsSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))

	s := localfs.NewSource()
	body, size, err := s.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = body.Close() })

	require.Equal(t, int64(11), size)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestSource_Open_Missing_ReturnsErrSkippable(t *testing.T) {
	s := localfs.NewSource()
	_, _, err := s.Open(context.Background(), "/no/such/path/xyzzy")
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrSkippable)
}

func TestSource_Open_Directory_ReturnsErrPermanent(t *testing.T) {
	s := localfs.NewSource()
	_, _, err := s.Open(context.Background(), t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, transfer.ErrPermanent)
}
