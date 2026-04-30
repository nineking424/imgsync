package main

import (
	"github.com/nineking424/imgsync/internal/cli"
	"github.com/spf13/cobra"
)

func newSnifferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sniffer",
		Short: "Poll a source DB and enqueue new transfer jobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := cli.ParseSnifferConfig()
			if err != nil {
				return err
			}
			return cli.RunSniffer(cmd.Context(), cfg)
		},
	}
	return cmd
}
