package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

type queuePauseResponse struct {
	QueuePaused bool `json:"queue_paused"`
}

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause queue processing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setQueuePaused(true)
		},
	}
}

func unpauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpause",
		Short: "Resume queue processing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setQueuePaused(false)
		},
	}
}

func setQueuePaused(paused bool) error {
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
	api := ep.APIClient(2 * time.Second)
	update := api.UnpauseQueueRaw
	if paused {
		update = api.PauseQueueRaw
	}
	resp, err := update(context.Background())
	if err != nil {
		return fmt.Errorf("update queue pause state: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update queue pause state: daemon returned %s", resp.Status)
	}

	var result queuePauseResponse
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		return fmt.Errorf("parse queue pause response: %w", err)
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
