// Package health exposes /livez, /readyz, /healthz HTTP endpoints.
package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is updated by the worker loop + sweeper. It is goroutine-safe.
type Status struct {
	mu                 sync.Mutex
	LastLeaseAttemptTS time.Time
	LastLeaseSuccessTS time.Time
	LastSweepTS        time.Time
}

func NewStatus() *Status { return &Status{} }

func (s *Status) OnLeaseAttempt(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastLeaseAttemptTS = time.Now()
	if success {
		s.LastLeaseSuccessTS = time.Now()
	}
}

func (s *Status) OnSweepCycle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastSweepTS = time.Now()
}

// Server is a lightweight HTTP server. Use Serve(listener) to bind, Close to stop.
type Server struct {
	pool    *pgxpool.Pool
	status  *Status
	mux     *http.ServeMux
	metrics http.Handler // nil if not configured
	hs      *http.Server
}

// Option configures a Server during NewServer.
type Option func(*Server)

// WithMetrics mounts the given handler at /metrics on the server's mux.
// Passing a nil handler is a no-op (the path remains unmounted).
func WithMetrics(h http.Handler) Option {
	return func(s *Server) {
		if h == nil {
			return
		}
		s.metrics = h
	}
}

func NewServer(pool *pgxpool.Pool, st *Status, opts ...Option) *Server {
	mux := http.NewServeMux()
	s := &Server{pool: pool, status: st, mux: mux}
	for _, opt := range opts {
		opt(s)
	}
	mux.HandleFunc("/livez", s.livez)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/healthz", s.healthz)
	if s.metrics != nil {
		mux.Handle("/metrics", s.metrics)
	}
	s.hs = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// MuxForTest exposes the internal mux for unit tests that need to drive
// requests directly without binding a TCP socket.
func (s *Server) MuxForTest() http.Handler { return s.mux }

func (s *Server) Serve(l net.Listener) error { return s.hs.Serve(l) }
func (s *Server) Close() error               { return s.hs.Close() }

func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	stat := s.pool.Stat()
	s.status.mu.Lock()
	body := map[string]any{
		"last_lease_attempt_ts": s.status.LastLeaseAttemptTS,
		"last_lease_success_ts": s.status.LastLeaseSuccessTS,
		"last_sweep_ts":         s.status.LastSweepTS,
		"pool_in_use":           stat.AcquiredConns(),
		"pool_idle":             stat.IdleConns(),
		"pool_max":              stat.MaxConns(),
	}
	s.status.mu.Unlock()

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
