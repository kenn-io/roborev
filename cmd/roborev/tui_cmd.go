package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/cmd/roborev/tui"
	"go.kenn.io/roborev/internal/daemon"
)

func tuiCmd() *cobra.Command {
	var addr string
	var repoFilter string
	var branchFilter string
	var controlSocket string
	var noQuit bool

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal UI for monitoring reviews",
		Long: `Interactive terminal UI for monitoring reviews.

Use --repo and --branch flags to launch the TUI pre-filtered, useful for
side-by-side working when you want to focus on a specific repo or branch.
When set via flags, the filter is locked and cannot be changed in the TUI.

Without a value, --repo resolves to the current repo and --branch resolves
to the current branch. Use = syntax for explicit values:
  roborev tui --repo                  # current repo
  roborev tui --repo=/path/to/repo    # explicit repo path
  roborev tui --branch                # current branch
  roborev tui --branch=feature-x      # explicit branch name
  roborev tui --repo --branch         # current repo + branch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon error: %w", err)
			}

			var ep daemon.DaemonEndpoint
			if addr == "" {
				ep = getDaemonEndpoint()
			} else {
				var err error
				ep, err = daemon.ParseEndpoint(addr)
				if err != nil {
					return fmt.Errorf("--addr: %w", err)
				}
			}

			if cmd.Flags().Changed("repo") {
				resolved, err := resolveRepoFlag(cmd.Context(), repoFilter)
				if err != nil {
					return fmt.Errorf("--repo: %w", err)
				}
				repoFilter = resolved
			}
			if cmd.Flags().Changed("branch") {
				branchRepo := "."
				if repoFilter != "" {
					branchRepo = repoFilter
				}
				resolved, err := resolveBranchFlag(
					cmd.Context(), branchFilter, branchRepo,
				)
				if err != nil {
					return fmt.Errorf("--branch: %w", err)
				}
				branchFilter = resolved
			}

			return tui.Run(tui.Config{
				Context:       cmd.Context(),
				Endpoint:      ep,
				RepoFilter:    repoFilter,
				BranchFilter:  branchFilter,
				ControlSocket: controlSocket,
				NoQuit:        noQuit,
			})
		},
	}

	cmd.Flags().StringVar(
		&addr, "addr", "",
		"daemon address (default: auto-detect)",
	)
	cmd.Flags().StringVar(
		&repoFilter, "repo", "",
		"lock filter to a repo (default: current repo)",
	)
	cmd.Flag("repo").NoOptDefVal = "."
	cmd.Flags().StringVar(
		&branchFilter, "branch", "",
		"lock filter to a branch (default: current branch)",
	)
	cmd.Flag("branch").NoOptDefVal = "HEAD"
	cmd.Flags().StringVar(
		&controlSocket, "control-socket", "",
		"Unix socket path for external control (default: auto)",
	)
	cmd.Flags().BoolVar(
		&noQuit, "no-quit", false,
		"suppress keyboard quit (for managed TUI instances)",
	)

	return cmd
}
