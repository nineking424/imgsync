package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/jobs"
	"github.com/spf13/cobra"
)

func newEnqueueCmd() *cobra.Command {
	var (
		traceID     string
		src         string
		dst         string
		srcProto    string
		dstProto    string
		maxAttempts int
	)
	cmd := &cobra.Command{
		Use:   "enqueue",
		Short: "Insert a transfer job (idempotent on trace_id, dst)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			pool, err := db.NewPool(ctx, db.PoolConfig{DSN: dsn, MaxConns: 4})
			if err != nil {
				return err
			}
			defer pool.Close()

			id, inserted, err := jobs.Enqueue(ctx, pool, jobs.EnqueueArgs{
				TraceID:     traceID,
				Src:         src,
				Dst:         dst,
				SrcProtocol: srcProto,
				DstProtocol: dstProto,
				MaxAttempts: maxAttempts,
			})
			if err != nil {
				return err
			}
			if inserted {
				fmt.Fprintf(cmd.OutOrStdout(), "enqueued id=%d trace_id=%s\n", id, traceID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "exists id=%d trace_id=%s (no-op)\n", id, traceID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&traceID, "trace-id", "", "stable trace identifier (required)")
	cmd.Flags().StringVar(&src, "src", "", "source URI (required)")
	cmd.Flags().StringVar(&dst, "dst", "", "destination URI (required)")
	cmd.Flags().StringVar(&srcProto, "src-protocol", "", "source protocol, e.g. localfs, ftp (required)")
	cmd.Flags().StringVar(&dstProto, "dst-protocol", "", "destination protocol (required)")
	cmd.Flags().IntVar(&maxAttempts, "max-attempts", 5, "retry budget")
	_ = cmd.MarkFlagRequired("trace-id")
	_ = cmd.MarkFlagRequired("src")
	_ = cmd.MarkFlagRequired("dst")
	_ = cmd.MarkFlagRequired("src-protocol")
	_ = cmd.MarkFlagRequired("dst-protocol")
	return cmd
}
