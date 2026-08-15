package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestPostAgentHookUsesRegularDaemonEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(agenthook.Response{
			SessionID: "session-1",
			Triggered: true,
		})
	}))
	t.Cleanup(server.Close)

	response, err := postAgentHook(
		context.Background(), strings.TrimPrefix(server.URL, "http://"),
		agenthook.Request{Event: agenthook.Input{SessionID: "session-1"}},
	)

	require.NoError(t, err)
	assert.Equal(t, "/api/agent-hook/event", gotPath)
	assert.Equal(t, "session-1", response.SessionID)
	assert.True(t, response.Triggered)
}

func TestRunAgentHookUsesConfiguredRegularDaemonEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(agenthook.Response{SessionID: "session-1"})
	}))
	t.Cleanup(server.Close)
	opts := agenthook.DefaultOptions()
	opts.RoborevServerAddr = strings.TrimPrefix(server.URL, "http://")

	err := runAgentHook(
		kitagenthook.AgentClaude,
		opts,
		strings.NewReader(`{"session_id":"session-1","hook_event_name":"Stop"}`),
		io.Discard,
		io.Discard,
	)

	require.NoError(t, err)
	assert.Equal(t, "/api/agent-hook/event", gotPath)
}

func TestManualAgentHookCommandsEnsureDaemon(t *testing.T) {
	origEnsureDaemon := agentHookEnsureDaemon
	ensureErr := errors.New("start daemon")
	agentHookEnsureDaemon = func() error { return ensureErr }
	t.Cleanup(func() { agentHookEnsureDaemon = origEnsureDaemon })

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "status", run: func() error { return runAgentHookStatus(io.Discard) }},
		{name: "reset", run: func() error {
			return runAgentHookReset(agenthook.ResetOptions{All: true}, "", io.Discard)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.run(), ensureErr)
		})
	}
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
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, _ string, req agenthook.Request) (agenthook.Response, error) {
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
	assert.JSONEq(t, `{"decision":"block","reason":"resolve reviews"}`, stdout.String())
}

// If a legacy or Grok encoder bypasses policy-aware output, users get different
// review handling depending on which supported hook registration they use.
func TestLegacyAndGrokAgentHooksAppendFixGuidelines(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, string, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	for _, tc := range []struct {
		name string
		run  func(agenthook.Options, io.Reader, io.Writer, io.Writer) error
	}{
		{name: "legacy", run: func(opts agenthook.Options, in io.Reader, out, errOut io.Writer) error {
			return runLegacyAgentHook(context.Background(), opts, in, out, errOut)
		}},
		{name: "grok", run: runGrokAgentHook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			opts := agenthook.DefaultOptions()
			opts.FixGuidelines = "Verify before editing."
			err := tc.run(opts, strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`), &stdout, io.Discard)
			require.NoError(t, err)

			var output map[string]any
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
			reason, ok := output["reason"].(string)
			require.True(t, ok)
			assert.True(t, strings.HasSuffix(strings.TrimSpace(reason), "Verify before editing."))
		})
	}
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

func TestRunAgentHookFailsOpenWhenDaemonUnavailable(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, string, agenthook.Request) (agenthook.Response, error) {
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
	postAgentHook = func(context.Context, string, agenthook.Request) (agenthook.Response, error) {
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
	assert.Equal(t, "resolve reviews", output["reason"])
}

// If kit-backed profiles omit policy composition, most supported hooks keep
// applying review findings without the user's evaluation policy.
func TestRunAgentHookAppendsFixGuidelinesToKitOutput(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, string, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	for _, profile := range []kitagenthook.Agent{kitagenthook.AgentQwen, kitagenthook.AgentDroid} {
		t.Run(string(profile), func(t *testing.T) {
			var stdout bytes.Buffer
			opts := agenthook.DefaultOptions()
			opts.FixGuidelines = "Verify before editing."
			err := runAgentHook(
				profile,
				opts,
				strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`),
				&stdout,
				io.Discard,
			)
			require.NoError(t, err)

			var output map[string]any
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
			reason, ok := output["reason"].(string)
			require.True(t, ok)
			assert.True(t, strings.HasSuffix(strings.TrimSpace(reason), "Verify before editing."))
		})
	}
}

// If the PostToolUse path is missed, commit-triggered reminders use a different
// policy contract from Stop-triggered reminders.
func TestRunAgentHookAppendsFixGuidelinesToPostToolUse(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, string, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	opts := agenthook.DefaultOptions()
	opts.FixGuidelines = "Verify before editing."
	err := runAgentHook(
		kitagenthook.AgentClaude,
		opts,
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`),
		&stdout,
		io.Discard,
	)
	require.NoError(t, err)

	var output struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	assert.True(t, strings.HasSuffix(strings.TrimSpace(output.HookSpecificOutput.AdditionalContext), "Verify before editing."))
}

func TestRunAgentHookCursorSuppressesUnsupportedControlOutput(t *testing.T) {
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, _ string, req agenthook.Request) (agenthook.Response, error) {
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
	postAgentHook = func(_ context.Context, _ string, req agenthook.Request) (agenthook.Response, error) {
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
			postAgentHook = func(_ context.Context, _ string, req agenthook.Request) (agenthook.Response, error) {
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
