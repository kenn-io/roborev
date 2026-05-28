package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/version"
)

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show roborev version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("roborev %s\n", version.Version)
		},
	}
}
