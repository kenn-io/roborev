package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

func cancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <job_id>",
		Short: "Cancel a queued or running review job",
		Long: `Cancel a review job by ID.

Only jobs that are still queued or running can be canceled; jobs that have
already finished (done, failed, canceled, etc.) cannot.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			jobID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || jobID <= 0 {
				return fmt.Errorf("invalid job_id: %s", args[0])
			}

			ep := getDaemonEndpoint()
			if err := cancelJob(ep.BaseURL(), jobID); err != nil {
				return err
			}

			fmt.Printf("Job %d canceled\n", jobID)
			return nil
		},
	}

	return cmd
}
