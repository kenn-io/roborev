package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/cmd/roborev/tui"
	"go.kenn.io/roborev/internal/config"
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
			// --addr follows the --server rule: a remote http://host:port
			// selects remote mode, and any local address overrides
			// [remote] server.
			var addrEndpoint *daemon.DaemonEndpoint
			if strings.HasPrefix(addr, "http://") {
				remote, isRemote, err := parseRemoteServer(addr)
				if err != nil {
					return fmt.Errorf("--addr: %w", err)
				}
				if isRemote {
					setRemoteEndpoint(&remote)
					addrEndpoint = &remote
				}
			}
			if addr != "" && addrEndpoint == nil {
				parsed, err := daemon.ParseEndpoint(addr)
				if err != nil {
					return fmt.Errorf("--addr: %w", err)
				}
				setRemoteEndpoint(nil)
				addrEndpoint = &parsed
			}

			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon error: %w", err)
			}

			ep := getDaemonEndpoint()
			if addrEndpoint != nil {
				ep = *addrEndpoint
			}

			remote, err := isRemoteMode()
			if err != nil {
				return err
			}

			// daemonRepo is set when a remote-mode --repo is not a local
			// checkout: it is a daemon root path, sent unchanged.
			var daemonRepo bool
			if cmd.Flags().Changed("repo") {
				resolved, err := resolveRepoFlag(cmd.Context(), repoFilter)
				switch {
				case err == nil:
					repoFilter = resolved
				case remote && filepath.IsAbs(repoFilter):
					daemonRepo = true
				case remote:
					return fmt.Errorf("--repo %q is neither a local checkout nor an absolute daemon root path", repoFilter)
				default:
					return fmt.Errorf("--repo: %w", err)
				}
			}
			if cmd.Flags().Changed("branch") {
				if daemonRepo && branchFilter == "HEAD" {
					return fmt.Errorf("--branch without a value needs a local checkout, and --repo %s is a daemon path; use --branch=<name>", repoFilter)
				}
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

			cfg := tui.Config{
				Context:       cmd.Context(),
				Endpoint:      ep,
				RepoFilter:    repoFilter,
				BranchFilter:  branchFilter,
				ControlSocket: controlSocket,
				NoQuit:        noQuit,
				Remote:        remote,
			}
			if remote {
				if err := resolveRemoteTUIRepo(cmd.Context(), ep, &cfg, daemonRepo); err != nil {
					return err
				}
			}
			return runTUI(cfg)
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

// runTUI starts the TUI; tests replace it.
var runTUI = tui.Run

// resolveRemoteTUIRepo turns local checkout paths into the daemon root
// paths that remote jobs carry, since the TUI filters jobs by those paths.
func resolveRemoteTUIRepo(ctx context.Context, ep daemon.DaemonEndpoint, cfg *tui.Config, daemonRepo bool) error {
	if cfg.RepoFilter != "" {
		if daemonRepo {
			return nil
		}
		root, err := remoteRepoRoot(ctx, ep, cfg.RepoFilter)
		if err != nil {
			return fmt.Errorf("--repo: %w", err)
		}
		cfg.RepoFilter = root
		return nil
	}
	// Only the automatic repo filter needs the current directory's path.
	// A config the TUI cannot load leaves the filter off there too.
	if globalCfg, err := config.LoadGlobal(); err != nil || !globalCfg.AutoFilterRepo {
		return nil
	}
	local, err := resolveRepoFlag(ctx, ".")
	if err != nil {
		return nil
	}
	// The current directory may not be registered on the daemon; the TUI
	// then starts unfiltered.
	if root, err := remoteRepoRoot(ctx, ep, local); err == nil {
		cfg.RemoteRepoRoot = root
	}
	return nil
}
