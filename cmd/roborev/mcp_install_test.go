package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPInstallCommandHTTPAndDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	original := []byte(`{"mcpServers":{"other":{"command":"other"}}}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	for _, dryRun := range []bool{true, false} {
		cmd := mcpCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		args := []string{"install", "--agent", "droid", "--config", path, "--transport", "http", "--url", "http://127.0.0.1:7373/mcp"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		if dryRun {
			assert.Equal(t, original, data)
			assert.Contains(t, out.String(), "http://127.0.0.1:7373/mcp")
		} else {
			var config struct {
				Servers map[string]map[string]any `json:"mcpServers"`
			}
			require.NoError(t, json.Unmarshal(data, &config))
			assert.Equal(t, "http", config.Servers["roborev"]["type"])
			assert.Equal(t, "other", config.Servers["other"]["command"])
		}
	}
}

func TestSkillsInstallCommandSelectsMCPMode(t *testing.T) {
	dir := t.TempDir()
	cmd := skillsCmd()
	cmd.SetArgs([]string{"install", "--path", dir, "--agent", "qwen", "--mcp"})
	require.NoError(t, cmd.Execute())
	body, err := os.ReadFile(filepath.Join(dir, "roborev-fix", "SKILL.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "roborev_get_review")
	assert.Contains(t, string(body), "roborev_complete_fix")
}

func TestMCPInstallValidatesBeforeAgentDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "HERMES_HOME", "QWEN_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
	}

	for _, args := range [][]string{{"install", "--transport", "invalid"}, {"install", "--transport", "http"}} {
		cmd := mcpCmd()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}
