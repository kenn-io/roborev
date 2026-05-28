package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
)

func backfillVerdictsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "backfill-verdicts",
		Short:  "Backfill verdict_bool for legacy reviews",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := storage.Open(storage.DefaultDBPath())
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			count, err := db.BackfillVerdictBool()
			if err != nil {
				return fmt.Errorf("backfill: %w", err)
			}

			if count == 0 {
				cmd.Println("No reviews need backfilling.")
			} else {
				cmd.Printf("Backfilled verdict_bool for %d reviews.\n", count)
			}
			return nil
		},
	}
	return cmd
}
