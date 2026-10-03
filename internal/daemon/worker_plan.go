package daemon

import (
	"context"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// prepareFixPlan uses a separate session and worktree, retaining only text.
func (wp *WorkerPool) prepareFixPlan(ctx context.Context, workerID string, a agent.Agent, job *storage.ReviewJob, repoPath, planningPrompt, implementationPrompt string, builder *prompt.Builder, configRepoPath string, output io.Writer) (string, string, error) {
	plan, err := agent.RunPlan(ctx, a, repoPath, "HEAD", planningPrompt, output, func(path, text string) (string, func(), error) {
		prepared, err := builder.Prepare(text, prompt.SnapshotTarget{RepoPath: path, ConfigRepoPath: configRepoPath})
		if err == nil {
			wp.markAgentInvoked(workerID, job, a.WithAgentic(false))
		}
		return prepared.Prompt, prepared.Cleanup, err
	})
	if err != nil {
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if job.ParentJobID != nil {
		if _, err := wp.db.AddCommentToJob(*job.ParentJobID, "roborev-plan", "## Plan\n\n"+plan); err != nil {
			return "", "", fmt.Errorf("store fix plan: %w", err)
		}
	}
	return plan, prompt.WithFixPlan(implementationPrompt, plan), nil
}
