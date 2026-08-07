package agenthook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeInputClaudeSnakeCase(t *testing.T) {
	assert := assert.New(t)
	in, err := DecodeInput(strings.NewReader(`{
		"session_id": "s1",
		"hook_event_name": "Stop",
		"cwd": "/repo",
		"stop_hook_active": true,
		"tool_name": "Bash",
		"tool_input": {"command": "git commit -m x"}
	}`))
	require.NoError(t, err)
	assert.Equal("s1", in.SessionID)
	assert.Equal("Stop", in.HookEventName)
	assert.Equal("/repo", in.CWD)
	assert.True(in.StopHookActive)
	assert.Equal("Bash", in.ToolName)
	assert.Equal("git commit -m x", in.Command())
}

func TestDecodeInputGrokCamelCase(t *testing.T) {
	// Fixture shape from xai-org/grok-build HookEventEnvelope serialization
	// and crates/codegen/xai-grok-shell extensions/hooks.rs client_hook_dispatch test.
	assert := assert.New(t)
	in, err := DecodeInput(strings.NewReader(`{
		"hookEventName": "pre_tool_use",
		"sessionId": "abc-123",
		"cwd": "/Users/you/project",
		"workspaceRoot": "/Users/you/project",
		"permissionMode": "default",
		"toolName": "run_terminal_command",
		"toolUseId": "call_1",
		"toolInput": {"command": "npm test"},
		"toolInputTruncated": false,
		"timestamp": "2026-04-14T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal("abc-123", in.SessionID)
	assert.Equal("PreToolUse", in.HookEventName)
	assert.Equal("/Users/you/project", in.CWD)
	assert.Equal("run_terminal_command", in.ToolName)
	assert.Equal("call_1", in.ToolUseID)
	assert.Equal("npm test", in.Command())
}

func TestDecodeInputGrokStopEvent(t *testing.T) {
	assert := assert.New(t)
	in, err := DecodeInput(strings.NewReader(`{
		"hookEventName": "stop",
		"sessionId": "s2",
		"cwd": "/repo",
		"stopHookActive": false,
		"lastAssistantMessage": "done"
	}`))
	require.NoError(t, err)
	assert.Equal("s2", in.SessionID)
	assert.Equal("Stop", in.HookEventName)
	assert.Equal("done", in.LastAssistant)
	assert.False(in.StopHookActive)
}

func TestFirstBoolPrefersFirstKeyIncludingFalse(t *testing.T) {
	// Explicit false on the first recognized key must win over a later true alias.
	in, err := DecodeInput(strings.NewReader(`{
		"session_id": "s",
		"hook_event_name": "Stop",
		"stop_hook_active": false,
		"stopHookActive": true
	}`))
	require.NoError(t, err)
	assert.False(t, in.StopHookActive)
}

func TestNormalizeHookEventName(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("PreToolUse", NormalizeHookEventName("pre_tool_use"))
	assert.Equal("PreToolUse", NormalizeHookEventName("PreToolUse"))
	assert.Equal("PreToolUse", NormalizeHookEventName("preToolUse"))
	assert.Equal("PostToolUse", NormalizeHookEventName("post_tool_use"))
	assert.Equal("PostToolUse", NormalizeHookEventName("PostToolUse"))
	assert.Equal("Stop", NormalizeHookEventName("stop"))
	assert.Equal("Stop", NormalizeHookEventName("Stop"))
	assert.Empty(NormalizeHookEventName(""))
	assert.Equal("SessionStart", NormalizeHookEventName("SessionStart"))
}

func TestSessionStateMigratesLegacyStopCountToKnownLineage(t *testing.T) {
	var state SessionState
	err := json.Unmarshal([]byte(`{
		"stop_count_since_prompt": 3,
		"worktree_lineage_keys": {"worktree": "lineage"},
		"repo_heads": {"worktree": "abc123", "lineage": "abc123"}
	}`), &state)

	require.NoError(t, err)
	assert.Equal(t, map[string]int{"lineage": 3}, state.StopCountsSincePrompt)
}

func TestSessionStateResetsAmbiguousLegacyStopCount(t *testing.T) {
	var state SessionState
	err := json.Unmarshal([]byte(`{
		"stop_count_since_prompt": 3,
		"last_failed_review_repo": "/repo-a",
		"last_failed_review_branch": "main",
		"worktree_lineage_keys": {"worktree-a": "lineage-a", "worktree-b": "lineage-b"}
	}`), &state)

	require.NoError(t, err)
	assert.Empty(t, state.StopCountsSincePrompt)
}
