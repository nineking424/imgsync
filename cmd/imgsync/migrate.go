package main

import (
	"fmt"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var (
		dsn string
		dir string
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := db.ApplyMigrations(ctx, dsn, dir); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL DSN (required)")
	cmd.Flags().StringVar(&dir, "dir", "migrations", "Directory containing migration files")

	_ = cmd.MarkFlagRequired("dsn")

	return cmd
}
