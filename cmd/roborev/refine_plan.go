package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"
	gitworktree "go.kenn.io/kit/git/worktree"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// refineReviewIncludes tests whether a re-review covers the last implemented fix.
func refineReviewIncludes(ctx context.Context, repoPath string, review *storage.Review, sha string) bool {
	if review.Job == nil {
		return false
	}
	endpoint, ok := refinePlanReviewEndpoint(review.Job.GitRef)
	if !ok {
		return false
	}
	resolved, err := gitrepo.Resolve(ctx, repoPath, endpoint)
	if err != nil {
		return false
	}
	includes, err := gitrepo.IsAncestor(ctx, repoPath, sha, resolved)
	return err == nil && includes
}

// runRefinePlanOnly takes one snapshot without closing or queueing reviews.
// Other branches are analyzed in detached worktrees, never checked out here.
func runRefinePlanOnly(cmd *cobra.Command, opts refineOptions) error {
	ctx := cmd.Context()
	repoPath, err := gitrepo.Root(ctx, ".")
	if err != nil {
		return err
	}
	var mergeBase string
	if opts.allBranches {
		if git.IsRebaseInProgress(repoPath) || !git.IsWorkingTreeClean(repoPath) {
			return fmt.Errorf("planning requires a clean working tree without a rebase")
		}
	} else {
		repoPath, _, _, mergeBase, err = validateRefineContext(ctx, repoPath, opts.since, opts.branch)
		if err != nil {
			return err
		}
	}
	cfg, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	var reasoning, minSev string
	if !opts.allBranches {
		if err := config.ValidateRepoConfig(repoPath); err != nil {
			return err
		}
		reasoning, err = config.ResolveRefineReasoning(opts.reasoning, repoPath, cfg)
		if err != nil {
			return err
		}
		minSev, err = config.ResolveRefineMinSeverity(opts.minSeverity, repoPath, cfg)
		if err != nil {
			return err
		}
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	agent.SetAnthropicAPIKey(cfg.AnthropicAPIKey)
	apiRoot := repoPath
	if root, e := gitrepo.MainRoot(ctx, repoPath); e == nil {
		apiRoot = root
	}
	branch := gitrepo.CurrentBranch(ctx, repoPath)
	queryBranch := branch
	if opts.allBranches {
		queryBranch = ""
	}
	jobs, err := queryOpenJobs(ctx, apiRoot, queryBranch)
	if err != nil {
		return err
	}
	eligible := jobs[:0]
	for _, job := range jobs {
		if job.Status == storage.JobStatusDone && job.Verdict != nil && *job.Verdict == "F" {
			eligible = append(eligible, job)
		}
	}
	jobs = eligible
	if len(jobs) == 0 {
		cmd.Println("No failed reviews to plan.")
		return nil
	}
	if !opts.newestFirst {
		for i, j := 0, len(jobs)-1; i < j; i, j = i+1, j-1 {
			jobs[i], jobs[j] = jobs[j], jobs[i]
		}
	}
	planned := 0
	for _, job := range jobs {
		target := "HEAD"
		if opts.allBranches {
			if job.Branch == "" {
				continue
			}
			target = "refs/heads/" + job.Branch
			resolved, resolveErr := gitrepo.Resolve(ctx, repoPath, target)
			if resolveErr != nil {
				continue
			}
			target = resolved
			reviewedRef, ok := refinePlanReviewEndpoint(job.GitRef)
			if !ok {
				continue
			}
			reviewedSHA, resolveErr := gitrepo.Resolve(ctx, repoPath, reviewedRef)
			if resolveErr != nil {
				continue
			}
			onTargetBranch, ancestryErr := gitrepo.IsAncestor(ctx, repoPath, reviewedSHA, target)
			if ancestryErr != nil {
				return ancestryErr
			}
			if !onTargetBranch {
				continue
			}
		}
		if !opts.allBranches {
			endpoint, ok := refinePlanReviewEndpoint(job.GitRef)
			if !ok {
				continue
			}
			sha, e := gitrepo.Resolve(ctx, repoPath, endpoint)
			if e != nil {
				continue
			}
			afterBase, e := gitrepo.IsAncestor(ctx, repoPath, mergeBase, sha)
			if e != nil {
				return e
			}
			onBranch, e := gitrepo.IsAncestor(ctx, repoPath, sha, "HEAD")
			if e != nil {
				return e
			}
			if sha == mergeBase || !afterBase || !onBranch {
				continue
			}
		}
		review, e := fetchReview(ctx, getDaemonEndpoint().BaseURL(), job.ID)
		if e != nil {
			return e
		}
		review.Job = &job
		commitID, lookupRef := job.LegacyCommentLookupTarget()
		comments, e := fetchComments(ctx, getDaemonEndpoint().BaseURL(), job.ID, commitID, lookupRef)
		if e != nil {
			return e
		}
		e = planRefineReviewAtTarget(ctx, cmd, repoPath, target, opts, cfg, reasoning, minSev, job, review, comments)
		if e != nil {
			return e
		}
		planned++
	}
	if planned == 0 {
		cmd.Println("No failed reviews to plan.")
	}
	return nil
}

func planRefineReviewAtTarget(
	ctx context.Context,
	cmd *cobra.Command,
	repoPath, target string,
	opts refineOptions,
	cfg *config.Config,
	reasoning, minSeverity string,
	job storage.ReviewJob,
	review *storage.Review,
	comments []storage.Response,
) (err error) {
	planningRepoPath := repoPath
	if opts.allBranches {
		worktree, createErr := gitworktree.Create(ctx, repoPath, target, gitworktree.Options{
			Prefix: "roborev-refine-plan-context-",
		})
		if createErr != nil {
			return fmt.Errorf("create target branch planning context: %w", createErr)
		}
		planningRepoPath = worktree.Dir
		defer func() {
			if closeErr := worktree.Close(context.Background()); closeErr != nil && err == nil {
				err = fmt.Errorf("close target branch planning context: %w", closeErr)
			}
		}()

		if err := config.ValidateRepoConfig(planningRepoPath); err != nil {
			return err
		}
		reasoning, err = config.ResolveRefineReasoning(opts.reasoning, planningRepoPath, cfg)
		if err != nil {
			return err
		}
		minSeverity, err = config.ResolveRefineMinSeverity(opts.minSeverity, planningRepoPath, cfg)
		if err != nil {
			return err
		}
	}

	resolution, err := agent.ResolveWorkflowConfig(opts.agentName, planningRepoPath, cfg, "refine", reasoning)
	if err != nil {
		return err
	}
	a, err := selectRefineAgent(
		planningRepoPath, cfg, resolution.PreferredAgent,
		agent.ParseReasoningLevel(reasoning), resolution.BackupAgent,
	)
	if err != nil {
		return err
	}
	a, _ = applyModelForAgent(
		a, resolution.PreferredAgent, resolution.BackupAgent,
		opts.model, planningRepoPath, cfg, "refine", reasoning,
	)
	planningPrompt, err := prompt.NewBuilderWithConfig(nil, cfg).
		ForRepo(planningRepoPath, 0).
		WithPlanSkillRoot(repoPath).
		BuildPlanPrompt(review, comments, minSeverity)
	if err != nil {
		return err
	}
	_, err = planFixAtRevision(
		ctx, cmd, planningRepoPath, target, a, planningPrompt, "", []int64{job.ID},
		fixOptions{planOnly: true, quiet: opts.quiet}, cfg,
	)
	return err
}

func refinePlanReviewEndpoint(gitRef string) (string, bool) {
	gitRef = strings.TrimSpace(gitRef)
	if _, endpoint, ok := strings.Cut(gitRef, "..."); ok {
		gitRef = endpoint
	} else if _, endpoint, ok := strings.Cut(gitRef, ".."); ok {
		gitRef = endpoint
	}
	return gitRef, gitRef != ""
}
