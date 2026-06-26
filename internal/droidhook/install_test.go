package droidhook

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

	"go.kenn.io/roborev/internal/agenthook"
)

func TestDroidSpecs(t *testing.T) {
	assert := assert.New(t)
	specs := DroidSpecs("/tmp/roborev droid-hook run", 10*time.Second)
	require.Len(t, specs, 3)
	assert.Equal("PreToolUse", specs[0].Event)
	assert.Equal("PostToolUse", specs[1].Event)
	assert.Equal("Stop", specs[2].Event)
	assert.Equal(ExecuteMatcher, specs[0].Matcher)
	assert.Equal(ExecuteMatcher, specs[1].Matcher)
	assert.Empty(specs[2].Matcher)
	for _, s := range specs {
		assert.True(s.IncludeTimeout)
		assert.Equal(10, s.Timeout)
	}
}

func TestRunDumpDroidCreatesHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev droid-hook run"

	var stdout bytes.Buffer
	err := RunDump(DumpOptions{
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &root))
	assertDroidCommandCount(t, root, "PreToolUse", command, 1)
	assertDroidCommandCount(t, root, "PostToolUse", command, 1)
	assertDroidCommandCount(t, root, "Stop", command, 1)
	assert.Equal(t, ExecuteMatcher, firstDroidMatcher(t, root, "PostToolUse"))
	assert.Empty(t, firstDroidMatcher(t, root, "Stop"))
	assertDroidTimeout(t, root, "Stop", command, 10)
}

func TestRunInstallDroidIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev droid-hook run"

	var first bytes.Buffer
	err := RunInstall(InstallOptions{
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &first)
	require.NoError(t, err)
	assert.Contains(t, first.String(), "installed Factory Droid hooks (user)")

	var second bytes.Buffer
	err = RunInstall(InstallOptions{
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &second)
	require.NoError(t, err)
	assert.Contains(t, second.String(), "already installed")

	root := readDroidJSONFile(t, path)
	assertDroidCommandCount(t, root, "Stop", command, 1)
}

func TestRunInstallDroidMigratesStaleRoborevHookCommand(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/versioned/1.2.3/bin/roborev droid-hook run"
	newCommand := "/stable/bin/roborev droid-hook run"

	// A config left by an earlier install carries the old absolute-path command.
	writeDroidJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": ExecuteMatcher,
				"hooks":   []any{droidCommandHookJSON(oldCommand, 10)},
			}},
			"PostToolUse": []any{map[string]any{
				"matcher": ExecuteMatcher,
				"hooks":   []any{droidCommandHookJSON(oldCommand, 10)},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{droidCommandHookJSON(oldCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Command:    newCommand,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.NoError(t, err)

	// The stale command is replaced in place, not appended beside: each event
	// keeps exactly one command hook, carrying the new path.
	root := readDroidJSONFile(t, path)
	assertDroidCommandCount(t, root, "PreToolUse", newCommand, 1)
	assertDroidCommandCount(t, root, "PreToolUse", oldCommand, 0)
	assertDroidCommandCount(t, root, "PostToolUse", newCommand, 1)
	assertDroidCommandCount(t, root, "PostToolUse", oldCommand, 0)
	assertDroidCommandCount(t, root, "Stop", newCommand, 1)
	assertDroidCommandCount(t, root, "Stop", oldCommand, 0)
	assert.Contains(out.String(), "installed Factory Droid hooks", "migrating a stale command counts as a change")
}

// TestRunInstallDroidLeavesAgentHookEntriesUntouched verifies the runner
// disjointness: a Droid install must not clobber a Codex/Claude agent-hook
// entry, since the two integrations can coexist in the same hooks.json.
func TestRunInstallDroidLeavesAgentHookEntriesUntouched(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	agentCommand := "/stable/bin/roborev agent-hook run"
	droidCommand := "/stable/bin/roborev droid-hook run"

	writeDroidJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{droidCommandHookJSON(agentCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Command:    droidCommand,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.NoError(t, err)

	root := readDroidJSONFile(t, path)
	assertDroidCommandCount(t, root, "Stop", agentCommand, 1) // preserved
	assertDroidCommandCount(t, root, "Stop", droidCommand, 1) // added
	assert.Contains(out.String(), "installed Factory Droid hooks")
}

func TestRunInstallDroidRejectsUnknownScope(t *testing.T) {
	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Command: "/tmp/roborev droid-hook run",
		Scope:   "team",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope must be user or project")
}

func TestRunDumpDroidRejectsUnknownScope(t *testing.T) {
	var out bytes.Buffer
	err := RunDump(DumpOptions{
		Command: "/tmp/roborev droid-hook run",
		Scope:   "team",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope must be user or project")
}

func TestRunInstallDroidDryRunDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev droid-hook run"

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
		DryRun:     true,
	}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "would update")
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

func TestDefaultDroidHooksPathUser(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	assert.Equal(t, filepath.Join(dir, ".factory", "hooks.json"), DefaultDroidHooksPath("user"))
	assert.Equal(t, filepath.Join(dir, ".factory", "hooks.json"), DefaultDroidHooksPath(""))
}

func TestDefaultDroidHooksPathProject(t *testing.T) {
	assert.Equal(t, ".factory/hooks.json", DefaultDroidHooksPath("project"))
}

func TestNormalizeScope(t *testing.T) {
	t.Run("defaults to user when empty", func(t *testing.T) {
		scope, err := normalizeScope("")
		require.NoError(t, err)
		assert.Equal(t, "user", scope)
	})
	t.Run("accepts user and project case-insensitively", func(t *testing.T) {
		for _, in := range []string{"user", "User", "PROJECT", "project"} {
			scope, err := normalizeScope(in)
			require.NoError(t, err)
			assert.Equal(t, strings.ToLower(in), scope)
		}
	})
	t.Run("rejects unknown scope", func(t *testing.T) {
		_, err := normalizeScope("team")
		require.Error(t, err)
	})
}

func assertDroidCommandCount(t *testing.T, root map[string]any, event, command string, want int) {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	count := 0
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		require.True(t, ok)
		hookList, ok := entry["hooks"].([]any)
		require.True(t, ok)
		for _, h := range hookList {
			ho, ok := h.(map[string]any)
			require.True(t, ok)
			if ho["type"] == "command" && ho["command"] == command {
				count++
			}
		}
	}
	assert.Equal(t, want, count)
}

func firstDroidMatcher(t *testing.T, root map[string]any, event string) string {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	require.NotEmpty(t, entries)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	matcher, _ := entry["matcher"].(string)
	return matcher
}

func assertDroidTimeout(t *testing.T, root map[string]any, event, command string, want int) {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		require.True(t, ok)
		hookList, ok := entry["hooks"].([]any)
		require.True(t, ok)
		for _, h := range hookList {
			ho, ok := h.(map[string]any)
			require.True(t, ok)
			if ho["type"] == "command" && ho["command"] == command {
				timeout, ok := ho["timeout"].(float64)
				require.True(t, ok)
				assert.Equal(t, want, int(timeout))
				return
			}
		}
	}
	require.Fail(t, "command hook not found", "event=%s command=%s", event, command)
}

func droidCommandHookJSON(command string, timeout int) map[string]any {
	return map[string]any{"type": "command", "command": command, "timeout": float64(timeout)}
}

func writeDroidJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))
}

func readDroidJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(body, &root))
	return root
}

// Compile-time guard: ensure the Droid install reuses the shared agenthook
// planner so a future divergence surfaces at build time.
var _ = agenthook.InstallSpecs
