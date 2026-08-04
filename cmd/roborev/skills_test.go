package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillsInstallPathDefaultsToClaude(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "pi", "skills")
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--path", skillsDir})

	require.NoError(t, cmd.Execute())

	_, err := os.Stat(filepath.Join(skillsDir, "roborev-review", "SKILL.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(skillsDir, "roborev-review", "agents", "openai.yaml"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSkillsInstallPathSupportsExplicitAgent(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "codex", "skills")
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--path", skillsDir, "--agent", "codex"})

	require.NoError(t, cmd.Execute())

	_, err := os.Stat(filepath.Join(skillsDir, "roborev-review", "agents", "openai.yaml"))
	require.NoError(t, err)
}

func TestSkillsInstallRejectsAgentWithoutPath(t *testing.T) {
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--agent", "codex"})

	err := cmd.Execute()
	require.EqualError(t, err, "--agent requires --path")
}

func TestSkillsInstallRejectsEmptyPath(t *testing.T) {
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--path="})

	err := cmd.Execute()
	require.EqualError(t, err, "--path cannot be empty")
}

func TestSkillsInstallRejectsUnsupportedAgentWithoutCreatingPath(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--path", skillsDir, "--agent", "unknown"})

	err := cmd.Execute()
	require.EqualError(t, err, `unsupported agent "unknown" (expected claude, codex, droid, or grok)`)

	_, statErr := os.Stat(skillsDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
