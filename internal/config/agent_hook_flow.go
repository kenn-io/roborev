package config

import (
	"context"
	"fmt"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agentguide"
)

// RepoAgentHookConfig is the repository part of [agent_hook]. Other hook
// settings belong to global config.
type RepoAgentHookConfig struct {
	ReviewFlow string `toml:"review_flow" comment:"DOT digraph appended to Agent Hook review reminders. Read from the default branch."`
}

func ValidateAgentHookReviewFlow(flow string) error {
	flow = strings.TrimSpace(strings.ReplaceAll(flow, "\r\n", "\n"))
	if flow == "" {
		return nil
	}
	if strings.Contains(flow, "~~~") || strings.Contains(flow, "```") {
		return fmt.Errorf("agent_hook.review_flow: store the digraph without fences")
	}
	graph, err := agentguide.LintDOT(agentguide.FenceDOT(flow))
	if err != nil {
		return fmt.Errorf("agent_hook.review_flow: %w", err)
	}
	if graph.Name == "roborev_triage" {
		return fmt.Errorf("agent_hook.review_flow: graph name roborev_triage is reserved")
	}
	return nil
}

// AgentHookReviewFlow pairs a validated flow with its source. An empty Ref
// means it came from the working tree.
type AgentHookReviewFlow struct{ Text, Ref string }

// ResolveDefaultBranchRevision returns the friendly default-branch ref and
// the commit it names. Remote refs take precedence over local branches with
// the same abbreviated name.
func ResolveDefaultBranchRevision(ctx context.Context, repoPath string) (ref, revision string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	ref, err = gitrepo.DefaultBranch(ctx, repoPath)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err != nil || ref == "" {
		return "", "", nil
	}

	// Fully qualify before resolving: a local branch named origin/main must
	// not shadow the remote default branch. Pin readers to the commit.
	fullRef := "refs/heads/" + ref
	if strings.HasPrefix(ref, "origin/") && gitrepo.RefExists(ctx, repoPath, "refs/remotes/"+ref) {
		fullRef = "refs/remotes/" + ref
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	out, err := gitcmd.New().Output(ctx, repoPath, "rev-parse", "--verify", fullRef+"^{commit}")
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err != nil {
		return ref, "", err
	}
	return ref, strings.TrimSpace(string(out)), nil
}

func LoadAgentHookReviewFlow(ctx context.Context, repoPath string) (AgentHookReviewFlow, error) {
	if strings.TrimSpace(repoPath) == "" {
		return AgentHookReviewFlow{}, nil
	}
	ref, revision, err := ResolveDefaultBranchRevision(ctx, repoPath)
	if err != nil {
		return AgentHookReviewFlow{}, err
	}
	source, err := LoadRepoConfigWithFallbackContext(ctx, repoPath, revision)
	if err != nil {
		return AgentHookReviewFlow{}, err
	}
	result := AgentHookReviewFlow{}
	if source.Ref != "" {
		result.Ref = ref
	}
	if source.Config != nil {
		result.Text = strings.TrimSpace(source.Config.AgentHook.ReviewFlow)
	}
	return result, nil
}
