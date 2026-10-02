package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause queue processing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setQueuePaused(cmd.Context(), true)
		},
	}
}

func unpauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpause",
		Short: "Resume queue processing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setQueuePaused(cmd.Context(), false)
		},
	}
}

func setQueuePaused(ctx context.Context, paused bool) error {
	// For a local daemon, carry the desired pause state into daemon startup so a
	// cold-started or restarted daemon comes up with the flag already applied,
	// before its workers can claim jobs. startDaemon passes it to the new
	// daemon, which persists it before workers start. A healthy, current-version
	// daemon is left running and picks up the state from the POST below instead.
	if serverAddr == "" {
		pendingStartPause = &paused
		defer func() { pendingStartPause = nil }()
	}
	if err := ensureDaemon(); err != nil {
		return err
	}

	ep := getDaemonEndpoint()
	api := ep.APIClient(0)
	update := api.UnpauseQueue
	if paused {
		update = api.PauseQueue
	}
	result, err := update(ctx)
	if err != nil {
		return daemonRequestError("update queue pause state", err)
	}

	if result.QueuePaused {
		fmt.Println("Queue paused. Running jobs will continue; no new jobs will start.")
	} else {
		fmt.Println("Queue unpaused. Workers will start queued jobs again.")
	}
	return nil
}

// pendingStartPause carries queue-pause intent into the daemon run arguments.
// Only the new daemon persists the flag, before workers can claim jobs.
var pendingStartPause *bool
