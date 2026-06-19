package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func runCmd() *cobra.Command {
	var (
		agentName string
		model     string
		reasoning string
		wait      bool
		quiet     bool
		noContext bool
		agentic   bool
		label     string
	)

	cmd := &cobra.Command{
		Use:   "run [task]",
		Short: "Execute a task with an AI agent",
		Long: `Execute a task using an AI agent.

This command runs a task directly with an agent, useful for ad-hoc
work that may not be a traditional code review.

The task can be provided as:
  1. A positional argument: roborev run "your task here"
  2. Via stdin: echo "your task" | roborev run

By default, the job is enqueued and the command returns immediately.
Use --wait to wait for completion and display the result.

By default, context about the repository (name, path, and any project
guidelines from .roborev.toml) is included. Use --no-context to disable.

By default, agents run in review mode (read-only tools). Use --agentic
to enable write tools (Edit, Write, Bash) for tasks that modify files.

Examples:
  roborev run "Explain the architecture of this codebase"
  roborev run --agent claude-code "Refactor the error handling in main.go"
  roborev run --reasoning thorough "Find potential security issues"
  roborev run --wait "What does the main function do?"
  roborev run --no-context "What is 2+2?"
  roborev run --agentic "Create a new test file for main.go"
  roborev run --label refactor "Refactor the config module"
  cat instructions.txt | roborev run --wait
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrompt(cmd, args, agentName, model, reasoning, wait, quiet, !noContext, agentic, label)
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "agent to use (default: from config)")
	cmd.Flags().StringVar(&model, "model", "", "model for agent (format varies: opencode uses provider/model, others use model name)")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level: fast, standard, medium, thorough (default), or maximum")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for job to complete and show result")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress output (just enqueue)")
	cmd.Flags().BoolVar(&noContext, "no-context", false, "don't include repository context in prompt")
	cmd.Flags().BoolVar(&agentic, "agentic", false, "enable agentic mode (allow file edits and commands)")
	cmd.Flags().BoolVar(&agentic, "yolo", false, "alias for --agentic")
	cmd.Flags().StringVar(&label, "label", "", "custom label to display in TUI (default: run)")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)

	return cmd
}

// promptCmd returns a hidden alias for backward compatibility
func promptCmd() *cobra.Command {
	cmd := runCmd()
	cmd.Use = "prompt [task]"
	cmd.Hidden = true
	return cmd
}

func runPrompt(cmd *cobra.Command, args []string, agentName, modelStr, reasoningStr string, wait, quiet, includeContext, agentic bool, label string) error {
	// Get prompt from args or stdin
	var promptText string
	if len(args) > 0 {
		promptText = strings.Join(args, " ")
	} else {
		// Read from stdin
		stat, err := os.Stdin.Stat()
		if err != nil {
			return fmt.Errorf("unable to read stdin: %w", err)
		}
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			// Stdin has data (piped) - use io.ReadAll to handle large prompts
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("reading stdin: %w", err)
			}
			promptText = string(data)
		} else {
			return fmt.Errorf("no prompt provided - pass as argument or pipe via stdin")
		}
	}

	if strings.TrimSpace(promptText) == "" {
		return fmt.Errorf("empty prompt")
	}

	// Determine working directory (use git repo root if in a repo, otherwise cwd)
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Try to use git repo root if available
	repoRoot := workDir
	if root, err := gitrepo.Root(cmd.Context(), workDir); err == nil {
		repoRoot = root
	}

	// Build the full prompt with context if enabled
	fullPrompt := promptText
	if includeContext {
		fullPrompt = buildPromptWithContext(repoRoot, promptText)
	}

	// Ensure daemon is running
	if err := ensureDaemon(); err != nil {
		return err
	}

	// Build the request
	gitRef := "run"
	if label != "" {
		gitRef = label
	}
	reqBody, _ := json.Marshal(daemon.EnqueueRequest{
		RepoPath:     repoRoot,
		GitRef:       gitRef,
		Agent:        agentName,
		Model:        modelStr,
		Reasoning:    reasoningStr,
		CustomPrompt: fullPrompt,
		Agentic:      agentic,
	})

	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(10*time.Second).Post(ep.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("enqueue failed: %s", body)
	}

	var job storage.ReviewJob
	if err := json.Unmarshal(body, &job); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if !quiet {
		cmd.Printf("Enqueued task %d (agent: %s)\n", job.ID, job.Agent)
	}

	// If --wait, poll until job completes and show result
	if wait {
		return waitForPromptJob(cmd, ep, job.ID, quiet, promptPollInterval)
	}

	return nil
}

// promptPollInterval is the initial poll interval for waiting on prompt jobs.
// Can be overridden in tests to speed them up.
var promptPollInterval = 500 * time.Millisecond

// waitForPromptJob waits for a prompt job to complete and displays the result.
// Unlike waitForJob, this doesn't apply verdict-based exit codes since prompt
// jobs don't have PASS/FAIL verdicts.
func waitForPromptJob(cmd *cobra.Command, ep daemon.DaemonEndpoint, jobID int64, quiet bool, pollInterval time.Duration) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	api := newDaemonReviewAPI(ep.BaseURL(), ep.HTTPClient(5*time.Second))

	if pollInterval <= 0 {
		pollInterval = promptPollInterval
	}

	if !quiet {
		cmd.Printf("Waiting for task to complete...")
	}

	// Poll with exponential backoff
	maxInterval := 5 * time.Second
	unknownStatusCount := 0
	const maxUnknownRetries = 10 // Give up after 10 consecutive unknown statuses

	for {
		job, err := api.getJob(ctx, jobID)
		if err != nil {
			return fmt.Errorf("failed to check job status: %w", err)
		}

		switch job.Status {
		case storage.JobStatusDone:
			// Pass done message to showPromptResult - it prints after successful fetch
			return showPromptResult(cmd, ep.BaseURL(), jobID, quiet, " done!\n\n")

		case storage.JobStatusFailed:
			if !quiet {
				cmd.Printf(" failed!\n")
			}
			return fmt.Errorf("prompt failed: %s", job.Error)

		case storage.JobStatusCanceled:
			if !quiet {
				cmd.Printf(" canceled!\n")
			}
			return fmt.Errorf("prompt was canceled")

		case storage.JobStatusQueued, storage.JobStatusRunning:
			// Still in progress, continue polling
			unknownStatusCount = 0 // Reset counter on known status
			time.Sleep(pollInterval)
			if pollInterval < maxInterval {
				pollInterval = min(time.Duration(float64(pollInterval)*1.5), maxInterval)
			}

		default:
			// Unknown status - treat as transient for forward-compatibility
			// (daemon may add new statuses in the future)
			unknownStatusCount++
			if unknownStatusCount >= maxUnknownRetries {
				return fmt.Errorf("received unknown status %q %d times, giving up (daemon may be newer than CLI)", job.Status, unknownStatusCount)
			}
			if !quiet {
				cmd.Printf("\n(unknown status %q, continuing to poll...)", job.Status)
			}
			time.Sleep(pollInterval)
			if pollInterval < maxInterval {
				pollInterval = min(time.Duration(float64(pollInterval)*1.5), maxInterval)
			}
		}
	}
}

// showPromptResult fetches and displays the result of a prompt job.
// Unlike showReview, this doesn't apply verdict-based exit codes.
// The doneMsg parameter is printed before the result on success (used for "done!" message).
func showPromptResult(cmd *cobra.Command, addr string, jobID int64, quiet bool, doneMsg string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	api := newDaemonReviewAPI(addr, getDaemonHTTPClient(5*time.Second))
	review, err := api.getReview(ctx, jobID, "result")
	if errors.Is(err, errReviewNotFound) {
		return fmt.Errorf("no result found for job %d", jobID)
	}
	if err != nil {
		return err
	}

	// Only print after successful fetch to avoid "done!" followed by error
	if !quiet {
		if doneMsg != "" {
			cmd.Print(doneMsg)
		}
		cmd.Printf("Result (by %s)\n", review.Agent)
		cmd.Println(strings.Repeat("-", 60))
		cmd.Println(review.Output)
	}

	// Prompt jobs always exit 0 on success (no verdict-based exit codes)
	return nil
}

// buildPromptWithContext wraps the user's prompt with repository context
func buildPromptWithContext(repoPath, userPrompt string) string {
	var sb strings.Builder

	repoName := filepath.Base(repoPath)

	sb.WriteString("## Context\n\n")
	fmt.Fprintf(&sb, "You are working in the repository \"%s\" at %s.\n", repoName, repoPath)

	// Load project guidelines if available.
	globalCfg, _ := config.LoadGlobal()
	guidelines := prompt.LoadGuidelinesLocal(repoPath, globalCfg)
	if guidelines != "" {
		sb.WriteString("\n## Project Guidelines\n\n")
		sb.WriteString("The following are project-specific guidelines for this repository:\n\n")
		sb.WriteString(strings.TrimSpace(guidelines))
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Request\n\n")
	sb.WriteString(userPrompt)

	return sb.String()
}
