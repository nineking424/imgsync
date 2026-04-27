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

	mu     sync.Mutex
	hosts  map[string]*hostPool
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
		var acquired *ftp.ServerConn
		var needsPing bool
		for len(hp.idle) > 0 {
			ie := hp.idle[len(hp.idle)-1]
			hp.idle = hp.idle[:len(hp.idle)-1]
			if time.Since(ie.enqueue) > p.cfg.IdleTTL {
				_ = ie.c.Quit()
				// inUse was not incremented for idle conns; continue scanning
				continue
			}
			// Found a live candidate.
			needsPing = time.Since(ie.lastUse) > p.cfg.NoopAfter
			acquired = ie.c
			hp.inUse++
			break
		}

		if acquired != nil {
			p.mu.Unlock()
			if needsPing {
				if err := acquired.NoOp(); err != nil {
					_ = acquired.Quit()
					p.mu.Lock()
					p.hosts[host].inUse--
					// Wake a waiter if any, since we freed a slot.
					if hp2 := p.hosts[host]; len(hp2.waiters) > 0 {
						ch := hp2.waiters[0]
						hp2.waiters = hp2.waiters[1:]
						select {
						case ch <- struct{}{}:
						default:
						}
					}
					p.mu.Unlock()
					// Loop back to try again (idle or fresh dial).
					continue
				}
			}
			return &PooledConn{c: acquired, host: host, pool: p, lastUsed: time.Now()}, nil
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
