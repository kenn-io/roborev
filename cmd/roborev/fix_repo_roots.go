package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/pkg/client/generated"
)

// currentRepoRoots captures the worktree root used for local git/file
// operations and the main repo root used for daemon/API queries.
type currentRepoRoots struct {
	inRepo       bool
	worktreeRoot string
	mainRepoRoot string
}

// resolveCurrentRepoRoots resolves repo roots from the current working
// directory. Outside a git repo, both roots fall back to the working
// directory so existing fix/compact behavior stays unchanged.
func resolveCurrentRepoRoots(ctx context.Context) (currentRepoRoots, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return currentRepoRoots{}, fmt.Errorf("get working directory: %w", err)
	}

	roots := currentRepoRoots{
		worktreeRoot: workDir,
		mainRepoRoot: workDir,
	}
	if root, err := gitrepo.Root(ctx, workDir); err == nil {
		roots.inRepo = true
		roots.worktreeRoot = root
		roots.mainRepoRoot = root
	}
	if root, err := gitrepo.MainRoot(ctx, workDir); err == nil {
		roots.mainRepoRoot = root
	}

	return roots, nil
}

func resolveCurrentBranchFilter(ctx context.Context, worktreeRoot, branch string, allBranches bool) string {
	if allBranches {
		return ""
	}
	if branch != "" {
		return branch
	}
	return gitrepo.CurrentBranch(ctx, worktreeRoot)
}

func printOutsideRepoHint(ctx context.Context, cmd *cobra.Command, roots currentRepoRoots) {
	if roots.inRepo {
		return
	}
	_, stats := queryHintJobs(ctx, generated.ListJobsQuery{
		Status: new("done"), Closed: new(generated.ListJobsQueryClosedFalse),
	})
	if stats.RepoCount > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "hint: no open jobs for repository path %s; the daemon has open jobs in %d repos; see roborev repo list\n", roots.mainRepoRoot, stats.RepoCount)
	}
}
