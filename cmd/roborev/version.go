package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/version"
	webassets "go.kenn.io/roborev/internal/web"
)

// webAssetsEmbedded reports whether this binary embeds the production web
// distribution. Var so tests can pin both outcomes.
var webAssetsEmbedded = webassets.EmbeddedReleaseAvailable

func versionCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show roborev version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			embedded := webAssetsEmbedded()
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Name      string `json:"name"`
					Version   string `json:"version"`
					WebAssets bool   `json:"web_assets"`
				}{
					Name:      "roborev",
					Version:   version.Version,
					WebAssets: embedded,
				})
			}

			suffix := ""
			if !embedded {
				suffix = " (no embedded web assets; reinstall from an official release or build with 'make build')"
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "roborev %s%s\n", version.Version, suffix)
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output version information as JSON")
	return cmd
}
