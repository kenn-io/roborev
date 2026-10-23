package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/client/generated"
)

func costCmd() *cobra.Command {
	var (
		repoPath   string
		branch     string
		since      string
		allRepos   bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "cost",
		Args:  cobra.NoArgs,
		Short: "Show approximate aggregate review cost for a repo or branch",
		Long: `Show approximate aggregate agent spend from existing review data.

Cost is partial: only some agents report it, so the figure is a lower bound and
shows coverage (priced jobs / eligible jobs).

Examples:
  roborev cost                     # All-time, current repo
  roborev cost --all               # All-time, all repos
  roborev cost --branch main       # Filter by branch
  roborev cost --repo /path/to/repo
  roborev cost --since 30d         # Last 30 days
  roborev cost --json              # Structured output for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()

			if !allRepos && repoPath == "" {
				root, err := gitrepo.MainRoot(ctx, ".")
				if err != nil {
					return fmt.Errorf("not in a git repo; use --all for all repos or --repo to specify one")
				}
				repoPath = root
			} else if repoPath != "" {
				if root, err := gitrepo.MainRoot(ctx, repoPath); err == nil {
					repoPath = root
				}
			}

			params := generated.GetCostQuery{}
			if repoPath != "" {
				params.Repo = []string{repoPath}
			}
			if branch != "" {
				params.Branch = new(branch)
			}
			if since != "" {
				params.Since = new(since)
			}

			client := ep.APIClient(10 * time.Second)
			resp, err := client.GetCostRaw(cmd.Context(), &generated.GetCostRequestOptions{Query: &params})
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon returned %s", resp.Status)
			}

			var cost storage.CostAggregate
			if err := json.UnmarshalRead(resp.Body, &cost); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			if jsonOutput {
				enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
				return json.MarshalEncode(enc, cost)
			}

			cmd.Println(formatCostLine(cost))
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "scope to a single repo (default: current repo)")
	cmd.Flags().StringVar(&branch, "branch", "", "scope to a single branch")
	cmd.Flags().StringVar(&since, "since", "", "time window (e.g. 24h, 7d, 30d); default all-time")
	cmd.Flags().BoolVar(&allRepos, "all", false, "aggregate across all repos")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "structured output for scripting")
	cmd.MarkFlagsMutuallyExclusive("all", "repo")

	return cmd
}

// formatCostLine renders the human-readable cost summary. Coverage is shown only
// when the aggregate is partial.
func formatCostLine(c storage.CostAggregate) string {
	if c.JobsTotal == 0 {
		return "No eligible agent-spend jobs in scope yet."
	}
	if c.Complete {
		return fmt.Sprintf("Approx cost: ~$%.2f", c.TotalUSD)
	}
	return fmt.Sprintf("Approx cost: ~$%.2f  (%d/%d jobs reported cost)",
		c.TotalUSD, c.JobsWithCost, c.JobsTotal)
}
