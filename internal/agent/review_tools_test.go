package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

type strictReviewTestAgent struct{ *TestAgent }

func (a *strictReviewTestAgent) WithStrictReview(config.ReviewToolPolicy, string) Agent { return a }

func TestReviewToolPolicySelection(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	capable := &strictReviewTestAgent{NewTestAgent()}
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), false))
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), true))
	require.NoError(t, ValidateReviewToolSelection(strict, capable, false))
	require.ErrorContains(t, ValidateReviewToolSelection(strict, capable, true), "agentic")
	require.ErrorContains(t, ValidateReviewToolSelection(&config.ReviewToolPolicy{}, capable, false), "mode")
	require.ErrorContains(t, ValidateReviewToolSelection(strict, nil, false), "agent")
	// None of the production adapters promise strict mode in this intermediate PR.
	for _, name := range []string{"claude-code", "codex", "pi", "test"} {
		t.Run(name, func(t *testing.T) {
			a, err := Get(name)
			require.NoError(t, err)
			require.ErrorContains(t, ValidateReviewToolSelection(strict, a, false), "does not support strict review tools")
		})
	}
}

func TestReviewToolPolicyBackup(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	resolution := WorkflowConfig{RepoConfig: &config.RepoConfig{}, GlobalConfig: config.DefaultConfig(), BackupAgent: "test"}
	require.NoError(t, ValidateReviewToolBackup(nil, resolution, "claude-code"))
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "invalid backup agent")
	resolution.BackupAgent = "claude"
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"))
	resolution.BackupAgent = ""
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"))
	resolution.BackupAgent = "no-such-backup"
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "resolve backup agent")
	resolution.BackupAgent = "acp.configured"
	resolution.RepoConfig.ACP = config.ACPAgentConfigs{"configured": {Command: "not-installed"}}
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "does not support strict review tools")
}
