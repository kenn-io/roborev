package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/version"
)

func versionCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show roborev version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				}{
					Name:    "roborev",
					Version: version.Version,
				})
			}

			_, err := fmt.Fprintf(cmd.OutOrStdout(), "roborev %s\n", version.Version)
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output version information as JSON")
	return cmd
}
