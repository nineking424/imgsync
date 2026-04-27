package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "imgsync",
		Short:         "imgsync: file transfer queue (Go + PostgreSQL)",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
