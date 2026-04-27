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

	applied := map[string]bool{}
	if hasTable(ctx, conn, "schema_migrations") {
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

func hasTable(ctx context.Context, conn *pgx.Conn, name string) bool {
	var exists bool
	_ = conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name=$1)`,
		name,
	).Scan(&exists)
	return exists
}
