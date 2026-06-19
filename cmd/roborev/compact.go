package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func compactCmd() *cobra.Command {
	var (
		agentName   string
		model       string
		reasoning   string
		quiet       bool
		allBranches bool
		branch      string
		dryRun      bool
		limit       int
		wait        bool
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Verify and consolidate open review findings",
		Long: `Verify and consolidate open review findings.

Discovers all completed review jobs still open, sends them to an agent
for verification against the current codebase, consolidates related findings,
and creates a new consolidated review job.

By default, compact runs in the background. Use --wait to block until complete.

When consolidation finishes successfully, original jobs are automatically
closed. Check progress with 'roborev status' or 'roborev tui'.

This adds a quality layer between 'review' and 'fix' to reduce false positives
and consolidate findings from multiple reviews.

Note: This operation is not atomic. Avoid running multiple compact commands
concurrently on the same branch to prevent inconsistent state.

Examples:
  roborev compact                              # Enqueue consolidation (background)
  roborev compact --wait                       # Wait for completion
  roborev compact --branch main                # Compact jobs on main branch
  roborev compact --all-branches               # Compact jobs across all branches
  roborev compact --dry-run                    # Show what would be done
  roborev compact --limit 10                   # Process at most 10 jobs
  roborev compact --agent claude-code          # Use specific agent for verification
  roborev compact --reasoning thorough         # Use thorough reasoning level
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate flags
			if branch != "" && allBranches {
				return usageErr(cmd, fmt.Errorf("--branch and --all-branches are mutually exclusive"))
			}

			return runCompact(cmd, compactOptions{
				agentName:   agentName,
				model:       model,
				reasoning:   reasoning,
				quiet:       quiet,
				allBranches: allBranches,
				branch:      branch,
				dryRun:      dryRun,
				limit:       limit,
				wait:        wait,
				timeout:     timeout,
			})
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "agent to use for verification (defaults to fix agent from config)")
	cmd.Flags().StringVar(&model, "model", "", "model to use")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level (fast/standard/medium/thorough/maximum)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress progress output")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false, "include all branches")
	cmd.Flags().StringVar(&branch, "branch", "", "filter by branch (default: current branch)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be done without executing")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of jobs to compact at once")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for consolidation to complete and show result")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "timeout for --wait mode (e.g., 15m, 1h); 0 or negative values use the 10m default")

	return cmd
}

type compactOptions struct {
	agentName   string
	model       string
	reasoning   string
	quiet       bool
	allBranches bool
	branch      string
	dryRun      bool
	limit       int
	wait        bool
	timeout     time.Duration
}

type jobReview struct {
	jobID  int64
	job    *storage.ReviewJob
	review *storage.Review
}

// maxBatchFetch matches the server-side maxBatchSize for /api/jobs/batch.
const maxBatchFetch = 100

// fetchJobReviews fetches job and review data for all job IDs, chunking
// into batches of maxBatchFetch to respect the server-side limit.
// Returns successfully fetched entries and list of IDs that were processed.
func fetchJobReviews(ctx context.Context, jobIDs []int64, quiet bool, cmd *cobra.Command) ([]jobReview, []int64, error) {
	if !quiet {
		cmd.Printf("  Fetching %d job(s)... ", len(jobIDs))
	}

	// Fetch in chunks to stay within server batch limit
	allResults := make(map[int64]storage.JobWithReview)
	for i := 0; i < len(jobIDs); i += maxBatchFetch {
		end := min(i+maxBatchFetch, len(jobIDs))
		results, err := fetchJobBatch(ctx, jobIDs[i:end])
		if err != nil {
			return nil, nil, err
		}
		maps.Copy(allResults, results)
	}

	var jobReviews []jobReview
	var successfulJobIDs []int64

	// Iterate in the original order to maintain deterministic output
	for _, jobID := range jobIDs {
		entry, ok := allResults[jobID]
		if !ok || entry.Review == nil {
			if !quiet {
				cmd.Printf("\n  Skipping job %d (no review found)", jobID)
			}
			continue
		}

		successfulJobIDs = append(successfulJobIDs, jobID)
		jobReviews = append(jobReviews, jobReview{
			jobID:  jobID,
			job:    &entry.Job,
			review: entry.Review,
		})
	}

	if !quiet {
		cmd.Printf("done (%d fetched)\n", len(jobReviews))
	}

	if len(jobReviews) == 0 {
		return nil, nil, fmt.Errorf("failed to fetch any review outputs")
	}

	return jobReviews, successfulJobIDs, nil
}

// fetchJobBatch fetches a single batch of job reviews from the daemon.
func fetchJobBatch(ctx context.Context, ids []int64) (map[int64]storage.JobWithReview, error) {
	reqBody, err := json.Marshal(map[string]any{
		"job_ids": ids,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal batch request: %w", err)
	}

	ep := getDaemonEndpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", ep.BaseURL()+"/api/jobs/batch", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := ep.HTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("batch fetch failed (%d): %s", resp.StatusCode, body)
	}

	var batchResp struct {
		Results map[int64]storage.JobWithReview `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, fmt.Errorf("decode batch response: %w", err)
	}
	return batchResp.Results, nil
}

// enqueueConsolidation creates and enqueues a consolidation job.
// Returns the job ID for tracking.
func enqueueConsolidation(ctx context.Context, cmd *cobra.Command, repoRoot string, jobReviews []jobReview, branchFilter string, opts compactOptions) (int64, error) {
	// Resolve agent/model/reasoning using "fix" workflow so compact
	// uses the same config as `roborev fix`.  Pass the resolved values
	// to the daemon so it doesn't re-resolve under the "review" workflow.
	cfg, err := config.LoadGlobal()
	if err != nil {
		return 0, fmt.Errorf("load config: %w", err)
	}
	reasoning, err := config.ResolveFixReasoning(opts.reasoning, repoRoot, cfg)
	if err != nil {
		return 0, fmt.Errorf("resolve reasoning: %w", err)
	}
	if err := config.ValidateRepoConfig(repoRoot); err != nil {
		return 0, fmt.Errorf("resolve agent/model: %w", err)
	}
	agentName := config.ResolveAgentForWorkflow(
		opts.agentName, repoRoot, cfg, "fix", reasoning,
	)
	// Build verification prompt after agent resolution so compact can reuse
	// the selected agent's regular review output contract.
	compactPrompt := buildCompactPrompt(ctx, jobReviews, branchFilter, repoRoot, agentName)
	// Resolve model locally for display; the daemon re-resolves with
	// its own canonical-agent comparison to skip generic default_model
	// when the agent was overridden on CLI.
	configAgent := config.ResolveAgentForWorkflow("", repoRoot, cfg, "fix", reasoning)
	cliAgentChanged := opts.agentName != "" &&
		agent.CanonicalName(opts.agentName) != agent.CanonicalName(configAgent)
	var model string
	if cliAgentChanged && opts.model == "" {
		model = config.ResolveWorkflowModel(repoRoot, cfg, "fix", reasoning)
	} else {
		model = config.ResolveModelForWorkflow(
			opts.model, repoRoot, cfg, "fix", reasoning,
		)
	}

	if !opts.quiet {
		cmd.Printf("\nRunning verification agent (%s) to check findings against current codebase...\n\n", agentName)
	}

	// Enqueue consolidated task job
	timestamp := time.Now().Format("20060102-150405")
	label := fmt.Sprintf("compact-%s-%s", branchFilter, timestamp)
	if branchFilter == "" {
		label = fmt.Sprintf("compact-all-%s", timestamp)
	}

	outputPrefix := buildCompactOutputPrefix(len(jobReviews), branchFilter, extractJobIDs(jobReviews))

	// Use resolved values so daemon enqueues with the correct agent/model
	resolved := opts
	resolved.agentName = agentName
	resolved.model = model
	resolved.reasoning = reasoning
	job, err := enqueueCompactJob(ctx, repoRoot, compactPrompt, outputPrefix, label, branchFilter, resolved)
	if err != nil {
		return 0, fmt.Errorf("enqueue verification job: %w", err)
	}

	return job.ID, nil
}

// waitForConsolidation waits for a consolidation job to complete.
func waitForConsolidation(ctx context.Context, cmd *cobra.Command, jobID int64, opts compactOptions) error {
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output io.Writer
	if !opts.quiet {
		output = cmd.OutOrStdout()
	}

	_, err := waitForJobCompletion(ctx, getDaemonEndpoint().BaseURL(), jobID, output)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	return nil
}

// runCompact verifies and consolidates open review findings.
//
// Known limitation: This is not an atomic operation. If another process closes jobs
// or modifies them between the initial query and the final marking step,
// there could be inconsistent state. This is acceptable since compact is typically
// run manually by a single user. For concurrent operations, users should coordinate
// to avoid running multiple compact commands simultaneously on the same branch.
func runCompact(cmd *cobra.Command, opts compactOptions) error {
	// Setup
	if err := ensureDaemon(); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	roots, err := resolveCurrentRepoRoots(ctx)
	if err != nil {
		return err
	}

	branchFilter := resolveCurrentBranchFilter(
		ctx, roots.worktreeRoot, opts.branch, opts.allBranches,
	)
	explicitBranch := opts.branch != ""

	// When the branch was auto-resolved, skip the API-level branch
	// filter so jobs from merged branches are included.
	apiBranch := branchFilter
	if !explicitBranch {
		apiBranch = ""
	}

	// Query and limit jobs, excluding non-review types (compact, task)
	// to prevent recursive self-compaction loops
	allJobs, err := queryOpenJobs(ctx, roots.mainRepoRoot, apiBranch)
	if err != nil {
		return err
	}
	if !opts.allBranches {
		filterBranch := ""
		if explicitBranch {
			filterBranch = branchFilter
		}
		allJobs = filterReachableJobs(ctx,
			roots.worktreeRoot, filterBranch, allJobs,
		)
	}
	jobs := filterReviewJobs(allJobs)

	if len(jobs) == 0 {
		if !opts.quiet {
			cmd.Println("No open jobs found.")
		}
		return nil
	}

	// Apply limit
	if opts.limit > 0 && len(jobs) > opts.limit {
		if !opts.quiet {
			cmd.Printf("Found %d open jobs, limiting to %d (use --limit to adjust)\n\n",
				len(jobs), opts.limit)
		}
		jobs = jobs[:opts.limit]
	} else if !opts.quiet {
		branchMsg := ""
		if branchFilter != "" {
			branchMsg = fmt.Sprintf(" on branch %s", branchFilter)
		}
		cmd.Printf("Found %d open job(s)%s\n\n", len(jobs), branchMsg)
	}

	// Warn about very large limits
	if opts.limit > 50 && !opts.quiet {
		cmd.Printf("Warning: --limit=%d may create a very large prompt\n\n", opts.limit)
	}

	// Extract job IDs
	jobIDs := make([]int64, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}

	// Dry-run early exit
	if opts.dryRun {
		cmd.Println("Would verify and consolidate these reviews")
		cmd.Println("Would create 1 consolidated review job")
		cmd.Printf("Would close %d jobs\n\n", len(jobIDs))
		cmd.Println("Run without --dry-run to execute.")
		return nil
	}

	// Fetch review outputs
	if !opts.quiet {
		cmd.Println("Fetching review outputs:")
	}

	jobReviews, successfulJobIDs, err := fetchJobReviews(ctx, jobIDs, opts.quiet, cmd)
	if err != nil {
		return err
	}

	// Enqueue consolidation job
	consolidatedJobID, err := enqueueConsolidation(ctx, cmd, roots.mainRepoRoot, jobReviews, branchFilter, opts)
	if err != nil {
		return err
	}

	// Store source job IDs for automatic marking when job completes
	// CRITICAL: This must succeed or source jobs will never be closed
	if err := writeCompactMetadata(consolidatedJobID, successfulJobIDs); err != nil {
		// Try to cancel the job we just created
		if cancelErr := cancelJob(getDaemonEndpoint().BaseURL(), consolidatedJobID); cancelErr != nil {
			// Best effort - log but don't mask the original error
			log.Printf("Failed to cancel job %d after metadata write failure: %v", consolidatedJobID, cancelErr)
		}
		return fmt.Errorf("failed to write compact metadata: %w", err)
	}

	if !opts.quiet {
		cmd.Printf("\nEnqueued consolidation job %d\n", consolidatedJobID)
	}

	// If --wait flag not set, return immediately (background mode)
	if !opts.wait {
		if !opts.quiet {
			cmd.Println("\nConsolidation running in background.")
			cmd.Println("\nCheck progress: roborev status")
			cmd.Println("View results:   roborev tui")
			cmd.Printf("Wait for job:   roborev compact --wait (or wait for job %d)\n", consolidatedJobID)
		}
		return nil
	}

	// Wait for completion
	if !opts.quiet {
		cmd.Println("\nWaiting for consolidation to complete...")
	}

	err = waitForConsolidation(ctx, cmd, consolidatedJobID, opts)
	if err != nil {
		return err
	}

	if !opts.quiet {
		cmd.Println("\nVerification complete!")
		cmd.Printf("\nConsolidated review created: job %d\n", consolidatedJobID)
	}

	// Note: source jobs are automatically closed by the daemon worker
	// when the compact job completes (see worker.go markCompactSourceJobs).

	// Show next steps
	if !opts.quiet {
		cmd.Println("\nView with: roborev tui")
		cmd.Printf("Fix with: roborev fix %d\n", consolidatedJobID)
	}

	return nil
}

func buildCompactPrompt(ctx context.Context, jobReviews []jobReview, branch, repoRoot, agentName string) string {
	var sb strings.Builder

	if reviewPrompt := strings.TrimSpace(prompt.GetSystemPrompt(agentName, "review")); reviewPrompt != "" {
		sb.WriteString(reviewPrompt)
		sb.WriteString("\n\n")
	}

	sb.WriteString("# Verification and Consolidation Request\n\n")
	sb.WriteString("You are a code reviewer tasked with verifying and consolidating previous review findings.\n\n")

	// Include project review guidelines so the compact agent
	// respects the same rules as regular reviews.
	globalCfg, _ := config.LoadGlobal()
	guidelines := prompt.LoadGuidelinesWithConfig(ctx, repoRoot, globalCfg)
	if guidelines != "" {
		sb.WriteString("## Project Guidelines\n\n")
		sb.WriteString(strings.TrimSpace(guidelines))
		sb.WriteString("\n\n")
	}

	sb.WriteString(review.VerifyDedupePreamble())

	sb.WriteString("## Open Review Findings\n\n")
	reviewWord := "review"
	if len(jobReviews) != 1 {
		reviewWord = "reviews"
	}
	fmt.Fprintf(&sb, "Below are %d open %s", len(jobReviews), reviewWord)
	if branch != "" {
		fmt.Fprintf(&sb, " from branch %s", branch)
	}
	sb.WriteString(":\n\n")

	for i, jr := range jobReviews {
		fmt.Fprintf(&sb, "--- Review %d (Job %d", i+1, jr.jobID)
		if jr.job.GitRef != "" {
			fmt.Fprintf(&sb, " — %s", gitrepo.ShortSHA(jr.job.GitRef))
		}
		sb.WriteString(") ---\n")
		sb.WriteString(jr.review.Output)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Expected Output\n\n")
	sb.WriteString("Provide a consolidated review output containing only verified findings.\n")
	sb.WriteString("Use the same general structure as regular reviews so verdict parsing and downstream fix flows continue to work.\n")
	sb.WriteString("Do not include false positives or already-fixed issues as findings.\n")
	sb.WriteString("If any verified findings remain, repeat each remaining finding in the final review output; do not output only counts, totals, or a summary.\n")
	sb.WriteString("If NO findings remain after verification, use the no-issues form from the regular review format.\n")

	return sb.String()
}

func buildCompactOutputPrefix(jobCount int, branch string, jobIDs []int64) string {
	var sb strings.Builder
	sb.WriteString("## Compact Analysis\n\n")
	reviewWord := "review"
	if jobCount != 1 {
		reviewWord = "reviews"
	}
	fmt.Fprintf(&sb, "Verified and consolidated %d open %s", jobCount, reviewWord)
	if branch != "" {
		fmt.Fprintf(&sb, " from branch %s", branch)
	}
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "Original jobs: %s\n\n", formatJobIDs(jobIDs))
	sb.WriteString("---\n\n")
	return sb.String()
}

func enqueueCompactJob(ctx context.Context, repoRoot, prompt, outputPrefix, label, branch string, opts compactOptions) (*storage.ReviewJob, error) {
	if branch == "" {
		branch = gitrepo.CurrentBranch(ctx, repoRoot)
	}

	reqBody, err := json.Marshal(daemon.EnqueueRequest{
		RepoPath:     repoRoot,
		GitRef:       label,
		Branch:       branch,
		Agent:        opts.agentName,
		Model:        opts.model,
		Reasoning:    opts.reasoning,
		CustomPrompt: prompt,
		OutputPrefix: outputPrefix,
		Agentic:      true,
		JobType:      "compact",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal enqueue request: %w", err)
	}

	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(10*time.Second).Post(ep.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("enqueue failed: %s", body)
	}

	var job storage.ReviewJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &job, nil
}

// isValidConsolidatedReview delegates to the shared daemon package
// so CLI --wait and daemon background mode use the same validation.
var isValidConsolidatedReview = daemon.IsValidCompactOutput

// filterReviewJobs excludes non-review job types (compact, task) from
// the source list to prevent recursive self-compaction loops.
func filterReviewJobs(jobs []storage.ReviewJob) []storage.ReviewJob {
	filtered := make([]storage.ReviewJob, 0, len(jobs))
	for _, j := range jobs {
		switch j.JobType {
		case "compact", "task":
			continue
		default:
			filtered = append(filtered, j)
		}
	}
	return filtered
}

// extractJobIDs extracts job IDs from jobReview slice
func extractJobIDs(reviews []jobReview) []int64 {
	ids := make([]int64, len(reviews))
	for i, jr := range reviews {
		ids[i] = jr.jobID
	}
	return ids
}

// cancelJob cancels a job by ID via the daemon API
func cancelJob(serverAddr string, jobID int64) error {
	reqBody, err := json.Marshal(map[string]any{"job_id": jobID})
	if err != nil {
		return fmt.Errorf("marshal cancel request: %w", err)
	}
	resp, err := getDaemonHTTPClient(10*time.Second).Post(serverAddr+"/api/job/cancel", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel failed: %s", body)
	}

	return nil
}

func getCompactMetadataFilename(id int64) string {
	return fmt.Sprintf("compact-%d.json", id)
}

// writeCompactMetadata writes source job IDs to a metadata file for later processing
func writeCompactMetadata(consolidatedJobID int64, sourceJobIDs []int64) error {
	if len(sourceJobIDs) == 0 {
		// Nothing to track - skip metadata file creation
		return nil
	}

	metadata := struct {
		SourceJobIDs []int64 `json:"source_job_ids"`
	}{
		SourceJobIDs: sourceJobIDs,
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	dataDir := config.DataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	path := filepath.Join(dataDir, getCompactMetadataFilename(consolidatedJobID))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write metadata file: %w", err)
	}

	return nil
}
