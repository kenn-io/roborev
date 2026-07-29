package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveClassifyModel(t *testing.T) {
	t.Run("explicit CLI model takes precedence", func(t *testing.T) {
		model := ResolveClassifyModel("claude-3-opus", "/tmp/repo", nil)
		assert.Equal(t, "claude-3-opus", model)
	})

	t.Run("falls back to repo config", func(t *testing.T) {
		testDir := t.TempDir()
		writeRepoConfigStr(t, testDir, `classify_model = "claude-3-sonnet"`)

		model := ResolveClassifyModel("", testDir, nil)
		assert.Equal(t, "claude-3-sonnet", model)
	})

	t.Run("falls back to global config", func(t *testing.T) {
		globalCfg := &Config{
			ClassifyModel: "claude-3-haiku",
		}

		model := ResolveClassifyModel("", "/tmp/nonexistent", globalCfg)
		assert.Equal(t, "claude-3-haiku", model)
	})

	t.Run("returns empty when no config", func(t *testing.T) {
		model := ResolveClassifyModel("", "/tmp/nonexistent", nil)
		assert.Empty(t, model)
	})
}

func TestResolveBackupClassifyModel(t *testing.T) {
	t.Run("repo config takes precedence", func(t *testing.T) {
		testDir := t.TempDir()
		writeRepoConfigStr(t, testDir, `classify_backup_model = "backup-sonnet"`)

		globalCfg := &Config{
			ClassifyBackupModel: "global-backup",
		}

		model := ResolveBackupClassifyModel(testDir, globalCfg)
		assert.Equal(t, "backup-sonnet", model)
	})

	t.Run("falls back to global config", func(t *testing.T) {
		globalCfg := &Config{
			ClassifyBackupModel: "global-backup",
		}

		model := ResolveBackupClassifyModel("/tmp/nonexistent", globalCfg)
		assert.Equal(t, "global-backup", model)
	})

	t.Run("returns empty when no config", func(t *testing.T) {
		model := ResolveBackupClassifyModel("/tmp/nonexistent", nil)
		assert.Empty(t, model)
	})
}

func TestResolveClassifyAgent(t *testing.T) {
	t.Run("explicit CLI agent takes precedence", func(t *testing.T) {
		agent, err := ResolveClassifyAgent("claude-3-opus", "/tmp/repo", nil)
		require.NoError(t, err)
		assert.Equal(t, "claude-3-opus", agent)
	})

	t.Run("falls back to repo config", func(t *testing.T) {
		testDir := t.TempDir()
		writeRepoConfigStr(t, testDir, `classify_agent = "gemini"`)

		agent, err := ResolveClassifyAgent("", testDir, nil)
		require.NoError(t, err)
		assert.Equal(t, "gemini", agent)
	})

	t.Run("falls back to global config", func(t *testing.T) {
		globalCfg := &Config{
			ClassifyAgent: "global-agent",
		}

		agent, err := ResolveClassifyAgent("", "/tmp/nonexistent", globalCfg)
		require.NoError(t, err)
		assert.Equal(t, "global-agent", agent)
	})

	t.Run("defaults to constant", func(t *testing.T) {
		agent, err := ResolveClassifyAgent("", "/tmp/nonexistent", nil)
		require.NoError(t, err)
		assert.Equal(t, DefaultClassifyAgent, agent)
	})
}

func TestResolveClassifyReasoning(t *testing.T) {
	t.Run("explicit CLI reasoning takes precedence", func(t *testing.T) {
		reasoning := ResolveClassifyReasoning("thorough", "/tmp/repo", nil)
		assert.Equal(t, "thorough", reasoning)
	})

	t.Run("falls back to repo config", func(t *testing.T) {
		testDir := t.TempDir()
		writeRepoConfigStr(t, testDir, `classify_reasoning = "medium"`)

		reasoning := ResolveClassifyReasoning("", testDir, nil)
		assert.Equal(t, "medium", reasoning)
	})

	t.Run("falls back to global config", func(t *testing.T) {
		globalCfg := &Config{
			ClassifyReasoning: "fast",
		}

		reasoning := ResolveClassifyReasoning("", "/tmp/nonexistent", globalCfg)
		assert.Equal(t, "fast", reasoning)
	})

	t.Run("defaults to fast", func(t *testing.T) {
		reasoning := ResolveClassifyReasoning("", "/tmp/nonexistent", nil)
		assert.Equal(t, DefaultClassifyReasoning, reasoning)
	})
}

func TestResolveBackupClassifyAgent(t *testing.T) {
	t.Run("repo config takes precedence", func(t *testing.T) {
		testDir := t.TempDir()
		writeRepoConfigStr(t, testDir, `classify_backup_agent = "gemini"`)

		globalCfg := &Config{
			ClassifyBackupAgent: "global-backup",
		}

		agent, err := ResolveBackupClassifyAgent(testDir, globalCfg)
		require.NoError(t, err)
		assert.Equal(t, "gemini", agent)
	})

	t.Run("falls back to global config", func(t *testing.T) {
		globalCfg := &Config{
			ClassifyBackupAgent: "global-backup",
		}

		agent, err := ResolveBackupClassifyAgent("/tmp/nonexistent", globalCfg)
		require.NoError(t, err)
		assert.Equal(t, "global-backup", agent)
	})

	t.Run("returns empty when no config", func(t *testing.T) {
		agent, err := ResolveBackupClassifyAgent("/tmp/nonexistent", nil)
		require.NoError(t, err)
		assert.Empty(t, agent)
	})
}
