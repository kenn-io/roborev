package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"
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

			// Build URL with optional repo filter
			ep := getDaemonEndpoint()
			streamURL := ep.BaseURL() + "/api/stream/events"
			if repoFilter != "" {
				streamURL += "?" + url.Values{"repo": {repoFilter}}.Encode()
			}

			// Create request
			req, err := http.NewRequest("GET", streamURL, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}

			// Set up context for Ctrl+C handling
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req = req.WithContext(ctx)

			// Handle Ctrl+C
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			go func() {
				<-sigCh
				cancel()
			}()

			// Make request
			client := ep.HTTPClient(0) // No timeout for streaming
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("stream failed: %s", body)
			}

			// Stream events - pass through lines directly to preserve all fields.
			// Use a 1MB buffer because review.completed events can include
			// large findings payloads that exceed the default 64KB limit.
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Println(scanner.Text())
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				return fmt.Errorf("read stream: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFilter, "repo", "", "filter events by repository path")

	return cmd
}
