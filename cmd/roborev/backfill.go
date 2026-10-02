package main

import (
	"github.com/spf13/cobra"
)

func backfillVerdictsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "backfill-verdicts",
		Short:  "Backfill verdict_bool for legacy reviews",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			ep := getDaemonEndpoint()
			result, err := ep.APIClient(0).BackfillVerdicts(cmd.Context())
			if err != nil {
				return daemonRequestError("backfill", err)
			}
			if result.Count == 0 {
				cmd.Println("No reviews need backfilling.")
			} else {
				cmd.Printf("Backfilled verdict_bool for %d reviews.\n", result.Count)
			}
			return nil
		},
	}
}
