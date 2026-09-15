package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/pkg/client/generated"
)

func streamCmd() *cobra.Command {
	var repoFilter string

	cmd := &cobra.Command{
		Use:   "stream",
		Short: "Stream review events in real-time",
		Long: `Stream review events from the daemon in real-time.

Events are printed as JSONL (one JSON object per line).

Examples:
  roborev stream              # Stream all events
  roborev stream --repo .     # Stream events for current repo only
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			// Resolve repo filter if set - use main repo root for worktree compatibility
			if repoFilter != "" {
				root, err := gitrepo.MainRoot(ctx, repoFilter)
				if err != nil {
					return fmt.Errorf("resolve repo path: %w", err)
				}
				repoFilter = root
			}

			ep := getDaemonEndpoint()
			options := &generated.StreamEventsRequestOptions{}
			if repoFilter != "" {
				options.Query = &generated.StreamEventsQuery{Repo: &repoFilter}
			}
			ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
			defer cancel()
			resp, err := ep.APIClient(0).StreamEventsRaw(ctx, options)
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("stream failed: %s", body)
			}

			// Forward complete NDJSON bytes without a per-record scanner ceiling.
			if _, err := io.Copy(cmd.OutOrStdout(), resp.Body); err != nil && ctx.Err() == nil {
				return fmt.Errorf("read stream: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFilter, "repo", "", "filter events by repository path")

	return cmd
}
