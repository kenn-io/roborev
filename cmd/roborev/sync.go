package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
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
			// Load config
			cfg, err := config.LoadGlobal()
			if err != nil {
				cfg = config.DefaultConfig()
			}

			if !cfg.Sync.Enabled {
				fmt.Println("Sync: disabled")
				fmt.Println()
				fmt.Println("Enable in ~/.roborev/config.toml:")
				fmt.Println("  [sync]")
				fmt.Println("  enabled = true")
				fmt.Println("  postgres_url = \"postgres://...\"")
				return nil
			}

			fmt.Println("Sync: enabled")
			fmt.Printf("Interval: %s\n", cfg.Sync.Interval)
			if cfg.Sync.MachineName != "" {
				fmt.Printf("Machine name: %s\n", cfg.Sync.MachineName)
			}

			// Validate config
			warnings := cfg.Sync.Validate()
			for _, w := range warnings {
				fmt.Printf("Warning: %s\n", w)
			}

			// Open database to check pending items
			db, err := storage.Open(storage.DefaultDBPath())
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()

			machineID, err := db.GetMachineID()
			if err != nil {
				return fmt.Errorf("failed to get machine ID: %w", err)
			}
			fmt.Printf("Machine ID: %s\n", machineID)

			// Count pending items
			const maxPending = 1000
			jobs, jobsErr := db.GetJobsToSync(machineID, maxPending)
			reviews, reviewsErr := db.GetReviewsToSync(machineID, maxPending)
			responses, responsesErr := db.GetCommentsToSync(machineID, maxPending)

			fmt.Println()
			if jobsErr != nil || reviewsErr != nil || responsesErr != nil {
				fmt.Println("Warning: could not count all pending items")
			}

			// Format counts with >= indicator when hitting the cap
			formatCount := func(count int) string {
				if count >= maxPending {
					return fmt.Sprintf(">=%d", count)
				}
				return fmt.Sprintf("%d", count)
			}
			fmt.Printf("Pending push: %s jobs, %s reviews, %s comments\n",
				formatCount(len(jobs)), formatCount(len(reviews)), formatCount(len(responses)))

			// Try to connect to PostgreSQL
			fmt.Println()
			fmt.Print("PostgreSQL: ")
			url := cfg.Sync.PostgresURLExpanded()
			if url == "" {
				fmt.Println("not configured")
				return nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			pgCfg := storage.DefaultPgPoolConfig()
			pgCfg.ConnectTimeout = 5 * time.Second
			pool, err := storage.NewPgPool(ctx, url, pgCfg)
			if err != nil {
				fmt.Printf("connection failed (%v)\n", err)
				return nil
			}
			defer pool.Close()

			fmt.Println("connected")

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
			resp, err := client.Post(addr+"/api/sync/now?stream=1", "application/json", nil)
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
