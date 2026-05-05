package ftp_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/ftpserver"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	"github.com/stretchr/testify/require"
)

func TestPool_AcquireReleaseRoundTrip(t *testing.T) {
	srv := ftpserver.Start(t)
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   4,
		IdleTTL:      5 * time.Minute,
		NoopAfter:    60 * time.Second,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	pc, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err)

	require.NoError(t, pc.Conn().Stor("a.txt", strings.NewReader("hi")))
	pc.Release(false)

	pc2, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err)
	r, err := pc2.Conn().Retr("a.txt")
	require.NoError(t, err)
	_ = r.Close()
	pc2.Release(false)
}

func TestPool_BrokenConnection_NotReused(t *testing.T) {
	srv := ftpserver.Start(t)
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   4,
		IdleTTL:      5 * time.Minute,
		NoopAfter:    60 * time.Second,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	pc, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err)
	pc.Release(true) // mark broken

	require.Equal(t, 0, pool.IdleCount(srv.Addr), "broken conn must not be returned to idle pool")
}

func TestPool_MaxPerHost_BlocksUntilRelease(t *testing.T) {
	srv := ftpserver.Start(t)
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   2,
		IdleTTL:      5 * time.Minute,
		NoopAfter:    60 * time.Second,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	a, _ := pool.Acquire(context.Background(), srv.Addr)
	b, _ := pool.Acquire(context.Background(), srv.Addr)
	t.Cleanup(func() { a.Release(false); b.Release(false) })

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := pool.Acquire(ctx, srv.Addr)
	require.ErrorIs(t, err, context.DeadlineExceeded, "third acquire must block past cap")
}

func TestPool_OnPoolChangeFiresOnAcquireAndRelease(t *testing.T) {
	type call struct {
		host  string
		inUse int
		idle  int
	}
	var (
		mu    sync.Mutex
		calls []call
	)
	cb := func(host string, inUse, idle int) {
		mu.Lock()
		calls = append(calls, call{host, inUse, idle})
		mu.Unlock()
	}

	srv := ftpserver.Start(t)
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   2,
		IdleTTL:      1 * time.Minute,
		NoopAfter:    1 * time.Minute,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
		OnPoolChange: cb,
	})
	t.Cleanup(pool.Close)

	pc, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err)
	pc.Release(false)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(calls), 2, "expected >=2 callback fires (acquire+release)")
	last := calls[len(calls)-1]
	require.Equal(t, srv.Addr, last.host)
	require.Equal(t, 0, last.inUse, "after release: in_use should be 0")
	require.Equal(t, 1, last.idle, "after release: idle should be 1")
}

func TestPool_IdleTTLExpiry_DiscardsConn(t *testing.T) {
	srv := ftpserver.Start(t)
	pool := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   4,
		IdleTTL:      50 * time.Millisecond, // tiny TTL for the test
		NoopAfter:    60 * time.Second,
		AuthUser:     srv.User,
		AuthPassword: srv.Pass,
	})
	t.Cleanup(pool.Close)

	pc, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err)
	pc.Release(false)
	require.Equal(t, 1, pool.IdleCount(srv.Addr))

	time.Sleep(120 * time.Millisecond)
	pc2, err := pool.Acquire(context.Background(), srv.Addr)
	require.NoError(t, err, "expired idle conn must be discarded and a fresh one created")
	pc2.Release(false)
}
