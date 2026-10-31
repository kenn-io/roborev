package main

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/client/generated"
)

func summaryCmd() *cobra.Command {
	var (
		repoPath   string
		branch     string
		since      string
		allRepos   bool
		findings   bool
		groupBy    string
		minSamples int
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
			if !findings && (cmd.Flags().Changed("by") || cmd.Flags().Changed("min-samples")) {
				return fmt.Errorf("--by and --min-samples require --findings")
			}
			if err := storage.ValidateFindingOptions(findings, groupBy, 0); err != nil {
				return err
			}
			if minSamples < 1 {
				return fmt.Errorf("--min-samples must be a positive integer")
			}
			ctx := cmd.Context()
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()

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

			params := generated.GetSummaryQuery{}
			if findings {
				params.Findings = new(generated.GetSummaryQueryFindings("true"))
				params.By = new(groupBy)
				params.MinSamples = new(fmt.Sprint(minSamples))
			}
			if repoPath != "" {
				params.Repo = new(repoPath)
			}
			if branch != "" {
				params.Branch = new(branch)
			}
			if since != "" {
				params.Since = new(since)
			}
			if allRepos {
				params.All = new(generated.GetSummaryQueryAll("true"))
			}

			client := ep.APIClient(10 * time.Second)
			resp, err := client.GetSummaryRaw(cmd.Context(), &generated.GetSummaryRequestOptions{Query: &params})
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon returned %s", resp.Status)
			}

			var summary storage.Summary
			if err := json.UnmarshalRead(resp.Body, &summary); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			if jsonOutput {
				enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
				return json.MarshalEncode(enc, summary)
			}

			printSummary(cmd, summary)
			return nil
		},
	}

	cmd.Flags().BoolVar(&findings, "findings", false, "show version-bound finding outcomes")
	cmd.Flags().StringVar(&groupBy, "by", "", "group findings by severity, review-type, agent, model, or arm (default severity)")
	cmd.Flags().IntVar(&minSamples, "min-samples", storage.DefaultFindingMinSamples, "minimum labeled findings for displaying a rate")
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

	if s.Findings != nil {
		printFindingSummary(cmd, s.Findings)
	}
	if s.Overview.Total == 0 && s.Findings == nil {
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

// printFindingSummary keeps unavailable rates separate from raw counts.
func printFindingSummary(cmd *cobra.Command, f *storage.FindingStats) {
	cmd.Printf("Finding outcomes by %s (finished_at; as of %s)\n", f.GroupBy, f.AsOf.UTC().Format(time.RFC3339))
	cmd.Printf("  Structured reviews: %d/%d | Labeled findings: %d/%d (R: %d, K: %d)\n", f.StructuredReviews, f.LogicalReviews, f.Total.Labeled, f.Total.Findings, f.TierLabeled["R"], f.TierLabeled["K"])
	cohort := f.TierCoverage["R"]
	cmd.Printf("  R cohort labeled findings: %d/%d (local standalone reviews)\n", cohort.Labeled, cohort.Findings)
	cmd.Printf("  Documents unavailable: %d missing, %d invalid, %d legacy, %d unable to review\n", f.MissingDocuments, f.InvalidDocuments, f.LegacyDocuments, f.UnableDocuments)
	cmd.Printf("  Rebuttals: %d valid, %d unbound, %d stale, %d unmatched/ambiguous, %d invalid, %d ineligible\n", f.Rebuttals.Valid, f.Rebuttals.Unbound, f.Rebuttals.Stale, f.Rebuttals.Ambiguous, f.Rebuttals.Invalid, f.Rebuttals.Ineligible)
	k := f.Kata
	state := k.State
	if state == "" {
		state = "unavailable"
	}
	cmd.Printf("  K coverage (%s): %d/%d mapped, %d/%d readable | labeled: %d | explicit reasons: %d/%d refutes\n", state, k.Mapped, k.Eligible, k.Readable, k.Eligible, k.Labeled, k.ExplicitReasons, k.Refutes)
	cmd.Printf("  K evidence: %d ineligible, %d unmapped, %d unavailable (%d incomplete, %d stale), %d invalid, %d conflicting, %d excluded, %d edited\n", k.Ineligible, k.Unmapped, k.Unavailable, k.Incomplete, k.Stale, k.Invalid, k.Conflicting, k.Excluded, k.Edited)
	reasons := f.Total.RefuteReasons
	cmd.Printf("  Refutes: false-positive: %d | spurious: %d | risk-accepted: %d | unspecified: %d\n", reasons.FalsePositive, reasons.Spurious, reasons.RiskAccepted, reasons.Unspecified)
	cmd.Printf("  Refute reason coverage: %d absent, %d invalid, %d conflicting\n", reasons.Absent, reasons.Invalid, reasons.Conflicting)
	cmd.Println("  Confirmed share describes assertions; risk-accepted records a real tradeoff. Closure is resolution only.")
	if len(f.UnavailableTiers) > 0 {
		cmd.Printf("  Unavailable tiers: %s\n", strings.Join(f.UnavailableTiers, ", "))
	}
	priced := false
	for _, r := range f.Rows {
		if r.Cost.JobsWithCost > 0 {
			priced = true
			break
		}
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "  Group\tFindings\tLabeled\tPositive\tNegative\tExcluded\tUnlabeled\tFalse-positive\tSpurious\tRisk-accepted\tUnspecified\tConfirmed share\tResolution")
	if priced {
		fmt.Fprint(w, "\tRetained spend\tCost/positive")
	}
	fmt.Fprintln(w)
	for _, r := range f.Rows {
		rate := "-"
		if r.RateState == "below_minimum" {
			rate = fmt.Sprintf("n<%d", f.MinSamples)
		} else if r.ActedOnRate != nil {
			rate = fmt.Sprintf("%.0f%%", 100**r.ActedOnRate)
		}
		resolution := fmt.Sprintf("%d/%d", r.ClosedReviews, r.FailingReviews)
		if r.ResolutionRate != nil {
			resolution += fmt.Sprintf(" (%.0f%%)", 100**r.ResolutionRate)
		}
		key := r.Key
		if key == "" {
			key = "(unspecified)"
		}
		fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s", key, r.Findings, r.Labeled, r.Positive, r.Negative, r.Excluded, r.Unlabeled, r.RefuteReasons.FalsePositive, r.RefuteReasons.Spurious, r.RefuteReasons.RiskAccepted, r.RefuteReasons.Unspecified, rate, resolution)
		if priced {
			spend, per := "-", "-"
			if r.Cost.JobsWithCost > 0 {
				spend = fmt.Sprintf("~$%.2f", r.Cost.TotalUSD)
				if !r.Cost.Complete {
					spend = fmt.Sprintf("≥$%.2f (%d/%d priced)", r.Cost.TotalUSD, r.Cost.JobsWithCost, r.Cost.JobsTotal)
				}
				if r.CostPerPositive != nil {
					per = fmt.Sprintf("~$%.2f", *r.CostPerPositive)
					if !r.Cost.Complete {
						per = fmt.Sprintf("≥$%.2f (%d/%d priced)", *r.CostPerPositive, r.Cost.JobsWithCost, r.Cost.JobsTotal)
					}
				}
			}
			fmt.Fprintf(w, "\t%s\t%s", spend, per)
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
	if f.GroupBy == "severity" && priced {
		cmd.Println("  Severity spend overlaps across cells; each review is charged once per cell.")
	}
	cmd.Println()
}
