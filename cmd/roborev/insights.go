package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func insightsCmd() *cobra.Command {
	var (
		repoPath   string
		branch     string
		since      string
		agentName  string
		model      string
		reasoning  string
		wait       bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Analyze review patterns and suggest guideline improvements",
		Long: `Analyze failing code reviews to identify recurring patterns and suggest
improvements to review guidelines.

This is an LLM-powered command that:
1. Queries completed reviews (focusing on failures) from the database
2. Includes the currently resolved review_guidelines from global and repo config
3. Sends the batch to an agent with a structured analysis prompt
4. Returns actionable recommendations for guideline changes

The agent produces:
- Recurring finding patterns across reviews
- Hotspot areas (files/packages that concentrate failures)
- Noise candidates (findings consistently dismissed without code changes)
- Guideline gaps (patterns flagged by reviews but not in guidelines)
- Suggested guideline additions (concrete text for .roborev.toml or config.toml)

Examples:
  roborev insights                          # Analyze last 30 days of reviews
  roborev insights --since 7d               # Last 7 days only
  roborev insights --branch main            # Only reviews on main branch
  roborev insights --repo /path/to/repo     # Specific repo
  roborev insights --agent gemini --wait    # Use specific agent, wait for result
  roborev insights --json                   # Output job info as JSON`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInsights(cmd.Context(), cmd, insightsOptions{
				repoPath:   repoPath,
				branch:     branch,
				since:      since,
				agentName:  agentName,
				model:      model,
				reasoning:  reasoning,
				wait:       wait,
				jsonOutput: jsonOutput,
			})
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "scope to a single repo (default: current repo if tracked)")
	cmd.Flags().StringVar(&branch, "branch", "", "scope to a single branch")
	cmd.Flags().StringVar(&since, "since", "30d", "time window for reviews (e.g., 7d, 30d, 90d)")
	cmd.Flags().StringVar(&agentName, "agent", "", "agent to use for analysis (default: from config)")
	cmd.Flags().StringVar(&model, "model", "", "model for agent")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level: fast, standard, or thorough")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for completion and display result")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output job info as JSON")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)

	return cmd
}

type insightsOptions struct {
	repoPath   string
	branch     string
	since      string
	agentName  string
	model      string
	reasoning  string
	wait       bool
	jsonOutput bool
}

func runInsights(ctx context.Context, cmd *cobra.Command, opts insightsOptions) error {
	repoPath := opts.repoPath
	if repoPath == "" {
		workDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		repoPath = workDir
	} else {
		var err error
		repoPath, err = filepath.Abs(repoPath)
		if err != nil {
			return fmt.Errorf("resolve repo path: %w", err)
		}
	}

	if _, err := gitrepo.Root(ctx, repoPath); err != nil {
		if opts.repoPath == "" {
			return fmt.Errorf("not in a git repository (use --repo to specify one)")
		}
		return fmt.Errorf("--repo %q is not a git repository", opts.repoPath)
	}

	// Parse --since duration
	sinceTime, err := parseSinceDuration(opts.since)
	if err != nil {
		return fmt.Errorf("invalid --since value %q: %w", opts.since, err)
	}

	// Ensure daemon is running
	if err := ensureDaemon(); err != nil {
		return err
	}

	if !opts.jsonOutput {
		cmd.Printf("Queueing insights analysis since %s...\n", sinceTime.Format("2006-01-02"))
	}

	reqBody, _ := json.Marshal(daemon.EnqueueRequest{
		RepoPath:  repoPath,
		GitRef:    "insights",
		Branch:    opts.branch,
		Since:     sinceTime.Format(time.RFC3339),
		Agent:     opts.agentName,
		Model:     opts.model,
		Reasoning: opts.reasoning,
		JobType:   storage.JobTypeInsights,
	})

	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(30*time.Second).Post(ep.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var skipped struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal(body, &skipped); err == nil && skipped.Skipped {
			if opts.jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(map[string]any{
					"skipped": true,
					"reason":  skipped.Reason,
					"since":   sinceTime.Format(time.RFC3339),
				})
			}
			cmd.Println(skipped.Reason)
			return nil
		}
	}

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("enqueue failed: %s", body)
	}

	var job storage.ReviewJob
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	// JSON output mode
	if opts.jsonOutput {
		result := map[string]any{
			"job_id": job.ID,
			"agent":  job.Agent,
			"since":  sinceTime.Format(time.RFC3339),
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(result)
	}

	cmd.Printf("Enqueued insights job %d (agent: %s)\n", job.ID, job.Agent)

	// Wait for completion
	if opts.wait {
		return waitForPromptJob(cmd, ep, job.ID, false, promptPollInterval)
	}

	return nil
}

// parseSinceDuration parses a duration string like "7d", "30d", "90d" into a time.Time.
func parseSinceDuration(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().AddDate(0, 0, -30), nil
	}

	// Try standard Go duration first (e.g., "720h")
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{},
				fmt.Errorf("duration must be positive, got %s", s)
		}
		return time.Now().Add(-d), nil
	}

	// Parse day-based durations (e.g., "7d", "30d")
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days > 0 {
			return time.Now().AddDate(0, 0, -days), nil
		}
	}

	// Parse week-based durations (e.g., "2w", "4w")
	if strings.HasSuffix(s, "w") {
		var weeks int
		if _, err := fmt.Sscanf(s, "%dw", &weeks); err == nil && weeks > 0 {
			return time.Now().AddDate(0, 0, -weeks*7), nil
		}
	}

	return time.Time{}, fmt.Errorf("expected format like 7d, 4w, or 720h")
}
