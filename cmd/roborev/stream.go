package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/pkg/client/generated"
)

func streamCmd() *cobra.Command {
	return streamCmdWithWait(waitForStreamReconnect)
}

func waitForStreamReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func streamCmdWithWait(wait func(context.Context, time.Duration) error) *cobra.Command {
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
			if repoFilter != "" && getDaemonEndpoint().IsRemote() {
				return usageErr(cmd, fmt.Errorf("remote stream uses server repository grants; local path filters are unavailable"))
			}
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
			api := ep.APIClient(0)
			// Reuse the native job-poll bounds in daemon_lifecycle.go (1s to
			// 5s). Empty remote streams back off; received events reset it.
			delay := pollStartInterval
			warnedReconnect := false
			for {
				resp, err := api.StreamEventsRaw(ctx, options)
				if ctx.Err() != nil {
					if resp != nil {
						_ = resp.Body.Close()
					}
					return nil
				}
				if err != nil {
					return fmt.Errorf("connect to daemon: %w", err)
				}
				if resp.StatusCode != http.StatusOK {
					if ep.IsRemote() {
						_ = resp.Body.Close()
						return fmt.Errorf("remote stream failed: HTTP %d", resp.StatusCode)
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					return fmt.Errorf("stream failed: %s", body)
				}
				// Forward complete NDJSON bytes without a scanner ceiling.
				n, err := io.Copy(cmd.OutOrStdout(), resp.Body)
				_ = resp.Body.Close()
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return fmt.Errorf("read stream: %w", err)
				}
				if !ep.IsRemote() {
					return nil
				}
				if n > 0 {
					delay = pollStartInterval
				}
				if !warnedReconnect {
					cmd.PrintErrln("Remote event streams reconnect periodically; events between connections may be missed.")
					warnedReconnect = true
				}
				if err := wait(ctx, delay); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				delay = min(delay*2, pollMaxInterval)
			}
		},
	}

	cmd.Flags().StringVar(&repoFilter, "repo", "", "filter events by repository path")

	return cmd
}
