package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestIsConfiguredACPAgentName(t *testing.T) {
	t.Run("default ACP name", func(t *testing.T) {
		assert.True(t, isConfiguredACPAgentName(defaultACPName, nil, "/tmp/repo"))
	})

	t.Run("matches configured name", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name: "custom-acp",
			},
		}
		assert.True(t, isConfiguredACPAgentName("custom-acp", cfg, "/tmp/repo"))
	})

	t.Run("does not match different name", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name: "custom-acp",
			},
		}
		assert.False(t, isConfiguredACPAgentName("other-acp", cfg, "/tmp/repo"))
	})

	t.Run("empty name returns false", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name: "custom-acp",
			},
		}
		assert.False(t, isConfiguredACPAgentName("", cfg, "/tmp/repo"))
	})

	t.Run("nil config returns false for non-default name", func(t *testing.T) {
		assert.False(t, isConfiguredACPAgentName("custom-acp", nil, "/tmp/repo"))
	})

	t.Run("repo config takes precedence", func(t *testing.T) {
		// Create a temp directory with .roborev.toml
		testDir := t.TempDir()
		configPath := filepath.Join(testDir, ".roborev.toml")
		content := `[acp]
name = "repo-acp"
`
		err := os.WriteFile(configPath, []byte(content), 0o644)
		require.NoError(t, err)

		// With repo config, should match repo-acp
		assert.True(t, isConfiguredACPAgentName("repo-acp", &config.Config{}, testDir))

		// Should not match different name
		assert.False(t, isConfiguredACPAgentName("other-acp", &config.Config{}, testDir))
	})

	t.Run("whitespace trimming", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name: "  custom-acp  ",
			},
		}
		// Should match with whitespace trimmed
		assert.True(t, isConfiguredACPAgentName("custom-acp", cfg, "/tmp/repo"))
	})

	t.Run("configured name with whitespace", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name: "  custom-acp  ",
			},
		}
		// Should match rawName with whitespace
		assert.True(t, isConfiguredACPAgentName("  custom-acp  ", cfg, "/tmp/repo"))
	})
}

func TestDefaultACPAgentConfig(t *testing.T) {
	cfg := defaultACPAgentConfig()
	assert.Equal(t, defaultACPName, cfg.Name)
	assert.Equal(t, defaultACPCommand, cfg.Command)
	assert.Equal(t, defaultACPReadOnlyMode, cfg.ReadOnlyMode)
	assert.Equal(t, defaultACPAutoApproveMode, cfg.AutoApproveMode)
	assert.Equal(t, defaultACPReadOnlyMode, cfg.Mode)
	assert.Equal(t, defaultACPTimeoutSeconds, cfg.Timeout)
}

func TestConfiguredACPAgent(t *testing.T) {
	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "custom-acp",
			Command: "custom-cmd",
			Model:   "custom-model",
		},
	}

	agent := configuredACPAgent("/tmp/repo", cfg)
	assert.Equal(t, defaultACPName, agent.agentName)
	assert.Equal(t, "custom-cmd", agent.Command)
	assert.Equal(t, "custom-model", agent.Model)
}

func TestGetAvailableWithConfigACPAgent(t *testing.T) {
	// Use a real, resolvable fake executable rather than a bare command name
	// like "echo": on Windows "echo" is a cmd.exe builtin, not a PATH binary,
	// so exec.LookPath fails and ACP resolution falls back to the default
	// agent. An absolute path to a tiny script is available on every platform.
	acpBin := filepath.Join(t.TempDir(), "fake-acp")
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		acpBin += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(acpBin, []byte(script), 0o755))

	t.Run("resolves configured ACP agent name", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name:    "my-acp",
				Command: acpBin,
			},
		}

		// When the requested name matches the configured ACP name
		agent, err := GetAvailableWithConfig("", "my-acp", cfg)
		require.NoError(t, err)
		// The agent name should be the canonical ACP name
		assert.Equal(t, defaultACPName, agent.Name())
	})

	t.Run("resolves default acp name with configured command", func(t *testing.T) {
		cfg := &config.Config{
			ACP: &config.ACPAgentConfig{
				Name:    defaultACPName,
				Command: acpBin,
			},
		}

		agent, err := GetAvailableWithConfig("", defaultACPName, cfg)
		require.NoError(t, err)
		assert.Equal(t, defaultACPName, agent.Name())
	})
}

func TestGetAvailableWithConfigACPAsBackup(t *testing.T) {
	// A configured ACP agent must be selectable as a backup agent, honoring
	// [acp].command the same way the preferred-agent path does. The command is
	// an absolute path so it stays resolvable while PATH is isolated below to
	// make the preferred agent's default binary unavailable.
	acpBin := filepath.Join(t.TempDir(), "fake-acp")
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		acpBin += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(acpBin, []byte(script), 0o755))
	t.Setenv("PATH", t.TempDir())

	t.Run("literal acp backup honors [acp].command", func(t *testing.T) {
		cfg := &config.Config{ACP: &config.ACPAgentConfig{Command: acpBin}}
		// Preferred "codex" is unavailable on the isolated PATH; the "acp"
		// backup must resolve through the configured-ACP path.
		got, err := GetAvailableWithConfig("", "codex", cfg, defaultACPName)
		require.NoError(t, err)
		assert.Equal(t, defaultACPName, got.Name())
	})

	t.Run("custom [acp].name backup honors configured command", func(t *testing.T) {
		cfg := &config.Config{ACP: &config.ACPAgentConfig{Name: "my-acp", Command: acpBin}}
		got, err := GetAvailableWithConfig("", "codex", cfg, "my-acp")
		require.NoError(t, err)
		assert.Equal(t, defaultACPName, got.Name())
	})
}
