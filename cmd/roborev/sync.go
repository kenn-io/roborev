package main

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/pkg/client/generated"
)

func syncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Manage PostgreSQL sync",
		Long:  "Commands for managing synchronization with a PostgreSQL database.",
	}

	cmd.AddCommand(syncStatusCmd())
	cmd.AddCommand(syncNowCmd())

	return cmd
}

func syncStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			ep := getDaemonEndpoint()
			status, err := ep.APIClient(0).GetSyncStatus(cmd.Context())
			if err != nil {
				return daemonRequestError("fetch sync status", err)
			}
			if !status.Enabled {
				cmd.Println("Sync: disabled")
				cmd.Println()
				cmd.Println("Enable in ~/.roborev/config.toml:")
				cmd.Println("  [sync]")
				cmd.Println("  enabled = true")
				cmd.Println("  postgres_url = \"postgres://...\"")
				return nil
			}
			cmd.Println("Sync: enabled")
			cmd.Printf("Interval: %s\n", status.Interval)
			if status.MachineName != "" {
				cmd.Printf("Machine name: %s\n", status.MachineName)
			}
			for _, warning := range status.Warnings {
				cmd.Printf("Warning: %s\n", warning)
			}
			if status.MachineID != nil {
				cmd.Printf("Machine ID: %s\n", *status.MachineID)
			}
			cmd.Println()
			if status.PendingIncomplete {
				cmd.Println("Warning: could not count all pending items")
			}
			formatCount := func(count int64) string {
				if count >= status.PendingLimit {
					return fmt.Sprintf(">=%d", count)
				}
				return fmt.Sprintf("%d", count)
			}
			cmd.Printf("Pending push: %s jobs, %s reviews, %s comments\n",
				formatCount(status.PendingPush.Jobs), formatCount(status.PendingPush.Reviews), formatCount(status.PendingPush.Comments))
			cmd.Println()

			if status.Connected {
				cmd.Println("PostgreSQL: connected")
			} else {
				cmd.Println("PostgreSQL: disconnected")
			}
			if status.Message != "" {
				cmd.Println(status.Message)
			}

			return nil
		},
	}
}

func syncNowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "now",
		Short: "Trigger immediate sync",
		Long:  "Triggers an immediate sync cycle. Requires the daemon to be running with sync enabled.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			// Use longer timeout since sync operations can take up to 5 minutes
			client := ep.HTTPClient(6 * time.Minute)

			// Use streaming endpoint to show progress
			resp, err := newDaemonAPI(addr, client).SyncNowRaw(cmd.Context(), &generated.SyncNowRequestOptions{Query: &generated.SyncNowQuery{Stream: new("1")}})
			if err != nil {
				return fmt.Errorf("failed to trigger sync: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				fmt.Println("Sync not enabled on daemon")
				return nil
			}

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("sync failed: %s", string(body))
			}

			// Read streaming progress
			scanner := bufio.NewScanner(resp.Body)
			var finalPushed, finalPulled struct {
				Jobs      int `json:"jobs"`
				Reviews   int `json:"reviews"`
				Responses int `json:"responses"`
			}

			// Helper to safely get int from map
			getInt := func(m map[string]any, key string) int {
				if v, ok := m[key].(float64); ok {
					return int(v)
				}
				return 0
			}
			getString := func(m map[string]any, key string) string {
				if v, ok := m[key].(string); ok {
					return v
				}
				return ""
			}

			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}

				var msg map[string]any
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					continue
				}

				switch getString(msg, "type") {
				case "progress":
					phase := getString(msg, "phase")
					switch phase {
					case "push":
						batch := getInt(msg, "batch")
						totalJobs := getInt(msg, "total_jobs")
						totalRevs := getInt(msg, "total_revs")
						totalResps := getInt(msg, "total_resps")
						fmt.Printf("\rPushing: batch %d (total: %d jobs, %d reviews, %d comments)     ",
							batch, totalJobs, totalRevs, totalResps)
					case "pull":
						totalJobs := getInt(msg, "total_jobs")
						totalRevs := getInt(msg, "total_revs")
						totalResps := getInt(msg, "total_resps")
						fmt.Printf("\rPulled: %d jobs, %d reviews, %d comments     \n",
							totalJobs, totalRevs, totalResps)
					}
				case "error":
					fmt.Println()
					return fmt.Errorf("sync failed: %s", getString(msg, "error"))
				case "complete":
					fmt.Println() // Clear the progress line
					if pushed, ok := msg["pushed"].(map[string]any); ok {
						finalPushed.Jobs = getInt(pushed, "jobs")
						finalPushed.Reviews = getInt(pushed, "reviews")
						finalPushed.Responses = getInt(pushed, "responses")
					}
					if pulled, ok := msg["pulled"].(map[string]any); ok {
						finalPulled.Jobs = getInt(pulled, "jobs")
						finalPulled.Reviews = getInt(pulled, "reviews")
						finalPulled.Responses = getInt(pulled, "responses")
					}
				}
			}

			if err := scanner.Err(); err != nil {
				return fmt.Errorf("error reading sync progress: %w", err)
			}

			fmt.Println("Sync completed")
			fmt.Printf("Pushed: %d jobs, %d reviews, %d comments\n",
				finalPushed.Jobs, finalPushed.Reviews, finalPushed.Responses)
			fmt.Printf("Pulled: %d jobs, %d reviews, %d comments\n",
				finalPulled.Jobs, finalPulled.Reviews, finalPulled.Responses)

			return nil
		},
	}
}
