package agenthook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunInstallGrokUsesDedicatedHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roborev.json")
	legacy := "/opt/roborev agent-hook run --agent grok"
	unowned := "/opt/user-hook agent-hook run --agent grok"
	fixture, err := json.Marshal(map[string]any{
		"hooks": map[string]any{"Stop": []any{map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": legacy},
				map[string]any{"type": "command", "command": unowned},
			},
		}}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, fixture, 0o600))
	opts := InstallOptions{
		Agent: "grok", Command: legacy,
		ConfigPath: path, Timeout: 10 * time.Second,
	}
	var first, second bytes.Buffer

	require.NoError(t, RunInstall(opts, &first))
	require.NoError(t, RunInstall(opts, &second))

	assert.Contains(t, first.String(), "installed Grok Build agent hooks")
	assert.Contains(t, second.String(), "Grok Build agent hooks already installed")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(body, &root))
	hooks := root["hooks"].(map[string]any)
	assert.Len(t, hooks, 3)
	assert.Contains(t, string(body), GrokShellMatcher)
	assert.Contains(t, string(body), agentHookMarker)
	assert.Contains(t, string(body), unowned)
	installed := 0
	for _, rawEntries := range hooks {
		for _, rawEntry := range rawEntries.([]any) {
			for _, rawHandler := range rawEntry.(map[string]any)["hooks"].([]any) {
				command := rawHandler.(map[string]any)["command"].(string)
				assert.NotEqual(t, legacy, command)
				if strings.Contains(command, agentHookMarker) {
					installed++
				}
			}
		}
	}
	assert.Equal(t, 3, installed)
}

func TestRunDumpGrokDoesNotWriteSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roborev.json")
	source := []byte(`{"custom":"preserved"}`)
	require.NoError(t, os.WriteFile(path, source, 0o600))
	var stdout bytes.Buffer

	err := RunDump(DumpOptions{
		Agent: "grok", Command: "/opt/roborev agent-hook run --agent grok",
		ConfigPath: path, Timeout: 10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), `"custom": "preserved"`)
	assert.Contains(t, stdout.String(), "agent-hook run --agent grok")
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, source, unchanged)
}
