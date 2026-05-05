package sniffer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds all parameters for a single Sniffer instance. SrcProtocol and
// DstProtocol are passed verbatim to transfer_jobs; they must match the
// protocol identifiers expected by the worker (NOT NULL in schema).
type Config struct {
	SourceID string
	Query    Query
	Dst      DstTemplate
	// SrcPattern is a text/template body rendered against the source row's
	// columns; the result is stored in transfer_jobs.src verbatim (no URL
	// validation — the worker decides how to interpret it via SrcProtocol).
	SrcPattern  string
	SrcProtocol string
	DstProtocol string
	ImgsyncPool *pgxpool.Pool
	SourcePool  *pgxpool.Pool

	// OnEnqueue, if non-nil, fires once at the end of every successful RunOnce
	// with the SourceID and the number of rows just enqueued (n=0 is allowed).
	OnEnqueue func(source string, n int)
	// OnError, if non-nil, fires once when RunOnce returns a non-nil error.
	OnError func(source string)
}

// Sniffer composes state, query, traceid, and enqueue into a single poll loop.
type Sniffer struct {
	cfg   Config
	state *StateRepo
	enq   *Enqueuer
	src   DstTemplate // reuses DstTemplate renderer for the src-URL pattern
}

// New constructs a Sniffer from cfg. Pool lifecycle is caller-owned.
func New(cfg Config) *Sniffer {
	return &Sniffer{
		cfg:   cfg,
		state: NewStateRepo(cfg.ImgsyncPool),
		enq:   NewEnqueuer(cfg.ImgsyncPool),
		src:   DstTemplate{Pattern: cfg.SrcPattern},
	}
}

// RunOnce executes one poll iteration: load watermark → fetch batch → enqueue
// each row → advance watermark to the last row's (ts, pk).
// Returns the count of rows inserted (UNIQUE conflicts count as 0).
// Watermark is advanced only after all enqueue calls succeed so that a
// mid-batch error preserves the old watermark and the batch is retried in full.
// On success, OnEnqueue (if set) fires once with (SourceID, n). On error,
// OnError (if set) fires once with SourceID.
func (s *Sniffer) RunOnce(ctx context.Context) (int, error) {
	n, err := s.runOnceImpl(ctx)
	if err != nil {
		if s.cfg.OnError != nil {
			s.cfg.OnError(s.cfg.SourceID)
		}
		return n, err
	}
	if s.cfg.OnEnqueue != nil {
		s.cfg.OnEnqueue(s.cfg.SourceID, n)
	}
	return n, nil
}

func (s *Sniffer) runOnceImpl(ctx context.Context) (int, error) {
	st, err := s.state.Load(ctx, s.cfg.SourceID)
	if err != nil {
		return 0, fmt.Errorf("load state: %w", err)
	}

	rows, err := s.cfg.Query.Fetch(ctx, s.cfg.SourcePool, st)
	if err != nil {
		return 0, fmt.Errorf("fetch: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	inserted := 0
	for _, r := range rows {
		dst, err := s.cfg.Dst.Render(r.Fields)
		if err != nil {
			return inserted, fmt.Errorf("render dst pk=%s: %w", r.PK, err)
		}
		src, err := s.src.Render(r.Fields)
		if err != nil {
			return inserted, fmt.Errorf("render src pk=%s: %w", r.PK, err)
		}
		ok, err := s.enq.Enqueue(ctx, JobSpec{
			TraceID:     TraceID(s.cfg.Query.Table, r.PK),
			Src:         src,
			Dst:         dst,
			SrcProtocol: s.cfg.SrcProtocol,
			DstProtocol: s.cfg.DstProtocol,
		})
		if err != nil {
			return inserted, fmt.Errorf("enqueue pk=%s: %w", r.PK, err)
		}
		if ok {
			inserted++
		}
	}

	// Advance watermark only after the full batch enqueues successfully.
	last := rows[len(rows)-1]
	if err := s.state.Upsert(ctx, State{
		SourceID:  s.cfg.SourceID,
		LastRunTS: last.TS,
		LastRunPK: last.PK,
	}); err != nil {
		return inserted, fmt.Errorf("upsert state: %w", err)
	}
	return inserted, nil
}
