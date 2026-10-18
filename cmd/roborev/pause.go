package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
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
	var token string
	if serverAddr == "" {
		var err error
		token, err = daemon.PublishQueueIntent(ctx, paused)
		if err != nil {
			return err
		}
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
	result, err := update(ctx, func(_ context.Context, req *http.Request) error {
		if token != "" {
			req.Header.Set(daemon.QueueIntentHeader, token)
		}
		return nil
	})
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
