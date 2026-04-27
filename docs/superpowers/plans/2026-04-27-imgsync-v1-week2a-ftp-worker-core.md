# imgsync v1 — Week 2A: FTP and Worker Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the FTP Source/Transport (with per-host connection pool and NOOP ping policy), the worker dispatch SQL with lease/return semantics, and the per-job processor that streams Source → Transport with sha256 + byte-count + size-mismatch verification (D6/F4). Wire `imgsync worker` to drain the queue with N goroutines.

**Architecture:** `internal/transports/ftp` owns the connection pool plus FTPSource/FTPTransport using `jlaffaye/ftp`. `internal/worker` owns dispatch SQL (FOR UPDATE SKIP LOCKED), the per-job processor (Source.Open → counting reader → Transport.Send → state write), and the loop scaffolding for `imgsync worker`. Sweeper, idle backoff, FTP host cluster cap, health endpoints, and EVAL invariants are deferred to Week 2B — this plan keeps the worker single-pod, naive-sleep-on-idle so each piece can land independently.

**Tech Stack:** `jlaffaye/ftp` for FTP client, `fclairamb/ftpserverlib` for in-process FTP test server (no docker required), pgx/v5 for DB, `crypto/sha256` for hashing.

**Series:** This is plan 2A of 4 for v1 base. Predecessor: `2026-04-27-imgsync-v1-week1-foundation.md` (must be complete and green). Successors: Week 2B (sweeper, idle backoff, FTP host cap, health, EVAL invariants), Week 3 (Helm + cutover).

**Spec reference:** `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md` (rev 4 APPROVED). Sections: "Source / Transport interfaces", "Worker 처리 흐름 (single job)", "Worker dispatch SQL", "FTP Connection Pool".

---

## File Structure

After Week 2A completes, the new tree under `internal/` is:

```
internal/
├── ftpserver/                       # in-process FTP server for tests
│   └── testserver.go
├── transports/ftp/
│   ├── pool.go                      # per-host pool, max conns, idle TTL, NOOP ping
│   ├── pool_test.go
│   ├── transport.go                 # FTPTransport (STOR streaming, sha256 + bytes)
│   └── transport_test.go
├── sources/ftp/
│   ├── source.go                    # FTPSource (SIZE + RETR streaming)
│   └── source_test.go
└── worker/
    ├── job.go                       # Job struct + LeaseJob (FOR UPDATE SKIP LOCKED)
    ├── job_test.go
    ├── process.go                   # ProcessJob: Source → counting → Transport → verify → state
    ├── process_test.go
    ├── runner.go                    # WorkerLoop: N goroutines, sleep-on-idle (Week 2B replaces with backoff)
    └── runner_test.go

cmd/imgsync/worker.go                # `imgsync worker` subcommand
```

Boundaries: `internal/sources/ftp` only knows how to open a stream; `internal/transports/ftp` only knows how to write one; the pool sits inside `internal/transports/ftp` because that's where conns are most numerous and pinning will matter in Week 2B. `internal/worker` orchestrates Source→Transport, knows nothing about FTP internals.

---

## Task 1: In-process FTP test server fixture

**Files:**
- Create: `internal/ftpserver/testserver.go`
- Modify: `go.mod` (add `fclairamb/ftpserverlib`, `spf13/afero`)

The Week 1 streaming guard forbids `io.ReadAll` everywhere except tests. We need a way to run FTP integration tests without docker (CI speed, dev ergonomics). `fclairamb/ftpserverlib` runs an FTP server inside the test process backed by a virtual filesystem from `spf13/afero`. We build a tiny `StartTestServer(t)` helper that returns the connection details and a writable backing dir.

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/fclairamb/ftpserverlib@latest
go get github.com/spf13/afero@latest
go get github.com/jlaffaye/ftp@latest
```

- [ ] **Step 2: Write the failing test**

Create `internal/ftpserver/testserver_test.go`:

```go
package ftpserver_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/jlaffaye/ftp"
	"github.com/nineking424/imgsync/internal/ftpserver"
	"github.com/stretchr/testify/require"
)

func TestStartTestServer_StoreThenRetrieve(t *testing.T) {
	srv := ftpserver.Start(t)

	c, err := ftp.Dial(srv.Addr)
	require.NoError(t, err)
	defer func() { _ = c.Quit() }()
	require.NoError(t, c.Login(srv.User, srv.Pass))

	require.NoError(t, c.Stor("hello.txt", strings.NewReader("hello world")))

	r, err := c.Retr("hello.txt")
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, []byte("hello world")), "got=%q", got)
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/ftpserver/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Write `internal/ftpserver/testserver.go`**

```go
// Package ftpserver runs an in-process FTP server for tests. Not for prod use.
package ftpserver

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// Server describes a running test FTP server.
type Server struct {
	Addr    string // host:port for ftp.Dial
	User    string
	Pass    string
	RootDir string // host filesystem path where uploads land
}

// Start spins up an FTP server bound to a random localhost port using a
// per-test temp dir as the backing filesystem. The server is shut down via
// t.Cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ftpserver: listen: %v", err)
	}

	driver := &driver{
		root:     root,
		user:     "imgsync",
		pass:     "imgsync",
		listener: listener,
	}
	srv := ftpserver.NewFtpServer(driver)
	driver.server = srv

	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("ftpserver: serve exited: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Stop() })

	return &Server{
		Addr:    listener.Addr().String(),
		User:    driver.user,
		Pass:    driver.pass,
		RootDir: root,
	}
}

type driver struct {
	root     string
	user     string
	pass     string
	listener net.Listener
	server   *ftpserver.FtpServer
}

func (d *driver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{
		Listener:               d.listener,
		DefaultTransferType:    ftpserver.TransferTypeBinary,
		DisableMLSD:            true,
		DisableMLST:            true,
	}, nil
}

func (d *driver) ClientConnected(_ ftpserver.ClientContext) (string, error) {
	return "imgsync test ftp", nil
}

func (d *driver) ClientDisconnected(_ ftpserver.ClientContext) {}

func (d *driver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != d.user || pass != d.pass {
		return nil, fmt.Errorf("auth: bad credentials")
	}
	root := d.root
	return &clientDriver{Fs: afero.NewBasePathFs(afero.NewOsFs(), root)}, nil
}

func (d *driver) GetTLSConfig() (any, error) { return nil, errors.New("tls not configured") }

type clientDriver struct {
	afero.Fs
}

// AbsPath helpers used by older ftpserverlib versions; modern versions use
// afero.Fs methods directly. Kept defensive for forward compat.
func (c *clientDriver) AbsPath(p string) string { return filepath.Clean("/" + p) }
```

> **Library note:** `fclairamb/ftpserverlib` v0.x ABI changes occasionally. If the build fails because the `MainDriver` interface added a method after Jan 2026 (e.g., `PreAuthUser`), implement the missing method as `return nil` or a sane default. Do not pin to an unreleased version — fix the local code.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/ftpserver/... -v`
Expected: PASS. The test does upload + download via `jlaffaye/ftp`.

- [ ] **Step 6: Commit**

```bash
git add internal/ftpserver/ go.mod go.sum
git commit -m "test(ftpserver): add in-process FTP test server fixture"
```

---

## Task 2: FTP connection pool

**Files:**
- Create: `internal/transports/ftp/pool.go`, `internal/transports/ftp/pool_test.go`

Spec: per-host max=4 connections, idle TTL=5min, NOOP ping only when last-used >60s ago. Pool returns conns via `Acquire(ctx, host) → *PooledConn`; the caller calls `pc.Release(broken bool)` when done. A broken release closes the underlying conn instead of returning it.

The pool does NOT yet implement the cluster-wide host concurrency cap (Week 2B adds the advisory-lock semaphore on top of this).

- [ ] **Step 1: Write the failing test**

Create `internal/transports/ftp/pool_test.go`:

```go
package ftp_test

import (
	"context"
	"strings"
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

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := pool.Acquire(ctx, srv.Addr)
	require.ErrorIs(t, err, context.DeadlineExceeded, "third acquire must block past cap")
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transports/ftp/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/transports/ftp/pool.go`**

```go
// Package ftp owns the FTP connection pool plus FTPTransport implementation.
package ftp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// PoolConfig configures a per-host FTP connection pool.
type PoolConfig struct {
	MaxPerHost   int           // hard cap on concurrent in-use + idle conns per host
	IdleTTL      time.Duration // discard idle conns older than this
	NoopAfter    time.Duration // NOOP-ping a conn on Acquire if last-used was >NoopAfter ago
	AuthUser     string
	AuthPassword string
	DialTimeout  time.Duration // optional; defaults to 10s
}

// PooledConn wraps a live FTP connection plus its checkout state.
type PooledConn struct {
	c        *ftp.ServerConn
	host     string
	pool     *Pool
	lastUsed time.Time
}

// Conn exposes the underlying jlaffaye/ftp connection.
func (p *PooledConn) Conn() *ftp.ServerConn { return p.c }

// Release returns the conn to the pool. If broken=true, the conn is closed
// instead of being returned to idle.
func (p *PooledConn) Release(broken bool) {
	if p == nil || p.c == nil {
		return
	}
	p.pool.release(p.host, p.c, broken)
	p.c = nil
}

// Pool is a per-host FTP connection pool.
type Pool struct {
	cfg PoolConfig

	mu    sync.Mutex
	hosts map[string]*hostPool
	closed bool
}

type hostPool struct {
	idle    []*idleEntry
	inUse   int
	waiters []chan struct{}
}

type idleEntry struct {
	c       *ftp.ServerConn
	enqueue time.Time // when added to idle (idle TTL clock)
	lastUse time.Time // when last released (NOOP-after clock)
}

// NewPool builds a pool. Caller MUST call Close on shutdown.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.MaxPerHost <= 0 {
		cfg.MaxPerHost = 4
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 5 * time.Minute
	}
	if cfg.NoopAfter <= 0 {
		cfg.NoopAfter = 60 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Pool{
		cfg:   cfg,
		hosts: make(map[string]*hostPool),
	}
}

// Close drops all idle conns. In-use conns are not forcibly killed; callers
// release them and the close happens on Release.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, hp := range p.hosts {
		for _, ie := range hp.idle {
			_ = ie.c.Quit()
		}
		hp.idle = nil
	}
}

// IdleCount returns the number of idle conns held for host. Test helper.
func (p *Pool) IdleCount(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if hp, ok := p.hosts[host]; ok {
		return len(hp.idle)
	}
	return 0
}

// Acquire returns a usable connection to host. Blocks if MaxPerHost is reached.
func (p *Pool) Acquire(ctx context.Context, host string) (*PooledConn, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("ftp pool: closed")
		}
		hp := p.getHost(host)

		// Try to take an idle conn.
		for len(hp.idle) > 0 {
			ie := hp.idle[len(hp.idle)-1]
			hp.idle = hp.idle[:len(hp.idle)-1]
			if time.Since(ie.enqueue) > p.cfg.IdleTTL {
				_ = ie.c.Quit()
				continue
			}
			// Found a candidate. NOOP only if stale.
			conn := ie.c
			needsPing := time.Since(ie.lastUse) > p.cfg.NoopAfter
			hp.inUse++
			p.mu.Unlock()

			if needsPing {
				if err := conn.NoOp(); err != nil {
					_ = conn.Quit()
					p.mu.Lock()
					p.hosts[host].inUse--
					p.mu.Unlock()
					// loop and try again (idle or fresh)
					continue
				}
			}
			return &PooledConn{c: conn, host: host, pool: p, lastUsed: time.Now()}, nil
		}

		// No idle. Can we open a new one?
		if hp.inUse < p.cfg.MaxPerHost {
			hp.inUse++
			p.mu.Unlock()
			conn, err := p.dial(ctx, host)
			if err != nil {
				p.mu.Lock()
				p.hosts[host].inUse--
				p.mu.Unlock()
				return nil, err
			}
			return &PooledConn{c: conn, host: host, pool: p, lastUsed: time.Now()}, nil
		}

		// At cap. Wait for a release.
		ch := make(chan struct{}, 1)
		hp.waiters = append(hp.waiters, ch)
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			// Best-effort waiter eviction.
			p.mu.Lock()
			if hp2, ok := p.hosts[host]; ok {
				for i, w := range hp2.waiters {
					if w == ch {
						hp2.waiters = append(hp2.waiters[:i], hp2.waiters[i+1:]...)
						break
					}
				}
			}
			p.mu.Unlock()
			return nil, ctx.Err()
		case <-ch:
			// Loop and retry.
		}
	}
}

func (p *Pool) getHost(host string) *hostPool {
	hp, ok := p.hosts[host]
	if !ok {
		hp = &hostPool{}
		p.hosts[host] = hp
	}
	return hp
}

func (p *Pool) dial(ctx context.Context, host string) (*ftp.ServerConn, error) {
	dialOpts := []ftp.DialOption{
		ftp.DialWithTimeout(p.cfg.DialTimeout),
		ftp.DialWithContext(ctx),
	}
	c, err := ftp.Dial(host, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("ftp dial %s: %w", host, err)
	}
	if err := c.Login(p.cfg.AuthUser, p.cfg.AuthPassword); err != nil {
		_ = c.Quit()
		return nil, fmt.Errorf("ftp login %s: %w", host, err)
	}
	return c, nil
}

func (p *Pool) release(host string, c *ftp.ServerConn, broken bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	hp, ok := p.hosts[host]
	if !ok {
		_ = c.Quit()
		return
	}
	hp.inUse--
	if broken || p.closed {
		_ = c.Quit()
	} else {
		hp.idle = append(hp.idle, &idleEntry{
			c:       c,
			enqueue: time.Now(),
			lastUse: time.Now(),
		})
	}
	// Wake one waiter if any.
	if len(hp.waiters) > 0 {
		ch := hp.waiters[0]
		hp.waiters = hp.waiters[1:]
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transports/ftp/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transports/ftp/pool.go internal/transports/ftp/pool_test.go
git commit -m "feat(transports/ftp): add per-host connection pool with idle TTL and NOOP ping"
```

---

## Task 3: FTPSource

**Files:**
- Create: `internal/sources/ftp/source.go`, `internal/sources/ftp/source_test.go`

`FTPSource.Open` parses an `ftp://host[:port]/path` URI, acquires a pool conn, calls `SIZE` (returning -1 if unsupported or errored), and starts a `RETR` returning a `ReadCloser`. The `ReadCloser` MUST release the pooled conn on `Close()`. If RETR fails because the remote path doesn't exist, return `ErrSkippable` (sourcefile-not-found is a normal operational case per design).

- [ ] **Step 1: Write the failing test**

Create `internal/sources/ftp/source_test.go`:

```go
package ftp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// silence unused imports for tests that don't use strings:
var _ = strings.Reader{}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sources/ftp/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/sources/ftp/source.go`**

```go
// Package ftp provides FTPSource for streaming reads from FTP servers.
package ftp

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

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

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// jlaffaye/ftp surfaces server reply codes in the error string. 550 is the
	// classic "file unavailable / not found" code. Match also "no such".
	return strings.Contains(msg, "550") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "not found")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sources/ftp/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sources/ftp/
git commit -m "feat(sources/ftp): implement streaming FTPSource with pool-aware Close"
```

---

## Task 4: FTPTransport

**Files:**
- Create: `internal/transports/ftp/transport.go`, `internal/transports/ftp/transport_test.go`

`FTPTransport.Send` parses `ftp://host/path`, acquires a conn, ensures parent directories via `MKD`, and streams `body` into a temp file via `STOR <path>.imgsync.tmp`. After STOR ACK, it `RENAME` to the final dst. sha256 is computed in-stream via a MultiWriter; the byte counter is the writer's own counter. On any failure, the conn is released as broken.

The Worker (Task 6) layers a counting reader OUTSIDE Send so it has its own `bytesRead` independent of `writtenBytes` — this is what F4 size verification compares.

- [ ] **Step 1: Write the failing test**

Create `internal/transports/ftp/transport_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transports/ftp/... -run TestFTPTransport -v`
Expected: FAIL — `pftp.NewTransport` undefined.

- [ ] **Step 3: Write `internal/transports/ftp/transport.go`**

```go
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
		_ = conn.MakeDir(dir) // best-effort; existing dir is OK
	}

	hasher := sha256.New()
	cw := &countingHashWriter{h: hasher}
	tee := io.TeeReader(body, cw)

	if err := conn.Stor(strings.TrimPrefix(tmpPath, "/"), tee); err != nil {
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
```

> **Note on writtenBytes semantics:** The design defines `writtenBytes` as "bytes ACK'd by remote (FTP STOR success boundary)". `jlaffaye/ftp.Stor` does not return a byte count. We count bytes the TeeReader actually copied. The STOR ACK confirms the server received those bytes; we treat them as semantically equivalent. If a future audit needs server-side byte counts, switch to `Stor` + `MLST`/`SIZE` recheck — out of scope for v1.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transports/ftp/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transports/ftp/transport.go internal/transports/ftp/transport_test.go
git commit -m "feat(transports/ftp): implement streaming FTPTransport with temp-file + RNFR/RNTO"
```

---

## Task 5: Worker job model + dispatch SQL

**Files:**
- Create: `internal/worker/job.go`, `internal/worker/job_test.go`

The dispatch query is the load-bearing claim for at-most-once processing. Reproduce the spec SQL exactly. `LeaseJob` returns `(nil, nil)` when the queue is empty — callers (Week 2B idle backoff, today: a naive 1s sleep) decide what to do.

- [ ] **Step 1: Write the failing test**

Create `internal/worker/job_test.go`:

```go
package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tc "github.com/testcontainers/testcontainers-go"
)

func mustDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("imgsync_test"),
		postgres.WithUsername("imgsync"),
		postgres.WithPassword("imgsync"),
		tc.WithWaitStrategy(postgres.DefaultWaitStrategy(30*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, db.ApplyMigrations(ctx, dsn, "../../migrations"))
	pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestLeaseJob_EmptyQueue_ReturnsNil(t *testing.T) {
	pool := mustDB(t)
	j, err := worker.LeaseJob(context.Background(), pool, "worker-1")
	require.NoError(t, err)
	require.Nil(t, j)
}

func TestLeaseJob_PendingRow_TransitionsToLeased(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	id, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-1", Src: "localfs:///in/a", Dst: "localfs:///out/a",
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
	})
	require.NoError(t, err)

	j, err := worker.LeaseJob(ctx, pool, "worker-1")
	require.NoError(t, err)
	require.NotNil(t, j)
	require.Equal(t, id, j.ID)
	require.Equal(t, "leased", j.Status)
	require.Equal(t, "worker-1", j.LockedBy)
	require.NotNil(t, j.LockedAt)

	// Second lease must return nil (no pending rows left).
	j2, err := worker.LeaseJob(ctx, pool, "worker-2")
	require.NoError(t, err)
	require.Nil(t, j2)
}

func TestLeaseJob_FutureNextRunAt_NotLeased(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "t-future", Src: "x", Dst: "y",
		SrcProtocol: "localfs", DstProtocol: "localfs",
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`UPDATE transfer_jobs SET next_run_at = NOW() + INTERVAL '1 hour' WHERE trace_id='t-future'`)
	require.NoError(t, err)

	j, err := worker.LeaseJob(ctx, pool, "worker-1")
	require.NoError(t, err)
	require.Nil(t, j, "future next_run_at must not be leased")
}

func TestLeaseJob_ConcurrentLeases_DoNotCollide(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: string(rune('a' + i)), Src: "x", Dst: "y" + string(rune('a'+i)),
			SrcProtocol: "localfs", DstProtocol: "localfs",
		})
		require.NoError(t, err)
	}

	type result struct {
		id  int64
		err error
	}
	const N = 4
	out := make(chan result, N*5)
	for w := 0; w < N; w++ {
		go func(idx int) {
			for k := 0; k < 5; k++ {
				j, err := worker.LeaseJob(ctx, pool, "worker-x")
				if j == nil {
					out <- result{0, err}
					continue
				}
				out <- result{j.ID, err}
			}
		}(w)
	}
	seen := map[int64]int{}
	for i := 0; i < N*5; i++ {
		r := <-out
		require.NoError(t, r.err)
		if r.id != 0 {
			seen[r.id]++
		}
	}
	for id, cnt := range seen {
		require.Equal(t, 1, cnt, "job %d leased %d times — SKIP LOCKED contract violated", id, cnt)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/worker/job.go`**

```go
// Package worker owns dispatch, per-job processing, and the worker loop.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is a snapshot of a transfer_jobs row at lease time.
type Job struct {
	ID           int64
	TraceID      string
	Src          string
	Dst          string
	SrcProtocol  string
	DstProtocol  string
	Payload      []byte
	Status       string
	Attempts     int
	MaxAttempts  int
	LockedAt     *time.Time
	LockedBy     string
	NextRunAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// LeaseJob runs the spec dispatch SQL: pick the oldest pending row whose
// next_run_at has come due, mark it leased, and return its full row.
// Returns (nil, nil) when the queue is empty.
func LeaseJob(ctx context.Context, pool *pgxpool.Pool, lockedBy string) (*Job, error) {
	var j Job
	err := pool.QueryRow(ctx, `
WITH next AS (
  SELECT id FROM transfer_jobs
  WHERE status='pending' AND next_run_at <= NOW()
  ORDER BY next_run_at, id
  FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE transfer_jobs j
SET status='leased', locked_at=NOW(), locked_by=$1, updated_at=NOW()
FROM next WHERE j.id = next.id
RETURNING j.id, j.trace_id, j.src, j.dst, j.src_protocol, j.dst_protocol,
          j.payload, j.status, j.attempts, j.max_attempts,
          j.locked_at, j.locked_by, j.next_run_at, j.created_at, j.updated_at`,
		lockedBy,
	).Scan(
		&j.ID, &j.TraceID, &j.Src, &j.Dst, &j.SrcProtocol, &j.DstProtocol,
		&j.Payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.LockedAt, &j.LockedBy, &j.NextRunAt, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lease: %w", err)
	}
	return &j, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worker/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/job.go internal/worker/job_test.go
git commit -m "feat(worker): implement LeaseJob with FOR UPDATE SKIP LOCKED dispatch"
```

---

## Task 6: Per-job processor

**Files:**
- Create: `internal/worker/process.go`, `internal/worker/process_test.go`

`ProcessJob` is the orchestration that turns a leased Job into a terminal status. The control flow comes from the design's "Worker 처리 흐름 (single job)" section. Order of operations:

1. `Source.Open(src)` — error classification:
   - `ErrSkippable` → write `skipped` + `skip` event.
   - `ErrPermanent` → write `dead` + `dead` event, attempts++.
   - retryable → `handleFailure` (basic version: attempts++, status='dead' if maxAttempts hit, else status='pending' with backoff `1<<attempts` seconds).
2. Wrap the Source reader in a counting reader so we know `bytesRead` independent of Transport.
3. `Transport.Send(dst, counter, srcSize)` returns `writtenBytes`, `sha256Hex`.
4. F4 size verification:
   - srcSize >= 0: `writtenBytes != srcSize` → ErrPermanent (treated as truncated transfer).
   - srcSize == -1: `bytesRead != writtenBytes` → ErrPermanent.
5. On success: write `succeeded` + `success` event with `{size, sha256, duration_ms}`.

The Source/Transport are injected via a `Deps` struct so tests can use LocalFS without FTP.

- [ ] **Step 1: Write the failing test**

Create `internal/worker/process_test.go`:

```go
package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/transfer"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

func enqueueLocal(t *testing.T, pool *pgxpool.Pool, traceID, src, dst string, max int) int64 {
	t.Helper()
	id, _, err := jobs.Enqueue(context.Background(), pool, jobs.EnqueueArgs{
		TraceID: traceID, Src: src, Dst: dst,
		SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: max,
	})
	require.NoError(t, err)
	return id
}

func mustEvent(t *testing.T, pool *pgxpool.Pool, jobID int64, status string) {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transfer_events WHERE job_id=$1 AND status=$2`, jobID, status,
	).Scan(&n))
	require.Equal(t, 1, n, "expected one transfer_events row with status=%q for job %d", status, jobID)
}

func TestProcessJob_Success_TransitionsToSucceededWithEvent(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "ok-1", srcPath, dstPath, 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)
	require.NotNil(t, job)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status))
	require.Equal(t, "succeeded", status)

	mustEvent(t, pool, job.ID, "success")

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(got))

	want := sha256.Sum256([]byte("hello world"))
	var detail []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT detail FROM transfer_events WHERE job_id=$1 AND status='success'`, job.ID,
	).Scan(&detail))
	require.Contains(t, string(detail), hex.EncodeToString(want[:]))
}

func TestProcessJob_SourceMissing_TransitionsToSkippedNoAttemptsBump(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	enqueueLocal(t, pool, "skip-1", "/no/such/src", "/tmp/imgsync-skip-out", 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "skipped", status)
	require.Equal(t, 0, attempts, "skipped must not bump attempts")
	mustEvent(t, pool, job.ID, "skip")
}

func TestProcessJob_PermanentSrcError_TransitionsToDeadAttemptsBumped(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	// Permanent: source path is a directory.
	enqueueLocal(t, pool, "perm-1", t.TempDir(), "/tmp/imgsync-perm", 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "dead", status)
	require.Equal(t, 1, attempts, "permanent err must bump attempts once on the dead transition")
	mustEvent(t, pool, job.ID, "dead")
}

func TestProcessJob_RetryableTransportError_BackoffPending(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	enqueueLocal(t, pool, "retry-1", srcPath, "/no/such/dst-dir/out", 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "pending", status)
	require.Equal(t, 1, attempts)
	mustEvent(t, pool, job.ID, "fail")
}

func TestProcessJob_RetryableHitsMaxAttempts_TransitionsToDead(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	enqueueLocal(t, pool, "exhaust-1", srcPath, "/no/such/dst-dir/out", 1) // max_attempts=1
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: tlocalfs.NewTransport(),
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status, &attempts))
	require.Equal(t, "dead", status)
	require.Equal(t, 1, attempts)
	mustEvent(t, pool, job.ID, "dead")
}

func TestProcessJob_TruncatedTransfer_PermanentDead(t *testing.T) {
	// Verifies F4: writtenBytes != srcSize must demote to ErrPermanent.
	pool := mustDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "in.txt")
	dstPath := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("hello world"), 0o644))

	enqueueLocal(t, pool, "trunc-1", srcPath, dstPath, 3)
	job, err := worker.LeaseJob(ctx, pool, "w-1")
	require.NoError(t, err)

	deps := worker.Deps{
		Pool:      pool,
		LockedBy:  "w-1",
		Source:    localfs.NewSource(),
		Transport: &truncatingTransport{actual: 5}, // claims 5 bytes ACK'd vs srcSize=11
	}
	require.NoError(t, worker.ProcessJob(ctx, deps, job))

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM transfer_jobs WHERE id=$1`, job.ID,
	).Scan(&status))
	require.Equal(t, "dead", status, "truncated transfer must be classified ErrPermanent (dead)")
	mustEvent(t, pool, job.ID, "dead")
}

type truncatingTransport struct{ actual int64 }

func (t *truncatingTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body) // consume reader so callers don't deadlock
	return t.actual, "deadbeef", nil
}

// silence unused
var _ = fmt.Sprintf
var _ = transfer.ErrPermanent
var _ = errors.Is
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/... -run TestProcessJob -v`
Expected: FAIL — `worker.ProcessJob`, `worker.Deps` undefined.

- [ ] **Step 3: Write `internal/worker/process.go`**

```go
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

// Deps is what ProcessJob needs from the outside world.
type Deps struct {
	Pool      *pgxpool.Pool
	LockedBy  string
	Source    transfer.Source
	Transport transfer.Transport
}

// ProcessJob drives a single leased job to a terminal status. It never returns
// the worker-loop error from a job-level outcome — only DB write failures
// propagate. Terminal status writes use a tx so events match status.
func ProcessJob(ctx context.Context, d Deps, job *Job) error {
	start := time.Now()

	body, srcSize, openErr := d.Source.Open(ctx, job.Src)
	if openErr != nil {
		return classifyAndWrite(ctx, d, job, openErr, openErrDetails(openErr), start)
	}
	defer func() { _ = body.Close() }()

	cw := &counter{r: body}
	written, shaHex, sendErr := d.Transport.Send(ctx, job.Dst, cw, srcSize)
	if sendErr != nil {
		return classifyAndWrite(ctx, d, job, sendErr, transportErrDetails(sendErr), start)
	}

	// F4 size verification.
	if srcSize >= 0 && written != srcSize {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: src=%d written=%d: %w", srcSize, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch", "src_size": srcSize, "written": written},
			start)
	}
	if srcSize < 0 && cw.n != written {
		return classifyAndWrite(ctx, d, job,
			fmt.Errorf("size mismatch: read=%d written=%d: %w", cw.n, written, transfer.ErrPermanent),
			map[string]any{"reason": "size_mismatch_unknown_src", "read": cw.n, "written": written},
			start)
	}

	return writeSuccess(ctx, d, job, written, shaHex, start)
}

// counter wraps an io.Reader to record bytes read.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func writeSuccess(ctx context.Context, d Deps, job *Job, written int64, shaHex string, start time.Time) error {
	detail, _ := json.Marshal(map[string]any{
		"size":        written,
		"sha256":      shaHex,
		"duration_ms": time.Since(start).Milliseconds(),
	})
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs SET status='succeeded', locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,'success',$3)`,
		job.TraceID, job.ID, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func classifyAndWrite(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any, _ time.Time) error {
	switch {
	case errors.Is(jobErr, transfer.ErrSkippable):
		return writeTerminal(ctx, d, job, "skipped", "skip", detail, false)
	case errors.Is(jobErr, transfer.ErrPermanent):
		return writeTerminal(ctx, d, job, "dead", "dead", detail, true)
	default:
		return writeRetryOrDead(ctx, d, job, jobErr, detail)
	}
}

func writeRetryOrDead(ctx context.Context, d Deps, job *Job, jobErr error, detail map[string]any) error {
	nextAttempts := job.Attempts + 1
	if nextAttempts >= job.MaxAttempts {
		return writeTerminalWithAttempts(ctx, d, job, "dead", "dead", detail, nextAttempts)
	}
	backoff := time.Duration(1<<nextAttempts) * time.Second // 2,4,8,16,32...
	detailJSON, _ := json.Marshal(detail)
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs
SET status='pending', attempts=$2, next_run_at=NOW()+$3::INTERVAL,
    locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID, nextAttempts, fmt.Sprintf("%d seconds", int(backoff.Seconds()))); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,'fail',$3)`,
		job.TraceID, job.ID, detailJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func writeTerminal(ctx context.Context, d Deps, job *Job, jobStatus, eventStatus string, detail map[string]any, bumpAttempts bool) error {
	attempts := job.Attempts
	if bumpAttempts {
		attempts++
	}
	return writeTerminalWithAttempts(ctx, d, job, jobStatus, eventStatus, detail, attempts)
}

func writeTerminalWithAttempts(ctx context.Context, d Deps, job *Job, jobStatus, eventStatus string, detail map[string]any, attempts int) error {
	detailJSON, _ := json.Marshal(detail)
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE transfer_jobs
SET status=$2, attempts=$3, locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE id=$1`, job.ID, jobStatus, attempts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO transfer_events (trace_id, job_id, status, detail) VALUES ($1,$2,$3,$4)`,
		job.TraceID, job.ID, eventStatus, detailJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func openErrDetails(err error) map[string]any {
	d := map[string]any{"error": err.Error()}
	if errors.Is(err, transfer.ErrSkippable) {
		d["reason"] = "source_not_found"
	}
	return d
}

func transportErrDetails(err error) map[string]any {
	return map[string]any{"error": err.Error(), "stage": "transport"}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worker/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/process.go internal/worker/process_test.go
git commit -m "feat(worker): implement ProcessJob with TeeReader-equivalent counter and F4 size verify"
```

---

## Task 7: `imgsync worker` CLI subcommand and runner loop

**Files:**
- Create: `internal/worker/runner.go`, `internal/worker/runner_test.go`
- Create: `cmd/imgsync/worker.go`
- Modify: `cmd/imgsync/main.go` (wire the new subcommand)

The runner spins up N goroutines. Each goroutine: lease → process → repeat. When `LeaseJob` returns nil, it sleeps for a fixed `IdleSleep` (default 1s; Week 2B replaces with shared jittered backoff). The CLI reads env: `IMGSYNC_DSN`, `IMGSYNC_WORKERS` (default 4), `IMGSYNC_POD_NAME` (else hostname), and FTP creds for the pool. Source/Transport routing uses `src_protocol` / `dst_protocol`: `localfs` → LocalFS impls, `ftp` → FTP impls.

- [ ] **Step 1: Write the failing test**

Create `internal/worker/runner_test.go`:

```go
package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/stretchr/testify/require"
)

func TestRunner_DrainsQueue(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		src := filepath.Join(dir, "src", "f"+string(rune('0'+i)))
		dst := filepath.Join(dir, "dst", "f"+string(rune('0'+i)))
		require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.WriteFile(src, []byte("hi-"+string(rune('0'+i))), 0o644))
		_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
			TraceID: "t-" + string(rune('0'+i)), Src: src, Dst: dst,
			SrcProtocol: "localfs", DstProtocol: "localfs", MaxAttempts: 3,
		})
		require.NoError(t, err)
	}

	r := &worker.Runner{
		Pool:        pool,
		Workers:     2,
		PodName:     "test-pod",
		IdleSleep:   50 * time.Millisecond,
		SourceFor:   func(_ string) (worker.SourceLike, error) { return localfs.NewSource(), nil },
		TransportFor: func(_ string) (worker.TransportLike, error) { return tlocalfs.NewTransport(), nil },
	}

	var processed int64
	r.OnFinish = func(_ *worker.Job) { atomic.AddInt64(&processed, 1) }

	go func() { _ = r.Run(ctx) }()

	require.Eventually(t, func() bool {
		var pending int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs WHERE status='pending'`).Scan(&pending)
		return pending == 0
	}, 10*time.Second, 100*time.Millisecond, "queue did not drain")

	require.GreaterOrEqual(t, atomic.LoadInt64(&processed), int64(8))

	var succeeded int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs WHERE status='succeeded'`).Scan(&succeeded))
	require.Equal(t, 8, succeeded)
}

func TestRunner_UnknownProtocol_RetriesUntilDead(t *testing.T) {
	pool := mustDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
		TraceID: "bad-proto", Src: "x", Dst: "y",
		SrcProtocol: "made-up", DstProtocol: "localfs", MaxAttempts: 1,
	})
	require.NoError(t, err)

	r := &worker.Runner{
		Pool:      pool,
		Workers:   1,
		PodName:   "test-pod",
		IdleSleep: 50 * time.Millisecond,
		SourceFor: func(p string) (worker.SourceLike, error) {
			if p == "localfs" {
				return localfs.NewSource(), nil
			}
			return nil, worker.ErrUnknownProtocol
		},
		TransportFor: func(p string) (worker.TransportLike, error) {
			return tlocalfs.NewTransport(), nil
		},
	}
	go func() { _ = r.Run(ctx) }()

	require.Eventually(t, func() bool {
		var status string
		_ = pool.QueryRow(ctx, `SELECT status FROM transfer_jobs WHERE trace_id='bad-proto'`).Scan(&status)
		return status == "dead"
	}, 10*time.Second, 100*time.Millisecond, "unknown protocol must terminate dead")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/... -run TestRunner -v`
Expected: FAIL — `worker.Runner`, `worker.SourceLike`, `worker.TransportLike`, `worker.ErrUnknownProtocol` undefined.

- [ ] **Step 3: Write `internal/worker/runner.go`**

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

// SourceLike and TransportLike are aliases for the streaming interfaces. Used
// by the runner factories.
type SourceLike = transfer.Source
type TransportLike = transfer.Transport

// ErrUnknownProtocol is returned by Source/Transport factories when src_protocol
// or dst_protocol does not match a registered impl.
var ErrUnknownProtocol = errors.New("unknown protocol")

// Runner drains the queue with N goroutines.
type Runner struct {
	Pool         *pgxpool.Pool
	Workers      int
	PodName      string
	IdleSleep    time.Duration
	SourceFor    func(protocol string) (SourceLike, error)
	TransportFor func(protocol string) (TransportLike, error)
	OnFinish     func(*Job) // optional, test hook
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Workers <= 0 {
		r.Workers = 4
	}
	if r.IdleSleep <= 0 {
		r.IdleSleep = 1 * time.Second
	}
	if r.PodName == "" {
		r.PodName = "imgsync-worker"
	}

	var wg sync.WaitGroup
	for i := 0; i < r.Workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.loop(ctx, idx)
		}(i)
	}
	wg.Wait()
	return nil
}

func (r *Runner) loop(ctx context.Context, idx int) {
	lockedBy := fmt.Sprintf("%s-w%d", r.PodName, idx)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := LeaseJob(ctx, r.Pool, lockedBy)
		if err != nil {
			// Transient DB error: short sleep and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.IdleSleep):
				continue
			}
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.IdleSleep):
				continue
			}
		}

		src, err := r.SourceFor(job.SrcProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "source-factory"}, true)
			r.fire(job)
			continue
		}
		tr, err := r.TransportFor(job.DstProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "transport-factory"}, true)
			r.fire(job)
			continue
		}

		_ = ProcessJob(ctx, Deps{
			Pool: r.Pool, LockedBy: lockedBy, Source: src, Transport: tr,
		}, job)
		r.fire(job)
	}
}

func (r *Runner) fire(job *Job) {
	if r.OnFinish != nil {
		r.OnFinish(job)
	}
}
```

- [ ] **Step 4: Write `cmd/imgsync/worker.go`**

```go
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/sources/ftp"
	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Drain the transfer_jobs queue",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			workers := envInt("IMGSYNC_WORKERS", 4)
			podName := os.Getenv("IMGSYNC_POD_NAME")
			if podName == "" {
				h, _ := os.Hostname()
				podName = h
			}

			pool, err := db.NewPool(ctx, db.PoolConfig{
				DSN:      dsn,
				MaxConns: int32(2 + workers),
			})
			if err != nil {
				return err
			}
			defer pool.Close()

			ftpPool := pftp.NewPool(pftp.PoolConfig{
				MaxPerHost:   envInt("IMGSYNC_FTP_MAX_PER_HOST", 4),
				IdleTTL:      time.Duration(envInt("IMGSYNC_FTP_IDLE_TTL_SEC", 300)) * time.Second,
				NoopAfter:    time.Duration(envInt("IMGSYNC_FTP_NOOP_AFTER_SEC", 60)) * time.Second,
				AuthUser:     os.Getenv("IMGSYNC_FTP_USER"),
				AuthPassword: os.Getenv("IMGSYNC_FTP_PASSWORD"),
			})
			defer ftpPool.Close()

			localSource := localfs.NewSource()
			localTransport := tlocalfs.NewTransport()
			ftpSrc := srcftp.NewSource(ftpPool)
			ftpTr := pftp.NewTransport(ftpPool)

			r := &worker.Runner{
				Pool:      pool,
				Workers:   workers,
				PodName:   podName,
				IdleSleep: 1 * time.Second,
				SourceFor: func(proto string) (worker.SourceLike, error) {
					switch proto {
					case "localfs":
						return localSource, nil
					case "ftp":
						return ftpSrc, nil
					}
					return nil, worker.ErrUnknownProtocol
				},
				TransportFor: func(proto string) (worker.TransportLike, error) {
					switch proto {
					case "localfs":
						return localTransport, nil
					case "ftp":
						return ftpTr, nil
					}
					return nil, worker.ErrUnknownProtocol
				},
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"imgsync worker starting: pod=%s workers=%d\n", podName, workers)
			return r.Run(ctx)
		},
	}
	return cmd
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// silence unused alias if SDK rearranges
var _ = ftp.NewSource
```

- [ ] **Step 5: Wire the new subcommand**

Edit `cmd/imgsync/main.go` (one line addition):

```go
root.AddCommand(newMigrateCmd())
root.AddCommand(newEnqueueCmd())
root.AddCommand(newWorkerCmd()) // NEW
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./... -race -count=1`
Expected: all PASS.

- [ ] **Step 7: Smoke-test the CLI build**

Run:
```bash
go build -o bin/imgsync ./cmd/imgsync
./bin/imgsync worker --help
```
Expected: prints help text. (Don't actually start it — no DB.)

- [ ] **Step 8: Run full CI check locally**

Run: `make ci`
Expected: streaming-check OK, lint clean, all tests PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/worker/runner.go internal/worker/runner_test.go cmd/imgsync/worker.go cmd/imgsync/main.go
git commit -m "feat(worker,cli): add Runner loop and imgsync worker subcommand"
```

---

## Week 2A Exit Criteria

After Task 7 commits cleanly, the repo state is:

- `make ci` is green: streaming guard, golangci-lint, all tests (Week 1 + Week 2A).
- FTP test server runs in-process (no docker required for tests).
- FTP connection pool enforces per-host max with NOOP-after-60s and idle-TTL-5min policies.
- FTPSource streams files, returns ErrSkippable on 550/not-found, releases conn on Close.
- FTPTransport streams via temp + RNFR/RNTO, computes sha256 + bytes in-stream.
- Worker leases jobs with FOR UPDATE SKIP LOCKED, never double-leases (concurrent test passes).
- ProcessJob honors all four error classifications (skippable / permanent / retryable / max-attempts) and the F4 size verification.
- `imgsync worker` drains a mixed-protocol queue end-to-end.

Week 2A intentionally leaves these for Week 2B (the next plan):
- Sweeper (lease expiry recovery) — without it, a worker crash mid-process leaves a lease dangling until restart of every pod. Must land before Week 3 production.
- Idle backoff — `IdleSleep: 1 * time.Second` is a placeholder. Replace with shared 50ms→1s exponential + ±25% jitter per F2.
- FTP host cluster cap — without it, scale-out from 1 pod to N pods will exceed FTP cap. F1 conn-pin regression test required.
- Health endpoints — `/livez /readyz /healthz`.
- EVAL invariants C0/C1/C2/C3/C6 (size-unknown, streaming RSS<250MB, sweeper attempts==0 audit, ErrSkippable single-event invariant, 52-fixture suite).
