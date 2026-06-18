package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

var (
	// Retry up to 3 times after the initial daemon request. Each retry waits
	// for the daemon to come back for up to a minute so `roborev fix`
	// can survive daemon restarts without immediately aborting.
	fixDaemonMaxRetries          = 3
	fixDaemonRecoveryWait        = 1 * time.Minute
	fixDaemonRecoveryPoll        = 1 * time.Second
	fixDaemonEnsure              = ensureDaemon
	fixDaemonSleep               = time.Sleep
	enqueueIfNeededProbeAttempts = 10
	enqueueIfNeededProbeDelay    = 1 * time.Second
)

func fixCmd() *cobra.Command {
	var (
		agentName   string
		model       string
		reasoning   string
		minSeverity string
		quiet       bool
		open        bool // deprecated, silently ignored
		unaddressed bool // deprecated, silently ignored
		allBranches bool
		newestFirst bool
		branch      string
		batch       bool
		batchSize   int
		list        bool
		resume      bool
	)

	cmd := &cobra.Command{
		Use:   "fix [job_id...]",
		Short: "One-shot fix for review findings",
		Long: `Run an agent to address findings from one or more completed reviews.

This is a single-pass fix: the agent applies changes and commits, but
does not re-review or iterate. Use 'roborev refine' for an automated
loop that re-reviews fixes and retries until reviews pass.

The agent runs synchronously in your terminal, streaming output as it
works. The review output is printed first so you can see what needs
fixing. When complete, the job is closed.

With no arguments, discovers and fixes all open completed jobs on the
current branch.

Examples:
  roborev fix                            # 1 review per agent call (default)
  roborev fix 123                        # Fix a single job
  roborev fix 123 124 125                # Fix multiple jobs sequentially
  roborev fix --agent claude-code 123    # Use a specific agent
  roborev fix --branch main              # Fix all open jobs on main
  roborev fix --all-branches             # Fix all open jobs across all branches
  roborev fix --batch-size 5             # Up to 5 reviews per agent call
  roborev fix --batch                    # Pack until max_prompt_size
  roborev fix --resume                   # Resume agent session across calls
  roborev fix --batch-size 5 --resume    # 5 per call, session resumed
  roborev fix --list                     # List open jobs without fixing
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Migrate stale relative core.hooksPath to absolute
			// so linked worktrees resolve hooks correctly. Best-
			// effort: runs from a CLI path the user invokes
			// directly, unlike the post-commit hook which can't
			// self-heal when hooks are already misresolved.
			ctx := cmd.Context()
			if root, err := gitrepo.Root(ctx, "."); err == nil {
				_ = gitrepo.EnsureAbsoluteHooksPath(ctx, root)
			}

			if allBranches && branch != "" {
				return usageErr(cmd, fmt.Errorf("--all-branches and --branch are mutually exclusive"))
			}
			if allBranches && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("--all-branches cannot be used with positional job IDs"))
			}
			if branch != "" && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("--branch cannot be used with positional job IDs"))
			}
			if newestFirst && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("--newest-first cannot be used with positional job IDs"))
			}
			if list && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("--list cannot be used with positional job IDs"))
			}
			if list && batch {
				return usageErr(cmd, fmt.Errorf("--list and --batch are mutually exclusive"))
			}
			if list && batchSize > 0 {
				return fmt.Errorf("--list and --batch-size are mutually exclusive")
			}
			if batch && batchSize > 0 {
				return fmt.Errorf("--batch and --batch-size are mutually exclusive")
			}
			if cmd.Flags().Changed("batch-size") && batchSize < 1 {
				return fmt.Errorf("--batch-size must be >= 1")
			}
			if list {
				roots, err := resolveCurrentRepoRoots(ctx)
				if err != nil {
					return err
				}
				effectiveBranch := resolveCurrentBranchFilter(
					ctx, roots.worktreeRoot, branch, allBranches,
				)
				return runFixList(
					cmd, effectiveBranch,
					allBranches, branch != "", newestFirst,
				)
			}
			opts := fixOptions{
				agentName:   agentName,
				model:       model,
				reasoning:   reasoning,
				minSeverity: minSeverity,
				quiet:       quiet,
				resume:      resume,
				classify:    agent.ClassifyLimit,
			}

			roots, err := resolveCurrentRepoRoots(ctx)
			if err != nil {
				return err
			}
			tracker := &fixSessionTracker{
				enabled: opts.resume,
				quiet:   opts.quiet,
				out:     cmd.OutOrStdout(),
			}

			if batch || batchSize > 0 {
				var jobIDs []int64
				for _, arg := range args {
					var id int64
					if _, err := fmt.Sscanf(arg, "%d", &id); err != nil {
						return fmt.Errorf("invalid job ID %q: must be a number", arg)
					}
					jobIDs = append(jobIDs, id)
				}
				if len(jobIDs) > 0 && (branch != "" || allBranches || newestFirst) {
					return usageErr(cmd, fmt.Errorf("--branch, --all-branches, and --newest-first cannot be used with explicit job IDs"))
				}
				if len(jobIDs) == 0 {
					effectiveBranch := resolveCurrentBranchFilter(
						ctx, roots.worktreeRoot, branch, allBranches,
					)
					return runFixBatch(cmd, nil, effectiveBranch, allBranches, branch != "", newestFirst, batchSize, opts, tracker)
				}
				return runFixBatch(cmd, jobIDs, "", false, false, false, batchSize, opts, tracker)
			}

			if len(args) == 0 {
				// Resolve branch for API query filtering.
				// --branch X: use explicit branch
				// --all-branches: empty string (no filter)
				// default: current branch
				effectiveBranch := resolveCurrentBranchFilter(
					ctx, roots.worktreeRoot, branch, allBranches,
				)
				return runFixOpen(cmd, effectiveBranch, allBranches, branch != "", newestFirst, opts, tracker)
			}

			// Parse job IDs
			var jobIDs []int64
			for _, arg := range args {
				var id int64
				if _, err := fmt.Sscanf(arg, "%d", &id); err != nil {
					return fmt.Errorf("invalid job ID %q: must be a number", arg)
				}
				jobIDs = append(jobIDs, id)
			}

			return runFix(cmd, jobIDs, opts, tracker)
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "agent to use for fixes (default: from config)")
	cmd.Flags().StringVar(&model, "model", "", "model for agent")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level: fast, standard, medium, thorough, or maximum")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "", "minimum finding severity to address: critical, high, medium, or low")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress progress output")
	cmd.Flags().BoolVar(&open, "open", false, "deprecated: open is now the default behavior")
	cmd.Flags().BoolVar(&unaddressed, "unaddressed", false, "deprecated: open is now the default behavior")
	cmd.Flags().StringVar(&branch, "branch", "", "filter by branch (default: current branch)")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false, "include open jobs from all branches")
	cmd.Flags().BoolVar(&newestFirst, "newest-first", false, "process jobs newest first instead of oldest first")
	cmd.Flags().BoolVar(&batch, "batch", false, "concatenate reviews into a single prompt for the agent")
	cmd.Flags().IntVar(&batchSize, "batch-size", 0, "concatenate up to N reviews per agent invocation (cap by count, still bounded by max_prompt_size)")
	cmd.Flags().BoolVar(&list, "list", false, "list open jobs without fixing")
	cmd.Flags().BoolVar(&resume, "resume", false, "resume the agent's session ID across calls within this run")
	_ = cmd.Flags().MarkHidden("open")
	_ = cmd.Flags().MarkHidden("unaddressed")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)

	return cmd
}

type fixOptions struct {
	agentName   string
	model       string
	reasoning   string
	minSeverity string
	quiet       bool
	resume      bool

	// classify is the rate-limit classifier. Defaults to
	// agent.ClassifyLimit in the production cobra command's RunE; tests
	// inject a stub to drive deterministic KindQuota / KindSession
	// outcomes without depending on real agent error wording.
	classify agent.LimitClassifier
}

// agentLimitError is returned by the fix loop when the configured agent
// hits a quota or session limit. The fix command surfaces it as the
// process exit error so users see the reset time and a hint to retry.
type agentLimitError struct {
	Classification agent.LimitClassification
}

func (e *agentLimitError) Error() string {
	return formatAgentLimitMessage(e.Classification, time.Now())
}

// formatAgentLimitMessage builds the user-facing abort message. Pulled
// out so tests can assert against it without depending on time.Now.
// The label ("quota" / "session limit" / "rate limit") is derived from
// cls.Kind so a Gemini/Codex KindQuota abort doesn't mis-report itself
// as a session-cap.
func formatAgentLimitMessage(cls agent.LimitClassification, now time.Time) string {
	label := agentLimitLabel(cls.Kind)
	var dur time.Duration
	switch {
	case !cls.ResetAt.IsZero():
		dur = cls.ResetAt.Sub(now)
	case cls.CooldownFor > 0:
		dur = cls.CooldownFor
	}
	switch {
	case dur > 0 && !cls.ResetAt.IsZero():
		return fmt.Sprintf(
			"agent %s hit a %s. Cooldown until %s (in %s). "+
				"Re-run after that, or pass --agent <other> to switch.",
			cls.Agent,
			label,
			cls.ResetAt.Format("3:04 PM"),
			dur.Round(time.Minute),
		)
	case dur > 0:
		return fmt.Sprintf(
			"agent %s hit a %s. Cooldown for ~%s. "+
				"Re-run after that, or pass --agent <other> to switch.",
			cls.Agent,
			label,
			dur.Round(time.Minute),
		)
	default:
		flat := strings.ReplaceAll(cls.Message, "\n", " ")
		return fmt.Sprintf(
			"agent %s hit a %s (unknown reset time). "+
				"Re-run later, or pass --agent <other> to switch. "+
				"Original error: %s",
			cls.Agent,
			label,
			truncateString(flat, 200),
		)
	}
}

func agentLimitLabel(k agent.LimitKind) string {
	switch k {
	case agent.LimitKindSession:
		return "session limit"
	case agent.LimitKindQuota:
		return "quota limit"
	case agent.LimitKindTransient:
		return "rate limit"
	default:
		return "rate limit"
	}
}

// fixJobParams configures a fixJobDirect operation.
type fixJobParams struct {
	RepoRoot string
	Agent    agent.Agent
	Output   io.Writer // agent streaming output (nil = discard)
	// Classify is the rate-limit classifier used for the commit-retry
	// path. nil defaults to agent.ClassifyLimit. Tests inject a stub.
	Classify agent.LimitClassifier
}

// fixJobResult contains the outcome of a fix operation.
type fixJobResult struct {
	CommitCreated bool
	NewCommitSHA  string
	NoChanges     bool
	AgentOutput   string
}

// detectNewCommit checks whether HEAD has moved past headBefore.
func detectNewCommit(ctx context.Context, repoRoot, headBefore string) (string, bool) {
	head, err := gitrepo.Resolve(ctx, repoRoot, "HEAD")
	if err != nil {
		return "", false
	}
	if head != headBefore {
		return head, true
	}
	return "", false
}

// fixJobDirect runs the agent directly on the repo and detects commits.
// If the agent leaves uncommitted changes, it retries with a commit prompt.
func fixJobDirect(ctx context.Context, params fixJobParams, prompt string) (*fixJobResult, error) {
	out := params.Output
	if out == nil {
		out = io.Discard
	}

	headBefore, err := gitrepo.Resolve(ctx, params.RepoRoot, "HEAD")
	if err != nil {
		// Only proceed if this is specifically an unborn HEAD (empty repo).
		// Other errors (corrupt repo, permissions, non-git dir) should surface.
		if !gitrepo.IsUnbornHead(ctx, params.RepoRoot) {
			return nil, fmt.Errorf("resolve HEAD: %w", err)
		}
		// Unborn HEAD (empty repo) - run agent and check outcome
		agentOutput, agentErr := params.Agent.Review(ctx, params.RepoRoot, "HEAD", prompt, out)
		if agentErr != nil {
			return nil, fmt.Errorf("fix agent failed: %w", agentErr)
		}
		// Check if the agent created the first commit
		if headAfter, resolveErr := gitrepo.Resolve(ctx, params.RepoRoot, "HEAD"); resolveErr == nil {
			return &fixJobResult{CommitCreated: true, NewCommitSHA: headAfter, AgentOutput: agentOutput}, nil
		}
		// Still no commit - check working tree
		hasChanges, hcErr := gitrepo.HasUncommittedChanges(ctx, params.RepoRoot)
		if hcErr != nil {
			return nil, fmt.Errorf("failed to check working tree state: %w", hcErr)
		}
		return &fixJobResult{NoChanges: !hasChanges, AgentOutput: agentOutput}, nil
	}

	agentOutput, agentErr := params.Agent.Review(ctx, params.RepoRoot, "HEAD", prompt, out)
	if agentErr != nil {
		return nil, fmt.Errorf("fix agent failed: %w", agentErr)
	}

	if sha, ok := detectNewCommit(ctx, params.RepoRoot, headBefore); ok {
		return &fixJobResult{CommitCreated: true, NewCommitSHA: sha, AgentOutput: agentOutput}, nil
	}

	// No commit - retry if there are uncommitted changes
	hasChanges, err := gitrepo.HasUncommittedChanges(ctx, params.RepoRoot)
	if err != nil || !hasChanges {
		return &fixJobResult{NoChanges: (err == nil && !hasChanges), AgentOutput: agentOutput}, nil
	}

	fmt.Fprint(out, "\nNo commit was created. Re-running agent with commit instructions...\n\n")
	retryAgent := params.Agent
	// Thread the first call's session ID into the retry so the commit
	// step continues the same agent context. Without this, the retry
	// runs as a fresh session and the caller's tracker captures the
	// pre-retry session — leaving subsequent jobs to resume stale
	// context that's missing the actual fix work.
	if capture, ok := out.(*agent.SessionCaptureWriter); ok {
		capture.Flush()
		if id := capture.SessionID(); id != "" {
			if sa, ok := retryAgent.(agent.SessionAgent); ok {
				retryAgent = sa.WithSessionID(id)
			}
		}
	}
	if _, retryErr := retryAgent.Review(ctx, params.RepoRoot, "HEAD", buildGenericCommitPrompt(), out); retryErr != nil {
		// Classify the retry error so quota/session limits abort
		// instead of being demoted to a warning — otherwise the fix
		// loop keeps invoking the exhausted agent on every following
		// job until the queue is empty.
		classify := params.Classify
		if classify == nil {
			classify = agent.ClassifyLimit
		}
		cls := classify(agent.CanonicalName(retryAgent.Name()), retryErr.Error())
		if cls.Kind == agent.LimitKindQuota || cls.Kind == agent.LimitKindSession {
			// The first agent call left uncommitted changes; the
			// retry that would have committed them was aborted by
			// the limit. Surface the dirty-tree state so the user
			// can decide whether to commit manually before the
			// cooldown expires — the success path emits the same
			// warning, and bare cooldown text would otherwise hide
			// the regression.
			if hasChanges, _ := gitrepo.HasUncommittedChanges(ctx, params.RepoRoot); hasChanges {
				fmt.Fprintln(out, "Warning: Changes were made but not committed. Please review and commit manually.")
			}
			return nil, &agentLimitError{Classification: cls}
		}
		fmt.Fprintf(out, "Warning: commit agent failed: %v\n", retryErr)
	}
	if sha, ok := detectNewCommit(ctx, params.RepoRoot, headBefore); ok {
		return &fixJobResult{CommitCreated: true, NewCommitSHA: sha, AgentOutput: agentOutput}, nil
	}

	// Still no commit - report whether changes remain
	hasChanges, _ = gitrepo.HasUncommittedChanges(ctx, params.RepoRoot)
	return &fixJobResult{NoChanges: !hasChanges, AgentOutput: agentOutput}, nil
}

// resolveFixModel determines the model for a fix operation, skipping
// generic default_model when the actual fix agent that will run differs
// from the generic default agent. In that case, an empty result lets
// the fix agent keep its own built-in default model unless a
// fix-specific model override is configured.
func resolveFixModel(
	selectedAgent, cliModel, repoPath string,
	cfg *config.Config, reasoning string,
) (string, error) {
	if err := config.ValidateRepoConfig(repoPath); err != nil {
		return "", err
	}
	resolution, err := agent.ResolveWorkflowConfig(
		"", repoPath, cfg, "fix", reasoning,
	)
	if err != nil {
		return "", err
	}
	return resolution.ModelForSelectedAgent(
		selectedAgent, cliModel,
	), nil
}

// resolveFixAgent resolves and configures the agent for fix operations.
func resolveFixAgent(repoPath string, opts fixOptions) (agent.Agent, error) {
	cfg, err := config.LoadGlobal()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	reasoning, err := config.ResolveFixReasoning(opts.reasoning, repoPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve fix reasoning: %w", err)
	}
	if err := config.ValidateRepoConfig(repoPath); err != nil {
		return nil, fmt.Errorf("resolve workflow config: %w", err)
	}

	resolution, err := agent.ResolveWorkflowConfig(
		opts.agentName, repoPath, cfg, "fix", reasoning,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow config: %w", err)
	}

	a, err := agent.GetAvailableWithConfig(
		repoPath, resolution.PreferredAgent, cfg, resolution.BackupAgent,
	)
	if err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}

	modelStr := resolution.ModelForSelectedAgent(
		a.Name(), opts.model,
	)

	reasoningLevel := agent.ParseReasoningLevel(reasoning)
	a = a.WithAgentic(true).WithReasoning(reasoningLevel)
	if modelStr != "" {
		a = a.WithModel(modelStr)
	}
	return a, nil
}

func runFix(cmd *cobra.Command, jobIDs []int64, opts fixOptions, tracker *fixSessionTracker) error {
	return runFixWithSeen(cmd, jobIDs, opts, nil, tracker)
}

func runFixWithSeen(cmd *cobra.Command, jobIDs []int64, opts fixOptions, seen map[int64]bool, tracker *fixSessionTracker) error {
	// Ensure daemon is running
	if err := ensureDaemon(); err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		return fmt.Errorf("run fix: missing context")
	}

	roots, err := resolveCurrentRepoRoots(ctx)
	if err != nil {
		return err
	}

	// Process each job
	for i, jobID := range jobIDs {
		if len(jobIDs) > 1 && !opts.quiet {
			cmd.Printf("\n=== Fixing job %d (%d/%d) ===\n", jobID, i+1, len(jobIDs))
		}

		err := fixSingleJob(cmd, roots.worktreeRoot, jobID, opts, tracker)
		if err != nil {
			if isConnectionError(err) {
				return fmt.Errorf("daemon connection lost: %w", err)
			}
			// Agent quota/session-limit aborts must propagate even in
			// discovery mode — otherwise the re-query loop keeps
			// invoking the exhausted agent until every queued job is
			// burned through with the same error.
			var lim *agentLimitError
			if errors.As(err, &lim) {
				return err
			}
			// In discovery mode (seen != nil), log a warning and
			// continue best-effort. For explicit job IDs (seen ==
			// nil), return the error so the CLI exits non-zero.
			if seen != nil {
				cmd.Printf("Warning: error fixing job %d: %v\n", jobID, err)
				seen[jobID] = true
				continue
			}
			return fmt.Errorf("error fixing job %d: %w", jobID, err)
		}
		// Mark as seen so the re-query loop doesn't retry this job.
		// Only successfully attempted jobs reach here.
		if seen != nil {
			seen[jobID] = true
		}
	}

	return nil
}

// runFixOpen discovers and fixes open jobs.
//
// branch is used for the API query filter (current branch, explicit
// --branch, or "" for --all-branches). allBranches controls local
// filtering: when true, filterReachableJobs is skipped entirely.
// When false and the user passed --branch, the explicit branch is
// forwarded as a branchOverride for branch-field matching. When false
// and no --branch was passed, "" is used so filterReachableJobs falls
// back to commit-graph reachability.
//
// explicitBranch should be true when the caller set --branch (as
// opposed to auto-resolving the current branch).
func runFixOpen(cmd *cobra.Command, branch string, allBranches, explicitBranch, newestFirst bool, opts fixOptions, tracker *fixSessionTracker) error {
	// Ensure daemon is running
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

	seen := make(map[int64]bool)

	for {
		// When the branch was auto-resolved (not passed via --branch),
		// skip the API-level branch filter so jobs from merged branches
		// are included. filterReachableJobs handles the actual
		// filtering via commit-graph reachability.
		apiBranch := branch
		if !explicitBranch {
			apiBranch = ""
		}
		jobs, err := queryOpenJobs(ctx, roots.mainRepoRoot, apiBranch)
		if err != nil {
			return err
		}
		// --all-branches: skip filtering, user wants everything.
		// --branch X: pass branch so filterReachableJobs uses
		// branch-field matching (cross-branch from worktree).
		// Default: pass "" so filterReachableJobs uses commit-graph
		// reachability for SHA/range refs.
		if !allBranches {
			filterBranch := ""
			if explicitBranch {
				filterBranch = branch
			}
			jobs = filterReachableJobs(ctx, roots.worktreeRoot, filterBranch, jobs)
		}

		// Filter out jobs we've already processed
		var newIDs []int64
		for _, j := range jobs {
			if !seen[j.ID] {
				newIDs = append(newIDs, j.ID)
			}
		}

		if len(newIDs) == 0 {
			if len(seen) == 0 && !opts.quiet {
				cmd.Println("No open jobs found.")
			}
			return nil
		}

		// API returns newest first; reverse to process oldest first by default
		if !newestFirst {
			for i, j := 0, len(newIDs)-1; i < j; i, j = i+1, j-1 {
				newIDs[i], newIDs[j] = newIDs[j], newIDs[i]
			}
		}

		if !opts.quiet {
			if len(seen) > 0 {
				cmd.Printf("\nFound %d new open job(s): %v\n", len(newIDs), newIDs)
			} else {
				cmd.Printf("Found %d open job(s): %v\n", len(newIDs), newIDs)
			}
		}

		if err := runFixWithSeen(cmd, newIDs, opts, seen, tracker); err != nil {
			return err
		}
	}
}

// filterReachableJobs returns only those jobs relevant to the
// current worktree by matching the job's stored Branch field
// against the current (or overridden) branch. branchOverride is
// the explicit --branch value for non-mutating flows (e.g. --list).
// Mutating flows (fix, --batch) pass "" so that the current branch
// is auto-detected. Callers that want all branches (--all-branches)
// skip this function entirely.
func filterReachableJobs(
	ctx context.Context,
	worktreeRoot, branchOverride string,
	jobs []storage.ReviewJob,
) []storage.ReviewJob {
	matchBranch := branchOverride
	if matchBranch == "" {
		matchBranch = gitrepo.CurrentBranch(ctx, worktreeRoot)
	}
	var detachedRefs map[string]struct{}
	if matchBranch == "" {
		detachedRefs = detachedHeadReviewRefs(ctx, worktreeRoot)
	}
	var filtered []storage.ReviewJob
	for _, j := range jobs {
		if branchMatch(matchBranch, j.Branch) {
			filtered = append(filtered, j)
			continue
		}
		if len(detachedRefs) > 0 && jobMatchesDetachedRef(ctx, worktreeRoot, detachedRefs, j) {
			filtered = append(filtered, j)
		}
	}
	return filtered
}

// branchMatch returns true when a job's branch matches the target.
// Both must be known and equal. Jobs with no branch are excluded
// (use --all-branches to include them).
func branchMatch(matchBranch, jobBranch string) bool {
	if matchBranch == "" || jobBranch == "" {
		return false
	}
	return jobBranch == matchBranch
}

func detachedHeadReviewRefs(ctx context.Context, worktreeRoot string) map[string]struct{} {
	head, err := gitrepo.Resolve(ctx, worktreeRoot, "HEAD")
	if err != nil {
		return nil
	}

	refs := make(map[string]struct{})
	seen := make(map[string]struct{})
	stack := []string{head}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[sha]; ok {
			continue
		}
		seen[sha] = struct{}{}
		refs[sha] = struct{}{}
		if git.GetBranchName(worktreeRoot, sha) != "" {
			continue
		}
		parents, err := git.GetCommitParents(worktreeRoot, sha)
		if err == nil {
			stack = append(stack, parents...)
		}
	}
	return refs
}

func jobMatchesDetachedRef(ctx context.Context, worktreeRoot string, refs map[string]struct{}, job storage.ReviewJob) bool {
	if job.GitRef == "" || job.GitRef == "dirty" {
		return false
	}
	ref := job.GitRef
	if _, end, ok := git.ParseRange(ref); ok {
		ref = end
	}
	sha, err := gitrepo.Resolve(ctx, worktreeRoot, ref)
	if err != nil {
		return false
	}
	_, ok := refs[sha]
	return ok
}

func queryOpenJobs(
	ctx context.Context,
	repoRoot, branch string,
) ([]storage.ReviewJob, error) {
	jobs, err := withFixDaemonRetryContext(ctx, getDaemonEndpoint().BaseURL(), func(addr string) ([]storage.ReviewJob, error) {
		queryURL := fmt.Sprintf(
			"%s/api/jobs?status=done&repo=%s&closed=false&limit=0",
			addr, url.QueryEscape(repoRoot),
		)
		if branch != "" {
			queryURL += "&branch=" + url.QueryEscape(branch) +
				"&branch_include_empty=true"
		}

		resp, err := doFixDaemonRequest(ctx, http.MethodGet, queryURL, nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf(
				"server error (%d): %s", resp.StatusCode, body,
			)
		}

		var jobsResp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&jobsResp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}

		return filterFixCandidateJobs(jobsResp.Jobs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	return jobs, nil
}

func filterFixCandidateJobs(jobs []storage.ReviewJob) []storage.ReviewJob {
	filtered := make([]storage.ReviewJob, 0, len(jobs))
	for _, job := range jobs {
		if !isFixCandidateJob(job) {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

func isFixCandidateJob(job storage.ReviewJob) bool {
	if job.Verdict == nil || !strings.EqualFold(strings.TrimSpace(*job.Verdict), "F") {
		return false
	}
	return job.IsReviewJob() ||
		job.JobType == storage.JobTypeCompact ||
		job.IsSynthesisJob()
}

func queryOpenJobIDs(
	ctx context.Context,
	repoRoot, branch string,
) ([]int64, error) {
	jobs, err := queryOpenJobs(ctx, repoRoot, branch)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids, nil
}

// runFixList prints open jobs with detailed information without running any agent.
func runFixList(
	cmd *cobra.Command,
	branch string,
	allBranches, explicitBranch, newestFirst bool,
) error {
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

	// When the branch was auto-resolved, skip the API-level branch
	// filter so jobs from merged branches are included.
	apiBranch := branch
	if !explicitBranch {
		apiBranch = ""
	}
	jobs, err := queryOpenJobs(ctx, roots.mainRepoRoot, apiBranch)
	if err != nil {
		return err
	}
	if !allBranches {
		filterBranch := ""
		if explicitBranch {
			filterBranch = branch
		}
		jobs = filterReachableJobs(ctx,
			roots.worktreeRoot, filterBranch, jobs,
		)
	}

	jobIDs := make([]int64, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}

	if !newestFirst {
		for i, j := 0, len(jobIDs)-1; i < j; i, j = i+1, j-1 {
			jobIDs[i], jobIDs[j] = jobIDs[j], jobIDs[i]
		}
	}

	if len(jobIDs) == 0 {
		cmd.Println("No open jobs found.")
		return nil
	}

	cmd.Printf("Found %d open job(s):\n\n", len(jobIDs))

	listAddr := getDaemonEndpoint().BaseURL()
	for _, id := range jobIDs {
		job, err := fetchJob(ctx, listAddr, id)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not fetch job %d: %v\n", id, err)
			continue
		}
		review, err := fetchReview(ctx, listAddr, id)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not fetch review for job %d: %v\n", id, err)
			continue
		}

		// Format the output with all available information
		cmd.Printf("Job #%d\n", id)
		cmd.Printf("  Git Ref:  %s\n", gitrepo.ShortSHA(job.GitRef))
		if job.Branch != "" {
			cmd.Printf("  Branch:   %s\n", job.Branch)
		}
		if job.CommitSubject != "" {
			cmd.Printf("  Subject:  %s\n", truncateString(job.CommitSubject, 60))
		}
		cmd.Printf("  Agent:    %s\n", job.Agent)
		if job.Model != "" {
			cmd.Printf("  Model:    %s\n", job.Model)
		}
		if job.FinishedAt != nil {
			cmd.Printf("  Finished: %s\n", job.FinishedAt.Local().Format("2006-01-02 15:04:05"))
		}
		if job.Verdict != nil && *job.Verdict != "" {
			cmd.Printf("  Verdict:  %s\n", *job.Verdict)
		}
		summary := firstLine(review.Output)
		if summary != "" {
			cmd.Printf("  Summary:  %s\n", summary)
		}
		cmd.Println()
	}

	cmd.Printf("To apply a fix: roborev fix <job_id>\n")
	cmd.Printf("To apply all:   roborev fix\n")

	return nil
}

// isConnectionError checks if an error indicates a network/connection failure
// (as opposed to an application-level error like 404 or invalid response).
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// truncateString truncates s to maxLen characters, adding "..." if truncated.
// It operates on Unicode runes to avoid cutting multi-byte characters.
func truncateString(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// firstLine returns the first non-empty line of s, truncated to 80 chars.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return truncateString(line, 80)
		}
	}
	return truncateString(s, 80)
}

// jobVerdict returns the verdict for a job. Uses the stored verdict
// if available, otherwise parses from the review output.
func jobVerdict(job *storage.ReviewJob, review *storage.Review) string {
	if job.Verdict != nil && *job.Verdict != "" {
		return *job.Verdict
	}
	return storage.ParseVerdict(review.Output)
}

func fixSingleJob(cmd *cobra.Command, repoRoot string, jobID int64, opts fixOptions, tracker *fixSessionTracker) error {
	if opts.classify == nil {
		opts.classify = agent.ClassifyLimit
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Snapshot the daemon address once for the entire operation.
	// The retry helpers (withFixDaemonRetryContext) handle re-resolution
	// internally if a connection error triggers daemon recovery.
	addr := getDaemonEndpoint().BaseURL()

	// Fetch the job to check status
	job, err := fetchJob(ctx, addr, jobID)
	if err != nil {
		return fmt.Errorf("fetch job: %w", err)
	}

	if job.Status != storage.JobStatusDone {
		return fmt.Errorf("job %d is not complete (status: %s)", jobID, job.Status)
	}

	// Fetch the review/analysis output
	review, err := fetchReview(ctx, addr, jobID)
	if err != nil {
		return fmt.Errorf("fetch review: %w", err)
	}

	// Skip reviews that passed — no findings to fix
	if jobVerdict(job, review) == "P" {
		if !opts.quiet {
			cmd.Printf("Job %d: review passed, skipping fix\n", jobID)
		}
		if err := markJobClosed(ctx, addr, jobID); err != nil && !opts.quiet {
			cmd.Printf("Warning: could not close job %d: %v\n", jobID, err)
		}
		return nil
	}

	if !opts.quiet {
		cmd.Printf("Job %d analysis output:\n", jobID)
		cmd.Println(strings.Repeat("-", 60))
		streamfmt.PrintMarkdownOrPlain(cmd.OutOrStdout(), review.Output)
		cmd.Println(strings.Repeat("-", 60))
		cmd.Println()
	}

	// Resolve minimum severity filter (only for review-type jobs;
	// task/analyze jobs have free-form output without severity labels)
	var minSev string
	if !job.IsTaskJob() {
		fixCfg, _ := config.LoadGlobal()
		minSev, err = config.ResolveFixMinSeverity(
			opts.minSeverity, repoRoot, fixCfg,
		)
		if err != nil {
			return fmt.Errorf("resolve min-severity: %w", err)
		}
	}

	// Fetch user comments for context (including legacy commit-based comments)
	var commitID int64
	var gitRef string
	if job != nil {
		commitID, gitRef = job.LegacyCommentLookupTarget()
	}
	comments, commentsErr := fetchComments(ctx, addr, jobID, commitID, gitRef)
	if commentsErr != nil && !opts.quiet {
		cmd.Printf("Warning: could not fetch comments for job %d: %v\n", jobID, commentsErr)
	}

	// Resolve the agent only after every check above that can return
	// without invoking it. This keeps no-op paths (verdict P, bad
	// min-severity config, job-not-done) from failing on missing-agent
	// resolution before the actual reason surfaces.
	if err := ensureBaseAgent(repoRoot, opts, tracker); err != nil {
		return err
	}
	currentAgent, resuming := tracker.NextAgent()

	if !opts.quiet {
		if resuming {
			cmd.Printf("Resuming session %s\n", shortSessionID(tracker.last))
		}
		cmd.Printf("Running fix agent (%s) to apply changes...\n\n", currentAgent.Name())
	}

	// Set up output
	underlying := io.Discard
	var fmtr *streamfmt.Formatter
	if !opts.quiet {
		fmtr = streamfmt.New(cmd.OutOrStdout(), streamfmt.WriterIsTerminal(cmd.OutOrStdout()))
		underlying = fmtr
	}
	capture := agent.NewSessionCaptureWriter(underlying, nil)

	result, err := fixJobDirect(ctx, fixJobParams{
		RepoRoot: repoRoot,
		Agent:    currentAgent,
		Output:   capture,
		Classify: opts.classify,
	}, buildGenericFixPrompt(review.Output, minSev, comments))
	// Flush capture FIRST so session extraction completes before reading SessionID.
	capture.Flush()
	if fmtr != nil {
		fmtr.Flush()
	}
	if err != nil {
		tracker.Reset()
		// fixJobDirect already returns *agentLimitError for retry-path
		// quota/session aborts; preserve it instead of re-classifying
		// its user-facing message string.
		var lim *agentLimitError
		if errors.As(err, &lim) {
			return err
		}
		cls := opts.classify(agent.CanonicalName(currentAgent.Name()), err.Error())
		switch cls.Kind {
		case agent.LimitKindQuota, agent.LimitKindSession:
			return &agentLimitError{Classification: cls}
		case agent.LimitKindNone:
			if err.Error() != "" && !opts.quiet {
				flat := strings.ReplaceAll(err.Error(), "\n", " ")
				cmd.PrintErrf(
					"warning: unclassified agent error from %s: %s\n",
					currentAgent.Name(),
					truncateString(flat, 200),
				)
			}
		}
		return err
	}
	tracker.Capture(capture.SessionID())

	if !opts.quiet {
		fmt.Fprintln(cmd.OutOrStdout())
	}

	// Report commit status
	if !opts.quiet {
		if result.CommitCreated {
			cmd.Println("\nChanges committed successfully.")
		} else if result.NoChanges {
			cmd.Println("\nNo changes were made by the fix agent.")
		} else {
			hasChanges, err := gitrepo.HasUncommittedChanges(ctx, repoRoot)
			if err == nil && hasChanges {
				cmd.Println("\nWarning: Changes were made but not committed. Please review and commit manually.")
			}
		}
	}

	// Enqueue review for fix commit
	if result.CommitCreated {
		if err := enqueueIfNeeded(ctx, addr, repoRoot, result.NewCommitSHA); err != nil && !opts.quiet {
			cmd.Printf("Warning: could not enqueue review for fix commit: %v\n", err)
		}
	}

	// Add response and mark as closed
	responseText := "Fix applied via `roborev fix` command"
	if result.CommitCreated {
		responseText = fmt.Sprintf("Fix applied via `roborev fix` command (commit: %s)", gitrepo.ShortSHA(result.NewCommitSHA))
	}

	if err := addJobResponse(ctx, addr, jobID, "roborev-fix", responseText); err != nil {
		if !opts.quiet {
			cmd.Printf("Warning: could not add response to job: %v\n", err)
		}
	}

	if err := markJobClosed(ctx, addr, jobID); err != nil {
		if !opts.quiet {
			cmd.Printf("Warning: could not close job: %v\n", err)
		}
	} else if !opts.quiet {
		cmd.Printf("Job %d closed\n", jobID)
	}

	return nil
}

// batchEntry holds a fetched job and its review for batch processing.
type batchEntry struct {
	jobID    int64
	job      *storage.ReviewJob
	review   *storage.Review
	comments []storage.Response
}

// runFixBatch discovers jobs (or uses provided IDs), splits them into batches
// respecting max prompt size, and runs each batch as a single agent invocation.
//
// In discovery mode (no explicit jobIDs), it re-queries for open jobs after
// each round of batches and continues until no new open jobs remain — matching
// the keep-going behavior of runFixOpen so reviews that complete mid-run get
// picked up. With explicit jobIDs the function processes the list once.
func runFixBatch(cmd *cobra.Command, jobIDs []int64, branch string, allBranches, explicitBranch, newestFirst bool, batchSize int, opts fixOptions, tracker *fixSessionTracker) error {
	if opts.classify == nil {
		opts.classify = agent.ClassifyLimit
	}
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

	if len(jobIDs) > 0 {
		return processFixBatch(ctx, cmd, roots, jobIDs, batchSize, opts, tracker)
	}

	seen := make(map[int64]bool)
	for {
		apiBranch := branch
		if !explicitBranch {
			apiBranch = ""
		}
		jobs, queryErr := queryOpenJobs(ctx, roots.mainRepoRoot, apiBranch)
		if queryErr != nil {
			return queryErr
		}
		if !allBranches {
			filterBranch := ""
			if explicitBranch {
				filterBranch = branch
			}
			jobs = filterReachableJobs(ctx, roots.worktreeRoot, filterBranch, jobs)
		}

		var newIDs []int64
		for _, j := range jobs {
			if !seen[j.ID] {
				newIDs = append(newIDs, j.ID)
			}
		}
		if !newestFirst {
			for i, j := 0, len(newIDs)-1; i < j; i, j = i+1, j-1 {
				newIDs[i], newIDs[j] = newIDs[j], newIDs[i]
			}
		}

		if len(newIDs) == 0 {
			if len(seen) == 0 && !opts.quiet {
				cmd.Println("No open jobs found.")
			}
			return nil
		}

		if !opts.quiet {
			if len(seen) > 0 {
				cmd.Printf("\nFound %d new open job(s): %v\n", len(newIDs), newIDs)
			} else {
				cmd.Printf("Found %d open job(s): %v\n", len(newIDs), newIDs)
			}
		}
		// Mark before processing so transient fetch errors or skipped
		// (passing) jobs don't get re-discovered on the next iteration.
		for _, id := range newIDs {
			seen[id] = true
		}

		if err := processFixBatch(ctx, cmd, roots, newIDs, batchSize, opts, tracker); err != nil {
			return err
		}
	}
}

// processFixBatch fetches reviews for the given job IDs, splits them into
// batches by prompt size and count, runs each batch as a single agent
// invocation, and marks the jobs closed when done.
func processFixBatch(ctx context.Context, cmd *cobra.Command, roots currentRepoRoots, jobIDs []int64, batchSize int, opts fixOptions, tracker *fixSessionTracker) error {
	if len(jobIDs) == 0 {
		if !opts.quiet {
			cmd.Println("No open jobs found.")
		}
		return nil
	}

	batchAddr := getDaemonEndpoint().BaseURL()
	var entries []batchEntry
	for _, id := range jobIDs {
		job, err := fetchJob(ctx, batchAddr, id)
		if err != nil {
			if !opts.quiet {
				cmd.Printf("Warning: skipping job %d: %v\n", id, err)
			}
			continue
		}
		if job.Status != storage.JobStatusDone {
			if !opts.quiet {
				cmd.Printf("Warning: skipping job %d (status: %s)\n", id, job.Status)
			}
			continue
		}
		review, err := fetchReview(ctx, batchAddr, id)
		if err != nil {
			if !opts.quiet {
				cmd.Printf("Warning: skipping job %d: %v\n", id, err)
			}
			continue
		}
		if jobVerdict(job, review) == "P" {
			if !opts.quiet {
				cmd.Printf("Skipping job %d (review passed)\n", id)
			}
			if err := markJobClosed(ctx, batchAddr, id); err != nil && !opts.quiet {
				cmd.Printf("Warning: could not close job %d: %v\n", id, err)
			}
			continue
		}
		var batchCommitID int64
		var batchGitRef string
		if job != nil {
			batchCommitID, batchGitRef = job.LegacyCommentLookupTarget()
		}
		comments, commentsErr := fetchComments(ctx, batchAddr, id, batchCommitID, batchGitRef)
		if commentsErr != nil && !opts.quiet {
			cmd.Printf("Warning: could not fetch comments for job %d: %v\n", id, commentsErr)
		}
		entries = append(entries, batchEntry{jobID: id, job: job, review: review, comments: comments})
	}

	if len(entries) == 0 {
		if !opts.quiet {
			cmd.Println("No eligible jobs to batch.")
		}
		return nil
	}

	cfg, _ := config.LoadGlobal()

	// Resolve minimum severity filter. Suppress if any entry is a
	// task job — task/analyze output has no severity labels, so the
	// instruction would confuse the agent for those entries.
	minSev, err := config.ResolveFixMinSeverity(
		opts.minSeverity, roots.worktreeRoot, cfg,
	)
	if err != nil {
		return fmt.Errorf("resolve min-severity: %w", err)
	}
	if minSev != "" {
		for _, e := range entries {
			if e.job.IsTaskJob() {
				minSev = ""
				break
			}
		}
	}

	// Split into batches by prompt size (after severity resolution
	// so the severity instruction overhead is accounted for)
	maxSize := config.ResolveMaxPromptSize(roots.worktreeRoot, cfg)
	batches := splitIntoBatches(entries, batchSplitOptions{
		MaxSize:     maxSize,
		MaxCount:    batchSize,
		MinSeverity: minSev,
	})

	if err := ensureBaseAgent(roots.worktreeRoot, opts, tracker); err != nil {
		return err
	}

	for i, batch := range batches {
		batchJobIDs := make([]int64, len(batch))
		for j, e := range batch {
			batchJobIDs[j] = e.jobID
		}

		currentAgent, resuming := tracker.NextAgent()

		if !opts.quiet {
			cmd.Printf("\n=== Batch %d/%d (jobs %s) ===\n\n", i+1, len(batches), formatJobIDs(batchJobIDs))
			w := cmd.OutOrStdout()
			for _, e := range batch {
				cmd.Printf("Job %d findings:\n", e.jobID)
				cmd.Println(strings.Repeat("-", 60))
				streamfmt.PrintMarkdownOrPlain(w, e.review.Output)
				cmd.Println(strings.Repeat("-", 60))
				cmd.Println()
			}
			if resuming {
				cmd.Printf("Resuming session %s\n", shortSessionID(tracker.last))
			}
			cmd.Printf("Running fix agent (%s) to apply changes...\n\n", currentAgent.Name())
		}

		prompt := buildBatchFixPrompt(batch, minSev)

		underlying := io.Discard
		var fmtr *streamfmt.Formatter
		if !opts.quiet {
			fmtr = streamfmt.New(cmd.OutOrStdout(), streamfmt.WriterIsTerminal(cmd.OutOrStdout()))
			underlying = fmtr
		}
		capture := agent.NewSessionCaptureWriter(underlying, nil)

		result, err := fixJobDirect(ctx, fixJobParams{
			RepoRoot: roots.worktreeRoot,
			Agent:    currentAgent,
			Output:   capture,
			Classify: opts.classify,
		}, prompt)
		// Flush capture FIRST so session extraction completes before reading SessionID.
		capture.Flush()
		if fmtr != nil {
			fmtr.Flush()
		}
		if err != nil {
			tracker.Reset()
			// Preserve a retry-path agentLimitError without
			// re-classifying its user-facing message string.
			var lim *agentLimitError
			if errors.As(err, &lim) {
				return err
			}
			cls := opts.classify(agent.CanonicalName(currentAgent.Name()), err.Error())
			switch cls.Kind {
			case agent.LimitKindQuota, agent.LimitKindSession:
				return &agentLimitError{Classification: cls}
			case agent.LimitKindNone:
				if err.Error() != "" && !opts.quiet {
					flat := strings.ReplaceAll(err.Error(), "\n", " ")
					cmd.PrintErrf(
						"warning: unclassified agent error from %s: %s\n",
						currentAgent.Name(),
						truncateString(flat, 200),
					)
				}
			}
			cmd.Printf("Warning: error in batch %d: %v\n", i+1, err)
			continue
		}
		tracker.Capture(capture.SessionID())

		if !opts.quiet {
			fmt.Fprintln(cmd.OutOrStdout())
			if result.CommitCreated {
				cmd.Println("Changes committed successfully.")
			} else if result.NoChanges {
				cmd.Println("No changes were made by the fix agent.")
			} else {
				if hasChanges, hcErr := gitrepo.HasUncommittedChanges(ctx, roots.worktreeRoot); hcErr == nil && hasChanges {
					cmd.Println("Warning: Changes were made but not committed. Please review and commit manually.")
				}
			}
		}

		// Enqueue review for fix commit
		if result.CommitCreated {
			if enqErr := enqueueIfNeeded(ctx, batchAddr, roots.worktreeRoot, result.NewCommitSHA); enqErr != nil && !opts.quiet {
				cmd.Printf("Warning: could not enqueue review for fix commit: %v\n", enqErr)
			}
		}

		// Mark all jobs in this batch as closed
		flagLabel := "--batch"
		if batchSize > 0 {
			flagLabel = "--batch-size"
		}
		responseText := fmt.Sprintf("Fix applied via `roborev fix %s`", flagLabel)
		if result.CommitCreated {
			responseText = fmt.Sprintf("Fix applied via `roborev fix %s` (commit: %s)", flagLabel, gitrepo.ShortSHA(result.NewCommitSHA))
		}
		for _, e := range batch {
			if addErr := addJobResponse(ctx, batchAddr, e.jobID, "roborev-fix", responseText); addErr != nil && !opts.quiet {
				cmd.Printf("Warning: could not add response to job %d: %v\n", e.jobID, addErr)
			}
			if markErr := markJobClosed(ctx, batchAddr, e.jobID); markErr != nil {
				if !opts.quiet {
					cmd.Printf("Warning: could not close job %d: %v\n", e.jobID, markErr)
				}
			} else if !opts.quiet {
				cmd.Printf("Job %d closed\n", e.jobID)
			}
		}
	}

	return nil
}

// batchPromptOverhead is the fixed size of the batch prompt header + footer.
var batchPromptOverhead = len(batchPromptHeader + batchPromptFooter)

const (
	batchPromptHeader = "# Batch Fix Request\n\nThe following reviews found issues that need to be fixed.\nAddress all findings across all reviews in a single pass.\n\n"
	batchPromptFooter = "## Instructions\n\nPlease apply fixes for all the findings above.\nFocus on the highest priority items first.\nAfter making changes, verify the code compiles/passes linting,\nrun relevant tests, and create a git commit summarizing all changes.\n"
)

// batchEntrySize returns the size of a single entry in the batch prompt.
// The index parameter is the 1-based position in the batch.
func batchEntrySize(index int, e batchEntry) int {
	toolAttempts, userComments := prompt.SplitResponses(e.comments)
	size := len(fmt.Sprintf("## Review %d (Job %d — %s)\n\n%s\n\n", index, e.jobID, gitrepo.ShortSHA(e.job.GitRef), e.review.Output))
	size += len(prompt.FormatToolAttempts(toolAttempts))
	size += len(prompt.FormatUserComments(userComments))
	return size
}

// batchSplitOptions configures how splitIntoBatches groups entries.
// Both caps are upper bounds; MaxCount = 0 means "no count cap".
type batchSplitOptions struct {
	MaxSize     int    // total prompt bytes per batch, including overhead
	MaxCount    int    // entries per batch (0 = unbounded)
	MinSeverity string // forwarded to overhead calculation
}

// splitIntoBatches groups entries into batches respecting opts.
// Greedy packing: a batch terminates when adding the next entry would
// exceed MaxSize, OR when MaxCount > 0 and the batch already has
// MaxCount entries. A single oversized entry still gets its own batch.
func splitIntoBatches(
	entries []batchEntry, opts batchSplitOptions,
) [][]batchEntry {
	overhead := batchPromptOverhead +
		len(config.SeverityInstruction(opts.MinSeverity))
	var batches [][]batchEntry
	var current []batchEntry
	currentSize := 0

	for _, e := range entries {
		entrySize := batchEntrySize(len(current)+1, e)

		countFull := opts.MaxCount > 0 && len(current) >= opts.MaxCount
		sizeFull := len(current) > 0 && currentSize+entrySize > opts.MaxSize
		if countFull || sizeFull {
			batches = append(batches, current)
			current = nil
			currentSize = 0
			entrySize = batchEntrySize(1, e)
		}

		current = append(current, e)
		if currentSize == 0 {
			currentSize = overhead
		}
		currentSize += entrySize
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// buildBatchFixPrompt creates a concatenated prompt from multiple reviews.
// When minSeverity is non-empty, a severity filtering instruction is injected.
// User comments attached to each entry are included inline.
func buildBatchFixPrompt(entries []batchEntry, minSeverity string) string {
	var sb strings.Builder
	sb.WriteString(batchPromptHeader)
	if inst := config.SeverityInstruction(minSeverity); inst != "" {
		sb.WriteString(inst)
		sb.WriteString("\n")
	}

	for i, e := range entries {
		toolAttempts, userComments := prompt.SplitResponses(e.comments)
		fmt.Fprintf(&sb, "## Review %d (Job %d — %s)\n\n", i+1, e.jobID, gitrepo.ShortSHA(e.job.GitRef))
		sb.WriteString(e.review.Output)
		sb.WriteString("\n\n")
		sb.WriteString(prompt.FormatToolAttempts(toolAttempts))
		sb.WriteString(prompt.FormatUserComments(userComments))
	}

	sb.WriteString(batchPromptFooter)
	return sb.String()
}

// formatJobIDs formats a slice of job IDs as a comma-separated string.
func formatJobIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, ", ")
}

// fetchJob retrieves a job from the daemon
func fetchJob(ctx context.Context, serverAddr string, jobID int64) (*storage.ReviewJob, error) {
	return withFixDaemonRetryContext(ctx, serverAddr, func(addr string) (*storage.ReviewJob, error) {
		client := getDaemonHTTPClient(30 * time.Second)

		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/jobs?id=%d", addr, jobID), nil)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, body)
		}

		var jobsResp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&jobsResp); err != nil {
			return nil, err
		}

		if len(jobsResp.Jobs) == 0 {
			return nil, fmt.Errorf("job %d not found", jobID)
		}

		return &jobsResp.Jobs[0], nil
	})
}

// fetchReview retrieves the review output for a job
func fetchReview(ctx context.Context, serverAddr string, jobID int64) (*storage.Review, error) {
	return withFixDaemonRetryContext(ctx, serverAddr, func(addr string) (*storage.Review, error) {
		client := getDaemonHTTPClient(30 * time.Second)

		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/review?job_id=%d", addr, jobID), nil)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, body)
		}

		var review storage.Review
		if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
			return nil, err
		}

		return &review, nil
	})
}

// fetchComments retrieves comments/responses for a job, including legacy
// commit-based comments merged via storage.MergeResponses. Prefers commit_id
// (unambiguous) when available, falls back to SHA for legacy jobs.
func fetchComments(ctx context.Context, serverAddr string, jobID, commitID int64, gitRef string) ([]storage.Response, error) {
	return withFixDaemonRetryContext(ctx, serverAddr, func(addr string) ([]storage.Response, error) {
		client := getDaemonHTTPClient(30 * time.Second)

		// Fetch by job ID
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/comments?job_id=%d", addr, jobID), nil)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, body)
		}

		var result struct {
			Responses []storage.Response `json:"responses"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		responses := result.Responses

		// Also fetch legacy commit-based comments and merge.
		// Prefer commit_id (unambiguous), fall back to SHA only when
		// gitRef looks like a hex SHA (not a task label like "run").
		commitID, gitRef = legacyCommentLookupTarget(commitID, gitRef)
		var legacyURL string
		if commitID > 0 {
			legacyURL = fmt.Sprintf("%s/api/comments?commit_id=%d", addr, commitID)
		} else if gitRef != "" {
			legacyURL = fmt.Sprintf("%s/api/comments?sha=%s", addr, gitRef)
		}
		if legacyURL != "" {
			legacyReq, err := http.NewRequestWithContext(ctx, "GET", legacyURL, nil)
			if err == nil {
				legacyResp, err := client.Do(legacyReq)
				if err == nil {
					defer legacyResp.Body.Close()
					if legacyResp.StatusCode == http.StatusOK {
						var legacyResult struct {
							Responses []storage.Response `json:"responses"`
						}
						if json.NewDecoder(legacyResp.Body).Decode(&legacyResult) == nil {
							responses = storage.MergeResponses(responses, legacyResult.Responses)
						}
					}
				}
			}
		}

		return responses, nil
	})
}

func legacyCommentLookupTarget(commitID int64, gitRef string) (int64, string) {
	var commitIDPtr *int64
	if commitID > 0 {
		commitIDPtr = &commitID
	}
	return storage.ReviewJob{
		CommitID: commitIDPtr,
		GitRef:   gitRef,
	}.LegacyCommentLookupTarget()
}

// buildGenericFixPrompt creates a fix prompt without knowing the analysis type.
// When minSeverity is non-empty, a severity filtering instruction is prepended.
// Responses are split into tool attempts and user comments so each type
// receives appropriate framing in the prompt.
func buildGenericFixPrompt(analysisOutput, minSeverity string, responses []storage.Response) string {
	toolAttempts, userComments := prompt.SplitResponses(responses)
	var sb strings.Builder
	sb.WriteString("# Fix Request\n\n")
	if inst := config.SeverityInstruction(minSeverity); inst != "" {
		sb.WriteString(inst)
		sb.WriteString("\n")
	}
	sb.WriteString("An analysis was performed and produced the following findings:\n\n")
	sb.WriteString("## Analysis Findings\n\n")
	sb.WriteString(analysisOutput)
	sb.WriteString("\n\n")
	sb.WriteString(prompt.FormatToolAttempts(toolAttempts))
	sb.WriteString(prompt.FormatUserComments(userComments))
	sb.WriteString("## Instructions\n\n")
	sb.WriteString("Please apply the suggested changes from the analysis above. ")
	sb.WriteString("Make the necessary edits to address each finding. ")
	sb.WriteString("Focus on the highest priority items first.\n\n")
	sb.WriteString("After making changes:\n")
	sb.WriteString("1. Verify the code still compiles/passes linting\n")
	sb.WriteString("2. Run any relevant tests to ensure nothing is broken\n")
	sb.WriteString("3. Create a git commit with a descriptive message summarizing the changes\n")
	return sb.String()
}

// buildGenericCommitPrompt creates a prompt to commit uncommitted changes
func buildGenericCommitPrompt() string {
	var sb strings.Builder
	sb.WriteString("# Commit Request\n\n")
	sb.WriteString("There are uncommitted changes from a previous fix operation.\n\n")
	sb.WriteString("## Instructions\n\n")
	sb.WriteString("1. Review the current uncommitted changes using `git status` and `git diff`\n")
	sb.WriteString("2. Stage the appropriate files\n")
	sb.WriteString("3. Create a git commit with a descriptive message\n\n")
	sb.WriteString("The commit message should:\n")
	sb.WriteString("- Summarize what was changed and why\n")
	sb.WriteString("- Be concise but informative\n")
	return sb.String()
}

// addJobResponse adds a response/comment to a job
func addJobResponse(ctx context.Context, serverAddr string, jobID int64, commenter, response string) error {
	reqBody, _ := json.Marshal(map[string]any{
		"job_id":    jobID,
		"commenter": commenter,
		"comment":   response,
	})

	currentAddr := serverAddr
	for attempt := 0; ; attempt++ {
		resp, err := doFixDaemonRequest(ctx, http.MethodPost, currentAddr+"/api/comment", reqBody)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("add response failed: %s", body)
			}
			return nil
		}
		if !isConnectionError(err) || attempt >= fixDaemonMaxRetries {
			return err
		}
		if shouldStopFixDaemonRetry(ctx) {
			return err
		}

		currentAddr, err = recoverFixDaemonAddr(ctx)
		if err != nil {
			return err
		}
		if shouldStopFixDaemonRetry(ctx) {
			return ctx.Err()
		}
		applied, verifyErr := hasJobResponseContext(ctx, currentAddr, jobID, commenter, response)
		if verifyErr != nil {
			return fmt.Errorf("verify response after retryable failure: %w", verifyErr)
		}
		if applied {
			return nil
		}
	}
}

// enqueueIfNeeded enqueues a review for a commit via the daemon API.
// This ensures fix commits get reviewed even if the post-commit hook
// didn't fire (e.g., agent subprocesses may not trigger hooks reliably).
func enqueueIfNeeded(ctx context.Context, serverAddr, repoPath, sha string) error {
	// Check if a review job already exists for this commit (e.g., from the
	// post-commit hook). If so, skip enqueuing to avoid duplicates.
	// The post-commit hook normally completes before control returns here,
	// but under heavy load it may take longer. Poll with short intervals
	// up to a max wait to avoid both unnecessary delays and duplicates.
	currentAddr := serverAddr
	for range enqueueIfNeededProbeAttempts {
		if shouldStopFixDaemonRetry(ctx) {
			return ctx.Err()
		}
		found, err := hasJobForSHAContext(ctx, currentAddr, sha)
		if err == nil && found {
			return nil
		}
		if isConnectionError(err) {
			if refreshedAddr, refreshErr := refreshFixDaemonAddr(ctx); refreshErr == nil {
				currentAddr = refreshedAddr
			} else if shouldStopFixDaemonRetry(ctx) {
				return refreshErr
			}
		}
		if shouldStopFixDaemonRetry(ctx) {
			return ctx.Err()
		}
		fixDaemonSleep(enqueueIfNeededProbeDelay)
	}
	if shouldStopFixDaemonRetry(ctx) {
		return ctx.Err()
	}
	found, err := hasJobForSHAContext(ctx, currentAddr, sha)
	if err == nil && found {
		return nil
	}

	branchName := gitrepo.CurrentBranch(ctx, repoPath)

	reqBody, _ := json.Marshal(daemon.EnqueueRequest{
		RepoPath: repoPath,
		GitRef:   sha,
		Branch:   branchName,
	})

	for attempt := 0; ; attempt++ {
		resp, err := doFixDaemonRequest(ctx, http.MethodPost, currentAddr+"/api/enqueue", reqBody)
		if err == nil {
			defer resp.Body.Close()

			// 200 (skipped) and 201 (enqueued) are both fine
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("enqueue failed: %s", body)
			}
			return nil
		}
		if !isConnectionError(err) || attempt >= fixDaemonMaxRetries {
			return err
		}
		if shouldStopFixDaemonRetry(ctx) {
			return err
		}

		currentAddr, err = recoverFixDaemonAddr(ctx)
		if err != nil {
			return err
		}
		if shouldStopFixDaemonRetry(ctx) {
			return ctx.Err()
		}
		exists, verifyErr := verifyJobForSHAContext(ctx, currentAddr, sha)
		if verifyErr != nil {
			return fmt.Errorf("verify enqueue after retryable failure: %w", verifyErr)
		}
		if exists {
			return nil
		}
	}
}

func refreshFixDaemonAddr(ctx context.Context) (string, error) {
	if shouldStopFixDaemonRetry(ctx) {
		return getDaemonEndpoint().BaseURL(), ctx.Err()
	}
	if err := fixDaemonEnsure(); err != nil {
		return getDaemonEndpoint().BaseURL(), err
	}
	if shouldStopFixDaemonRetry(ctx) {
		return getDaemonEndpoint().BaseURL(), ctx.Err()
	}
	return getDaemonEndpoint().BaseURL(), nil
}

// hasJobForSHA checks if a review job already exists for the given commit SHA.
func hasJobForSHA(serverAddr, sha string) (bool, error) {
	return hasJobForSHAContext(context.Background(), serverAddr, sha)
}

func hasJobForSHAContext(ctx context.Context, serverAddr, sha string) (bool, error) {
	checkURL := fmt.Sprintf("%s/api/jobs?git_ref=%s&limit=1", serverAddr, url.QueryEscape(sha))
	resp, err := doFixDaemonRequest(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var result struct {
		Jobs []struct{ ID int64 } `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, nil
	}
	return len(result.Jobs) > 0, nil
}

func verifyJobForSHAContext(ctx context.Context, serverAddr, sha string) (bool, error) {
	checkURL := fmt.Sprintf("%s/api/jobs?git_ref=%s&limit=1", serverAddr, url.QueryEscape(sha))
	resp, err := doFixDaemonRequest(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("fetch jobs failed (%d): %s", resp.StatusCode, body)
	}
	var result struct {
		Jobs []struct{ ID int64 } `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return len(result.Jobs) > 0, nil
}

func hasJobResponseContext(ctx context.Context, serverAddr string, jobID int64, commenter, response string) (bool, error) {
	checkURL := fmt.Sprintf("%s/api/comments?job_id=%d", serverAddr, jobID)
	resp, err := doFixDaemonRequest(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("fetch comments failed (%d): %s", resp.StatusCode, body)
	}
	var result struct {
		Responses []storage.Response `json:"responses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	for _, existing := range result.Responses {
		if existing.Responder == commenter && existing.Response == response {
			return true, nil
		}
	}
	return false, nil
}

func withFixDaemonRetryContext[T any](ctx context.Context, addr string, fn func(serverAddr string) (T, error)) (T, error) {
	currentAddr := addr
	var zero T

	for attempt := 0; ; attempt++ {
		value, err := fn(currentAddr)
		if err == nil {
			return value, nil
		}
		if shouldStopFixDaemonRetry(ctx) || !isConnectionError(err) || attempt >= fixDaemonMaxRetries {
			return zero, err
		}

		currentAddr, err = recoverFixDaemonAddr(ctx)
		if err != nil {
			return zero, err
		}
		if shouldStopFixDaemonRetry(ctx) {
			return zero, ctx.Err()
		}
	}
}

func shouldStopFixDaemonRetry(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func recoverFixDaemonAddr(ctx context.Context) (string, error) {
	return waitForFixDaemonRecovery(ctx)
}

func waitForFixDaemonRecovery(ctx context.Context) (string, error) {
	deadline := time.Now().Add(fixDaemonRecoveryWait)
	var lastErr error
	for {
		if ctx != nil && ctx.Err() != nil {
			return getDaemonEndpoint().BaseURL(), ctx.Err()
		}
		if err := fixDaemonEnsure(); err == nil {
			return getDaemonEndpoint().BaseURL(), nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return getDaemonEndpoint().BaseURL(), lastErr
			}
			return getDaemonEndpoint().BaseURL(), fmt.Errorf("daemon recovery timed out")
		}
		if ctx != nil && ctx.Err() != nil {
			return getDaemonEndpoint().BaseURL(), ctx.Err()
		}
		fixDaemonSleep(fixDaemonRecoveryPoll)
	}
}

func doFixDaemonRequest(ctx context.Context, method, requestURL string, body []byte) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return getDaemonHTTPClient(30 * time.Second).Do(req)
}
