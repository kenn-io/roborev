package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/storage"
)

type statusJSONResult struct {
	Running bool                  `json:"running"`
	Daemon  *storage.DaemonStatus `json:"daemon,omitempty"`
	Health  *storage.HealthStatus `json:"health,omitempty"`
	Jobs    []storage.ReviewJob   `json:"jobs,omitempty"`
	Error   string                `json:"error,omitempty"`
}

func statusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon and queue status",
		RunE: func(cmd *cobra.Command, args []string) error {
			writeJSONResult := func(result statusJSONResult) error {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			writeStatusUnavailable := func(err error) error {
				if jsonOutput {
					return writeJSONResult(statusJSONResult{
						Running: true,
						Error:   err.Error(),
					})
				}
				fmt.Println("Daemon: running")
				fmt.Printf("Status: unavailable: %v\n", err)
				return nil
			}

			// Ensure daemon is running (and restart if version mismatch)
			if err := ensureDaemon(); err != nil {
				if jsonOutput {
					return writeJSONResult(statusJSONResult{Running: false})
				}
				fmt.Println("Daemon: not running")
				fmt.Println()
				fmt.Println("Start with: roborev daemon start")
				return nil
			}

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			client := ep.HTTPClient(2 * time.Second)
			resp, err := client.Get(addr + "/api/status")
			if err != nil {
				return writeStatusUnavailable(err)
			}
			defer resp.Body.Close()

			var status storage.DaemonStatus
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			// Get health status
			healthResp, err := client.Get(addr + "/api/health")
			var health *storage.HealthStatus
			if err == nil {
				defer healthResp.Body.Close()
				var decoded storage.HealthStatus
				if err := json.NewDecoder(healthResp.Body).Decode(&decoded); err != nil {
					log.Printf("failed to parse health response: %v", err)
				} else {
					health = &decoded
				}
			}

			// Get recent jobs
			var jobs []storage.ReviewJob
			resp, err = client.Get(addr + "/api/jobs?limit=10")
			if err == nil {
				defer resp.Body.Close()

				var jobsResp struct {
					Jobs []storage.ReviewJob `json:"jobs"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&jobsResp); err == nil {
					jobs = jobsResp.Jobs
				}
			}

			if jsonOutput {
				return writeJSONResult(statusJSONResult{
					Running: true,
					Daemon:  &status,
					Health:  health,
					Jobs:    jobs,
				})
			}

			// Display daemon info with uptime and version
			daemonLine := "Daemon: running"
			if health != nil && health.Uptime != "" {
				daemonLine += fmt.Sprintf(" (uptime: %s)", health.Uptime)
			}
			if status.Version != "" {
				daemonLine += fmt.Sprintf(" [%s]", status.Version)
			}
			fmt.Println(daemonLine)
			workersLine := fmt.Sprintf("Workers: %d/%d active", status.ActiveWorkers, status.MaxWorkers)
			if status.QueuePaused {
				workersLine += " (paused)"
			}
			fmt.Println(workersLine)
			fmt.Printf("Jobs:    %d queued, %d running, %d completed, %d failed, %d skipped\n",
				status.QueuedJobs, status.RunningJobs, status.CompletedJobs, status.FailedJobs, status.SkippedJobs)
			fmt.Println()

			// Display health status
			if health != nil && health.Version != "" {
				if health.Healthy {
					fmt.Println("Health: OK")
				} else {
					fmt.Println("Health: DEGRADED")
				}
				for _, comp := range health.Components {
					checkmark := "+"
					if !comp.Healthy {
						checkmark = "!"
					}
					if comp.Message != "" {
						fmt.Printf("  %s %s: %s\n", checkmark, comp.Name, comp.Message)
					} else {
						fmt.Printf("  %s %s: healthy\n", checkmark, comp.Name)
					}
				}
				fmt.Println()

				// Display recent errors if any
				if health.ErrorCount > 0 {
					fmt.Printf("Recent Errors (last 24h): %d\n", health.ErrorCount)
					for _, e := range health.RecentErrors {
						ago := time.Since(e.Timestamp).Round(time.Minute)
						if e.JobID > 0 {
							fmt.Printf("  [%v ago] %s: job %d - %s\n", ago, e.Component, e.JobID, e.Message)
						} else {
							fmt.Printf("  [%v ago] %s: %s\n", ago, e.Component, e.Message)
						}
					}
					fmt.Println()
				}
			}

			if len(jobs) > 0 {
				fmt.Println("Recent Jobs:")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "  ID\tSHA\tRepo\tAgent\tStatus\tTime\n")
				for _, j := range jobs {
					elapsed := ""
					if j.StartedAt != nil {
						if j.FinishedAt != nil {
							elapsed = j.FinishedAt.Sub(*j.StartedAt).Round(time.Second).String()
						} else {
							elapsed = time.Since(*j.StartedAt).Round(time.Second).String() + "..."
						}
					}
					// Show [remote] indicator for jobs from other machines
					repoDisplay := j.RepoName
					if status.MachineID != "" && j.SourceMachineID != "" && j.SourceMachineID != status.MachineID {
						repoDisplay += " [remote]"
					}
					fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\n",
						j.ID, shortRef(j.GitRef), repoDisplay, j.Agent, j.Status, elapsed)
				}
				w.Flush()
			}

			// Check for outdated hooks in current repo
			if root, err := gitrepo.Root(cmd.Context(), "."); err == nil {
				if githook.NeedsUpgrade(cmd.Context(), root, "post-commit", githook.PostCommitVersionMarker) {
					fmt.Println()
					fmt.Println("Warning: post-commit hook is outdated -- run 'roborev init' to upgrade")
				}
				if githook.NeedsUpgrade(cmd.Context(), root, "post-rewrite", githook.PostRewriteVersionMarker) ||
					githook.Missing(cmd.Context(), root, "post-rewrite") {
					fmt.Println()
					fmt.Println("Warning: post-rewrite hook is missing or outdated -- run 'roborev init' to install")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "structured output for scripting")
	return cmd
}
