package main

import (
	"context"
	"fmt"
	"os"

	gitrepo "go.kenn.io/kit/git/repo"
)

// currentRepoRoots captures the worktree root used for local git/file
// operations and the main repo root used for daemon/API queries.
type currentRepoRoots struct {
	worktreeRoot string
	mainRepoRoot string
}

// resolveCurrentRepoRoots resolves repo roots from the current working
// directory. Outside a git repo, both roots fall back to the working
// directory so existing fix/compact behavior stays unchanged.
func resolveCurrentRepoRoots() (currentRepoRoots, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return currentRepoRoots{}, fmt.Errorf("get working directory: %w", err)
	}

	roots := currentRepoRoots{
		worktreeRoot: workDir,
		mainRepoRoot: workDir,
	}
	if root, err := gitrepo.Root(context.TODO(), workDir); err == nil {
		roots.worktreeRoot = root
		roots.mainRepoRoot = root
	}
	if root, err := gitrepo.MainRoot(context.TODO(), workDir); err == nil {
		roots.mainRepoRoot = root
	}

	return roots, nil
}

func resolveCurrentBranchFilter(worktreeRoot, branch string, allBranches bool) string {
	if allBranches {
		return ""
	}
	if branch != "" {
		return branch
	}
	return gitrepo.CurrentBranch(context.TODO(), worktreeRoot)
}
