package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
)

func summaryCmd() *cobra.Command {
	var (
		repoPath   string
		branch     string
		since      string
		allRepos   bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Show aggregate review statistics",
		Long: `Show aggregate review statistics from existing data.

Surfaces pass/fail trends, agent effectiveness, review duration,
fix adoption rates, and per-repo breakdowns.

Examples:
  roborev summary                     # Last 7 days, current repo
  roborev summary --all               # Last 7 days, all repos
  roborev summary --since 30d         # Last 30 days
  roborev summary --branch main       # Filter by branch
  roborev summary --repo /path/to/repo
  roborev summary --json              # Structured output for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()

			// Auto-resolve repo from cwd when not specified (unless --all)
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

			params := url.Values{}
			if repoPath != "" {
				params.Set("repo", repoPath)
			}
			if branch != "" {
				params.Set("branch", branch)
			}
			if since != "" {
				params.Set("since", since)
			}
			if allRepos {
				params.Set("all", "true")
			}

			client := ep.HTTPClient(10 * time.Second)
			resp, err := client.Get(addr + "/api/summary?" + params.Encode())
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon returned %s", resp.Status)
			}

			var summary storage.Summary
			if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}

			printSummary(cmd, summary)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "scope to a single repo (default: current repo)")
	cmd.Flags().StringVar(&branch, "branch", "", "scope to a single branch")
	cmd.Flags().StringVar(&since, "since", "7d", "time window (e.g. 24h, 7d, 30d)")
	cmd.Flags().BoolVar(&allRepos, "all", false, "show summary across all repos")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "structured output for scripting")
	cmd.MarkFlagsMutuallyExclusive("all", "repo")

	return cmd
}

func printSummary(cmd *cobra.Command, s storage.Summary) {
	repoLabel := s.RepoPath
	if repoLabel != "" {
		repoLabel = filepath.Base(repoLabel)
	} else {
		repoLabel = "all repos"
	}

	sinceLabel := formatSince(s.Since)
	header := fmt.Sprintf("Review Summary: %s", repoLabel)
	if s.Branch != "" {
		header += fmt.Sprintf(" (%s)", s.Branch)
	}
	header += fmt.Sprintf(" [last %s]", sinceLabel)
	cmd.Println(header)
	cmd.Println()

	// Overview
	cmd.Println("Overview")
	o := s.Overview
	cmd.Printf("  Total: %d | Done: %d | Failed: %d | Canceled: %d | Queued: %d | Running: %d\n",
		o.Total, o.Done+o.Applied+o.Rebased, o.Failed, o.Canceled, o.Queued, o.Running)
	if o.Applied > 0 || o.Rebased > 0 {
		cmd.Printf("  Fix patches: %d applied, %d rebased\n", o.Applied, o.Rebased)
	}
	cmd.Println()

	// Verdicts
	if s.Verdicts.Total > 0 {
		cmd.Println("Verdicts")
		v := s.Verdicts
		cmd.Printf("  Pass: %d | Fail: %d | Pass rate: %.0f%%\n",
			v.Passed, v.Failed, v.PassRate*100)
		if v.Failed > 0 {
			cmd.Printf("  Addressed: %d of %d failures (%.0f%% resolution rate)\n",
				v.Addressed, v.Failed, v.ResolutionRate*100)
		}
		cmd.Println()
	}

	// Agent breakdown
	if len(s.Agents) > 0 {
		cmd.Println("Agent Breakdown")
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "  Agent\tJobs\tPass\tFail\tErrors\tPass Rate\tMedian\n")
		for _, a := range s.Agents {
			passRate := "-"
			if a.Passed+a.Failed > 0 {
				passRate = fmt.Sprintf("%.0f%%", a.PassRate*100)
			}
			median := "-"
			if a.MedianSecs > 0 {
				median = formatDuration(a.MedianSecs)
			}
			fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%d\t%s\t%s\n",
				a.Agent, a.Total, a.Passed, a.Failed, a.Errors, passRate, median)
		}
		w.Flush()
		cmd.Println()
	}

	// Duration stats
	if s.Duration.ReviewP50 > 0 {
		cmd.Println("Duration")
		cmd.Printf("  Review:  p50=%s  p90=%s  p99=%s\n",
			formatDuration(s.Duration.ReviewP50),
			formatDuration(s.Duration.ReviewP90),
			formatDuration(s.Duration.ReviewP99))
		cmd.Printf("  Queue:   p50=%s  p90=%s  p99=%s\n",
			formatDuration(s.Duration.QueueP50),
			formatDuration(s.Duration.QueueP90),
			formatDuration(s.Duration.QueueP99))
		cmd.Println()
	}

	// Job types
	if len(s.JobTypes) > 0 {
		cmd.Println("Job Types")
		for _, t := range s.JobTypes {
			line := fmt.Sprintf("  %-10s %d", t.Type, t.Count)
			if t.Applied > 0 || t.Rebased > 0 {
				line += fmt.Sprintf(" (applied: %d, rebased: %d)", t.Applied, t.Rebased)
			}
			cmd.Println(line)
		}
		cmd.Println()
	}

	// Per-repo breakdown (only when showing all repos)
	if len(s.Repos) > 0 {
		cmd.Println("Repos")
		labels := repoLabels(s.Repos)
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "  Repo\tJobs\tPass\tFail\tAddressed\n")
		for i, r := range s.Repos {
			fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%d\n",
				labels[i], r.Total, r.Passed, r.Failed, r.Addressed)
		}
		w.Flush()
		cmd.Println()
	}

	// Failures
	if s.Failures.Total > 0 {
		cmd.Println("Failures")
		cmd.Printf("  Total: %d | Retries: %d\n", s.Failures.Total, s.Failures.Retries)
		if len(s.Failures.Errors) > 0 {
			cmd.Println("  By category:")
			for _, cat := range sortedErrorCategories(s.Failures.Errors) {
				cmd.Printf("    %-12s %d\n", cat, s.Failures.Errors[cat])
			}
		}
		cmd.Println()
	}

	// Cost (windowed, like the rest of summary)
	if s.Cost.JobsTotal > 0 {
		cmd.Println("Cost")
		if s.Cost.Complete {
			cmd.Printf("  Approx: ~$%.2f\n", s.Cost.TotalUSD)
		} else {
			cmd.Printf("  Approx: ~$%.2f  (%d/%d jobs reported cost)\n",
				s.Cost.TotalUSD, s.Cost.JobsWithCost, s.Cost.JobsTotal)
		}
		cmd.Println()
	}

	if s.Overview.Total == 0 {
		cmd.Println("No review data for this time window.")
	}
}

// repoLabels returns display labels for repos, disambiguating duplicate names
// by prepending parent directory components until all labels are unique.
func repoLabels(repos []storage.RepoSummary) []string {
	labels := make([]string, len(repos))
	// Track how many path components each label uses (1 = basename only)
	depth := make([]int, len(repos))
	for i, r := range repos {
		labels[i] = r.Name
		if labels[i] == "" {
			labels[i] = filepath.Base(r.Path)
		}
		depth[i] = 1
	}

	for {
		seen := make(map[string][]int)
		for i, l := range labels {
			seen[l] = append(seen[l], i)
		}

		progress := false
		for _, indices := range seen {
			if len(indices) < 2 {
				continue
			}
			for _, i := range indices {
				parts := splitPath(repos[i].Path)
				depth[i]++
				if depth[i] > len(parts) {
					// Fall back to full path (includes volume/root)
					if labels[i] != repos[i].Path {
						labels[i] = repos[i].Path
						progress = true
					}
					continue
				}
				prefix := filepath.Join(parts[len(parts)-depth[i] : len(parts)-depth[i]+1]...)
				labels[i] = prefix + "/" + labels[i]
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return labels
}

// splitPath splits a cleaned path into its components.
func splitPath(p string) []string {
	p = filepath.Clean(p)
	var parts []string
	for {
		dir, base := filepath.Dir(p), filepath.Base(p)
		if dir == p {
			break
		}
		parts = append([]string{base}, parts...)
		p = dir
	}
	return parts
}

// sortedErrorCategories returns error category keys in deterministic order.
func sortedErrorCategories(errors map[string]int) []string {
	keys := make([]string, 0, len(errors))
	for k := range errors {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if errors[a] != errors[b] {
			return cmp.Compare(errors[b], errors[a]) // descending
		}
		return cmp.Compare(a, b)
	})
	return keys
}

// formatDuration formats seconds into a human-readable string.
func formatDuration(secs float64) string {
	d := time.Duration(secs * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", secs)
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

// formatSince returns a human-friendly label for how long ago the since time was.
func formatSince(since time.Time) string {
	d := time.Since(since)
	hours := d.Hours()
	if hours < 48 {
		return fmt.Sprintf("%.0fh", hours)
	}
	days := hours / 24
	if days < 14 {
		return fmt.Sprintf("%.0fd", days)
	}
	return fmt.Sprintf("%.0fw", days/7)
}
