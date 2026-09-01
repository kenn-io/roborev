package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/storage"
)

var (
	statusEnsureDaemon = ensureDaemon
	statusDiscover     = uiRuntimeInfo
)

type statusJSONResult struct {
	Running           bool                  `json:"running"`
	WebURL            string                `json:"web_url"`
	WebDisabledReason string                `json:"web_disabled_reason,omitempty"`
	Daemon            *storage.DaemonStatus `json:"daemon,omitempty"`
	Health            *storage.HealthStatus `json:"health,omitempty"`
	Jobs              []storage.ReviewJob   `json:"jobs,omitempty"`
	Error             string                `json:"error,omitempty"`
}

func statusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon, browser UI, and queue status",
		RunE: func(cmd *cobra.Command, args []string) error {
			webStatus := webUIStatus{}
			writeJSONResult := func(result statusJSONResult) error {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			writeStatusUnavailable := func(err error) error {
				if jsonOutput {
					return writeJSONResult(statusJSONResult{
						Running:           true,
						WebURL:            webStatus.url,
						WebDisabledReason: webStatus.disabledReason,
						Error:             err.Error(),
					})
				}
				fmt.Println("Daemon: running")
				fmt.Printf("Web UI: %s\n", displayWebUI(webStatus))
				fmt.Printf("Status: unavailable: %v\n", err)
				return nil
			}

			// Ensure daemon is running (and restart if version mismatch)
			if err := statusEnsureDaemon(); err != nil {
				if errors.Is(err, daemon.ErrDaemonAccessDenied) {
					message := fmt.Sprintf(
						"%v; if roborev is running in a sandbox, allow loopback or Unix socket access and retry",
						err,
					)
					if jsonOutput {
						return writeJSONResult(statusJSONResult{
							Running:           true,
							WebURL:            webStatus.url,
							WebDisabledReason: webStatus.disabledReason,
							Error:             message,
						})
					}
					fmt.Println("Daemon: status unavailable")
					fmt.Printf("Web UI: %s\n", displayWebUI(webStatus))
					fmt.Println(message)
					return nil
				}
				if jsonOutput {
					return writeJSONResult(statusJSONResult{Running: false})
				}
				fmt.Println("Daemon: not running")
				fmt.Println()
				fmt.Println("Start with: roborev daemon start")
				return nil
			}
			webStatus = discoverWebUI(statusDiscover)

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			client := ep.HTTPClient(2 * time.Second)
			resp, err := client.Get(addr + "/api/status")
			if err != nil {
				return writeStatusUnavailable(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return writeStatusUnavailable(
					fmt.Errorf("daemon returned %s", resp.Status),
				)
			}

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
					Running:           true,
					WebURL:            webStatus.url,
					WebDisabledReason: webStatus.disabledReason,
					Daemon:            &status,
					Health:            health,
					Jobs:              jobs,
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
			fmt.Printf("Web UI: %s\n", displayWebUI(webStatus))
			workersLine := fmt.Sprintf("Workers: %d/%d active", status.ActiveWorkers, status.MaxWorkers)
			if status.QueuePaused {
				workersLine += " (paused)"
			}
			if updateDrain := formatUpdateDrainStatus(status, time.Now()); updateDrain != "" {
				workersLine += " (" + updateDrain + ")"
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

			if len(status.ActiveSnoozes) > 0 {
				fmt.Println("Active Snoozes:")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "  Repo\tWorktree\tBranch\tUntil")
				for _, snooze := range status.ActiveSnoozes {
					fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
						snooze.RepoName,
						snooze.WorktreePath,
						snooze.Branch,
						snooze.SnoozedUntil.Local().Format("Jan 02 15:04 MST"),
					)
				}
				if err := w.Flush(); err != nil {
					return fmt.Errorf("flush active snoozes: %w", err)
				}
				fmt.Println()
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
					if status.MachineID != nil && j.SourceMachineID != nil && *j.SourceMachineID != *status.MachineID {
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
				if githook.NeedsUpgrade(cmd.Context(), root, "pre-push", githook.PrePushVersionMarker) ||
					githook.Missing(cmd.Context(), root, "pre-push") {
					fmt.Println()
					fmt.Println("Warning: pre-push hook is missing or outdated -- run 'roborev init' to install")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "structured output for scripting")
	return cmd
}

func formatUpdateDrainStatus(status storage.DaemonStatus, now time.Time) string {
	if !status.UpdateDraining {
		return ""
	}
	expiresAt, err := time.Parse(time.RFC3339, status.UpdateDrainExpiresAt)
	if err == nil && !expiresAt.After(now) {
		return fmt.Sprintf("update recovery (%s)", status.UpdateDrainPolicy)
	}
	if err == nil {
		return fmt.Sprintf(
			"update %s (lease %s)",
			status.UpdateDrainPolicy,
			expiresAt.Sub(now).Round(time.Second),
		)
	}
	if status.UpdateDrainPolicy != "" {
		return "update " + status.UpdateDrainPolicy
	}
	return "update drain"
}
