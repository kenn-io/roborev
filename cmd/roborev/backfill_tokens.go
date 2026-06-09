package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/config"
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
			cfg, err := config.LoadGlobal()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			fetchConfig := backfillCostFetchConfig(cfg)

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

			candidates := backfill.TokenCandidates(jobs)

			var total, updated, skipped, failed int
			for _, job := range candidates {
				total++

				ctx, cancel := context.WithTimeout(
					context.Background(), 15*time.Second,
				)
				usage, fetchErr := tokens.FetchForSessionWithConfig(
					ctx, job.SessionID, fetchConfig,
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
				mergedUsage := backfill.MergeTokenUsage(job.TokenUsage, usage)

				if dryRun {
					fmt.Printf(
						"job %d (%s): %s\n",
						job.ID, job.Agent, mergedUsage.FormatSummary(),
					)
					updated++
					continue
				}

				j := tokens.ToJSON(mergedUsage)
				if err := db.SaveJobTokenUsage(job.ID, job.SessionID, j); err != nil {
					log.Printf(
						"job %d: save error: %v", job.ID, err,
					)
					failed++
					continue
				}
				updated++
				fmt.Printf(
					"job %d (%s): %s\n",
					job.ID, job.Agent, mergedUsage.FormatSummary(),
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

func backfillCostFetchConfig(cfg *config.Config) tokens.FetchConfig {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	return tokens.FetchConfig{
		Endpoint:   cfg.Cost.Endpoint,
		Timeout:    cfg.Cost.ResolvedTimeout(),
		RequireCLI: true,
	}
}
