package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

type strictReviewTestAgent struct{ *TestAgent }

func (a *strictReviewTestAgent) WithStrictReview(config.ReviewToolPolicy, string) Agent { return a }

func TestValidateReviewToolSelection(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	capable := &strictReviewTestAgent{NewTestAgent()}
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), true))
	require.NoError(t, ValidateReviewToolSelection(strict, capable, false))
	require.ErrorIs(t, ValidateReviewToolSelection(strict, capable, true), ErrReviewToolPolicy)
	require.ErrorContains(t, ValidateReviewToolSelection(strict, capable, true), "agentic")
	require.ErrorContains(t, ValidateReviewToolSelection(&config.ReviewToolPolicy{}, capable, false), "mode")
	withMCP := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {"read"}}}
	require.ErrorContains(t, ValidateReviewToolSelection(withMCP, capable, false), "MCP grants are not supported yet")
	claude, err := Get("claude-code")
	require.NoError(t, err)
	require.NoError(t, ValidateReviewToolSelection(strict, claude, false))
	require.ErrorContains(t, ValidateReviewToolSelection(withMCP, claude, false), "MCP grants are not supported yet")
	for _, name := range []string{"codex", "test"} {
		a, err := Get(name)
		require.NoError(t, err)
		require.ErrorContains(t, ValidateReviewToolSelection(strict, a, false), "does not support strict review tools", name)
	}
}

func TestValidateReviewToolBackup(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	resolution := WorkflowConfig{RepoConfig: &config.RepoConfig{}, GlobalConfig: config.DefaultConfig(), BackupAgent: "test"}
	require.NoError(t, ValidateReviewToolBackup(nil, resolution, "claude-code"))
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "invalid backup agent")
	resolution.BackupAgent = "claude"
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "a backup that is the selected agent adds nothing to check")
	resolution.BackupAgent = "no-such-backup"
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "resolve backup agent")
}
