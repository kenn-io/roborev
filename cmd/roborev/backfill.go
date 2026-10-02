package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/pkg/client/generated"
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
			result, err := newDaemonAPI(ep.BaseURL(), ep.HTTPClient(0)).BackfillVerdicts(cmd.Context())
			if err != nil {
				if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Detail != nil {
					return fmt.Errorf("backfill: %s", *problem.Detail)
				}
				return fmt.Errorf("backfill: %w", err)
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
