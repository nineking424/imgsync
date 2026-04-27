package ftp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	"github.com/stretchr/testify/require"
)

func newTestPool(t *testing.T, srv *ftpserver.Server) *pftp.Pool {
	t.Helper()
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost: 4, IdleTTL: 5 * time.Minute, NoopAfter: 60 * time.Second,
		AuthUser: srv.User, AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)
	return pool
}

func TestFTPTransport_Send_WritesFileAndReportsBytesAndSha(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))
	body := strings.NewReader("hello world")
	want := sha256.Sum256([]byte("hello world"))

	uri := fmt.Sprintf("ftp://%s/out.txt", srv.Addr)
	written, shaHex, err := tr.Send(context.Background(), uri, body, 11)
	require.NoError(t, err)
	require.Equal(t, int64(11), written)
	require.Equal(t, hex.EncodeToString(want[:]), shaHex)

	got, err := os.ReadFile(filepath.Join(srv.RootDir, "out.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestFTPTransport_Send_AtomicRename_NoTmpAtFinal(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))
	uri := fmt.Sprintf("ftp://%s/big.bin", srv.Addr)
	payload := bytes.Repeat([]byte{'A'}, 1<<20)

	_, _, err := tr.Send(context.Background(), uri, bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)

	entries, err := os.ReadDir(srv.RootDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".imgsync.tmp"),
			"tmp leftover at server root: %s", e.Name())
	}
	got, err := os.ReadFile(filepath.Join(srv.RootDir, "big.bin"))
	require.NoError(t, err)
	require.Equal(t, len(payload), len(got))
}

func TestFTPTransport_Send_BadURI_ReturnsError(t *testing.T) {
	tr := pftp.NewTransport(newTestPool(t, ftpserver.Start(t)))
	_, _, err := tr.Send(context.Background(), "not-a-url", strings.NewReader("x"), 1)
	require.Error(t, err)
}

func TestFTPTransport_Send_BodyError_NoFinalAtDst(t *testing.T) {
	srv := ftpserver.Start(t)
	tr := pftp.NewTransport(newTestPool(t, srv))
	uri := fmt.Sprintf("ftp://%s/fail.bin", srv.Addr)

	_, _, err := tr.Send(context.Background(), uri, &errReader{}, -1)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(srv.RootDir, "fail.bin"))
	require.True(t, errors.Is(statErr, os.ErrNotExist), "final dst must not exist after body error")
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
