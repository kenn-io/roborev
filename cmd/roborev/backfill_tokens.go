package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/roborev/pkg/client/generated"
)

func backfillTokensCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "backfill-tokens",
		Short: "Backfill token usage for completed jobs",
		Long: `Scan completed jobs missing token usage or cost data.

The daemon checks Codex job logs first for turn.completed usage events.
When available, agentsview is also queried to recover cost estimates.

This is best-effort: jobs whose session files have been deleted
will be skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			report, err := getDaemonEndpoint().APIClient(0).ScanTokenUsage(cmd.Context(), &generated.ScanTokenUsageRequestOptions{
				Body: &generated.ScanTokenUsageBody{DryRun: dryRun},
			})
			if err != nil {
				return daemonRequestError("backfill tokens", err)
			}

			for _, job := range report.Jobs {
				cmd.Printf("job %d (%s): %s\n", job.JobID, job.Agent, job.Summary)
			}
			action := "Updated"
			if dryRun {
				action = "Would update"
			}
			cmd.Printf("\n%s %d/%d jobs (%d skipped, %d failed)\n", action, report.Updated, report.Total, report.Skipped, report.Failed)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be updated without writing")
	return cmd
}
