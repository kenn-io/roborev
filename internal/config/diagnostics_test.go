package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnknownGlobalKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
defualt_agent = "claude"
default_agent = "codex"
review_guidelines = "be kind"

[acp.goose]
command = "goose"
args = ["acp"]

[ci]
enabled = true
repos = ["acme/*"]
revews = { codex = ["security"] }

[review.subagents.sec]
agent = "codex"
review_type = "security"

[[hooks]]
event = "review.failed"
command = "echo hi"
`), 0o600))

	keys, err := UnknownGlobalKeys(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"ci.revews", "ci.revews.codex", "defualt_agent"}, keys)
}

func TestUnknownGlobalKeysMissingFile(t *testing.T) {
	keys, err := UnknownGlobalKeys(filepath.Join(t.TempDir(), "missing.toml"))
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestUnknownRepoKeys(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(`
agent = "claude"
review_guidlines = "typo"
`), 0o600))

	keys, err := UnknownRepoKeys(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"review_guidlines"}, keys)
}

func TestAgentReferences(t *testing.T) {
	cfg := &Config{
		DefaultAgent:      "codex",
		ReviewBackupAgent: "gemini",
		CI: CIConfig{
			Agents:  []string{"claude"},
			Reviews: map[string][]string{"acp.goose": {"security"}},
		},
		Review: ReviewConfig{Subagents: map[string]SubagentSpec{
			"sec": {Agent: "pi"},
		}},
	}

	refs := AgentReferences(cfg)
	assert.ElementsMatch(t, []AgentReference{
		{Key: "default_agent", Name: "codex"},
		{Key: "review_backup_agent", Name: "gemini"},
		{Key: "ci.agents[0]", Name: "claude"},
		{Key: "ci.reviews.acp.goose", Name: "acp.goose"},
		{Key: "review.subagents.sec.agent", Name: "pi"},
	}, refs)
}
