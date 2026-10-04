package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
)

type planningStateError struct {
	reason string
}

func (e *planningStateError) Error() string { return e.reason }

// agentCallTimeout gives planning and implementation agent calls the same deadline.
const agentCallTimeout = time.Hour

func withAgentCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, agentCallTimeout)
}

// planFix validates and stores a read-only plan before any foreground edits.
func planFix(ctx context.Context, cmd *cobra.Command, repoPath string, a agent.Agent, planningPrompt, implementationPrompt string, jobIDs []int64, opts fixOptions, cfg *config.Config) (string, error) {
	return planFixAtRevision(ctx, cmd, repoPath, "HEAD", a, planningPrompt, implementationPrompt, jobIDs, opts, cfg)
}

func planFixAtRevision(ctx context.Context, cmd *cobra.Command, repoPath, ref string, a agent.Agent, planningPrompt, implementationPrompt string, jobIDs []int64, opts fixOptions, cfg *config.Config) (string, error) {
	head, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve planning revision: %w", err)
	}
	branch := gitrepo.CurrentBranch(ctx, repoPath)
	dirty, err := gitrepo.HasUncommittedChanges(ctx, repoPath)
	if err != nil {
		return "", err
	}
	if dirty {
		return "", &planningStateError{reason: "planned fixes require a clean working tree; commit or stash changes first"}
	}
	if !opts.quiet {
		cmd.Printf("Planning fix for jobs %s...\n", formatJobIDs(jobIDs))
	}
	planningHead, err := gitrepo.Resolve(ctx, repoPath, ref)
	if err != nil {
		return "", err
	}
	planCtx, cancelPlan := withAgentCallTimeout(ctx)
	plan, err := agent.RunPlan(planCtx, a, repoPath, planningHead, planningPrompt, io.Discard, func(path, text string) (string, func(), error) {
		prepared, err := prompt.NewBuilderWithConfig(nil, cfg).ForRepo(repoPath, 0).Prepare(text, prompt.SnapshotTarget{RepoPath: path, ConfigRepoPath: repoPath})
		return prepared.Prompt, prepared.Cleanup, err
	})
	cancelPlan()
	if err != nil {
		return "", err
	}
	after, err := gitrepo.Resolve(ctx, repoPath, "HEAD")
	if err != nil {
		return "", err
	}
	dirty, err = gitrepo.HasUncommittedChanges(ctx, repoPath)
	if err != nil {
		return "", err
	}
	if dirty || after != head || gitrepo.CurrentBranch(ctx, repoPath) != branch {
		return "", &planningStateError{reason: "working tree or HEAD changed during planning; refusing to implement"}
	}
	for _, jobID := range jobIDs {
		if err := addJobResponse(ctx, getDaemonEndpoint().BaseURL(), jobID, "roborev-plan", "## Plan\n\n"+plan); err != nil {
			return "", fmt.Errorf("store plan for job %d: %w", jobID, err)
		}
	}
	if opts.planOnly || !opts.quiet {
		cmd.Printf("\nPlan for jobs %s:\n%s\n\n", formatJobIDs(jobIDs), plan)
	}
	implementationPrompt = prompt.WithFixPlan(implementationPrompt, plan)
	return implementationPrompt, nil
}

func batchPlanningContext(repoPath string, entries []batchEntry) string {
	var findings strings.Builder
	for _, entry := range entries {
		review := *entry.review
		review.Job = entry.job
		fmt.Fprintf(&findings, "## Job %d\n\n%s\n\n", entry.jobID, prompt.FixPlanReviewContext(repoPath, &review))
		attempts, comments := prompt.SplitResponses(entry.comments)
		findings.WriteString(prompt.FormatToolAttempts(attempts))
		findings.WriteString(prompt.FormatUserComments(comments))
	}
	return findings.String()
}
