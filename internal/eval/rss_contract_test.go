package eval_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/stretchr/testify/require"
)

const fixtureSize = 2 << 30 // 2 GiB

func make2GBSparseFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	// Sparse: seek to last byte, write one byte. POSIX sparse file.
	_, err = f.Seek(int64(fixtureSize)-1, io.SeekStart)
	require.NoError(t, err)
	_, err = f.Write([]byte{0})
	require.NoError(t, err)
	return path
}

// startRSSWatcher samples HeapInuse every 100ms until ctx is done. Returns a
// pointer that holds peak in bytes.
func startRSSWatcher(ctx context.Context) *uint64 {
	var peak uint64
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runtime.ReadMemStats(&ms)
				for {
					old := atomic.LoadUint64(&peak)
					if ms.HeapInuse <= old {
						break
					}
					if atomic.CompareAndSwapUint64(&peak, old, ms.HeapInuse) {
						break
					}
				}
			}
		}
	}()
	return &peak
}

func TestC1_LocalFS_StreamingRSSUnder250MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 2GB streaming RSS test in -short mode")
	}
	srcPath := make2GBSparseFile(t)
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "out.bin")

	src := localfs.NewSource()
	tr := tlocalfs.NewTransport()

	body, srcSize, err := src.Open(context.Background(), srcPath)
	require.NoError(t, err)
	require.Equal(t, int64(fixtureSize), srcSize)
	defer func() { _ = body.Close() }()

	wctx, wcancel := context.WithCancel(context.Background())
	peak := startRSSWatcher(wctx)

	written, _, err := tr.Send(context.Background(), dstPath, body, srcSize)
	wcancel()
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, int64(fixtureSize), written)

	got := atomic.LoadUint64(peak)
	require.LessOrEqual(t, got, uint64(250<<20),
		"C1: HeapInuse peak %d MiB exceeds 250 MiB cap", got>>20)
}

func TestC1_FTP_StreamingRSSUnder250MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 2GB streaming RSS test in -short mode")
	}
	srv := ftpserver.Start(t)
	srcLocalPath := make2GBSparseFile(t)
	srcCopy := filepath.Join(srv.RootDir, "big.bin")
	require.NoError(t, hardLinkOrCopy(srcLocalPath, srcCopy))

	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost: 4, IdleTTL: 5 * time.Minute, NoopAfter: 60 * time.Second,
		AuthUser: srv.User, AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	src := srcftp.NewSource(pool)
	tr := pftp.NewTransport(pool)

	srcURI := fmt.Sprintf("ftp://%s/big.bin", srv.Addr)
	dstURI := fmt.Sprintf("ftp://%s/out.bin", srv.Addr)

	body, srcSize, err := src.Open(context.Background(), srcURI)
	require.NoError(t, err)
	defer func() { _ = body.Close() }()

	wctx, wcancel := context.WithCancel(context.Background())
	peak := startRSSWatcher(wctx)

	_, _, err = tr.Send(context.Background(), dstURI, body, srcSize)
	wcancel()
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, err)

	got := atomic.LoadUint64(peak)
	require.LessOrEqual(t, got, uint64(250<<20),
		"C1: HeapInuse peak %d MiB exceeds 250 MiB cap (FTP path)", got>>20)
}

// hardLinkOrCopy: try hard link first (no IO), fall back to copy.
func hardLinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
