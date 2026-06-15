package sniffer

import (
	"context"
	"fmt"
	"regexp"
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
	// QueryTimeout bounds a single Fetch against the source DB. The sniffer loop
	// ctx (signal.NotifyContext) has no deadline, so without this a hung source
	// query would block the poll loop indefinitely. Values <= 0 disable the
	// per-query timeout (caller ctx alone governs).
	QueryTimeout time.Duration
}

// validIdentifier enforces a strict allowlist for SQL identifiers that get
// interpolated into queries. SQL identifiers cannot be parameterized via
// placeholders, so unvalidated input here would allow SQL injection (e.g.
// via multi-tenant config defining a custom table name like
// "users; DROP TABLE foo; --"). Limiting to [a-zA-Z_][a-zA-Z0-9_]* mirrors
// PostgreSQL's unquoted-identifier grammar and rejects quotes, semicolons,
// dots (schema/cross-DB), and all other syntactic metacharacters.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateIdentifier(name string) error {
	if !validIdentifier.MatchString(name) {
		return fmt.Errorf("invalid SQL identifier: %s", name)
	}
	return nil
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
	if q.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, q.QueryTimeout)
		defer cancel()
	}

	// Security: SQL identifiers cannot be parameterized via $N placeholders, so
	// they are interpolated directly into the query string. Validate every
	// identifier against a strict allowlist before use to prevent SQL injection
	// from untrusted Query configuration (e.g. attacker-controlled table/column
	// names in a multi-tenant UI).
	if err := validateIdentifier(q.Table); err != nil {
		return nil, err
	}
	if err := validateIdentifier(q.PKColumn); err != nil {
		return nil, err
	}
	if err := validateIdentifier(q.TSColumn); err != nil {
		return nil, err
	}
	for _, c := range q.ExtraColumns {
		if err := validateIdentifier(c); err != nil {
			return nil, err
		}
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
