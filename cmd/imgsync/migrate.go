package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply forward-only SQL migrations from a directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			if err := db.ApplyMigrations(ctx, dsn, dir); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "/etc/imgsync/migrations", "directory containing *.up.sql files")
	return cmd
}
