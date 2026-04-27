package ftp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/stretchr/testify/require"
)

func newPool(t *testing.T, srv *ftpserver.Server) *pftp.Pool {
	t.Helper()
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   4,
		IdleTTL:      5 * time.Minute,
		NoopAfter:    60 * time.Second,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)
	return pool
}

func TestFTPSource_Open_StreamsAndReportsSize(t *testing.T) {
	srv := ftpserver.Start(t)
	require.NoError(t, os.WriteFile(filepath.Join(srv.RootDir, "x.bin"), []byte("hello world"), 0o644))

	s := srcftp.NewSource(newPool(t, srv))
	uri := fmt.Sprintf("ftp://%s/x.bin", srv.Addr)

	body, size, err := s.Open(context.Background(), uri)
	require.NoError(t, err)
	t.Cleanup(func() { _ = body.Close() })

	require.Equal(t, int64(11), size)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))
}

func TestFTPSource_Open_Missing_ReturnsErrSkippable(t *testing.T) {
	srv := ftpserver.Start(t)
	s := srcftp.NewSource(newPool(t, srv))
	uri := fmt.Sprintf("ftp://%s/nope.bin", srv.Addr)

	_, _, err := s.Open(context.Background(), uri)
	require.Error(t, err)
	require.True(t, errors.Is(err, transfer.ErrSkippable),
		"missing source file must return ErrSkippable, got %v", err)
}

func TestFTPSource_Open_BadURI_ReturnsErrPermanent(t *testing.T) {
	s := srcftp.NewSource(newPool(t, ftpserver.Start(t)))
	_, _, err := s.Open(context.Background(), "not-a-url")
	require.ErrorIs(t, err, transfer.ErrPermanent)
}

func TestFTPSource_Close_ReturnsConnToPool(t *testing.T) {
	srv := ftpserver.Start(t)
	require.NoError(t, os.WriteFile(filepath.Join(srv.RootDir, "y.txt"), []byte("y"), 0o644))
	pool := newPool(t, srv)

	s := srcftp.NewSource(pool)
	uri := fmt.Sprintf("ftp://%s/y.txt", srv.Addr)
	body, _, err := s.Open(context.Background(), uri)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, body)
	require.NoError(t, body.Close())

	require.Eventually(t, func() bool {
		return pool.IdleCount(srv.Addr) >= 1
	}, time.Second, 10*time.Millisecond, "conn must return to idle after Close")
}

func TestFTPSource_BadURIScheme_ReturnsErrPermanent(t *testing.T) {
	s := srcftp.NewSource(newPool(t, ftpserver.Start(t)))
	_, _, err := s.Open(context.Background(), "http://example.com/x.bin")
	require.True(t, errors.Is(err, transfer.ErrPermanent),
		"non-ftp scheme must be ErrPermanent, got %v", err)
}

func TestFTPSource_isNotFound_NarrowsOn550Permission(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"missing-no-such-file", "550 No such file or directory", true},
		{"missing-not-found", "550 file not found", true},
		{"missing-bare-550", "550 Requested action not taken", true},
		{"permission-denied", "550 Permission denied", false},
		{"access-denied", "550 Access denied", false},
		{"file-unavailable", "550 File unavailable", true},
		{"non-550-permission", "530 Login incorrect", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := srcftp.IsNotFoundForTest(errors.New(tc.msg))
			require.Equal(t, tc.want, got, "msg=%q", tc.msg)
		})
	}
}
