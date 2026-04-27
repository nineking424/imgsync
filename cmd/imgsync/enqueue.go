package main

import (
	"fmt"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/spf13/cobra"
)

func newEnqueueCmd() *cobra.Command {
	var (
		dsn         string
		traceID     string
		src         string
		dst         string
		srcProtocol string
		dstProtocol string
		maxAttempts int
	)

	cmd := &cobra.Command{
		Use:   "enqueue",
		Short: "Enqueue a file-transfer job",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 4})
			if err != nil {
				return fmt.Errorf("db: %w", err)
			}
			defer pool.Close()

			id, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
				TraceID:     traceID,
				Src:         src,
				Dst:         dst,
				SrcProtocol: srcProtocol,
				DstProtocol: dstProtocol,
				MaxAttempts: maxAttempts,
			})
			if err != nil {
				return err
			}

			if inserted {
				fmt.Fprintf(cmd.OutOrStdout(), "enqueued job id=%d\n", id)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "already exists job id=%d\n", id)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL DSN (required)")
	cmd.Flags().StringVar(&traceID, "trace-id", "", "Unique trace ID (required)")
	cmd.Flags().StringVar(&src, "src", "", "Source URI (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "Destination URI (required)")
	cmd.Flags().StringVar(&srcProtocol, "src-protocol", "", "Source protocol (required)")
	cmd.Flags().StringVar(&dstProtocol, "dst-protocol", "", "Destination protocol (required)")
	cmd.Flags().IntVar(&maxAttempts, "max-attempts", 5, "Maximum delivery attempts")

	_ = cmd.MarkFlagRequired("dsn")
	_ = cmd.MarkFlagRequired("trace-id")
	_ = cmd.MarkFlagRequired("src")
	_ = cmd.MarkFlagRequired("dst")
	_ = cmd.MarkFlagRequired("src-protocol")
	_ = cmd.MarkFlagRequired("dst-protocol")

	return cmd
}
