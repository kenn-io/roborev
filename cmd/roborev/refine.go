package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"
	gitworktree "go.kenn.io/kit/git/worktree"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// postCommitWaitDelay is the delay after creating a commit before checking
// if a review was queued by the post-commit hook. Tests can override this.
var postCommitWaitDelay = 1 * time.Second

// refineOptions groups all CLI parameters for the refine command.
type refineOptions struct {
	agentName         string
	model             string
	reasoning         string
	minSeverity       string
	maxIterations     int
	quiet             bool
	allowUnsafeAgents bool
	unsafeFlagChanged bool
	since             string
	branch            string
	allBranches       bool
	list              bool
	newestFirst       bool
}

func refineCmd() *cobra.Command {
	var (
		opts refineOptions
		fast bool
	)

	cmd := &cobra.Command{
		Use:   "refine",
		Short: "Iterative review-fix loop until all reviews pass",
		Long: `Automatically address failed code reviews in a loop.

Refine finds failed reviews on the current branch, runs an agent to fix
them, commits the changes, then waits for re-review. If the new commit
also fails review, it tries again. Once all per-commit reviews pass, it
runs a branch-level review covering the full commit range and addresses
any findings from that too. The loop continues until everything passes
or --max-iterations is reached.

Unlike 'roborev fix' (which is a single-pass fix with no re-review),
refine is fully automated: it reviews, fixes, re-reviews, and iterates.

The agent runs in an isolated worktree so your working tree is not
modified during the process.

Prerequisites:
- Must be in a git repository with a clean working tree
- Must be on a feature branch (or use --since on the default branch)

Use --since to specify a starting commit when on the main branch or to
limit how far back to look for reviews to address.

Use --list to preview which reviews would be refined without running.
Use --branch to validate the current branch before refining.
Use --all-branches to discover and refine all branches with failed reviews.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --fast is shorthand for --reasoning fast
			opts.reasoning = resolveReasoningWithFast(
				opts.reasoning, fast,
				cmd.Flags().Changed("reasoning"),
			)
			opts.unsafeFlagChanged = cmd.Flags().Changed(
				"allow-unsafe-agents",
			)

			// Flag validation
			if opts.allBranches && opts.branch != "" {
				return fmt.Errorf(
					"--all-branches and --branch are " +
						"mutually exclusive",
				)
			}
			if opts.allBranches && opts.since != "" {
				return fmt.Errorf(
					"--all-branches and --since are " +
						"mutually exclusive",
				)
			}
			if opts.newestFirst && !opts.allBranches && !opts.list {
				return fmt.Errorf(
					"--newest-first requires " +
						"--all-branches or --list",
				)
			}
			if opts.list && opts.since != "" {
				return fmt.Errorf(
					"--list and --since are " +
						"mutually exclusive",
				)
			}

			if opts.list {
				return runRefineList(cmd, opts)
			}
			if opts.allBranches {
				return runRefineAllBranches(cmd, opts)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return runRefine(RunContext{
				Context:    cmd.Context(),
				WorkingDir: cwd,
			}, opts)
		},
	}

	cmd.Flags().StringVar(&opts.agentName, "agent", "", "agent to use for addressing findings (default: from config)")
	cmd.Flags().StringVar(&opts.model, "model", "", "model for agent (format varies: opencode uses provider/model, others use model name)")
	cmd.Flags().StringVar(&opts.reasoning, "reasoning", "", "reasoning level: fast, standard (default), medium, thorough, or maximum")
	cmd.Flags().StringVar(&opts.minSeverity, "min-severity", "", "minimum finding severity to address: critical, high, medium, or low")
	cmd.Flags().BoolVar(&fast, "fast", false, "shorthand for --reasoning fast")
	cmd.Flags().IntVar(&opts.maxIterations, "max-iterations", 10, "maximum refinement iterations")
	cmd.Flags().BoolVar(&opts.quiet, "quiet", false, "suppress agent output, show elapsed time instead")
	cmd.Flags().BoolVar(&opts.allowUnsafeAgents, "allow-unsafe-agents", false, "allow agents to run without sandboxing")
	cmd.Flags().StringVar(&opts.since, "since", "", "base commit to refine from (exclusive, like git's .. range)")
	cmd.Flags().StringVar(&opts.branch, "branch", "", "validate current branch before refining")
	cmd.Flags().BoolVar(&opts.allBranches, "all-branches", false, "discover and refine all branches with failed reviews")
	cmd.Flags().BoolVar(&opts.list, "list", false, "list reviews that would be refined without running")
	cmd.Flags().BoolVar(&opts.newestFirst, "newest-first", false, "process branches/jobs newest first (requires --all-branches or --list)")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)

	return cmd
}

// stepTimer tracks elapsed time for quiet mode display
type stepTimer struct {
	start  time.Time
	stop   chan struct{}
	done   chan struct{}
	prefix string
}

var isTerminal = func(fd uintptr) bool {
	return isatty.IsTerminal(fd)
}

func newStepTimer() *stepTimer {
	return &stepTimer{start: time.Now()}
}

func (t *stepTimer) elapsed() string {
	d := time.Since(t.start)
	return fmt.Sprintf("[%d:%02d]", int(d.Minutes()), int(d.Seconds())%60)
}

// startLive begins a live-updating timer display. Call stopLive() when done.
func (t *stepTimer) startLive(prefix string) {
	t.prefix = prefix
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	t.start = time.Now()

	go func() {
		defer close(t.done)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// Print initial state
		fmt.Printf("\r%s %s", t.prefix, t.elapsed())

		for {
			select {
			case <-t.stop:
				return
			case <-ticker.C:
				fmt.Printf("\r%s %s", t.prefix, t.elapsed())
			}
		}
	}()
}

// stopLive stops the live timer and prints the final elapsed time
func (t *stepTimer) stopLive() {
	if t.stop != nil {
		close(t.stop)
		<-t.done // Wait for goroutine to exit
	}
	// Clear line and print final time with newline
	fmt.Printf("\r%s %s\n", t.prefix, t.elapsed())
}

// validateRefineContext validates git and branch preconditions for refine.
// Returns repoPath, currentBranch, base, mergeBase, or an error.
// base is the ref that HEAD was diverged from — the branch's upstream
// tracking ref when configured, otherwise the repository's default branch.
// If branchFlag is non-empty, validates that the user is on that branch.
// This validation happens before any daemon interaction.
func validateRefineContext(
	ctx context.Context, cwd, since, branchFlag string,
) (repoPath, currentBranch, base, mergeBase string, err error) {
	repoPath, err = gitrepo.Root(ctx, cwd)
	if err != nil {
		return "", "", "", "",
			fmt.Errorf("not in a git repository: %w", err)
	}

	if git.IsRebaseInProgress(repoPath) {
		return "", "", "", "",
			fmt.Errorf(
				"rebase in progress - " +
					"complete or abort it first",
			)
	}

	if !git.IsWorkingTreeClean(repoPath) {
		return "", "", "", "",
			fmt.Errorf(
				"working tree not clean - " +
					"commit or stash your changes first",
			)
	}

	currentBranch = gitrepo.CurrentBranch(ctx, repoPath)

	// --branch: validate the user is on the expected branch
	if branchFlag != "" && currentBranch != branchFlag {
		return "", "", "", "", fmt.Errorf(
			"not on branch %q (currently on %q) -- "+
				"run 'git checkout %s' first",
			branchFlag, currentBranch, branchFlag,
		)
	}

	if since != "" {
		// --since provides an explicit merge base, so upstream/default-branch
		// resolution is unnecessary. Skip it so a misconfigured or unfetched
		// upstream doesn't block an otherwise-valid --since invocation.
		mergeBase, err = gitrepo.Resolve(ctx, repoPath, since)
		if err != nil {
			return "", "", "", "",
				fmt.Errorf(
					"cannot resolve --since %q: %w",
					since, err,
				)
		}
		isAncestor, ancestorErr := gitrepo.IsAncestor(ctx,
			repoPath, mergeBase, "HEAD")

		if ancestorErr != nil {
			return "", "", "", "",
				fmt.Errorf(
					"checking --since ancestry: %w",
					ancestorErr,
				)
		}
		if !isAncestor {
			return "", "", "", "",
				fmt.Errorf(
					"--since %q is not an ancestor of HEAD",
					since,
				)
		}
	} else {
		base = git.GetBranchBase(repoPath, "HEAD")
		if base == "" {
			// Prefer the current branch's upstream tracking ref only when it
			// resolves to a trunk-named branch (e.g., local main tracking
			// upstream/main in a fork). A branch tracking its own remote
			// counterpart is not trunk — use GetDefaultBranch instead.
			upstream, uerr := git.GetUpstream(repoPath, "HEAD")
			var missing *git.UpstreamMissingError
			if errors.As(uerr, &missing) {
				return "", "", "", "",
					fmt.Errorf(
						"%w (run 'git fetch' or pass --since)", missing,
					)
			}
			if uerr != nil {
				return "", "", "", "",
					fmt.Errorf(
						"resolve upstream for HEAD: %w (pass --since to skip)", uerr,
					)
			}
			if upstream != "" && git.UpstreamIsTrunk(repoPath, "HEAD") {
				base = upstream
			}
		}
		if base == "" {
			base, err = gitrepo.DefaultBranch(ctx, repoPath)
			if err != nil {
				return "", "", "", "",
					fmt.Errorf(
						"cannot determine default branch: %w", err,
					)
			}
		}

		if git.IsOnBaseBranch(repoPath, currentBranch, base) {
			return "", "", "", "", fmt.Errorf(
				"refusing to refine on %s branch "+
					"without --since flag",
				currentBranch,
			)
		}

		mergeBase, err = git.GetMergeBase(
			repoPath, base, "HEAD",
		)
		if err != nil {
			return "", "", "", "",
				fmt.Errorf(
					"cannot find merge-base with %s: %w",
					base, err,
				)
		}
	}

	return repoPath, currentBranch, base, mergeBase, nil
}

// RunContext encapsulates the runtime context for the refine command,
// allowing tests to override the working directory and polling interval.
type RunContext struct {
	Context         context.Context
	WorkingDir      string
	PollInterval    time.Duration
	PostCommitDelay time.Duration
}

func runRefine(runCtx RunContext, opts refineOptions) error {
	ctx := runCtx.Context
	if ctx == nil {
		return fmt.Errorf("run refine: missing context")
	}

	// 1. Validate git and branch context (before touching daemon)
	repoPath, currentBranch, base, mergeBase, err := validateRefineContext(
		ctx, runCtx.WorkingDir, opts.since, opts.branch,
	)
	if err != nil {
		return err
	}

	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve config-driven workflow settings before touching the daemon so
	// malformed repo config fails during validation, not after startup.
	resolvedReasoning, err := config.ResolveRefineReasoning(
		opts.reasoning, repoPath, cfg,
	)
	if err != nil {
		return err
	}
	reasoningLevel := agent.ParseReasoningLevel(resolvedReasoning)
	if err := config.ValidateRepoConfig(repoPath); err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}
	metadata, err := config.ResolveFixCommitMetadata(repoPath, cfg)
	if err != nil {
		return fmt.Errorf("resolve fix commit metadata: %w", err)
	}
	commitOpts := git.CommitOptions{
		Author:    metadata.Author,
		CoAuthors: metadata.CoAuthors,
	}
	resolution, err := agent.ResolveWorkflowConfig(
		opts.agentName, repoPath, cfg, "refine", resolvedReasoning,
	)
	if err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}

	// 2. Connect to daemon (only after all validation passes)
	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}

	client, err := daemon.NewHTTPClientFromRuntime()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	if runCtx.PollInterval > 0 {
		client.SetPollInterval(runCtx.PollInterval)
	}

	// Determine delays
	commitWaitDelay := postCommitWaitDelay
	if runCtx.PostCommitDelay > 0 {
		commitWaitDelay = runCtx.PostCommitDelay
	}

	// Print branch context after successful connection
	if opts.since != "" {
		fmt.Printf("Refining commits since %s on branch %q\n", gitrepo.ShortSHA(mergeBase), currentBranch)
	} else {
		fmt.Printf("Refining branch %q (diverged from %s at %s)\n", currentBranch, base, gitrepo.ShortSHA(mergeBase))
	}

	allowUnsafe := resolveAllowUnsafeAgents(opts.allowUnsafeAgents, opts.unsafeFlagChanged, cfg)
	agent.SetAllowUnsafeAgents(allowUnsafe)
	if cfg != nil {
		agent.SetAnthropicAPIKey(cfg.AnthropicAPIKey)
	}

	// Get the agent with configured reasoning level (model applied after
	// backup determination to avoid baking the primary model into a
	// backup agent).
	addressAgent, err := selectRefineAgent(
		repoPath, cfg, resolution.PreferredAgent, reasoningLevel, resolution.BackupAgent,
	)
	if err != nil {
		return fmt.Errorf("no agent available: %w", err)
	}
	addressAgent, _ = applyModelForAgent(
		addressAgent, resolution.PreferredAgent, resolution.BackupAgent,
		opts.model, repoPath, cfg, "refine", resolvedReasoning,
	)
	fmt.Printf("Using agent: %s\n", addressAgent.Name())

	// Resolve minimum severity filter
	minSev, err := config.ResolveRefineMinSeverity(
		opts.minSeverity, repoPath, cfg,
	)
	if err != nil {
		return fmt.Errorf("resolve min-severity: %w", err)
	}

	// 3. Refinement loop
	// Track current failed review - when a fix fails, we continue fixing it
	// before moving on to the next oldest failed commit
	var currentFailedReview *storage.Review
	// Track reviews we've given up on this run to avoid re-selecting them
	skippedReviews := make(map[int64]bool)

	for iteration := 1; iteration <= opts.maxIterations; {
		// Get commits on current branch
		commits, err := git.GetCommitsSince(repoPath, mergeBase)
		if err != nil {
			return fmt.Errorf("cannot get commits: %w", err)
		}

		if len(commits) == 0 {
			fmt.Println("No commits on branch - nothing to refine")
			return nil
		}

		// Only search for a new failed review if we don't have one to work on
		// (either first iteration, or previous fix passed)
		if currentFailedReview == nil {
			currentFailedReview, err = findFailedReviewForBranch(client, commits, skippedReviews)
			if err != nil {
				return fmt.Errorf("error finding reviews: %w", err)
			}
		}

		if currentFailedReview == nil {
			// Check for pending jobs before triggering a branch review
			pendingJob, err := findPendingJobForBranch(ctx, client, repoPath, commits)
			if err != nil {
				return fmt.Errorf("error checking pending jobs: %w", err)
			}
			if pendingJob != nil {
				// Wait for the pending job to complete, then loop back to check its result
				// This does NOT consume an iteration - we only count actual fix attempts
				fmt.Printf("Waiting for in-progress review (job %d)...\n", pendingJob.ID)
				review, err := client.WaitForReview(pendingJob.ID)
				if err != nil {
					fmt.Printf("Warning: review failed: %v\n", err)
					continue // Loop back, will re-check
				}
				verdict := storage.ParseVerdict(review.Output)
				if verdict == "F" && !review.Closed {
					currentFailedReview = review
				} else if verdict == "P" {
					if err := client.MarkReviewClosed(review.JobID); err != nil {
						fmt.Printf("Warning: failed to close review (job %d): %v\n", review.JobID, err)
					}
					continue // Loop back to check for more
				}
				// If we have a failed review now, fall through to address it
				// Otherwise loop back
				if currentFailedReview == nil {
					continue
				}
			} else {
				// No pending commit jobs and no failed reviews - check for branch review
				// Resolve HEAD to SHA to ensure stable rangeRef (avoids stale results if HEAD moves)
				headSHA, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
				if err != nil {
					return fmt.Errorf("cannot resolve HEAD: %w", err)
				}
				rangeRef := mergeBase + ".." + headSHA

				// Check if a branch review job already exists (queued or running).
				// Note: We don't filter by agent here because the --agent flag controls
				// the ADDRESSING agent (which fixes code), not the REVIEW agent.
				// We use the SHA-based rangeRef to ensure we only reuse jobs for the
				// exact same HEAD - if HEAD has moved, we want a fresh review.
				existingJob, err := client.FindPendingJobForRef(ctx, repoPath, rangeRef)
				if err != nil {
					return fmt.Errorf("error checking for existing branch review: %w", err)
				}

				var jobID int64
				if existingJob != nil {
					// Wait for existing pending branch review
					fmt.Printf("Waiting for in-progress branch review (job %d)...\n", existingJob.ID)
					jobID = existingJob.ID
				} else {
					// No pending branch review - enqueue a new one
					fmt.Println("No individual failed reviews - running branch review...")
					jobID, err = client.EnqueueReview(repoPath, rangeRef, "")
					if err != nil {
						return fmt.Errorf("failed to enqueue branch review: %w", err)
					}
					fmt.Printf("Waiting for branch review (job %d)...\n", jobID)
				}

				review, err := client.WaitForReview(jobID)
				if err != nil {
					return fmt.Errorf("branch review failed: %w", err)
				}

				verdict := storage.ParseVerdict(review.Output)
				if verdict == "P" {
					fmt.Println("\nAll reviews passed! Branch is ready.")
					return nil
				}

				// Branch review failed - address its findings
				fmt.Printf("\nBranch review failed. Addressing findings...\n")
				currentFailedReview = review
			}
		}

		// Now we have a review to address - this counts as an iteration
		fmt.Printf("\n=== Refinement iteration %d/%d ===\n", iteration, opts.maxIterations)
		iteration++

		// Address the failed review
		liveTimer := opts.quiet && isTerminal(os.Stdout.Fd())
		if !opts.quiet {
			fmt.Printf("Addressing review (job %d)...\n", currentFailedReview.JobID)
		}

		// Get previous attempts for context (including legacy commit-based)
		var reviewCommitID int64
		var reviewGitRef string
		if currentFailedReview.Job != nil {
			reviewCommitID, reviewGitRef = currentFailedReview.Job.LegacyCommentLookupTarget()
		}
		previousAttempts, err := client.GetAllCommentsForJob(currentFailedReview.JobID, reviewCommitID, reviewGitRef)
		if err != nil {
			return fmt.Errorf("fetch previous comments: %w", err)
		}

		// Build address prompt
		builder := prompt.NewBuilderWithConfig(nil, cfg).ForRepo(repoPath, 0)
		addressPrompt, err := builder.BuildAddressPrompt(currentFailedReview, previousAttempts, minSev)
		if err != nil {
			return fmt.Errorf("build address prompt: %w", err)
		}

		// Record pre-agent state for safety checks
		wasCleanBefore := git.IsWorkingTreeClean(repoPath)
		headBefore, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
		if err != nil {
			return fmt.Errorf("cannot determine HEAD: %w", err)
		}
		branchBefore := gitrepo.CurrentBranch(ctx, repoPath)

		// Create temp worktree to isolate agent from user's working tree
		wt, err := gitworktree.Create(ctx, repoPath, "HEAD", gitworktree.Options{
			Prefix:         "roborev-worktree-",
			InitSubmodules: true,
			PullLFS:        true,
		})
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}
		worktreePath := wt.Dir
		// NOTE: not using defer here because we're inside a loop;
		// defer wouldn't run until runRefine returns, leaking worktrees.
		// Instead, wt.Close() is called explicitly before every exit point.

		// Determine output writer
		var agentOutput io.Writer
		var fmtr *streamfmt.Formatter
		if opts.quiet {
			agentOutput = io.Discard
		} else {
			fmtr = streamfmt.New(os.Stdout, isTerminal(os.Stdout.Fd()))
			agentOutput = fmtr
		}

		// Run agent in isolated worktree (1 hour timeout)
		timer := newStepTimer()
		if liveTimer {
			timer.startLive(fmt.Sprintf("Addressing review (job %d)...", currentFailedReview.JobID))
		}
		fixCtx, fixCancel := context.WithTimeout(ctx, 1*time.Hour)
		output, agentErr := addressAgent.Review(fixCtx, worktreePath, "HEAD", addressPrompt, agentOutput)
		fixCancel()
		if fmtr != nil {
			fmtr.Flush()
		}

		// Show elapsed time
		if liveTimer {
			timer.stopLive()
		} else if opts.quiet {
			fmt.Printf("Addressing review (job %d)... %s\n", currentFailedReview.JobID, timer.elapsed())
		} else {
			fmt.Printf("Agent completed %s\n", timer.elapsed())
		}

		// Safety checks on main repo (before applying any changes)
		if wasCleanBefore && !git.IsWorkingTreeClean(repoPath) {
			_ = wt.Close(ctx)
			return fmt.Errorf("working tree changed during refine - aborting to prevent data loss")
		}
		headAfterAgent, resolveErr := gitrepo.Resolve(ctx, repoPath, "HEAD")
		if resolveErr != nil {
			_ = wt.Close(ctx)
			return fmt.Errorf("cannot determine HEAD after agent run: %w", resolveErr)
		}
		branchAfterAgent := gitrepo.CurrentBranch(ctx, repoPath)
		if headAfterAgent != headBefore || branchAfterAgent != branchBefore {
			_ = wt.Close(ctx)
			return fmt.Errorf("HEAD changed during refine (was %s on %s, now %s on %s) - aborting to prevent applying patch to wrong commit",
				gitrepo.ShortSHA(headBefore), branchBefore, gitrepo.ShortSHA(headAfterAgent), branchAfterAgent)
		}

		if agentErr != nil {
			_ = wt.Close(ctx)
			fmt.Printf("Agent error: %v\n", agentErr)
			fmt.Println("Will retry in next iteration")
			continue
		}

		// Check if agent made changes in worktree
		if git.IsWorkingTreeClean(worktreePath) {
			_ = wt.Close(ctx)

			// When severity filtering is active and the agent
			// signals all findings are below threshold, treat as
			// resolved rather than a fix failure. Require the
			// marker to stand alone so prose findings echoed
			// alongside it cannot silently close the review.
			if minSev != "" && config.IsMarkerOnlyOutput(output) {
				fmt.Println(
					"All findings below severity " +
						"threshold - closing review",
				)
				if err := client.MarkReviewClosed(
					currentFailedReview.JobID,
				); err != nil {
					fmt.Printf(
						"Warning: failed to close "+
							"review (job %d): %v\n",
						currentFailedReview.JobID, err,
					)
				}
				currentFailedReview = nil
				continue
			}

			fmt.Println("Agent made no changes - skipping this review")
			if err := client.AddComment(currentFailedReview.JobID, "roborev-refine", "Agent could not determine how to address findings"); err != nil {
				fmt.Printf("Warning: failed to add comment to job %d: %v\n", currentFailedReview.JobID, err)
			}
			skippedReviews[currentFailedReview.ID] = true
			currentFailedReview = nil
			continue
		}

		// Capture patch from worktree and apply to main repo
		patch, err := wt.CapturePatch(ctx)
		if err != nil {
			_ = wt.Close(ctx)
			return fmt.Errorf("capture worktree patch: %w", err)
		}
		if err := gitworktree.ApplyPatch(ctx, repoPath, patch); err != nil {
			_ = wt.Close(ctx)
			return fmt.Errorf("apply worktree patch: %w", err)
		}
		_ = wt.Close(ctx)

		commitMsg := fmt.Sprintf("Address review findings (job %d)\n\n%s", currentFailedReview.JobID, summarizeAgentOutput(output))
		newCommit, err := commitWithHookRetry(ctx, repoPath, commitMsg, addressAgent, opts.quiet, commitOpts)
		if err != nil {
			return fmt.Errorf("failed to commit changes: %w", err)
		}
		fmt.Printf("Created commit %s\n", gitrepo.ShortSHA(newCommit))

		// Add response recording what was done
		responseText := fmt.Sprintf("Created commit %s to address findings\n\n%s", gitrepo.ShortSHA(newCommit), output)
		if err := client.AddComment(currentFailedReview.JobID, "roborev-refine", responseText); err != nil {
			fmt.Printf("Warning: failed to add comment to job %d: %v\n", currentFailedReview.JobID, err)
		}

		// Close old review
		if err := client.MarkReviewClosed(currentFailedReview.JobID); err != nil {
			fmt.Printf("Warning: failed to close review (job %d): %v\n", currentFailedReview.JobID, err)
		}

		// Wait for new commit to be reviewed
		time.Sleep(commitWaitDelay)

		newJob, err := client.FindJobForCommit(ctx, repoPath, newCommit)
		if err != nil || newJob == nil {
			currentFailedReview = nil
			continue
		}

		fmt.Printf("Waiting for review of new commit (job %d)...\n", newJob.ID)
		review, err := client.WaitForReview(newJob.ID)
		if err != nil {
			fmt.Printf("Warning: review failed: %v\n", err)
			currentFailedReview = nil
			continue
		}

		verdict := storage.ParseVerdict(review.Output)
		if verdict == "P" {
			fmt.Println("New commit passed review!")
			if err := client.MarkReviewClosed(review.JobID); err != nil {
				fmt.Printf("Warning: failed to close review (job %d): %v\n", review.JobID, err)
			}
			currentFailedReview = nil
		} else {
			fmt.Println("New commit failed review - continuing to address")
			currentFailedReview = review
		}
	}

	return fmt.Errorf("max iterations (%d) reached without all reviews passing", opts.maxIterations)
}

// runRefineList lists reviews that would be refined, without running.
// Filters to failed verdicts only (refine only cares about failures).
func runRefineList(
	cmd *cobra.Command, opts refineOptions,
) error {
	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	ctx := cmd.Context()

	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Use the current worktree root for branch detection (so linked
	// worktrees resolve their own branch, not the main worktree's).
	// Use the main repo root for daemon API queries (jobs are stored
	// under the main repo path).
	worktreeRoot, err := gitrepo.Root(ctx, workDir)
	if err != nil {
		return fmt.Errorf("not in a git repository: %w", err)
	}
	apiRoot := worktreeRoot
	if root, err := gitrepo.MainRoot(ctx, workDir); err == nil {
		apiRoot = root
	}

	// Determine effective branch filter
	effectiveBranch := opts.branch
	if !opts.allBranches && effectiveBranch == "" {
		effectiveBranch = gitrepo.CurrentBranch(ctx, worktreeRoot)
	}

	// Empty string for allBranches means no branch filter
	queryBranch := effectiveBranch
	if opts.allBranches {
		queryBranch = ""
	}

	jobs, err := queryOpenJobs(ctx, apiRoot, queryBranch)
	if err != nil {
		return err
	}

	// Filter to failed verdicts only
	var failed []storage.ReviewJob
	for _, j := range jobs {
		if j.Verdict != nil && *j.Verdict == "F" {
			failed = append(failed, j)
		}
	}

	// Reverse for oldest-first by default (API returns newest first)
	if !opts.newestFirst {
		for i, j := 0, len(failed)-1; i < j; i, j = i+1, j-1 {
			failed[i], failed[j] = failed[j], failed[i]
		}
	}

	if len(failed) == 0 {
		cmd.Println("No failed reviews to refine.")
		return nil
	}

	cmd.Printf("Found %d failed review(s) to refine:\n\n", len(failed))

	refineListAddr := getDaemonEndpoint().BaseURL()
	for _, job := range failed {
		review, err := fetchReview(ctx, refineListAddr, job.ID)
		if err != nil {
			fmt.Fprintf(
				cmd.ErrOrStderr(),
				"Warning: could not fetch review for job %d: %v\n",
				job.ID, err,
			)
			continue
		}

		cmd.Printf("Job #%d\n", job.ID)
		cmd.Printf("  Git Ref:  %s\n", gitrepo.ShortSHA(job.GitRef))
		if job.Branch != "" {
			cmd.Printf("  Branch:   %s\n", job.Branch)
		}
		if job.CommitSubject != "" {
			cmd.Printf(
				"  Subject:  %s\n",
				truncateString(job.CommitSubject, 60),
			)
		}
		cmd.Printf("  Agent:    %s\n", job.Agent)
		if job.Model != "" {
			cmd.Printf("  Model:    %s\n", job.Model)
		}
		if job.FinishedAt != nil {
			cmd.Printf(
				"  Finished: %s\n",
				job.FinishedAt.Local().Format("2006-01-02 15:04:05"),
			)
		}
		summary := firstLine(review.Output)
		if summary != "" {
			cmd.Printf("  Summary:  %s\n", summary)
		}
		cmd.Println()
	}

	cmd.Println("To refine: roborev refine")
	return nil
}

// runRefineAllBranches discovers all branches with failed reviews and
// refines each one in sequence, checking out each branch in turn.
// The user's explicit --all-branches flag serves as confirmation for
// branch switching.
func runRefineAllBranches(
	cmd *cobra.Command, opts refineOptions,
) error {
	ctx := cmd.Context()

	repoPath, err := gitrepo.Root(ctx, ".")
	if err != nil {
		return fmt.Errorf("not in a git repository: %w", err)
	}

	if git.IsRebaseInProgress(repoPath) {
		return fmt.Errorf(
			"rebase in progress - complete or abort it first",
		)
	}
	if !git.IsWorkingTreeClean(repoPath) {
		return fmt.Errorf(
			"working tree not clean - " +
				"commit or stash your changes first",
		)
	}

	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	// Use main repo root for API queries
	apiRepoRoot := repoPath
	if root, err := gitrepo.MainRoot(ctx, repoPath); err == nil {
		apiRepoRoot = root
	}

	originalBranch := gitrepo.CurrentBranch(ctx, repoPath)
	var originalHEAD string
	if originalBranch == "" {
		if gitrepo.IsUnbornHead(ctx, repoPath) {
			return fmt.Errorf(
				"cannot run --all-branches from an unborn HEAD " +
					"(no commits on current branch)",
			)
		}
		// Detached HEAD — capture SHA for restore after processing.
		originalHEAD, err = gitrepo.Resolve(ctx, repoPath, "HEAD")
		if err != nil {
			return fmt.Errorf("cannot resolve HEAD: %w", err)
		}
	}

	// Query all open jobs (no branch filter)
	jobs, err := queryOpenJobs(ctx, apiRepoRoot, "")
	if err != nil {
		return err
	}

	// Track the newest failed-job timestamp per branch so we can
	// sort by recency rather than alphabetically.
	branchNewest := make(map[string]time.Time)
	for _, j := range jobs {
		if j.Branch == "" || j.Verdict == nil || *j.Verdict != "F" {
			continue
		}
		var ts time.Time
		if j.FinishedAt != nil {
			ts = *j.FinishedAt
		}
		if cur, ok := branchNewest[j.Branch]; !ok || ts.After(cur) {
			branchNewest[j.Branch] = ts
		}
	}

	if len(branchNewest) == 0 {
		fmt.Println("No branches with failed reviews found.")
		return nil
	}

	branches := make([]string, 0, len(branchNewest))
	for b := range branchNewest {
		branches = append(branches, b)
	}

	// Default: oldest branch first (by newest failed job).
	// --newest-first reverses to newest branch first.
	sort.Slice(branches, func(i, j int) bool {
		ti := branchNewest[branches[i]]
		tj := branchNewest[branches[j]]
		if opts.newestFirst {
			return ti.After(tj)
		}
		return ti.Before(tj)
	})

	fmt.Printf(
		"Found %d branch(es) with failed reviews: %s\n",
		len(branches), strings.Join(branches, ", "),
	)

	var failedBranches []string

	for _, b := range branches {
		fmt.Printf("\n=== Refining branch %q ===\n", b)

		if err := git.CheckoutBranch(repoPath, b); err != nil {
			fmt.Printf(
				"Warning: cannot checkout %q: %v (skipping)\n",
				b, err,
			)
			failedBranches = append(failedBranches, b)
			continue
		}

		branchOpts := opts
		branchOpts.branch = b
		branchOpts.allBranches = false

		if err := runRefine(RunContext{
			Context:    ctx,
			WorkingDir: repoPath,
		}, branchOpts); err != nil {
			fmt.Printf(
				"Warning: refine on %q: %v\n", b, err,
			)
			failedBranches = append(failedBranches, b)
			// Reset dirty tree so the next checkout can succeed
			if !git.IsWorkingTreeClean(repoPath) {
				if resetErr := git.ResetWorkingTree(repoPath); resetErr != nil {
					fmt.Printf(
						"Warning: reset working tree: %v\n",
						resetErr,
					)
				}
			}
		}
	}

	// Restore original branch (or detached HEAD)
	if originalBranch != "" {
		if err := git.CheckoutBranch(repoPath, originalBranch); err != nil {
			return fmt.Errorf(
				"cannot restore original branch %q: %w",
				originalBranch, err,
			)
		}
		fmt.Printf("\nRestored to branch %q\n", originalBranch)
	} else if originalHEAD != "" {
		// Detached HEAD — restore to the original commit
		if err := git.CheckoutBranch(repoPath, originalHEAD); err != nil {
			return fmt.Errorf(
				"cannot restore detached HEAD %s: %w",
				gitrepo.ShortSHA(originalHEAD), err,
			)
		}
		fmt.Printf(
			"\nRestored to detached HEAD at %s\n",
			gitrepo.ShortSHA(originalHEAD),
		)
	}

	if len(failedBranches) > 0 {
		return fmt.Errorf(
			"refine failed on %d branch(es): %s",
			len(failedBranches),
			strings.Join(failedBranches, ", "),
		)
	}

	return nil
}

// resolveAllowUnsafeAgents determines whether to allow unsafe agents.
// Priority: CLI flag > config file > default (true for refine).
// Refine defaults to true because it fundamentally requires file modifications.
// Users can disable with --allow-unsafe-agents=false or config if they want (though refine won't work).
func resolveAllowUnsafeAgents(flag bool, flagChanged bool, cfg *config.Config) bool {
	// If user explicitly set the CLI flag, honor their choice
	if flagChanged {
		return flag
	}
	// If config file explicitly sets allow_unsafe_agents, honor it
	if cfg != nil && cfg.AllowUnsafeAgents != nil {
		return *cfg.AllowUnsafeAgents
	}
	// Default to true for refine - it can't work without file modifications
	return true
}

// findFailedReviewForBranch finds an open failed review for any of the given commits.
// Iterates oldest to newest so earlier commits are fixed before later ones.
// Passing reviews are closed automatically.
// Reviews in the skip set are ignored (used for reviews we've given up on this run).
func findFailedReviewForBranch(client daemon.Client, commits []string, skip map[int64]bool) (*storage.Review, error) {
	// Iterate oldest to newest (commits are in chronological order)
	for _, sha := range commits {
		review, err := client.GetReviewBySHA(sha)
		if err != nil {
			return nil, fmt.Errorf("fetching review for %s: %w", gitrepo.ShortSHA(sha), err)
		}
		if review == nil {
			continue
		}

		// Skip already closed reviews
		if review.Closed {
			continue
		}

		// Skip reviews we've given up on this run
		if skip[review.ID] {
			continue
		}

		verdict := storage.ParseVerdict(review.Output)
		if verdict == "F" {
			return review, nil
		}

		// Close passing reviews so they don't need to be checked again
		if verdict == "P" {
			if err := client.MarkReviewClosed(review.JobID); err != nil {
				return nil, fmt.Errorf("closing review (job %d): %w", review.JobID, err)
			}
		}
	}

	return nil, nil
}

// findPendingJobForBranch finds a queued or running job for any of the given commits.
// Returns the first pending job found (oldest commit first), or nil if all jobs are complete.
func findPendingJobForBranch(ctx context.Context, client daemon.Client, repoPath string, commits []string) (*storage.ReviewJob, error) {
	for _, sha := range commits {
		job, err := client.FindJobForCommit(ctx, repoPath, sha)
		if err != nil {
			return nil, err
		}
		if job == nil {
			continue
		}
		// Check if job is still pending (queued or running)
		if job.Status == storage.JobStatusQueued || job.Status == storage.JobStatusRunning {
			return job, nil
		}
	}
	return nil, nil
}

// summarizeAgentOutput extracts a short summary from agent output
func summarizeAgentOutput(output string) string {
	lines := strings.Split(output, "\n")
	// Take first non-empty lines as summary
	var summary []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			summary = append(summary, line)
			if len(summary) >= 10 {
				break
			}
		}
	}
	if len(summary) == 0 {
		return "Automated fix"
	}
	return strings.Join(summary, "\n")
}

// commitWithHookRetry attempts git.CreateCommit and, on failure,
// runs the agent to fix whatever the hook complained about. Only
// retries when a hook (pre-commit, commit-msg, etc.) caused the
// failure — other commit failures (missing identity, empty commit,
// lockfile) are returned immediately. Retries up to 3 total attempts.
func commitWithHookRetry(
	ctx context.Context,
	repoPath, commitMsg string,
	fixAgent agent.Agent,
	quiet bool,
	commitOpts git.CommitOptions,
) (string, error) {
	const maxAttempts = 3

	expectedHead, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot determine HEAD: %w", err)
	}
	expectedBranch := gitrepo.CurrentBranch(ctx, repoPath)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sha, err := git.CreateCommitWithOptions(repoPath, commitMsg, commitOpts)
		if err == nil {
			return sha, nil
		}

		// Only retry when a hook positively caused the failure.
		// Add-phase errors, non-hook commit errors, and commit
		// failures without hooks are returned immediately.
		var commitErr *git.CommitError
		if !errors.As(err, &commitErr) || !commitErr.HookFailed {
			return "", err
		}

		if attempt == maxAttempts {
			return "", fmt.Errorf(
				"hook failed after %d attempts: %w",
				maxAttempts, err,
			)
		}

		hookErr := err.Error()
		if !quiet {
			fmt.Printf(
				"Hook failed (attempt %d/%d), "+
					"running agent to fix:\n%s\n",
				attempt, maxAttempts, hookErr,
			)
		}

		if err := verifyRepoState(
			ctx, repoPath, expectedHead, expectedBranch,
		); err != nil {
			return "", fmt.Errorf(
				"aborting hook retry: %w", err,
			)
		}

		fixPrompt := fmt.Sprintf(
			"A git hook rejected this commit with the "+
				"following error output. Fix the issues so "+
				"the commit can succeed.\n\n%s",
			hookErr,
		)

		var agentOutput io.Writer
		if quiet {
			agentOutput = io.Discard
		} else {
			agentOutput = os.Stdout
		}

		fixCtx, cancel := context.WithTimeout(
			ctx, 5*time.Minute,
		)
		_, agentErr := fixAgent.Review(
			fixCtx, repoPath, "HEAD", fixPrompt, agentOutput,
		)
		cancel()

		if agentErr != nil {
			return "", fmt.Errorf(
				"agent failed to fix hook issues: %w", agentErr,
			)
		}

		if err := verifyRepoState(
			ctx, repoPath, expectedHead, expectedBranch,
		); err != nil {
			return "", fmt.Errorf(
				"agent changed repo state during hook fix: %w",
				err,
			)
		}
	}

	// unreachable, but satisfies the compiler
	return "", fmt.Errorf("commit retry loop exited unexpectedly")
}

// verifyRepoState checks that HEAD and current branch match expected
// values. Returns an error describing the drift if they don't.
func verifyRepoState(
	ctx context.Context, repoPath, expectedHead, expectedBranch string,
) error {
	currentHead, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
	if err != nil {
		return fmt.Errorf("cannot verify HEAD: %w", err)
	}
	currentBranch := gitrepo.CurrentBranch(ctx, repoPath)
	if currentHead != expectedHead ||
		currentBranch != expectedBranch {
		return fmt.Errorf(
			"HEAD was %s on %s, now %s on %s",
			gitrepo.ShortSHA(expectedHead), expectedBranch,
			gitrepo.ShortSHA(currentHead), currentBranch,
		)
	}
	return nil
}

func selectRefineAgent(repoPath string, cfg *config.Config, resolvedAgent string, reasoningLevel agent.ReasoningLevel, backups ...string) (agent.Agent, error) {
	baseAgent, err := agent.GetPreferredOrBackupWithConfig(repoPath, resolvedAgent, cfg, backups...)
	if err != nil {
		return nil, err
	}
	return baseAgent.WithReasoning(reasoningLevel), nil
}

// applyModelForAgent resolves the correct model for the selected agent
// and applies it. When the selected agent is the configured backup (not
// the preferred primary), the backup model is used instead of the
// primary model. Returns the agent with the model applied (if any) and
// the resolved model string.
func applyModelForAgent(
	a agent.Agent,
	preferredAgent string,
	backupAgentName string,
	cliModel string,
	repoPath string,
	cfg *config.Config,
	workflow string,
	reasoning string,
) (agent.Agent, string) {
	resolution := agent.WorkflowConfig{
		RepoPath:       repoPath,
		GlobalConfig:   cfg,
		Workflow:       workflow,
		Reasoning:      reasoning,
		PreferredAgent: preferredAgent,
		BackupAgent:    backupAgentName,
	}
	model := resolution.ModelForSelectedAgent(a.Name(), cliModel)

	if model != "" {
		a = a.WithModel(model)
	}
	return a, model
}
