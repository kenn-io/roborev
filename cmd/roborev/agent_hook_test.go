package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
)

func TestAgentHookInstallSupportsExplicitQwenProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	cmd := agentHookCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{
		"install", "--agent", "qwen", "--config", path,
		"--command", "roborev agent-hook run --agent qwen",
	})

	require.NoError(t, cmd.Execute())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "agent-hook run --agent qwen")
	assert.Contains(t, string(body), "--source=roborev-agent-hook")
}

func TestAgentHookInstallRejectsMultiProfileConfigOverride(t *testing.T) {
	cmd := agentHookCmd()
	cmd.SetArgs([]string{"install", "--agent", "all", "--config", "hooks.json"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.ErrorContains(t, err, "--config requires one explicit agent")
}

func TestAgentHookInstallRejectsBinaryWithCommand(t *testing.T) {
	cmd := agentHookCmd()
	cmd.SetArgs([]string{
		"install", "--agent", "qwen", "--binary", "/opt/roborev",
		"--command", "roborev agent-hook run --agent qwen",
	})

	err := cmd.Execute()

	require.Error(t, err)
	assert.ErrorContains(t, err, "--binary and --command cannot be used together")
}

func TestAgentHookRunSupportsLegacyProfilelessRegistration(t *testing.T) {
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		got = req
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	cmd := agentHookCmd()
	cmd.SetIn(strings.NewReader(`{"session_id":"legacy-1","hook_event_name":"Stop"}`))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"run"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, "legacy-1", got.Event.SessionID)
	assert.JSONEq(t, `{"decision":"block","reason":"resolve reviews If Roborev issues are found, fix them, then continue the task you were doing before this hook interrupted you."}`, stdout.String())
}

func TestAgentHookRemovedFlagsAreRejected(t *testing.T) {
	for _, flag := range []string{"--codex-config", "--claude-config", "--scope"} {
		t.Run(flag, func(t *testing.T) {
			cmd := agentHookCmd()
			cmd.SetArgs([]string{"install", flag, "value"})

			err := cmd.Execute()

			require.Error(t, err)
			assert.ErrorContains(t, err, "unknown flag")
		})
	}
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
	oldPost := postAgentHook
	postAgentHook = func(context.Context, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{}, errors.New("daemon unavailable")
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout, stderr bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentClaude,
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		&stdout,
		&stderr,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, stdout.String())
	assert.Contains(t, stderr.String(), "daemon unavailable")
}

func TestRunAgentHookEncodesKitStopResponse(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentQwen,
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`),
		&stdout,
		io.Discard,
	)

	require.NoError(t, err)
	var output map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	assert.Equal(t, "block", output["decision"])
	assert.Contains(t, output["reason"], "resolve reviews")
	assert.Contains(t, output["reason"], "continue the task")
}

func TestRunAgentHookCursorSuppressesUnsupportedControlOutput(t *testing.T) {
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		got = req
		return agenthook.Response{Triggered: true, Reason: "must not be encoded"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentCursor,
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"s1","hook_event_name":"stop"}`),
		&stdout,
		io.Discard,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, stdout.String())
	assert.Equal(t, agenthook.DefaultTurnThreshold, got.Threshold)
	assert.Equal(t, agenthook.DefaultCommitThreshold, got.CommitThreshold)
	assert.Equal(t, agenthook.DefaultFailedReviewThreshold, got.FailedReviewThreshold)
}

func TestRunAgentHookHermesDefersPostToolReminder(t *testing.T) {
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		got = req
		return agenthook.Response{Triggered: true, Reason: "defer this"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentHermes,
		agenthook.DefaultOptions(),
		strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"post_tool_call",
  "tool_name":"terminal",
  "tool_input":{"command":"git status"},
  "extra":{"result":"ok","tool_call_id":"call-1","turn_id":"turn-1"}
}`),
		&stdout,
		io.Discard,
	)

	require.NoError(t, err)
	assert.True(t, got.DeferPostToolReminder)
	assert.JSONEq(t, `{}`, stdout.String())
}

func TestRunAgentHookPreservesNormalizedEventFields(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(*testing.T, agenthook.Input)
	}{
		{
			name: "pre tool use",
			payload: `{"session_id":"s1","cwd":"/repo","hook_event_name":"PreToolUse",` +
				`"tool_name":"Bash","tool_input":{"command":"git status"},"tool_use_id":"tool-1"}`,
			check: func(t *testing.T, input agenthook.Input) {
				assert.Equal(t, "tool-1", input.ToolUseID)
				assert.Equal(t, "git status", input.Command())
			},
		},
		{
			name: "post tool use",
			payload: `{"session_id":"s1","cwd":"/repo","hook_event_name":"PostToolUse",` +
				`"tool_name":"Bash","tool_input":{"command":"go test ./..."},` +
				`"tool_response":{"output":"ok"},"tool_use_id":"tool-2"}`,
			check: func(t *testing.T, input agenthook.Input) {
				assert.Equal(t, "tool-2", input.ToolUseID)
				assert.JSONEq(t, `{"output":"ok"}`, string(input.ToolResponse))
			},
		},
		{
			name: "stop",
			payload: `{"session_id":"s1","cwd":"/repo","hook_event_name":"Stop",` +
				`"stop_hook_active":true,"last_assistant_message":"done"}`,
			check: func(t *testing.T, input agenthook.Input) {
				assert.True(t, input.StopHookActive)
				assert.Equal(t, "done", input.LastAssistant)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldPost := postAgentHook
			var got agenthook.Request
			postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
				got = req
				return agenthook.Response{}, nil
			}
			t.Cleanup(func() { postAgentHook = oldPost })

			err := runAgentHook(
				kitagenthook.AgentClaude,
				agenthook.DefaultOptions(),
				strings.NewReader(tt.payload),
				io.Discard,
				io.Discard,
			)

			require.NoError(t, err)
			assert.Equal(t, "/repo", got.Event.CWD)
			tt.check(t, got.Event)
		})
	}
}
