package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/git"
)

//go:embed quickstart_guide.md
var quickstartGuide string

func quickstartCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "quickstart",
		Short: "Print an agent-oriented guide to setting up roborev in this repo",
		Long: `Print a repo-aware guide describing how roborev works and what is
configured. Designed to be read by a coding agent ("run roborev quickstart")
so it can help you finish setup. Detection is read-only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			repoRoot, rootErr := git.GetRepoRoot(".")
			inGitRepo := rootErr == nil

			state := detectState(cmd.Context(), repoRoot, inGitRepo)

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(state)
			}

			if !inGitRepo {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Not inside a git repository. Run roborev quickstart from your repo, then 'roborev init'.")
				return silentExit(cmd, 1)
			}

			renderHuman(out, state)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit detected state as JSON (no explainer)")
	return cmd
}

func renderHuman(w io.Writer, s quickstartState) {
	fmt.Fprintln(w, "# roborev setup")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Current state")
	fmt.Fprintln(w)
	for _, c := range s.Checks {
		mark := map[checkStatus]string{
			statusOK: "[ok]", statusMissing: "[missing]", statusUnknown: "[unknown]",
		}[c.Status]
		fmt.Fprintf(w, "%-10s %s", mark, c.ID)
		if c.Details != "" {
			fmt.Fprintf(w, " - %s", c.Details)
		}
		fmt.Fprintln(w)
		if c.Status == statusMissing && c.FixCommand != "" {
			fmt.Fprintf(w, "           fix: %s\n", c.FixCommand)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, quickstartGuide)
}
