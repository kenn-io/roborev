package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
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
	// before its workers can claim jobs. startDaemon persists it in the safe
	// window after any previous daemon has stopped, so the CLI never migrates a
	// live database. A healthy, current-version daemon is left running and picks
	// up the state from the POST below instead.
	if serverAddr == "" {
		pendingStartPause = &paused
		defer func() { pendingStartPause = nil }()
	}
	if err := ensureDaemon(); err != nil {
		return err
	}

	ep := getDaemonEndpoint()
	addr := ep.BaseURL()
	path := "/api/queue/unpause"
	if paused {
		path = "/api/queue/pause"
	}

	resp, err := ep.HTTPClient(2*time.Second).Post(addr+path, "application/json", nil)
	if err != nil {
		return fmt.Errorf("update queue pause state: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update queue pause state: daemon returned %s", resp.Status)
	}

	var result queuePauseResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse queue pause response: %w", err)
	}

	if result.QueuePaused {
		fmt.Println("Queue paused. Running jobs will continue; no new jobs will start.")
	} else {
		fmt.Println("Queue unpaused. Workers will start queued jobs again.")
	}
	return nil
}

// pendingStartPause carries the queue-pause state that startDaemon must persist
// to the local database immediately before launching a daemon. It lets a
// cold-started or restarted daemon come up with the pause flag already applied
// without the CLI opening the database while an older daemon still owns it.
var pendingStartPause *bool

// writeLocalQueuePaused persists the queue-pause flag to the default local
// database. The caller must ensure no daemon currently owns the database (for
// example, immediately before startDaemon launches a new one).
func writeLocalQueuePaused(paused bool) error {
	db, err := storage.Open(storage.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("open local daemon database: %w", err)
	}
	defer db.Close()
	if err := db.SetQueuePaused(paused); err != nil {
		return fmt.Errorf("persist local queue pause state: %w", err)
	}
	return nil
}
