package sniffer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Row struct {
	PK     string            // serialized as TEXT regardless of source pk type
	TS     time.Time         // updated_at
	Fields map[string]string // additional columns for dst-path templating
}

type Query struct {
	Table        string   // source table, e.g. "images"
	PKColumn     string   // primary key column, e.g. "id"
	TSColumn     string   // watermark column, e.g. "updated_at"
	ExtraColumns []string // additional columns to SELECT for dst rendering
	BatchSize    int      // LIMIT
	// BiasDuration excludes rows newer than NOW()-bias. Stored at second
	// resolution in SQL; sub-second values truncate to 0s (silently disabling
	// bias).
	BiasDuration time.Duration
}

// Fetch runs the windowed query against the source DB.
//
// LastRunPK == "" is treated as the "no carried-over pk" sentinel, which
// applies on the first run AND after a watermark reset (e.g. S0 sets
// last_run_pk = NULL). In that case a simple ts-only predicate is used:
//
//	WHERE ts > last_ts
//
// When LastRunPK is set, an expanded OR predicate is used so that PostgreSQL
// compares the PK column using its native type rather than casting to TEXT.
// This correctly handles multi-digit integer PKs (where lexicographic text
// ordering would break pagination at boundaries like 9→10):
//
//	WHERE ts > last_ts OR (ts = last_ts AND pk > last_pk)
//
// Caveat: source schemas whose PK is TEXT and can legitimately contain the
// empty string would alias to the "no carried-over pk" sentinel and skip
// tie-break filtering. v1 source schemas use BIGINT PKs so this is not a
// concern in practice.
func (q Query) Fetch(ctx context.Context, pool *pgxpool.Pool, from State) ([]Row, error) {
	if q.BatchSize <= 0 {
		return nil, fmt.Errorf("batch_size must be > 0")
	}
	cols := append([]string{q.PKColumn, q.TSColumn}, q.ExtraColumns...)
	colList := ""
	for i, c := range cols {
		if i > 0 {
			colList += ", "
		}
		colList += c
	}
	biasSec := int(q.BiasDuration.Seconds())

	var (
		sqlQuery string
		args     []any
	)
	if from.LastRunPK == "" {
		// First run: no pk watermark, filter by ts only.
		sqlQuery = fmt.Sprintf(`
			SELECT %s FROM %s
			WHERE %s > $1
			  AND %s <= NOW() - ($2::INT || ' seconds')::INTERVAL
			ORDER BY %s, %s
			LIMIT %d`,
			colList, q.Table,
			q.TSColumn,
			q.TSColumn,
			q.TSColumn, q.PKColumn,
			q.BatchSize)
		args = []any{from.LastRunTS, biasSec}
	} else {
		// Subsequent runs: use expanded OR so PostgreSQL uses the column's
		// native type for pk comparison (avoids text-sort bugs with integers).
		sqlQuery = fmt.Sprintf(`
			SELECT %s FROM %s
			WHERE (%s > $1 OR (%s = $1 AND %s > $2))
			  AND %s <= NOW() - ($3::INT || ' seconds')::INTERVAL
			ORDER BY %s, %s
			LIMIT %d`,
			colList, q.Table,
			q.TSColumn, q.TSColumn, q.PKColumn,
			q.TSColumn,
			q.TSColumn, q.PKColumn,
			q.BatchSize)
		args = []any{from.LastRunTS, from.LastRunPK, biasSec}
	}

	rows, err := pool.Query(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := Row{Fields: map[string]string{}}
		for i, c := range cols {
			switch v := vals[i].(type) {
			case nil:
				r.Fields[c] = ""
			case time.Time:
				r.Fields[c] = v.UTC().Format(time.RFC3339Nano)
			default:
				r.Fields[c] = fmt.Sprintf("%v", v)
			}
		}
		r.PK = r.Fields[q.PKColumn]
		ts, ok := vals[1].(time.Time)
		if !ok {
			return nil, fmt.Errorf("unexpected ts type %T", vals[1])
		}
		r.TS = ts
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
