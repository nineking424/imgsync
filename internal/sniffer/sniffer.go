package sniffer

import (
	"context"
	"fmt"
	"log"

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

// RunOnce executes one poll iteration: load watermark → fetch batch → render +
// batch-enqueue the renderable rows → advance watermark to the last row's
// (ts, pk). Returns the count of rows inserted (UNIQUE conflicts count as 0).
// A row that cannot be rendered is a deterministic poison row: it is logged and
// skipped (not counted), and the watermark still advances past it so it can
// never stall the source. The watermark is advanced only after the batch
// enqueues cleanly, so a transient enqueue/DB error preserves the old watermark
// and the batch is retried in full. On success, OnEnqueue (if set) fires once
// with (SourceID, n). On error, OnError (if set) fires once with SourceID.
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

	// Render every row up front, collecting the renderable ones into a single
	// batch. A render error is DETERMINISTIC — the same row fails identically on
	// every retry (e.g. a NULL/missing templated column under missingkey=error),
	// so it can never self-heal. We log+skip such a poison row and continue the
	// batch so the watermark still advances past it; otherwise one un-renderable
	// row would stall ALL ingest for this source forever (#29). Transient
	// enqueue/DB errors below still early-return to preserve the watermark for a
	// whole-batch retry (the crash-safety guarantee).
	specs := make([]JobSpec, 0, len(rows))
	for _, r := range rows {
		dst, err := s.cfg.Dst.Render(r.Fields)
		if err != nil {
			log.Printf("sniffer %s: skipping un-renderable row pk=%s: render dst: %v", s.cfg.SourceID, r.PK, err)
			continue
		}
		src, err := s.src.Render(r.Fields)
		if err != nil {
			log.Printf("sniffer %s: skipping un-renderable row pk=%s: render src: %v", s.cfg.SourceID, r.PK, err)
			continue
		}
		specs = append(specs, JobSpec{
			TraceID:     TraceID(s.cfg.Query.Table, r.PK),
			Src:         src,
			Dst:         dst,
			SrcProtocol: s.cfg.SrcProtocol,
			DstProtocol: s.cfg.DstProtocol,
		})
	}

	inserted, err := s.enq.EnqueueBatch(ctx, specs)
	if err != nil {
		return 0, fmt.Errorf("enqueue batch: %w", err)
	}

	// Advance watermark only after the full batch enqueues successfully. We
	// advance to the last FETCHED row (not the last enqueued one) so the
	// watermark moves past a trailing poison row too.
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
