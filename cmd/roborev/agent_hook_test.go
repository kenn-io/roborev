package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
)

func TestAgentHookDumpCodexCreatesHookConfig(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run"

	cmd := agentHookCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"dump",
		"--agent", "codex",
		"--command", command,
		"--config", path,
	})

	require.NoError(t, cmd.Execute())

	var root map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &root))
	assertAgentHookCommandCount(t, root, "PreToolUse", command, 1)
	assertAgentHookCommandCount(t, root, "PostToolUse", command, 1)
	assertAgentHookCommandCount(t, root, "Stop", command, 1)
	assert.Equal("^Bash$", firstAgentHookMatcher(t, root, "PreToolUse"))
	assert.Equal("^Bash$", firstAgentHookMatcher(t, root, "PostToolUse"))
	assert.InDelta(10, firstAgentHookCommandTimeout(t, root, "Stop", command), 0)
}

func TestAgentHookDaemonHasLifecycleSubcommands(t *testing.T) {
	daemonCmd, _, err := agentHookCmd().Find([]string{"daemon"})
	require.NoError(t, err)
	require.Equal(t, "daemon", daemonCmd.Name())

	got := map[string]bool{}
	for _, sub := range daemonCmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"run", "start", "status", "stop", "restart"} {
		assert.True(t, got[want], "missing daemon subcommand %q", want)
	}
}

func TestRunAgentHookFailsOpenWhenDaemonUnavailable(t *testing.T) {
	assert := assert.New(t)
	oldPost := postAgentHook
	postAgentHook = func(context.Context, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{}, errors.New("daemon unavailable")
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout, stderr bytes.Buffer
	err := runAgentHook(
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)

	require.NoError(t, err)
	assert.JSONEq(`{}`, stdout.String())
	assert.Contains(stderr.String(), "daemon unavailable")
}

func assertAgentHookCommandCount(t *testing.T, root map[string]any, event, command string, want int) {
	t.Helper()
	count := 0
	for _, hook := range agentHookEventEntriesForTest(t, root, event) {
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

func firstAgentHookMatcher(t *testing.T, root map[string]any, event string) string {
	t.Helper()
	entries := agentHookEventEntriesForTest(t, root, event)
	require.NotEmpty(t, entries)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	matcher, _ := entry["matcher"].(string)
	return matcher
}

func firstAgentHookCommandTimeout(t *testing.T, root map[string]any, event, command string) any {
	t.Helper()
	var found any
	for _, hook := range agentHookEventEntriesForTest(t, root, event) {
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

func agentHookEventEntriesForTest(t *testing.T, root map[string]any, event string) []any {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	return entries
}
