package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGlobalFromAgentHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[agent_hook]
turn_threshold = 6
commit_threshold = 2
failed_review_threshold = 3
instruction = "Run roborev fix."
`), 0o600))

	cfg, err := LoadGlobalFrom(path)

	require.NoError(t, err)
	assert.Equal(t, 6, cfg.AgentHook.TurnThreshold)
	assert.Equal(t, 2, cfg.AgentHook.CommitThreshold)
	assert.Equal(t, 3, cfg.AgentHook.FailedReviewThreshold)
	assert.Equal(t, "Run roborev fix.", cfg.AgentHook.Instruction)
}
