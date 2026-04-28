package sniffer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type State struct {
	SourceID string
	// LastRunTS is stored as Postgres TIMESTAMPTZ (microsecond precision).
	// Go nanoseconds are truncated on round-trip; do not compare with == on
	// values that have flowed through Load/Upsert.
	LastRunTS time.Time
	LastRunPK string
}

type StateRepo struct {
	pool *pgxpool.Pool
}

func NewStateRepo(pool *pgxpool.Pool) *StateRepo {
	return &StateRepo{pool: pool}
}

// Load returns the watermark for source_id. If no row exists, returns
// State{SourceID: id, LastRunTS: zero, LastRunPK: ""} with nil error —
// caller distinguishes "first run" from "found row" via LastRunTS.IsZero().
// SourceID is always populated (even on miss) so the returned value is
// safe to pass directly to Upsert.
func (r *StateRepo) Load(ctx context.Context, sourceID string) (State, error) {
	var st State
	st.SourceID = sourceID
	err := r.pool.QueryRow(ctx, `
		SELECT last_run_ts, COALESCE(last_run_pk, '')
		  FROM sniffer_state
		 WHERE source_id = $1`, sourceID).Scan(&st.LastRunTS, &st.LastRunPK)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("load %q: %w", sourceID, err)
	}
	return st, nil
}

// Upsert writes the new watermark. last_run_pk "" stores as SQL NULL.
// Concurrent writers are safe (atomic ON CONFLICT) but last-writer-wins;
// v1 assumes a single sniffer pod (see migration 0002 comment).
func (r *StateRepo) Upsert(ctx context.Context, s State) error {
	var pk any = s.LastRunPK
	if s.LastRunPK == "" {
		pk = nil
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO sniffer_state (source_id, last_run_ts, last_run_pk, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (source_id) DO UPDATE
		   SET last_run_ts = EXCLUDED.last_run_ts,
		       last_run_pk = EXCLUDED.last_run_pk,
		       updated_at  = NOW()`,
		s.SourceID, s.LastRunTS, pk)
	if err != nil {
		return fmt.Errorf("upsert %q: %w", s.SourceID, err)
	}
	return nil
}
