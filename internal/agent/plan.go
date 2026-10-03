package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"
	gitworktree "go.kenn.io/kit/git/worktree"
)

type planningContextKey struct{}

func planningReadOnly(ctx context.Context) bool {
	readOnly, _ := ctx.Value(planningContextKey{}).(bool)
	return readOnly
}

func effectiveAgentic(ctx context.Context, agentic bool) bool {
	return !planningReadOnly(ctx) && (agentic || AllowUnsafeAgents())
}

// RunPlan invokes an agent in a disposable worktree with read-only permissions.
// Planning never resumes an implementation session or promotes file changes.
func RunPlan(ctx context.Context, a Agent, repoPath, ref, planningPrompt string, output io.Writer, prepare func(repoPath, text string) (string, func(), error)) (string, error) {
	ctx = context.WithValue(ctx, planningContextKey{}, true)
	sha, err := gitrepo.Resolve(ctx, repoPath, ref)
	if err != nil {
		return "", fmt.Errorf("resolve planning revision: %w", err)
	}
	wt, err := gitworktree.Create(ctx, repoPath, sha, gitworktree.Options{
		Prefix: "roborev-plan-", InitSubmodules: true, PullLFS: true,
		// Preserve user URL rewrites and credentials, while disabling hooks
		// for every setup command, including nested submodule checkouts.
		Runner: gitcmd.Runner{
			Env: os.Environ(), StripEnv: true, DisableSafeDirectoryForward: true,
			Config: []gitcmd.Config{{Key: "core.askPass", Value: ""}, {Key: "core.hooksPath", Value: os.DevNull}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create planning worktree: %w", err)
	}
	defer func() { _ = wt.Close(context.Background()) }()
	if prepare != nil {
		prepared, cleanup, prepareErr := prepare(wt.Dir, planningPrompt)
		if cleanup != nil {
			defer cleanup()
		}
		if prepareErr != nil {
			return "", fmt.Errorf("prepare planning prompt: %w", prepareErr)
		}
		planningPrompt = prepared
	}

	a = a.WithAgentic(false)
	if sa, ok := a.(SessionAgent); ok {
		a = sa.WithSessionID("")
	}
	result, err := a.Review(ctx, wt.Dir, sha, planningPrompt, output)
	if err != nil {
		return "", fmt.Errorf("planning agent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	head, err := gitrepo.Resolve(ctx, wt.Dir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("check planning revision: %w", err)
	}
	dirty, err := gitrepo.HasUncommittedChanges(ctx, wt.Dir)
	if err != nil {
		return "", fmt.Errorf("check planning changes: %w", err)
	}
	if dirty {
		return "", fmt.Errorf("planning agent left uncommitted changes in its worktree; refusing to implement")
	}
	if head != sha {
		return "", fmt.Errorf("planning agent changed HEAD; refusing to implement")
	}
	if gitrepo.CurrentBranch(ctx, wt.Dir) != "" {
		return "", fmt.Errorf("planning agent attached a branch; refusing to implement")
	}
	result = strings.TrimSpace(result)
	if result == "" || result == "No review output generated" {
		return "", fmt.Errorf("planning agent returned an empty plan")
	}
	return result, nil
}
