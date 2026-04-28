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
	Table        string        // source table, e.g. "images"
	PKColumn     string        // primary key column, e.g. "id"
	TSColumn     string        // watermark column, e.g. "updated_at"
	ExtraColumns []string      // additional columns to SELECT for dst rendering
	BatchSize    int // LIMIT
	// BiasDuration excludes rows newer than NOW()-bias. Stored at second
	// resolution in SQL; sub-second values truncate to 0s (silently disabling
	// bias).
	BiasDuration time.Duration
}

// Fetch runs the windowed query against the source DB. The predicate uses
// (TSColumn, PKColumn::TEXT) > (last_run_ts, last_run_pk) so that batches of
// rows sharing the same TS are split correctly across calls.
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
	pk := from.LastRunPK
	sql := fmt.Sprintf(`
		SELECT %s FROM %s
		WHERE (%s, %s::TEXT) > ($1, $2)
		  AND %s <= NOW() - ($3::INT || ' seconds')::INTERVAL
		ORDER BY %s, %s
		LIMIT %d`,
		colList, q.Table,
		q.TSColumn, q.PKColumn,
		q.TSColumn,
		q.TSColumn, q.PKColumn,
		q.BatchSize)

	rows, err := pool.Query(ctx, sql, from.LastRunTS, pk, biasSec)
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
