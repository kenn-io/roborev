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
	cfg := &config.Config{ACP: config.ACPAgentConfigs{
		"custom-acp": {Command: "custom-acp"},
	}}
	assert.True(t, isConfiguredACPAgentName("custom-acp", cfg, "/tmp/repo"))
	assert.True(t, isConfiguredACPAgentName("  custom-acp  ", cfg, "/tmp/repo"))
	assert.False(t, isConfiguredACPAgentName("other-acp", cfg, "/tmp/repo"))
	assert.False(t, isConfiguredACPAgentName("", cfg, "/tmp/repo"))
	assert.False(t, isConfiguredACPAgentName(defaultACPName, nil, "/tmp/repo"))

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte(`
[acp.repo-acp]
command = "repo-acp"
`), 0o644))
	assert.True(t, isConfiguredACPAgentName("repo-acp", &config.Config{}, repo))
}

func TestDefaultACPAgentConfig(t *testing.T) {
	cfg := defaultACPAgentConfig()
	assert.Equal(t, defaultACPCommand, cfg.Command)
	assert.Equal(t, defaultACPReadOnlyMode, cfg.ReadOnlyMode)
	assert.Equal(t, defaultACPAutoApproveMode, cfg.AutoApproveMode)
	assert.Equal(t, defaultACPReadOnlyMode, cfg.Mode)
	assert.Equal(t, defaultACPTimeoutSeconds, cfg.Timeout)
}

func TestConfiguredACPAgent(t *testing.T) {
	cfg := &config.Config{ACP: config.ACPAgentConfigs{
		"custom-acp": {
			Command: "custom-cmd",
			Model:   "custom-model",
		},
	}}

	agent, err := configuredACPAgent("custom-acp", "/tmp/repo", cfg)
	require.NoError(t, err)
	assert.Equal(t, "custom-acp", agent.agentName)
	assert.Equal(t, "custom-cmd", agent.Command)
	assert.Equal(t, "custom-model", agent.Model)
}

func writeAvailableACPCommand(t *testing.T, name string) string {
	t.Helper()
	acpBin := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		acpBin += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(acpBin, []byte(script), 0o755))
	return acpBin
}

func TestGetAvailableExactWithConfigSupportsMultipleACPAgents(t *testing.T) {
	gooseBin := writeAvailableACPCommand(t, "goose-acp")
	fooBin := writeAvailableACPCommand(t, "foo-acp")
	cfg := &config.Config{ACP: config.ACPAgentConfigs{
		"goose": {Command: gooseBin},
		"foo":   {Command: fooBin},
	}}

	goose, err := GetAvailableExactWithConfigFromConfig(nil, "goose", cfg)
	require.NoError(t, err)
	assert.Equal(t, "goose", goose.Name())
	assert.Equal(t, gooseBin, goose.(CommandAgent).CommandName())

	foo, err := GetAvailableExactWithConfigFromConfig(nil, "foo", cfg)
	require.NoError(t, err)
	assert.Equal(t, "foo", foo.Name())
	assert.Equal(t, fooBin, foo.(CommandAgent).CommandName())
}

func TestGetAvailableWithConfigACPAsBackup(t *testing.T) {
	acpBin := writeAvailableACPCommand(t, "backup-acp")
	t.Setenv("PATH", t.TempDir())

	cfg := &config.Config{ACP: config.ACPAgentConfigs{
		"my-acp": {Command: acpBin},
	}}
	got, err := GetAvailableWithConfig("", "codex", cfg, "my-acp")
	require.NoError(t, err)
	assert.Equal(t, "my-acp", got.Name())
}

func TestConfiguredACPAgentRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		config    config.ACPAgentConfig
		wantError string
	}{
		{name: "empty command", agentName: "goose", wantError: "requires a command"},
		{name: "built-in", agentName: "codex", config: config.ACPAgentConfig{Command: "goose"}, wantError: "conflicts"},
		{name: "alias", agentName: "claude", config: config.ACPAgentConfig{Command: "goose"}, wantError: "conflicts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ACP: config.ACPAgentConfigs{tc.agentName: tc.config}}
			_, err := GetAvailableExactWithConfigFromConfig(nil, tc.agentName, cfg)
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}
