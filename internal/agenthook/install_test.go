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

func TestRunInstallMigratesStaleRoborevHookCommand(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/versioned/1.2.3/bin/roborev agent-hook run"
	newCommand := "/stable/bin/roborev agent-hook run"

	// A config left by an earlier install carries the old absolute-path command.
	writeJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{map[string]any{
				"matcher": "^Bash$",
				"hooks":   []any{commandHookJSON(oldCommand, 10)},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{commandHookJSON(oldCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         newCommand,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &out)
	require.NoError(t, err)

	// The stale command is replaced in place, not appended beside: each event
	// keeps exactly one command hook, carrying the new path.
	root := readJSONFile(t, path)
	assertCommandCount(t, root, "PostToolUse", newCommand, 1)
	assertCommandCount(t, root, "PostToolUse", oldCommand, 0)
	assertCommandCount(t, root, "Stop", newCommand, 1)
	assertCommandCount(t, root, "Stop", oldCommand, 0)
	assert.Contains(out.String(), "installed Codex agent hooks", "migrating a stale command counts as a change")
}

func TestUpsertCommandHookCollapsesDuplicatesAndKeepsOthers(t *testing.T) {
	assert := assert.New(t)
	spec := installSpec{
		Event: "PostToolUse", Matcher: "^Bash$",
		Command: "/new/roborev agent-hook run", Timeout: 10, IncludeTimeout: true,
	}
	commandHook := map[string]any{"type": "command", "command": spec.Command, "timeout": spec.Timeout}
	list := []any{
		commandHookJSON("/old/a/roborev agent-hook run", 10),
		map[string]any{"type": "command", "command": "/usr/bin/other-tool run"},
		commandHookJSON("/old/b/roborev agent-hook run", 10),
	}

	updated, changed := upsertCommandHook(list, commandHook, spec)

	assert.True(changed)
	// Both stale roborev hooks collapse into one new command at the first one's
	// slot; the unrelated tool hook is preserved.
	commands := make([]string, 0, len(updated))
	for _, raw := range updated {
		commands = append(commands, raw.(map[string]any)["command"].(string))
	}
	assert.Equal([]string{spec.Command, "/usr/bin/other-tool run"}, commands)
}

func TestAgentHookNoticeTranslatesBinaryFlag(t *testing.T) {
	assert := assert.New(t)
	notice := "Warning: roborev appears to be running from a versioned mise install (/x); " +
		"use --binary to install hooks with a stable shim if available"

	got := agentHookNotice(notice)
	assert.NotContains(got, "--binary", "agent-hook commands have no --binary flag")
	assert.Contains(got, "--command", "the override flag is translated to --command")
	assert.Empty(agentHookNotice(""), "an empty notice stays empty")
}

func TestResolveHookCommandOverrideIsVerbatim(t *testing.T) {
	assert := assert.New(t)

	command, notice, err := ResolveHookCommand("/custom/roborev agent-hook run")
	require.NoError(t, err)
	assert.Equal("/custom/roborev agent-hook run", command, "an override is used verbatim")
	assert.Empty(notice, "an override yields no advisory notice")
}

func TestResolveHookCommandBlankOverrideResolvesBinary(t *testing.T) {
	assert := assert.New(t)

	// A blank override falls back to binary resolution rather than installing an
	// empty command. The resolved path is appended with the run subcommand.
	command, _, err := ResolveHookCommand("   ")
	require.NoError(t, err)
	assert.NotEmpty(command)
	assert.True(strings.HasSuffix(command, " agent-hook run"),
		"resolved command should invoke agent-hook run, got %q", command)
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

func commandHookJSON(command string, timeout int) map[string]any {
	return map[string]any{"type": "command", "command": command, "timeout": float64(timeout)}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(body, &root))
	return root
}
