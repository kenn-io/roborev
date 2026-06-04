package agenthook

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDumpCodexCreatesHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run"

	var stdout bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:      "codex",
		Command:    command,
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &root))
	assertCommandCount(t, root, "PostToolUse", command, 1)
	assertCommandCount(t, root, "Stop", command, 1)
	assert.Equal(t, "^Bash$", firstMatcher(t, root, "PostToolUse"))
	assert.InDelta(t, 10, firstCommandTimeout(t, root, "Stop", command), 0)
}

func TestRunInstallCodexIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run"

	var first bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         command,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &first)
	require.NoError(t, err)
	assert.Contains(t, first.String(), "installed Codex agent hooks")

	var second bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         command,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &second)
	require.NoError(t, err)
	assert.Contains(t, second.String(), "Codex agent hooks already installed")
}

func assertCommandCount(t *testing.T, root map[string]any, event, command string, want int) {
	t.Helper()
	count := 0
	for _, hook := range eventEntriesForTest(t, root, event) {
		entry, ok := hook.(map[string]any)
		require.True(t, ok)
		for _, raw := range entry["hooks"].([]any) {
			hookObj, ok := raw.(map[string]any)
			require.True(t, ok)
			if hookObj["type"] == "command" && hookObj["command"] == command {
				count++
			}
		}
	}
	assert.Equal(t, want, count)
}

func firstMatcher(t *testing.T, root map[string]any, event string) string {
	t.Helper()
	entries := eventEntriesForTest(t, root, event)
	require.NotEmpty(t, entries)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	matcher, _ := entry["matcher"].(string)
	return matcher
}

func firstCommandTimeout(t *testing.T, root map[string]any, event, command string) any {
	t.Helper()
	var found any
	for _, hook := range eventEntriesForTest(t, root, event) {
		entry, ok := hook.(map[string]any)
		require.True(t, ok)
		for _, raw := range entry["hooks"].([]any) {
			hookObj, ok := raw.(map[string]any)
			require.True(t, ok)
			if hookObj["type"] == "command" && hookObj["command"] == command {
				found = hookObj["timeout"]
			}
		}
	}
	require.NotNil(t, found, "command hook %q not found for %s", command, event)
	return found
}

func eventEntriesForTest(t *testing.T, root map[string]any, event string) []any {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	return entries
}
