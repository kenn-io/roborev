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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// Maximum time to wait for an analysis job to complete
const analyzeJobTimeout = 30 * time.Minute

func analyzeCmd() *cobra.Command {
	var (
		agentName  string
		model      string
		reasoning  string
		wait       bool
		quiet      bool
		listTypes  bool
		showPrompt bool
		fix        bool
		fixAgent   string
		fixModel   string
		perFile    bool
		jsonOutput bool
		branch     string
		baseBranch string
	)

	cmd := &cobra.Command{
		Use:   "analyze <type> <files...>",
		Short: "Run built-in analysis on files",
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				// Complete analysis type names
				var completions []string
				for _, t := range analyze.AllTypes {
					completions = append(completions, t.Name+"\t"+t.Description)
				}
				return completions, cobra.ShellCompDirectiveNoFileComp
			}
			// After the type, complete file paths
			return nil, cobra.ShellCompDirectiveDefault
		},
		Long: `Run a built-in analysis type on one or more files.

This command provides predefined analysis prompts for common code review
tasks like finding duplication, suggesting refactorings, or identifying
test fixture opportunities.

Analysis runs in agentic mode, allowing the agent to read files when the
prompt content exceeds the configured size limit (default_max_prompt_size
in config.toml or max_prompt_size in .roborev.toml, default 200KB).

The output is formatted for easy copy-paste into agent sessions, with
a header showing the analysis type and files analyzed.

Available analysis types:
  test-fixtures  Find test fixture and helper opportunities
  duplication    Find code duplication across files
  refactor       Suggest refactoring opportunities
  complexity     Analyze complexity and suggest simplifications
  api-design     Review API consistency and design patterns
  dead-code      Find unused exports and unreachable code
  architecture   Review architectural patterns and structure
  security       Find concrete security risks in existing code

Examples:
  roborev analyze test-fixtures internal/storage/*_test.go
  roborev analyze duplication cmd/roborev/*.go
  roborev analyze refactor --wait main.go utils.go
  roborev analyze complexity --agent gemini ./...
  roborev analyze architecture internal/storage/    # analyze a directory
  roborev analyze security ./...                    # security hardening pass
  roborev analyze --list

Per-file mode (--per-file):
  Creates one analysis job per file instead of bundling all files together.
  Useful for parallel analysis or when total content would be too large.

  roborev analyze refactor --per-file internal/storage/*.go
  roborev analyze complexity --per-file --wait *.go

Branch mode (--branch):
  Analyzes files changed on the current branch since diverging from the base.
  Non-code files (.md, .yml, .json, .toml, etc.) are excluded automatically.
  Optionally specify a branch name to analyze a different branch.

  roborev analyze refactor --branch                  # Current branch vs auto-detected base
  roborev analyze complexity --branch --per-file     # Per-file analysis of branch changes
  roborev analyze refactor --branch=feature-xyz      # Analyze a specific branch
  roborev analyze refactor --branch --base develop   # Compare against develop

Fix mode (--fix):
  Runs analysis, then invokes an agentic agent to apply the suggested changes.
  The analysis is saved to the database and marked as closed when complete.

  roborev analyze refactor --fix ./...
  roborev analyze duplication --fix --fix-agent claude-code *.go

To fix an existing analysis job, use: roborev fix <job_id>
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if listTypes {
				return nil
			}
			if showPrompt {
				if len(args) < 1 {
					return fmt.Errorf("--show-prompt requires an analysis type")
				}
				return nil
			}
			if branch != "" {
				if len(args) < 1 {
					return fmt.Errorf("--branch requires an analysis type")
				}
				if len(args) > 1 {
					return fmt.Errorf("cannot specify file patterns with --branch (to analyze a specific branch, use --branch=<name>)")
				}
				return nil
			}
			if len(args) < 2 {
				return fmt.Errorf("requires analysis type and at least one file")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if listTypes {
				return listAnalysisTypes(cmd)
			}
			if showPrompt {
				return showAnalysisPrompt(cmd, args[0])
			}
			opts := analyzeOptions{
				agentName:  agentName,
				model:      model,
				reasoning:  reasoning,
				wait:       wait,
				quiet:      quiet,
				fix:        fix,
				fixAgent:   fixAgent,
				fixModel:   fixModel,
				perFile:    perFile,
				jsonOutput: jsonOutput,
				branch:     branch,
				baseBranch: baseBranch,
			}
			var filePatterns []string
			if len(args) > 1 {
				filePatterns = args[1:]
			}
			return runAnalysis(cmd, args[0], filePatterns, opts)
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "agent to use for analysis (default: from config)")
	cmd.Flags().StringVar(&model, "model", "", "model for analysis agent")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level: fast, standard, medium, thorough, or maximum")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for job to complete and show result")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress output (just enqueue)")
	cmd.Flags().BoolVar(&listTypes, "list", false, "list available analysis types")
	cmd.Flags().BoolVar(&showPrompt, "show-prompt", false, "show the prompt template for an analysis type")
	cmd.Flags().BoolVar(&fix, "fix", false, "after analysis, run an agentic agent to apply fixes")
	cmd.Flags().StringVar(&fixAgent, "fix-agent", "", "agent to use for fixes (default: same as --agent)")
	cmd.Flags().StringVar(&fixModel, "fix-model", "", "model for fix agent (default: same as --model)")
	cmd.Flags().BoolVar(&perFile, "per-file", false, "create one analysis job per file")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output job info as JSON for programmatic use")
	cmd.Flags().StringVar(&branch, "branch", "", "analyze files changed on branch (default: current branch, or specify name)")
	cmd.Flags().Lookup("branch").NoOptDefVal = "HEAD"
	cmd.Flags().StringVar(&baseBranch, "base", "", "base branch for --branch comparison (default: auto-detect)")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)

	return cmd
}

type analyzeOptions struct {
	agentName  string
	model      string
	reasoning  string
	wait       bool
	quiet      bool
	fix        bool
	fixAgent   string
	fixModel   string
	perFile    bool
	jsonOutput bool
	branch     string
	baseBranch string
}

// AnalyzeResult is the JSON output format for analyze command
type AnalyzeResult struct {
	Jobs         []AnalyzeJobInfo `json:"jobs"`
	AnalysisType string           `json:"analysis_type"`
	Files        []string         `json:"files"`
}

// AnalyzeJobInfo contains job details for JSON output
type AnalyzeJobInfo struct {
	ID    int64  `json:"id"`
	Agent string `json:"agent"`
	File  string `json:"file,omitempty"` // Only set in per-file mode
}

func listAnalysisTypes(cmd *cobra.Command) error {
	cmd.Println("Available analysis types:")
	cmd.Println()
	for _, t := range analyze.AllTypes {
		cmd.Printf("  %-14s %s\n", t.Name, t.Description)
	}
	return nil
}

func showAnalysisPrompt(cmd *cobra.Command, typeName string) error {
	analysisType := analyze.GetType(typeName)
	if analysisType == nil {
		return fmt.Errorf("unknown analysis type %q (use --list to see available types)", typeName)
	}

	prompt, err := analysisType.GetPrompt()
	if err != nil {
		return err
	}

	cmd.Printf("# %s\n\n", analysisType.Name)
	cmd.Printf("Description: %s\n\n", analysisType.Description)
	cmd.Println("## Prompt Template")
	cmd.Println()
	cmd.Println(prompt)
	return nil
}

func runAnalysis(cmd *cobra.Command, typeName string, filePatterns []string, opts analyzeOptions) error {
	ctx := cmd.Context()

	// Validate analysis type
	analysisType := analyze.GetType(typeName)
	if analysisType == nil {
		return fmt.Errorf("unknown analysis type %q (use --list to see available types)", typeName)
	}

	// Get working directory and repo root
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	repoRoot := workDir
	if root, err := gitrepo.Root(ctx, workDir); err == nil {
		repoRoot = root
	}

	var files map[string]string

	if opts.branch != "" {
		// Branch mode: discover changed files from git
		var err error
		files, err = getBranchFiles(ctx, cmd, repoRoot, opts)
		if err != nil {
			return err
		}
	} else {
		// File pattern mode
		var err error
		files, err = expandAndReadFiles(workDir, repoRoot, filePatterns)
		if err != nil {
			return err
		}
	}

	if len(files) == 0 {
		return fmt.Errorf("no files matched the provided patterns")
	}

	// Ensure daemon is running
	if err := ensureDaemon(); err != nil {
		return err
	}

	// Load config and resolve max prompt size once for all analysis modes
	cfg, _ := config.LoadGlobal()
	maxPromptSize := config.ResolveMaxPromptSize(repoRoot, cfg)

	ep := getDaemonEndpoint()

	// Per-file mode: create one job per file
	if opts.perFile {
		return runPerFileAnalysis(ctx, cmd, ep, repoRoot, analysisType, files, opts, maxPromptSize)
	}

	// Standard mode: all files in one job
	return runSingleAnalysis(ctx, cmd, ep, repoRoot, analysisType, files, opts, maxPromptSize)
}

// runSingleAnalysis creates a single analysis job for all files
func runSingleAnalysis(ctx context.Context, cmd *cobra.Command, ep daemon.DaemonEndpoint, repoRoot string, analysisType *analyze.AnalysisType, files map[string]string, opts analyzeOptions, maxPromptSize int) error {
	if !opts.quiet && !opts.jsonOutput {
		cmd.Printf("Analyzing %d file(s) with %q analysis...\n", len(files), analysisType.Name)
	}

	// Build the full prompt with file contents
	fullPrompt, err := analysisType.BuildPrompt(files)
	if err != nil {
		return fmt.Errorf("build prompt: %w", err)
	}

	// Build relative file paths for output prefix
	relPaths := make([]string, 0, len(files))
	for name := range files {
		relPaths = append(relPaths, name)
	}
	outputPrefix := buildOutputPrefix(analysisType.Name, relPaths)

	// If prompt is too large, fall back to file paths only
	if len(fullPrompt) > maxPromptSize {
		if !opts.quiet && !opts.jsonOutput {
			cmd.Printf("Files too large to embed (%dKB), using file paths...\n", len(fullPrompt)/1024)
		}
		absPaths := make([]string, 0, len(files))
		for name := range files {
			absPaths = append(absPaths, filepath.Join(repoRoot, name))
		}
		fullPrompt, err = analysisType.BuildPromptWithPaths(repoRoot, absPaths)
		if err != nil {
			return fmt.Errorf("build prompt with paths: %w", err)
		}
	}

	// Enqueue the job
	job, err := enqueueAnalysisJob(ctx, ep, repoRoot, fullPrompt, outputPrefix, analysisType.Name, opts)
	if err != nil {
		return err
	}

	// JSON output mode
	if opts.jsonOutput {
		sort.Strings(relPaths)
		result := AnalyzeResult{
			Jobs:         []AnalyzeJobInfo{{ID: job.ID, Agent: job.Agent}},
			AnalysisType: analysisType.Name,
			Files:        relPaths,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(result)
	}

	if !opts.quiet {
		cmd.Printf("Enqueued analysis job %d (agent: %s)\n", job.ID, job.Agent)
	}

	// If --fix, we need to wait for analysis, run fixer, then mark closed
	if opts.fix {
		return runAnalyzeAndFix(cmd, ep, repoRoot, job.ID, analysisType, opts)
	}

	// If --wait, poll until job completes and show result
	if opts.wait {
		return waitForPromptJob(cmd, ep, job.ID, opts.quiet, promptPollInterval)
	}

	return nil
}

// runPerFileAnalysis creates one analysis job per file
func runPerFileAnalysis(ctx context.Context, cmd *cobra.Command, ep daemon.DaemonEndpoint, repoRoot string, analysisType *analyze.AnalysisType, files map[string]string, opts analyzeOptions, maxPromptSize int) error {
	// Sort files for deterministic order
	fileNames := make([]string, 0, len(files))
	for name := range files {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)

	if !opts.quiet && !opts.jsonOutput {
		cmd.Printf("Creating %d analysis jobs (%q, one per file)...\n", len(files), analysisType.Name)
	}

	var jobInfos []AnalyzeJobInfo
	for i, fileName := range fileNames {
		singleFile := map[string]string{fileName: files[fileName]}

		fullPrompt, err := analysisType.BuildPrompt(singleFile)
		if err != nil {
			return fmt.Errorf("build prompt for %s: %w", fileName, err)
		}

		// Build output prefix for this file
		outputPrefix := buildOutputPrefix(analysisType.Name, []string{fileName})

		// If single file is too large, fall back to file path only
		if len(fullPrompt) > maxPromptSize {
			if !opts.quiet && !opts.jsonOutput {
				cmd.Printf("  %s too large (%dKB), using file path...\n", fileName, len(fullPrompt)/1024)
			}
			filePath := filepath.Join(repoRoot, fileName)
			fullPrompt, err = analysisType.BuildPromptWithPaths(repoRoot, []string{filePath})
			if err != nil {
				return fmt.Errorf("build prompt with path for %s: %w", fileName, err)
			}
		}

		job, err := enqueueAnalysisJob(ctx, ep, repoRoot, fullPrompt, outputPrefix, analysisType.Name, opts)
		if err != nil {
			return fmt.Errorf("enqueue job for %s: %w", fileName, err)
		}

		jobInfos = append(jobInfos, AnalyzeJobInfo{ID: job.ID, Agent: job.Agent, File: fileName})

		if !opts.quiet && !opts.jsonOutput {
			cmd.Printf("  [%d/%d] Job %d: %s (agent: %s)\n", i+1, len(files), job.ID, fileName, job.Agent)
		}
	}

	// JSON output mode
	if opts.jsonOutput {
		result := AnalyzeResult{
			Jobs:         jobInfos,
			AnalysisType: analysisType.Name,
			Files:        fileNames,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(result)
	}

	if !opts.quiet {
		jobIDs := make([]int64, len(jobInfos))
		for i, info := range jobInfos {
			jobIDs[i] = info.ID
		}
		cmd.Printf("\nCreated %d jobs: %v\n", len(jobIDs), jobIDs)
		cmd.Println("Use 'roborev fix <job_id>' to apply fixes for individual jobs.")
	}

	// If --fix with per-file, run fixes sequentially
	if opts.fix {
		if !opts.quiet {
			cmd.Println("\nRunning fixes for each job...")
		}
		for i, info := range jobInfos {
			if !opts.quiet {
				cmd.Printf("\n=== Fixing job %d (%d/%d) ===\n", info.ID, i+1, len(jobInfos))
			}
			if err := runAnalyzeAndFix(cmd, ep, repoRoot, info.ID, analysisType, opts); err != nil {
				if !opts.quiet {
					cmd.Printf("Warning: fix for job %d failed: %v\n", info.ID, err)
				}
				// Continue with other jobs
			}
		}
		return nil
	}

	// If --wait with per-file, wait for all jobs
	if opts.wait {
		if !opts.quiet {
			cmd.Println("\nWaiting for all jobs to complete...")
		}
		for i, info := range jobInfos {
			if !opts.quiet {
				cmd.Printf("\n=== Job %d (%d/%d) ===\n", info.ID, i+1, len(jobInfos))
			}
			if err := waitForPromptJob(cmd, ep, info.ID, opts.quiet, promptPollInterval); err != nil {
				if !opts.quiet {
					cmd.Printf("Warning: job %d failed: %v\n", info.ID, err)
				}
			}
		}
	}

	return nil
}

// buildOutputPrefix creates a prefix showing which files were analyzed.
// This is prepended to the agent's output for reliable file identification.
func buildOutputPrefix(analysisType string, filePaths []string) string {
	sort.Strings(filePaths)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s Analysis\n\n", analysisType)
	sb.WriteString("**Files:**\n")
	for _, path := range filePaths {
		fmt.Fprintf(&sb, "- %s\n", path)
	}
	sb.WriteString("\n---\n\n")
	return sb.String()
}

// enqueueAnalysisJob sends a job to the daemon
func enqueueAnalysisJob(ctx context.Context, ep daemon.DaemonEndpoint, repoRoot, prompt, outputPrefix, label string, opts analyzeOptions) (*storage.ReviewJob, error) {
	branch := gitrepo.CurrentBranch(ctx, repoRoot)
	if opts.branch != "" && opts.branch != "HEAD" {
		branch = opts.branch
	}

	reviewType := analysisReviewType(label)
	cfg, err := config.LoadGlobal()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	resolved, err := config.ResolveAnalyzeConfig(
		opts.agentName, opts.model, opts.reasoning, repoRoot, cfg,
		label, analysisWorkflow(label), "",
	)
	if err != nil {
		return nil, fmt.Errorf("resolve analyze config: %w", err)
	}

	reqBody, _ := json.Marshal(daemon.EnqueueRequest{
		RepoPath:     repoRoot,
		GitRef:       label, // Use analysis type name as the TUI label
		Branch:       branch,
		Agent:        resolved.Agent,
		Model:        resolved.Model,
		Reasoning:    resolved.Reasoning,
		ReviewType:   reviewType,
		CustomPrompt: prompt,
		OutputPrefix: outputPrefix,
		Agentic:      true, // Agentic mode needed for reading files when prompt exceeds size limit
	})

	resp, err := ep.HTTPClient(10*time.Second).Post(ep.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("enqueue failed: %s", body)
	}

	var job storage.ReviewJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &job, nil
}

func analysisReviewType(label string) string {
	if label == config.ReviewTypeSecurity {
		return config.ReviewTypeSecurity
	}
	return ""
}

func analysisWorkflow(label string) string {
	if label == config.ReviewTypeSecurity {
		return config.ReviewTypeSecurity
	}
	return "review"
}

// runAnalyzeAndFix waits for analysis to complete, runs a fixer agent, then marks closed
func runAnalyzeAndFix(cmd *cobra.Command, ep daemon.DaemonEndpoint, repoRoot string, jobID int64, analysisType *analyze.AnalysisType, opts analyzeOptions) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if !opts.quiet {
		cmd.Printf("Waiting for analysis to complete...")
	}

	// Wait for analysis job to complete (with timeout)
	ctx, cancel := context.WithTimeout(ctx, analyzeJobTimeout)
	defer cancel()

	review, err := waitForAnalysisJob(ctx, ep, jobID)
	if err != nil {
		return fmt.Errorf("analysis failed: %w", err)
	}

	if !opts.quiet {
		cmd.Printf(" done!\n\n")
		cmd.Println("Analysis result:")
		cmd.Println(strings.Repeat("-", 60))
		cmd.Println(review.Output)
		cmd.Println(strings.Repeat("-", 60))
		cmd.Println()
	}

	cfg, _ := config.LoadGlobal()
	metadata, err := config.ResolveFixCommitMetadata(repoRoot, cfg)
	if err != nil {
		return fmt.Errorf("resolve fix commit metadata: %w", err)
	}

	// Build the fix prompt
	fixPrompt := buildFixPromptWithMetadata(analysisType, review.Output, metadata)

	// Resolve fix agent (defaults to analysis agent)
	fixAgentName := opts.fixAgent
	if fixAgentName == "" {
		fixAgentName = opts.agentName
	}
	fixModel := opts.fixModel
	if fixModel == "" {
		fixModel = opts.model
	}

	if !opts.quiet {
		cmd.Printf("Running fix agent (%s) to apply changes...\n\n", fixAgentName)
	}

	// Get HEAD before running fix agent (errors are non-fatal, just skip verification)
	headBefore, headErr := gitrepo.Resolve(ctx, repoRoot, "HEAD")
	canVerifyCommits := headErr == nil

	// Run the fix agent locally in agentic mode
	if err := runFixAgent(cmd, repoRoot, fixAgentName, fixModel, opts.reasoning, fixPrompt, opts.quiet); err != nil {
		return fmt.Errorf("fix agent failed: %w", err)
	}

	// Check if a commit was created (only if we could get HEAD before)
	var commitCreated bool
	if canVerifyCommits {
		headAfter, err := gitrepo.Resolve(ctx, repoRoot, "HEAD")
		if err == nil && headBefore != headAfter {
			commitCreated = true
		}

		// If no commit was created, check for uncommitted changes and retry with commit instructions
		if !commitCreated {
			hasChanges, err := gitrepo.HasUncommittedChanges(ctx, repoRoot)
			if err == nil && hasChanges {
				if !opts.quiet {
					cmd.Println("\nNo commit was created. Re-running agent with commit instructions...")
					cmd.Println()
				}

				commitPrompt := buildCommitPromptWithMetadata(analysisType, metadata)
				if err := runFixAgent(cmd, repoRoot, fixAgentName, fixModel, opts.reasoning, commitPrompt, opts.quiet); err != nil {
					if !opts.quiet {
						cmd.Printf("\nWarning: commit agent failed: %v\n", err)
					}
				}

				// Check again if commit was created
				headFinal, err := gitrepo.Resolve(ctx, repoRoot, "HEAD")
				if err == nil && headFinal != headAfter {
					commitCreated = true
				}
			}
		}
	}

	if !opts.quiet {
		if !canVerifyCommits {
			// Couldn't verify commits, don't report on commit status
		} else if commitCreated {
			cmd.Println("\nChanges committed successfully.")
		} else {
			hasChanges, err := gitrepo.HasUncommittedChanges(ctx, repoRoot)
			if err == nil && hasChanges {
				cmd.Println("\nWarning: Changes were made but not committed. Please review and commit manually.")
			} else if err == nil {
				cmd.Println("\nNo changes were made by the fix agent.")
			}
		}
	}

	// Ensure the fix commit gets a review enqueued
	if commitCreated {
		if head, err := gitrepo.Resolve(ctx, repoRoot, "HEAD"); err == nil {
			if err := enqueueIfNeeded(ctx, ep.BaseURL(), repoRoot, head); err != nil && !opts.quiet {
				cmd.Printf("Warning: could not enqueue review for fix commit: %v\n", err)
			}
		}
	}

	// Close the analysis job
	if err := markJobClosed(ctx, ep.BaseURL(), jobID); err != nil {
		// Non-fatal - the fixes were applied, just couldn't update status
		if !opts.quiet {
			cmd.Printf("\nWarning: could not close job: %v\n", err)
		}
	} else if !opts.quiet {
		cmd.Printf("Analysis job %d closed\n", jobID)
	}

	return nil
}

// waitForAnalysisJob polls until the job completes and returns the review.
// The context controls the maximum wait time.
func waitForAnalysisJob(ctx context.Context, ep daemon.DaemonEndpoint, jobID int64) (*storage.Review, error) {
	api := newDaemonReviewAPI(ep.BaseURL(), ep.HTTPClient(30*time.Second))
	pollInterval := 1 * time.Second
	maxInterval := 5 * time.Second

	for {
		// Check for cancellation/timeout
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for job: %w", ctx.Err())
		default:
		}

		job, err := api.getJob(ctx, jobID)
		if err != nil {
			return nil, fmt.Errorf("check job status: %w", err)
		}
		switch job.Status {
		case storage.JobStatusDone:
			return api.getReview(ctx, jobID, "review")
		case storage.JobStatusFailed:
			return nil, fmt.Errorf("job failed: %s", job.Error)
		case storage.JobStatusCanceled:
			return nil, fmt.Errorf("job was canceled")
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for job: %w", ctx.Err())
		case <-time.After(pollInterval):
		}

		if pollInterval < maxInterval {
			pollInterval = min(pollInterval*3/2, maxInterval)
		}
	}
}

// buildFixPrompt constructs a prompt for the fixer agent
func buildFixPrompt(analysisType *analyze.AnalysisType, analysisOutput string) string {
	return buildFixPromptWithMetadata(analysisType, analysisOutput, config.FixCommitMetadata{})
}

func buildFixPromptWithMetadata(
	analysisType *analyze.AnalysisType,
	analysisOutput string,
	metadata config.FixCommitMetadata,
) string {
	var sb strings.Builder
	sb.WriteString("# Fix Request\n\n")
	fmt.Fprintf(&sb, "An analysis of type **%s** was performed and produced the following findings:\n\n", analysisType.Name)
	sb.WriteString("## Analysis Findings\n\n")
	sb.WriteString(analysisOutput)
	sb.WriteString("\n\n## Instructions\n\n")
	sb.WriteString("Please apply the suggested changes from the analysis above. ")
	sb.WriteString("Make the necessary edits to address each finding. ")
	sb.WriteString("Focus on the highest priority items first.\n\n")
	sb.WriteString("After making changes:\n")
	sb.WriteString("1. Verify the code still compiles/passes linting\n")
	sb.WriteString("2. Run any relevant tests to ensure nothing is broken\n")
	sb.WriteString("3. Create a git commit with a descriptive message summarizing the changes\n")
	sb.WriteString(formatFixCommitMetadataInstructions(metadata))
	return sb.String()
}

// buildCommitPrompt constructs a prompt to commit uncommitted changes
func buildCommitPrompt(analysisType *analyze.AnalysisType) string {
	return buildCommitPromptWithMetadata(analysisType, config.FixCommitMetadata{})
}

func buildCommitPromptWithMetadata(
	analysisType *analyze.AnalysisType,
	metadata config.FixCommitMetadata,
) string {
	var sb strings.Builder
	sb.WriteString("# Commit Request\n\n")
	sb.WriteString("There are uncommitted changes from a previous fix operation.\n\n")
	sb.WriteString("## Instructions\n\n")
	sb.WriteString("1. Review the current uncommitted changes using `git status` and `git diff`\n")
	sb.WriteString("2. Stage the appropriate files\n")
	sb.WriteString("3. Create a git commit with a descriptive message\n\n")
	sb.WriteString("The commit message should:\n")
	fmt.Fprintf(&sb, "- Reference the '%s' analysis that prompted the changes\n", analysisType.Name)
	sb.WriteString("- Summarize what was changed and why\n")
	sb.WriteString("- Be concise but informative\n")
	sb.WriteString(formatFixCommitMetadataInstructions(metadata))
	return sb.String()
}

// runFixAgent runs an agent locally in agentic mode to apply fixes
func runFixAgent(cmd *cobra.Command, repoPath, agentName, model, reasoning, prompt string, quiet bool) error {
	// Load config
	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve reasoning level (defaults to "standard" for fix)
	reasoning, reasonErr := config.ResolveFixReasoning(reasoning, repoPath, cfg)
	if reasonErr != nil {
		return fmt.Errorf("resolve fix reasoning: %w", reasonErr)
	}
	if err := config.ValidateRepoConfig(repoPath); err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}

	// Resolve agent and model via fix workflow config.
	resolution, err := agent.ResolveWorkflowConfig(
		agentName, repoPath, cfg, "fix", reasoning,
	)
	if err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}
	agentName = resolution.PreferredAgent

	a, err := agent.GetPreferredOrBackupWithConfig(
		repoPath, agentName, cfg, resolution.BackupAgent,
	)
	if err != nil {
		return fmt.Errorf("get agent: %w", err)
	}

	model = resolution.ModelForSelectedAgent(a.Name(), model)

	// Configure agent: agentic mode, with model and reasoning
	reasoningLevel := agent.ParseReasoningLevel(reasoning)
	a = a.WithAgentic(true).WithReasoning(reasoningLevel)
	if model != "" {
		a = a.WithModel(model)
	}

	// Use stdout for streaming output, with stream formatting for TTY
	var out io.Writer
	var fmtr *streamfmt.Formatter
	if quiet {
		out = io.Discard
	} else {
		fmtr = streamfmt.New(cmd.OutOrStdout(), streamfmt.WriterIsTerminal(cmd.OutOrStdout()))
		out = fmtr
	}

	// Use command context for cancellation support
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	_, err = a.Review(ctx, repoPath, "fix", prompt, out)
	if fmtr != nil {
		fmtr.Flush()
	}
	if err != nil {
		return err
	}

	if !quiet {
		fmt.Fprintln(cmd.OutOrStdout()) // Final newline
	}
	return nil
}

// markJobClosed closes a job via the API
func markJobClosed(ctx context.Context, serverAddr string, jobID int64) error {
	reqBody, _ := json.Marshal(map[string]any{
		"job_id": jobID,
		"closed": true,
	})

	_, err := withFixDaemonRetryContext(ctx, serverAddr, func(addr string) (struct{}, error) {
		resp, err := doFixDaemonRequest(ctx, http.MethodPost, addr+"/api/review/close", reqBody)
		if err != nil {
			return struct{}{}, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return struct{}{}, fmt.Errorf("close job failed: %s", body)
		}
		return struct{}{}, nil
	})
	return err
}

// expandAndReadFiles expands glob patterns and reads file contents.
// Returns a map of relative path -> content.
func expandAndReadFiles(workDir, repoRoot string, patterns []string) (map[string]string, error) {
	files := make(map[string]string)
	seen := make(map[string]bool)

	for _, pattern := range patterns {
		// Handle ./... pattern (all files recursively)
		if pattern == "./..." || pattern == "..." {
			if err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					// Skip hidden directories and common non-code directories
					base := filepath.Base(path)
					if strings.HasPrefix(base, ".") || base == "node_modules" || base == "vendor" {
						return filepath.SkipDir
					}
					return nil
				}
				// Only include source files
				if isSourceFile(path) {
					relPath, err := filepath.Rel(repoRoot, path)
					if err != nil {
						relPath = path // Use absolute path as fallback
					}
					if !seen[relPath] {
						seen[relPath] = true
						content, err := os.ReadFile(path)
						if err != nil {
							return fmt.Errorf("read %s: %w", relPath, err)
						}
						files[relPath] = string(content)
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
			continue
		}

		// Make pattern absolute if relative (resolve against workDir where shell expansion happens)
		absPattern := pattern
		if !filepath.IsAbs(pattern) {
			absPattern = filepath.Join(workDir, pattern)
		}

		// Expand glob pattern
		matches, err := filepath.Glob(absPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}

		if len(matches) == 0 {
			// Try as literal file
			if _, err := os.Stat(absPattern); err == nil {
				matches = []string{absPattern}
			} else {
				return nil, fmt.Errorf("no files match pattern %q", pattern)
			}
		}

		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}

			if info.IsDir() {
				// If directory, include all source files in it
				if err := filepath.Walk(match, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.IsDir() {
						return nil
					}
					if isSourceFile(path) {
						relPath, err := filepath.Rel(repoRoot, path)
						if err != nil {
							relPath = path // Use absolute path as fallback
						}
						if !seen[relPath] {
							seen[relPath] = true
							content, err := os.ReadFile(path)
							if err != nil {
								return fmt.Errorf("read %s: %w", relPath, err)
							}
							files[relPath] = string(content)
						}
					}
					return nil
				}); err != nil {
					return nil, err
				}
			} else {
				relPath, err := filepath.Rel(repoRoot, match)
				if err != nil {
					relPath = match // Use absolute path as fallback
				}
				if !seen[relPath] {
					seen[relPath] = true
					content, err := os.ReadFile(match)
					if err != nil {
						return nil, fmt.Errorf("read %s: %w", relPath, err)
					}
					files[relPath] = string(content)
				}
			}
		}
	}

	return files, nil
}

// getBranchFiles discovers files changed on a branch and reads their contents.
// When opts.branch is "HEAD", uses the current branch. Otherwise uses the named branch.
func getBranchFiles(ctx context.Context, cmd *cobra.Command, repoRoot string, opts analyzeOptions) (map[string]string, error) {
	// Determine which branch to analyze
	targetRef := "HEAD"
	branchLabel := gitrepo.CurrentBranch(ctx, repoRoot)
	if opts.branch != "HEAD" {
		targetRef = opts.branch
		branchLabel = opts.branch
		// Verify the ref exists
		if _, err := gitrepo.Resolve(ctx, repoRoot, targetRef); err != nil {
			return nil, fmt.Errorf("cannot resolve branch %q: %w", opts.branch, err)
		}
	}

	// Determine base branch
	base := opts.baseBranch
	if base == "" {
		base = git.GetBranchBase(repoRoot, targetRef)
	}
	if base == "" {
		// Prefer the branch's upstream tracking ref only when it resolves to
		// a trunk-named branch (e.g., local main tracking upstream/main in a
		// fork). A branch tracking its own remote counterpart (feature
		// tracking origin/feature) is not trunk — using it would skip
		// already-pushed feature commits, contradicting "--branch analyzes
		// all commits since trunk".
		upstream, uerr := git.GetUpstream(repoRoot, targetRef)
		var missing *git.UpstreamMissingError
		if errors.As(uerr, &missing) {
			return nil, fmt.Errorf("%w (or pass --base <ref>)", missing)
		}
		if uerr != nil {
			return nil, fmt.Errorf("resolve upstream for %s: %w (pass --base <ref> to skip)", targetRef, uerr)
		}
		if upstream != "" && git.UpstreamIsTrunk(repoRoot, targetRef) {
			base = upstream
		}
	}
	if base == "" {
		var err error
		base, err = gitrepo.DefaultBranch(ctx, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("cannot determine base branch: %w", err)
		}
	}

	// Validate not on base branch (only when analyzing current branch)
	if targetRef == "HEAD" {
		currentBranch := gitrepo.CurrentBranch(ctx, repoRoot)
		if git.IsOnBaseBranch(repoRoot, currentBranch, base) {
			return nil, fmt.Errorf("already on %s - switch to a feature branch first", currentBranch)
		}
	}

	// Get merge-base
	mergeBase, err := git.GetMergeBase(repoRoot, base, targetRef)
	if err != nil {
		return nil, fmt.Errorf("cannot find merge-base with %s: %w", base, err)
	}

	// Validate has commits
	rangeRef := mergeBase + ".." + targetRef
	commits, err := git.GetRangeCommits(repoRoot, rangeRef)
	if err != nil {
		return nil, fmt.Errorf("cannot get commits: %w", err)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits on branch since %s", base)
	}

	// Get changed files
	changedFiles, err := git.GetRangeFilesChanged(repoRoot, rangeRef)
	if err != nil {
		return nil, fmt.Errorf("cannot get changed files: %w", err)
	}

	// Filter to code files only
	var codeFiles []string
	for _, f := range changedFiles {
		if isCodeFile(f) {
			codeFiles = append(codeFiles, f)
		}
	}

	if len(codeFiles) == 0 {
		return nil, fmt.Errorf("no code files changed on branch (found %d non-code files)", len(changedFiles))
	}

	if !opts.quiet && !opts.jsonOutput {
		cmd.Printf("Branch %q: %d commits, %d code files changed since %s\n",
			branchLabel, len(commits), len(codeFiles), base)
	}

	// Read file contents from git (not working tree) for consistency with the commit range
	files := make(map[string]string, len(codeFiles))
	for _, f := range codeFiles {
		content, readErr := git.ReadFile(repoRoot, targetRef, f)
		if readErr != nil {
			// Files deleted in the target ref will fail to read — skip them
			if strings.Contains(readErr.Error(), "does not exist") || strings.Contains(readErr.Error(), "bad object") {
				continue
			}
			return nil, fmt.Errorf("read %s at %s: %w", f, targetRef, readErr)
		}
		files[f] = string(content)
	}

	return files, nil
}

// isCodeFile returns true if the file is a code file (stricter than isSourceFile).
// Excludes documentation, configuration, and markup files that are typically not
// useful for code analysis on a branch.
func isCodeFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	codeExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
		".rs": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cc": true,
		".java": true, ".kt": true, ".scala": true, ".rb": true, ".php": true,
		".swift": true, ".m": true, ".cs": true, ".fs": true, ".vb": true,
		".sh": true, ".bash": true, ".zsh": true, ".fish": true,
		".sql": true, ".graphql": true, ".proto": true,
	}
	return codeExts[ext]
}

// isSourceFile returns true if the file looks like source code
func isSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	sourceExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
		".rs": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cc": true,
		".java": true, ".kt": true, ".scala": true, ".rb": true, ".php": true,
		".swift": true, ".m": true, ".cs": true, ".fs": true, ".vb": true,
		".sh": true, ".bash": true, ".zsh": true, ".fish": true,
		".sql": true, ".graphql": true, ".proto": true,
		".yaml": true, ".yml": true, ".toml": true, ".json": true,
		".md": true, ".txt": true, ".html": true, ".css": true, ".scss": true,
	}
	return sourceExts[ext]
}
