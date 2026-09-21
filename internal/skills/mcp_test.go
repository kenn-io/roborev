package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPInstallPreservesModeOnUpdate(t *testing.T) {
	for _, agent := range Agents() {
		t.Run(string(agent), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "skills")
			_, err := InstallToPath(agent, dir, new(true))
			require.NoError(t, err)
			require.True(t, installedMCPMode(dir))
			fix, err := os.ReadFile(skillInstallPath(dir, "roborev-fix"))
			require.NoError(t, err)
			assert.Contains(t, string(fix), "roborev_complete_fix")
			assert.Contains(t, string(fix), "roborev_add_comment")
			_, err = InstallToPath(agent, dir, nil)
			require.NoError(t, err)
			updated, err := os.ReadFile(skillInstallPath(dir, "roborev-fix"))
			require.NoError(t, err)
			assert.Equal(t, fix, updated)
			_, err = InstallToPath(agent, dir, new(false))
			require.NoError(t, err)
			assert.False(t, installedMCPMode(dir))
			cli, err := os.ReadFile(skillInstallPath(dir, "roborev-fix"))
			require.NoError(t, err)
			assert.Contains(t, string(cli), "roborev show --job")
		})
	}
}

func TestNewAgentHomeOverridesApplyToInstallAndStatus(t *testing.T) {
	for _, tc := range []struct {
		agent       Agent
		env, subdir string
	}{
		{AgentCopilot, "COPILOT_HOME", ""},
		{AgentGemini, "GEMINI_CLI_HOME", ".gemini"},
		{AgentHermes, "HERMES_HOME", ""},
		{AgentQwen, "QWEN_HOME", ""},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			setupTestEnv(t)
			root := t.TempDir()
			t.Setenv(tc.env, root)
			dir := filepath.Join(root, tc.subdir)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			_, err := Install(new(true))
			require.NoError(t, err)
			content, err := os.ReadFile(filepath.Join(dir, "skills", "roborev-fix", "SKILL.md"))
			require.NoError(t, err)
			assert.Contains(t, string(content), "roborev_complete_fix")
			status, ok := StatusForAgent(tc.agent)
			require.True(t, ok)
			assert.True(t, status.MCP)
			assert.Equal(t, SkillCurrent, status.Skills["roborev-fix"])
		})
	}
}
