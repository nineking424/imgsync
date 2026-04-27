package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrationAdvisoryLockID is a project-stable int64 used to serialize
// concurrent ApplyMigrations calls across pods. Session-level locks
// release automatically on disconnect, so a crashed migrate leaves no
// stranded lock. Value: ASCII bytes of "IMGSYNC" packed as int64.
const migrationAdvisoryLockID int64 = 0x494d4753594e43

// ApplyMigrations runs every *.up.sql under dir in lexical order, skipping
// versions already recorded in schema_migrations. The first migration creates
// schema_migrations itself, so a fresh DB starts empty.
func ApplyMigrations(ctx context.Context, dsn, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}

	applied := map[string]bool{}
	exists, err := hasTable(ctx, conn, "schema_migrations")
	if err != nil {
		return fmt.Errorf("check schema_migrations existence: %w", err)
	}
	if exists {
		rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			applied[v] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".up.sql")
		if applied[version] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

func hasTable(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
		name,
	).Scan(&exists)
	return exists, err
}
