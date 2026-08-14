package main

import (
	"github.com/spf13/cobra"

	webassets "go.kenn.io/roborev/internal/web"
)

func verifyWebAssetsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "verify-web-assets",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return webassets.ValidateEmbeddedRelease()
		},
	}
}
