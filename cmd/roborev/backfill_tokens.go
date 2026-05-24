package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

func backfillTokensCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "backfill-tokens",
		Short: "Backfill token usage for completed jobs via agentsview",
		Long: `Scan completed jobs that have a session ID but missing token cost data,
and attempt to fetch token consumption from agentsview.

This is best-effort: jobs whose session files have been deleted
will be skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := storage.Open(storage.DefaultDBPath())
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			// Query all jobs (no status filter) and filter for
			// terminal states that could have token data.
			jobs, err := db.ListJobs("", "", 0, 0)
			if err != nil {
				return fmt.Errorf("list jobs: %w", err)
			}

			candidates := backfillCandidates(jobs)

			var total, updated, skipped, failed int
			for _, job := range candidates {
				total++

				ctx, cancel := context.WithTimeout(
					context.Background(), 15*time.Second,
				)
				usage, fetchErr := tokens.FetchForSession(
					ctx, job.SessionID,
				)
				cancel()

				if fetchErr != nil {
					log.Printf(
						"job %d: fetch error: %v", job.ID, fetchErr,
					)
					failed++
					continue
				}
				if usage == nil {
					skipped++
					continue
				}

				if dryRun {
					fmt.Printf(
						"job %d (%s): %s\n",
						job.ID, job.Agent, usage.FormatSummary(),
					)
					updated++
					continue
				}

				j := tokens.ToJSON(usage)
				if err := db.SaveJobTokenUsage(job.ID, j); err != nil {
					log.Printf(
						"job %d: save error: %v", job.ID, err,
					)
					failed++
					continue
				}
				updated++
				fmt.Printf(
					"job %d (%s): %s\n",
					job.ID, job.Agent, usage.FormatSummary(),
				)
			}

			action := "Updated"
			if dryRun {
				action = "Would update"
			}
			fmt.Printf(
				"\n%s %d/%d jobs (%d skipped, %d failed)\n",
				action, updated, total, skipped, failed,
			)
			return nil
		},
	}

	cmd.Flags().BoolVar(
		&dryRun, "dry-run", false,
		"show what would be updated without writing",
	)
	return cmd
}

// backfillCandidates filters jobs to those eligible for token
// backfill: completed, has a session ID, missing cost data, and the
// session was not reused by another started job.
func backfillCandidates(
	jobs []storage.ReviewJob,
) []storage.ReviewJob {
	// Count jobs that actually started per session ID. If
	// multiple jobs ran on the same session, it was resumed
	// and agentsview totals are cumulative — skip to avoid
	// overcounting.
	sessionCount := make(map[string]int)
	for _, job := range jobs {
		if job.SessionID != "" && job.StartedAt != nil {
			sessionCount[job.SessionID]++
		}
	}

	var out []storage.ReviewJob
	for _, job := range jobs {
		if !job.HasViewableOutput() {
			continue
		}
		if !needsTokenCostBackfill(job.TokenUsage) {
			continue
		}
		if job.SessionID == "" {
			continue
		}
		if sessionCount[job.SessionID] > 1 {
			continue
		}
		out = append(out, job)
	}
	return out
}

func needsTokenCostBackfill(tokenUsage string) bool {
	usage := tokens.ParseJSON(tokenUsage)
	return usage == nil || !usage.HasCost
}
