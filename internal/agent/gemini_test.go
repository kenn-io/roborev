package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGeminiTempCommand(t *testing.T, script string) string {
	t.Helper()
	// Fake current Gemini CLI unless the script defines its own version response.
	parts := strings.SplitN(script, "\n", 2)
	if len(parts) != 2 {
		t.Fatal("Gemini test command must have a shebang and script body")
	}
	body := parts[1]
	if !strings.Contains(body, `"$1" = "--version"`) {
		body = "if [ \"$1\" = \"--version\" ]; then echo 0.34.0; exit 0; fi\n" + body
	}
	return writeTempCommand(t, parts[0]+"\ncase \"$1\" in *etxtbsy*) exit 0;; esac\n"+body)
}

func TestGeminiBuildArgs(t *testing.T) {
	tests := []struct {
		name         string
		agentic      bool
		wantFlags    []string          // Standalone boolean flags
		wantArgPairs map[string]string // Flag -> exact next token
		unwantedArgs []string          // Tokens expected NOT in args
	}{
		{
			name:    "ReviewMode",
			agentic: false,
			wantArgPairs: map[string]string{
				"--output-format": "stream-json",
				"--approval-mode": "plan",
			},
			unwantedArgs: []string{
				"--yolo",
				"--allowed-tools",
			},
		},
		{
			name:    "AgenticMode",
			agentic: true,
			wantArgPairs: map[string]string{
				"--output-format": "stream-json",
				"--approval-mode": "yolo",
			},
			unwantedArgs: []string{
				"--allowed-tools",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			a := NewGeminiAgent("gemini")
			args := a.buildArgs(tc.agentic)

			// Check standalone boolean flags
			for _, flag := range tc.wantFlags {
				assert.Contains(args, flag, "missing flag %q", flag)
			}

			// Check flag-value pairs by exact next token
			for flag, val := range tc.wantArgPairs {
				assertFlagValue(t, args, flag, val)
			}

			// Check absence of specific tokens
			for _, unwanted := range tc.unwantedArgs {
				assert.NotContains(args, unwanted, "unexpected token %q", unwanted)
			}
		})
	}
}

func TestGeminiReviewGatesCapabilitiesByVersion(t *testing.T) {
	skipIfWindows(t)
	tests := []struct {
		name         string
		version      string
		versionFails bool
		agentic      bool
		wantStream   bool
		wantResume   bool
		wantApproval string
	}{
		{name: "0.10 plain output", version: "0.10.0", wantApproval: "default"},
		{name: "0.10 agentic mode", version: "0.10.0", agentic: true, wantApproval: "yolo"},
		{name: "0.11 stream without resume", version: "0.11.0", wantStream: true, wantApproval: "default"},
		{name: "0.14 has no resume", version: "0.14.0", wantStream: true, wantApproval: "default"},
		{name: "0.15 supports resume", version: "0.15.0", wantStream: true, wantResume: true, wantApproval: "default"},
		{name: "0.33 keeps plan experimental", version: "0.33.0", wantStream: true, wantResume: true, wantApproval: "default"},
		{name: "0.34 supports plan", version: "0.34.0", wantStream: true, wantResume: true, wantApproval: "plan"},
		{name: "unparseable version", version: "unknown", wantApproval: "default"},
		{name: "failed version probe", version: "0.34.0", versionFails: true, wantApproval: "default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			t.Setenv("GEMINI_VERSION", tc.version)
			versionFail := "0"
			if tc.versionFails {
				versionFail = "1"
			}
			t.Setenv("GEMINI_VERSION_FAIL", versionFail)
			t.Setenv("ARGS_FILE", argsFile)
			script := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then
  if [ "$GEMINI_VERSION_FAIL" = "1" ]; then
    echo "version unavailable" >&2
    exit 1
  fi
  printf '%s\n' "$GEMINI_VERSION"
  exit 0
fi
printf '%s\n' "$@" > "$ARGS_FILE"
stream_json=0
has_resume=0
has_prompt=0
for arg in "$@"; do
  if [ "$arg" = "stream-json" ]; then stream_json=1; fi
  if [ "$arg" = "--resume" ]; then has_resume=1; fi
  if [ "$arg" = "--prompt" ]; then has_prompt=1; fi
done
if [ "$has_resume" = "1" ] && [ "$has_prompt" = "0" ]; then
  echo 'When resuming a session, provide --prompt or stdin' >&2
  exit 1
fi
if [ "$stream_json" = "1" ]; then
  if [ "$has_resume" = "1" ]; then
    echo '{"type":"init","session_id":"old-session"}'
  else
    echo '{"type":"init","session_id":"new-session"}'
  fi
  echo '{"type":"message","role":"assistant","content":"Stream review"}'
  echo '{"type":"result","status":"success"}'
else
  echo 'Plain review'
fi
`)
			savedSession := "old-session"
			var capturedSessions []string
			var resetCalls int
			capture := NewSessionCaptureWriter(&bytes.Buffer{}, func(sessionID string) {
				savedSession = sessionID
				capturedSessions = append(capturedSessions, sessionID)
			}, func() error {
				savedSession = ""
				resetCalls++
				return nil
			})
			a := NewGeminiAgent(script).WithSessionID("old-session").WithAgentic(tc.agentic).(*GeminiAgent)
			result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)
			require.NoError(t, err)
			if tc.wantStream {
				assert.Equal(t, "Stream review", result)
			} else {
				assert.Equal(t, "Plain review", result)
			}

			argsBytes, readErr := os.ReadFile(argsFile)
			require.NoError(t, readErr)
			args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
			if tc.wantStream {
				assertFlagValue(t, args, "--output-format", "stream-json")
			} else {
				assert.NotContains(t, args, "--output-format")
			}
			if tc.wantResume {
				assertFlagValue(t, args, "--resume", "old-session")
				assertFlagValue(t, args, "--prompt", "Review the supplied input.")
				assert.Equal(t, "old-session", capture.SessionID())
				assert.Equal(t, "old-session", savedSession)
				assert.Equal(t, []string{"old-session"}, capturedSessions)
				assert.Zero(t, resetCalls)
			} else {
				assert.NotContains(t, args, "--resume")
				assert.Empty(t, capture.SessionID())
				assert.Empty(t, savedSession)
				assert.Equal(t, 2, resetCalls)
				if tc.wantStream {
					assert.Equal(t, []string{"new-session"}, capturedSessions)
				} else {
					assert.Empty(t, capturedSessions)
				}
			}
			if tc.wantApproval == "" {
				assert.NotContains(t, args, "--approval-mode")
			} else {
				assertFlagValue(t, args, "--approval-mode", tc.wantApproval)
			}
		})
	}
}

func TestGeminiReadOnlyReviewOverridesAutoEdit(t *testing.T) {
	skipIfWindows(t)

	repo := t.TempDir()
	path := filepath.Join(repo, "reviewed.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	t.Setenv("REVIEWED_FILE", path)
	script := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "0.33.0"; exit 0; fi
mode=auto_edit
expect_mode=0
for arg in "$@"; do
  if [ "$expect_mode" = "1" ]; then mode="$arg"; expect_mode=0; fi
  if [ "$arg" = "--approval-mode" ]; then expect_mode=1; fi
done
if [ "$mode" = "auto_edit" ]; then printf modified > "$REVIEWED_FILE"; fi
echo '{"type":"init","session_id":"session-1"}'
echo '{"type":"message","role":"assistant","content":"Review complete"}'
echo '{"type":"result","status":"success"}'
`)
	a := NewGeminiAgent(script)

	_, err := a.Review(t.Context(), repo, "sha", "review", io.Discard)

	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "original", string(got))
}

func TestGeminiAntigravityBuildArgs(t *testing.T) {
	tests := []struct {
		name         string
		agentic      bool
		wantFlags    []string
		unwantedArgs []string
	}{
		{
			name:    "ReviewMode",
			agentic: false,
			// Print-mode reviews omit --sandbox so `pwd` is not gated, and
			// still omit --dangerously-skip-permissions (agentic-only).
			unwantedArgs: []string{
				"--output-format",
				"--approval-mode",
				"-m",
				"--dangerously-skip-permissions",
				"--sandbox",
				// A bare --print would swallow --print-timeout as the
				// prompt; the prompt is passed via --prompt at run time.
				"--print",
			},
		},
		{
			name:      "AgenticMode",
			agentic:   true,
			wantFlags: []string{"--dangerously-skip-permissions"},
			unwantedArgs: []string{
				"--output-format",
				"--approval-mode",
				"-m",
				"--sandbox",
				"--print",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewGeminiAgent("agy").WithModel("gemini-1.5-pro").(*GeminiAgent)
			args := a.buildArgs(tc.agentic)

			assertFlagValue(t, args, "--print-timeout", "30m")
			for _, flag := range tc.wantFlags {
				assert.Contains(t, args, flag)
			}
			for _, unwanted := range tc.unwantedArgs {
				assert.NotContains(t, args, unwanted)
			}
		})
	}
}

func TestGeminiWithSessionIDPreservesCloneSemantics(t *testing.T) {
	a := NewGeminiAgent("gemini").WithModel("gemini-1.5-pro").WithReasoning(ReasoningThorough).WithAgentic(true).(*GeminiAgent)
	assert.Empty(t, a.SessionID)

	a2 := a.WithSessionID("session-123").(*GeminiAgent)
	assert.Equal(t, "session-123", a2.SessionID)
	assert.Equal(t, "gemini-1.5-pro", a2.Model)
	assert.Equal(t, ReasoningThorough, a2.Reasoning)
	assert.True(t, a2.Agentic)
	assert.Empty(t, a.SessionID, "original agent session ID must remain empty")
}

func TestGeminiBuildArgsWithSessionResume(t *testing.T) {
	t.Run("GeminiCLIValidResume", func(t *testing.T) {
		a := NewGeminiAgent("gemini").WithSessionID("session-123").(*GeminiAgent)
		args := a.buildArgs(false)
		assertFlagValue(t, args, "--resume", "session-123")
	})

	t.Run("GeminiCLIRejectsBadSessionFlag", func(t *testing.T) {
		a := NewGeminiAgent("gemini").WithSessionID("-bad-session").(*GeminiAgent)
		args := a.buildArgs(false)
		assert.NotContains(t, args, "--resume")
		assert.NotContains(t, args, "-bad-session")
	})

	t.Run("AntigravityValidConversation", func(t *testing.T) {
		a := NewGeminiAgent("agy").WithSessionID("conv-123").(*GeminiAgent)
		args := a.buildArgs(false)
		assertFlagValue(t, args, "--conversation", "conv-123")
	})

	t.Run("AntigravityRejectsBadSessionFlag", func(t *testing.T) {
		a := NewGeminiAgent("agy").WithSessionID("-bad-session").(*GeminiAgent)
		args := a.buildArgs(false)
		assert.NotContains(t, args, "--conversation")
		assert.NotContains(t, args, "-bad-session")
	})
}

func TestGeminiPlanningCommandLineStripsSessionID(t *testing.T) {
	t.Run("GeminiCLI", func(t *testing.T) {
		a := NewGeminiAgent("gemini").WithSessionID("session-123").WithAgentic(true).(*GeminiAgent)
		cmd := a.PlanningCommandLine()
		assert.NotContains(t, cmd, "session-123")
		assert.NotContains(t, cmd, "--resume")
		assert.Contains(t, cmd, "--approval-mode plan")
	})

	t.Run("AntigravityCLI", func(t *testing.T) {
		a := NewGeminiAgent("agy").WithSessionID("conv-123").WithAgentic(true).(*GeminiAgent)
		cmd := a.PlanningCommandLine()
		assert.NotContains(t, cmd, "conv-123")
		assert.NotContains(t, cmd, "--conversation")
		assert.NotContains(t, cmd, "--dangerously-skip-permissions")
	})
}

func TestGeminiAntigravityReviewMergesSettingsAndOmitsYolo(t *testing.T) {
	skipIfWindows(t)

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, settingsPath, map[string]any{
		"model": "keep-me",
		"permissions": map[string]any{
			"allow": []any{"command(git)"},
			"deny":  []any{"command(rm -rf)"},
		},
	})
	prev := antigravitySettingsPathForTest
	antigravitySettingsPathForTest = func() string { return settingsPath }
	t.Cleanup(func() { antigravitySettingsPathForTest = prev })

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.1"; exit 0; fi
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Review after settings merge"
`)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", argsFile)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "Review after settings merge", res)

	assertSettingsAllow(t, settingsPath, "read_file(*)", "command(wc)", "command(pwd)", "command(git)")
	doc := readSettings(t, settingsPath)
	assert.Equal(t, "keep-me", doc["model"])
	permissions := doc["permissions"].(map[string]any)
	assert.Equal(t, []any{"command(rm -rf)"}, permissions["deny"])

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	argsOut := string(argsBytes)
	assert.NotContains(t, argsOut, "--sandbox\n")
	assert.NotContains(t, argsOut, "--dangerously-skip-permissions\n")
}

func TestGeminiAntigravityReviewAddsRepositoryWorkspace(t *testing.T) {
	skipIfWindows(t)

	repoPath := filepath.Join(t.TempDir(), "ci-worktree")
	require.NoError(t, os.Mkdir(repoPath, 0o755))
	argsFile := filepath.Join(t.TempDir(), "args")
	pwdFile := filepath.Join(t.TempDir(), "pwd")
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("PWD_FILE", pwdFile)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.1"; exit 0; fi
pwd > "$PWD_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Workspace review"
`)
	a := NewGeminiAgent(filepath.Join(filepath.Dir(scriptPath), "agy"))
	require.NoError(t, os.Rename(scriptPath, a.Command))

	res, err := a.Review(t.Context(), repoPath, "sha", "prompt", &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "Workspace review", res)

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	assertFlagValue(t, args, "--add-dir", repoPath)
	assert.NotContains(t, args, "--project")

	pwdBytes, readErr := os.ReadFile(pwdFile)
	require.NoError(t, readErr)
	assert.Equal(t, repoPath+"\n", string(pwdBytes))
}

func TestGeminiAntigravityReviewStopsWhenSettingsLockCanceled(t *testing.T) {
	skipIfWindows(t)

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, settingsPath, map[string]any{
		"permissions": map[string]any{"allow": []any{}},
	})
	prev := antigravitySettingsPathForTest
	antigravitySettingsPathForTest = func() string { return settingsPath }
	t.Cleanup(func() { antigravitySettingsPathForTest = prev })

	lock := flock.New(settingsPath+".lock", flock.SetPermissions(0o600))
	require.NoError(t, lock.Lock())
	t.Cleanup(func() {
		_ = lock.Unlock()
		require.NoError(t, lock.Close())
	})

	invokedPath := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("INVOKED_PATH", invokedPath)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
touch "$INVOKED_PATH"
exit 0
`)
	commandPath := filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, commandPath))

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, reviewErr := NewGeminiAgent(commandPath).Review(ctx, t.TempDir(), "sha", "prompt", &bytes.Buffer{})
		require.ErrorIs(t, reviewErr, context.DeadlineExceeded)
	})

	_, err := os.Stat(invokedPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGeminiDetectsAntigravityCommandNames(t *testing.T) {
	tests := []string{
		"agy",
		"agy.exe",
		"/usr/local/bin/agy",
		`C:\Users\marius\AppData\Local\bin\agy.exe`,
	}

	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			a := NewGeminiAgent(command)
			assert.True(t, a.usesAntigravity())
		})
	}
}

func assertFlagValue(t *testing.T, args []string, flag, expectedVal string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	count := 0
	for _, a := range args {
		if a == flag {
			count++
		}
	}
	require.Equal(1, count, "flag %q count in args %v", flag, args)

	idx := slices.Index(args, flag)
	require.Less(idx+1, len(args), "flag %q is last arg", flag)
	assert.Equal(expectedVal, args[idx+1], "flag %q value", flag)
}

func TestGeminiParseStreamJSON(t *testing.T) {
	geminiCapture, err := os.ReadFile("testdata/gemini-stream.jsonl")
	require.NoError(t, err)
	tests := []struct {
		name          string
		input         string
		want          string
		wantErr       error
		wantErrString string
	}{
		{
			name:  "CapturedGeminiCLIOutput",
			input: string(geminiCapture),
			want:  `{"passed":true,"findings":[]}`,
		},
		{
			name: "ConcatenatesAssistantMessageDeltas",
			input: `{"type":"message","role":"assistant","content":"First ","delta":true}
{"type":"message","role":"assistant","content":"second.","delta":true}
{"type":"result","status":"success","stats":{"total_tokens":1}}
`,
			want: "First second.",
		},
		{
			name: "KeepsOnlyTextAfterToolUse",
			input: `{"type":"message","role":"assistant","content":"Checking the file.","delta":true}
{"type":"tool_use","tool_name":"read_file","parameters":{"path":"main.go"}}
{"type":"tool_result","content":"package main"}
{"type":"message","role":"assistant","content":"No issues found.","delta":true}
{"type":"result","status":"success","stats":{"total_tokens":1}}
`,
			want: "No issues found.",
		},
		{
			name: "FailedResult",
			input: `{"type":"result","status":"error"}
`,
			wantErrString: `status "error"`,
		},
		{
			name: "ResultEventRequired",
			input: `{"type":"init","session_id":"s"}
{"type":"message","role":"assistant","content":"partial response","delta":true}
`,
			wantErr: errNoStreamJSON,
		},
		{
			name: "PlainTextError",
			input: `This is a plain text review.
No issues found in the code.
`,
			wantErr: errNoStreamJSON,
		},
	}

	a := NewGeminiAgent("gemini")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			parsed, err := a.parseStreamJSON(strings.NewReader(tc.input), nil)

			if tc.wantErr != nil {
				require.Error(err)
				require.ErrorIs(err, tc.wantErr)
				return
			}
			if tc.wantErrString != "" {
				require.Error(err)
				assert.Contains(err.Error(), tc.wantErrString)
				return
			}
			require.NoError(err)
			assert.Equal(tc.want, parsed.result)
		})
	}
}

func TestGeminiAntigravityStreamJSONSessionCapture(t *testing.T) {
	skipIfWindows(t)

	fixture, err := filepath.Abs(filepath.Join("testdata", "antigravity-stream.jsonl"))
	require.NoError(t, err)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.8"; exit 0; fi
printf '%s\n' "$@" > "$ARGS_FILE"
cat "$STREAM_FILE"
`)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("STREAM_FILE", fixture)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	var output bytes.Buffer
	capture := NewSessionCaptureWriter(&output, nil)
	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)
	require.NoError(t, err)
	assert.Equal(t, "Git rebase destructively rewrites a branch's commit history by systematically detaching its unique commits and sequentially reapplying them onto a new base commit.", res)
	assert.Equal(t, "c3b66b04-872b-4fbe-a3a4-058a026ef20a", capture.SessionID())

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	assertFlagValue(t, args, "--output-format", "stream-json")
}

func TestGeminiAntigravityStreamJSONRequiresResultEvent(t *testing.T) {
	skipIfWindows(t)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.8"; exit 0; fi
echo '{"event":"init","conversation_id":"c3b66b04-872b-4fbe-a3a4-058a026ef20a"}'
echo '{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"partial"}}'
`)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	_, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", io.Discard)
	require.ErrorIs(t, err, errNoStreamJSON)
}

func TestGeminiAntigravityRejectsTimedOutResult(t *testing.T) {
	input := `{"event":"init","conversation_id":"conversation-123"}
{"event":"result","result":{"status":"SUCCESS","response":"partial response","finished_reason":"print_timeout"}}
`
	_, err := NewGeminiAgent("agy").parseAntigravityStreamJSON(strings.NewReader(input), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "print_timeout")
}

func TestGeminiAntigravityResumeSessionPassesConversationFlag(t *testing.T) {
	skipIfWindows(t)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.8"; exit 0; fi
printf '%s\n' "$@" > "$ARGS_FILE"
cat "$STREAM_FILE"
`)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", argsFile)
	fixture, err := filepath.Abs(filepath.Join("testdata", "antigravity-stream.jsonl"))
	require.NoError(t, err)
	t.Setenv("STREAM_FILE", fixture)
	a := NewGeminiAgent(scriptPath).WithSessionID("c3b66b04-872b-4fbe-a3a4-058a026ef20a").(*GeminiAgent)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	var output bytes.Buffer
	capture := NewSessionCaptureWriter(&output, nil)
	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)
	require.NoError(t, err)
	assert.Contains(t, res, "Git rebase destructively rewrites")
	assert.Equal(t, "c3b66b04-872b-4fbe-a3a4-058a026ef20a", capture.SessionID())

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	assertFlagValue(t, args, "--output-format", "stream-json")
	assertFlagValue(t, args, "--conversation", "c3b66b04-872b-4fbe-a3a4-058a026ef20a")
}

func TestGeminiResumeFailureStartsFresh(t *testing.T) {
	skipIfWindows(t)

	argsFile := filepath.Join(t.TempDir(), "args")
	fixture, err := filepath.Abs(filepath.Join("testdata", "gemini-stream.jsonl"))
	require.NoError(t, err)
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("STREAM_FILE", fixture)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
printf '%s\n' "$@" >> "$ARGS_FILE"
printf '%s\n' '---' >> "$ARGS_FILE"
for arg in "$@"; do
  if [ "$arg" = "--resume" ]; then
    echo '{"type":"init","session_id":"old-session"}'
    exit 1
  fi
done
cat "$STREAM_FILE"
`)

	var output bytes.Buffer
	var captured []string
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, func(sessionID string) {
		captured = append(captured, sessionID)
	}, func() error {
		resetCalls++
		return nil
	})
	a := NewGeminiAgent(scriptPath).WithSessionID("old-session").(*GeminiAgent)
	result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)

	require.NoError(t, err)
	assert.JSONEq(t, `{"passed":true,"findings":[]}`, result)
	assert.Equal(t, 1, resetCalls)
	assert.Equal(t, []string{"old-session", "dba45695-e094-4cfb-80a4-663bff07a3ed"}, captured)
	assert.Equal(t, "dba45695-e094-4cfb-80a4-663bff07a3ed", capture.SessionID())

	args, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(args), "--resume\n"))
}

func TestGeminiAgenticResumeWithNoOutputDoesNotRetry(t *testing.T) {
	skipIfWindows(t)

	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("COUNT_FILE", countFile)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
printf 'run\n' >> "$COUNT_FILE"
echo '{"type":"init","session_id":"old-session"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`)
	var output bytes.Buffer
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, nil, func() error {
		resetCalls++
		return nil
	})
	a := NewGeminiAgent(scriptPath).WithSessionID("old-session").WithAgentic(true).(*GeminiAgent)
	result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)

	require.NoError(t, err)
	assert.Equal(t, "No review output generated", result)
	assert.True(t, a.Agentic)
	assert.Equal(t, 0, resetCalls)
	assert.Equal(t, "old-session", capture.SessionID())
	runs, readErr := os.ReadFile(countFile)
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(runs), "run\n"), "%q", runs)
}

func TestGeminiAgenticResumeFailureDoesNotRepeatWork(t *testing.T) {
	skipIfWindows(t)

	runsFile := filepath.Join(t.TempDir(), "runs")
	editsFile := filepath.Join(t.TempDir(), "edits")
	t.Setenv("RUNS_FILE", runsFile)
	t.Setenv("EDITS_FILE", editsFile)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
printf 'run\n' >> "$RUNS_FILE"
printf 'edit\n' >> "$EDITS_FILE"
for arg in "$@"; do
  if [ "$arg" = "--resume" ]; then
    echo '{"type":"init","session_id":"old-session"}'
    echo '{"type":"result","status":"error"}'
    exit 0
  fi
done
echo '{"type":"init","session_id":"fresh-session"}'
echo '{"type":"message","role":"assistant","content":"done"}'
echo '{"type":"result","status":"success"}'
`)
	var output bytes.Buffer
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, nil, func() error {
		resetCalls++
		return nil
	})
	a := NewGeminiAgent(scriptPath).WithSessionID("old-session").WithAgentic(true).(*GeminiAgent)
	_, err := a.Review(t.Context(), t.TempDir(), "sha", "make one edit", capture)

	require.Error(t, err)
	assert.Equal(t, "old-session", capture.SessionID())
	assert.Zero(t, resetCalls)
	runs, readErr := os.ReadFile(runsFile)
	require.NoError(t, readErr)
	assert.Equal(t, "run\n", string(runs))
	edits, readErr := os.ReadFile(editsFile)
	require.NoError(t, readErr)
	assert.Equal(t, "edit\n", string(edits))
}

func TestAntigravityResumeFailureStartsFresh(t *testing.T) {
	skipIfWindows(t)

	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", argsFile)
	fixture, err := filepath.Abs(filepath.Join("testdata", "antigravity-second-turn.jsonl"))
	require.NoError(t, err)
	t.Setenv("STREAM_FILE", fixture)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.8"; exit 0; fi
printf '%s\n' "$@" >> "$ARGS_FILE"
printf '%s\n' '---' >> "$ARGS_FILE"
for arg in "$@"; do
  if [ "$arg" = "--conversation" ]; then
    echo '{"event":"init","conversation_id":"c3b66b04-872b-4fbe-a3a4-058a026ef20a"}'
    echo '{"event":"result","result":{"status":"ERROR","error":"conversation not found"}}'
    exit 0
  fi
done
cat "$STREAM_FILE"
`)
	a := NewGeminiAgent(scriptPath).WithSessionID("c3b66b04-872b-4fbe-a3a4-058a026ef20a").(*GeminiAgent)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	var output bytes.Buffer
	var captured []string
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, func(sessionID string) {
		captured = append(captured, sessionID)
	}, func() error {
		resetCalls++
		return nil
	})
	result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)

	require.NoError(t, err)
	assert.Equal(t, "apple", result)
	assert.Equal(t, 1, resetCalls)
	assert.Equal(t, []string{
		"c3b66b04-872b-4fbe-a3a4-058a026ef20a",
		"9ec58bfd-4d67-4f5e-83a5-9d907e9c6b1f",
	}, captured)
	assert.Equal(t, "9ec58bfd-4d67-4f5e-83a5-9d907e9c6b1f", capture.SessionID())

	args, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(args), "--conversation\n"))
}

func TestAntigravitySuccessfulResumeStoresReturnedConversation(t *testing.T) {
	skipIfWindows(t)

	fixture, err := filepath.Abs(filepath.Join("testdata", "antigravity-stream.jsonl"))
	require.NoError(t, err)
	t.Setenv("STREAM_FILE", fixture)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.8"; exit 0; fi
cat "$STREAM_FILE"
`)
	agyPath := filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, agyPath))
	a := NewGeminiAgent(agyPath).WithSessionID("expired-session").(*GeminiAgent)

	var output bytes.Buffer
	storedSession := "expired-session"
	var captured []string
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, func(sessionID string) {
		captured = append(captured, sessionID)
		if storedSession == "" {
			storedSession = sessionID
		}
	}, func() error {
		storedSession = ""
		resetCalls++
		return nil
	})

	result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)

	require.NoError(t, err)
	assert.Contains(t, result, "Git rebase destructively rewrites")
	assert.Equal(t, "c3b66b04-872b-4fbe-a3a4-058a026ef20a", capture.SessionID())
	assert.Equal(t, "c3b66b04-872b-4fbe-a3a4-058a026ef20a", storedSession)
	assert.Equal(t, 1, resetCalls)
	assert.Equal(t, []string{
		"c3b66b04-872b-4fbe-a3a4-058a026ef20a",
		"c3b66b04-872b-4fbe-a3a4-058a026ef20a",
	}, captured)
}

func TestGeminiAntigravityReviewPlainText(t *testing.T) {
	skipIfWindows(t)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.1"; exit 0; fi
cat > "$STDIN_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Plain text review output"
echo '{"example":"json-snippet","findings":[]}'
echo '{"event":"result","result":{"status":"SUCCESS","response":"quoted JSON example"}}'
echo "No issues found."
`)

	stdinFile := filepath.Join(t.TempDir(), "stdin")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("STDIN_FILE", stdinFile)
	t.Setenv("ARGS_FILE", argsFile)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	var output bytes.Buffer
	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &output)

	require.NoError(t, err)
	assert.Equal(t, "Plain text review output\n{\"example\":\"json-snippet\",\"findings\":[]}\n{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"quoted JSON example\"}}\nNo issues found.", res)
	assert.Contains(t, output.String(), "Plain text review output")

	// agy >= 1.1.1 takes the prompt as the value of --prompt, not from stdin.
	stdinBytes, readErr := os.ReadFile(stdinFile)
	require.NoError(t, readErr)
	assert.Empty(t, string(stdinBytes))

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	argsOut := string(argsBytes)
	assert.Contains(t, argsOut, "--prompt\nprompt\n")
	assert.NotContains(t, argsOut, "--print\n")
	assert.NotContains(t, argsOut, "--output-format\n")
	assert.NotContains(t, argsOut, "--sandbox\n")
	assert.NotContains(t, argsOut, "--dangerously-skip-permissions\n")
}

func TestGeminiAntigravityLegacyStdinContract(t *testing.T) {
	skipIfWindows(t)

	// Old agy (<= 1.1.0) read the prompt from stdin with a bare --print.
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.0"; exit 0; fi
cat > "$STDIN_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Legacy review output"
echo "No issues found."
`)

	stdinFile := filepath.Join(t.TempDir(), "stdin")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("STDIN_FILE", stdinFile)
	t.Setenv("ARGS_FILE", argsFile)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	var output bytes.Buffer
	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &output)

	require.NoError(t, err)
	assert.Equal(t, "Legacy review output\nNo issues found.", res)

	// Prompt arrives on stdin; args carry a bare --print and no --prompt.
	stdinBytes, readErr := os.ReadFile(stdinFile)
	require.NoError(t, readErr)
	assert.Equal(t, "prompt\n", string(stdinBytes))

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	argsOut := string(argsBytes)
	assert.Contains(t, argsOut, "--print\n")
	assert.NotContains(t, argsOut, "--prompt\n")
	assert.NotContains(t, argsOut, "--output-format\n")
}

func TestGeminiAntigravityEmptyOutput(t *testing.T) {
	skipIfWindows(t)

	// agy >= 1.1.3 can exit 0 after soft-denying tools in headless print
	// mode; empty review output must error so the worker retries and fails
	// over. Agentic runs keep the non-fatal placeholder: fix jobs are
	// judged by their worktree patch, not text output.
	silentScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.3"; exit 0; fi
exit 0
`
	denialScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.3"; exit 0; fi
echo 'jetski: no output produced - a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied. Add an allow-rule under permissions.allow in settings.json (e.g. read_file(<target>)).' >&2
exit 0
`
	tests := []struct {
		name         string
		script       string
		agentic      bool
		unsafeGlobal bool
		wantResult   string
		wantInError  []string
	}{
		{
			name:        "SilentExitIsError",
			script:      silentScript,
			wantInError: []string{"produced no review output"},
		},
		{
			// allow_unsafe_agents changes tool permissions, not what a
			// review must produce: empty output stays an error.
			name:         "ReviewErrorsEvenWithUnsafeAgents",
			script:       silentScript,
			unsafeGlobal: true,
			wantInError:  []string{"produced no review output"},
		},
		{
			name:        "PermissionDenialHint",
			script:      denialScript,
			wantInError: []string{"produced no review output", "permissions.allow", "read_file(*)", "auto-denied"},
		},
		{
			name:       "AgenticStaysNonFatal",
			script:     silentScript,
			agentic:    true,
			wantResult: "No review output generated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scriptPath := writeGeminiTempCommand(t, tt.script)
			ga := NewGeminiAgent(scriptPath)
			ga.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
			require.NoError(t, os.Rename(scriptPath, ga.Command))
			withUnsafeAgents(t, tt.unsafeGlobal)
			var a Agent = ga
			if tt.agentic {
				a = a.WithAgentic(true)
			}

			var output bytes.Buffer
			res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &output)

			if tt.wantInError == nil {
				require.NoError(t, err)
				assert.Equal(t, tt.wantResult, res)
				return
			}
			require.Error(t, err)
			assert.Empty(t, res)
			for _, want := range tt.wantInError {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestUTF16CodeUnits(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(0, utf16CodeUnits(""))
	assert.Equal(3, utf16CodeUnits("abc"))
	assert.Equal(1, utf16CodeUnits("é"))          // é, BMP, 2 UTF-8 bytes but 1 UTF-16 unit
	assert.Equal(2, utf16CodeUnits("\U0001D11E")) // musical symbol, surrogate pair
	assert.Equal(4, utf16CodeUnits("a\U0001D11Eb"))
}

func TestGeminiAntigravityPromptTooLargeForArgv(t *testing.T) {
	skipIfWindows(t)

	// Overflowing the platform argv cap must not error. New agy (>= 1.1.1)
	// still reads the prompt from non-TTY stdin when no --prompt/--print/-p
	// flag is passed (google-antigravity/antigravity-cli#582).
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.1.1"; exit 0; fi
cat > "$STDIN_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Large prompt review output"
echo "No issues found."
`)
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("STDIN_FILE", stdinFile)
	t.Setenv("ARGS_FILE", argsFile)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	big := strings.Repeat("x", antigravityMaxPromptArgLen()+1)
	res, err := a.Review(t.Context(), t.TempDir(), "sha", big, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "Large prompt review output\nNo issues found.", res)

	stdinBytes, readErr := os.ReadFile(stdinFile)
	require.NoError(t, readErr)
	assert.Equal(t, big+"\n", string(stdinBytes))

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	argsOut := string(argsBytes)
	assert.Contains(t, argsOut, "--print-timeout\n")
	assert.NotContains(t, argsOut, "--prompt\n")
	assert.NotContains(t, argsOut, "--print\n")
	assert.NotContains(t, argsOut, "-p\n")
	assert.NotContains(t, argsOut, "--sandbox\n")
}

func TestGeminiAntigravityVersionProbeFailureDefaultsToPromptFlag(t *testing.T) {
	skipIfWindows(t)

	// If `agy --version` fails, detection defaults to the current (flag) contract.
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "boom" >&2; exit 1; fi
cat > "$STDIN_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
echo "Review output"
echo "No issues found."
`)

	stdinFile := filepath.Join(t.TempDir(), "stdin")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("STDIN_FILE", stdinFile)
	t.Setenv("ARGS_FILE", argsFile)
	a := NewGeminiAgent(scriptPath)
	a.Command = filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, a.Command))

	res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "Review output\nNo issues found.", res)

	// Defaulted to the flag contract: prompt via --prompt, stdin empty.
	stdinBytes, readErr := os.ReadFile(stdinFile)
	require.NoError(t, readErr)
	assert.Empty(t, string(stdinBytes))

	argsBytes, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)
	assert.Contains(t, string(argsBytes), "--prompt\nprompt\n")
	assert.NotContains(t, string(argsBytes), "--output-format\n")
}

func TestAntigravityVersionUsesPromptFlag(t *testing.T) {
	assert := assert.New(t)
	// New contract (>= 1.1.1).
	assert.True(antigravityVersionUsesPromptFlag("1.1.1"))
	assert.True(antigravityVersionUsesPromptFlag("1.1.2"))
	assert.True(antigravityVersionUsesPromptFlag("1.2.0"))
	assert.True(antigravityVersionUsesPromptFlag("2.0.0"))
	assert.True(antigravityVersionUsesPromptFlag("v1.1.1"))
	assert.True(antigravityVersionUsesPromptFlag("1.1.1-beta"))
	// Old stdin contract (<= 1.1.0).
	assert.False(antigravityVersionUsesPromptFlag("1.1.0"))
	assert.False(antigravityVersionUsesPromptFlag("1.0.16"))
	assert.False(antigravityVersionUsesPromptFlag("1.1")) // 1.1.0
	// Decorated / multi-token output resolves the first dotted version token.
	assert.True(antigravityVersionUsesPromptFlag("1.1.1 (abc123)"))
	assert.True(antigravityVersionUsesPromptFlag("agy 2.0.0\n"))
	assert.False(antigravityVersionUsesPromptFlag("agy version 1.1.0"))
	// Unparseable, empty, or dot-less output defaults to the current (flag) contract.
	assert.True(antigravityVersionUsesPromptFlag(""))
	assert.True(antigravityVersionUsesPromptFlag("unknown"))
	assert.True(antigravityVersionUsesPromptFlag("12345"))
}

func TestAntigravityVersionSupportsStreamJSON(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{version: "1.1.8", want: true},
		{version: "1.2.0", want: true},
		{version: "1.1.7", want: false},
		{version: "1.1.1", want: false},
		{version: "unknown", want: false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			assert.Equal(t, tc.want, antigravityVersionSupports(tc.version, antigravityStreamJSONVersion))
		})
	}
}

func TestGeminiAntigravityExplicitModelFailsClearly(t *testing.T) {
	a := NewGeminiAgent("agy").WithModel("gemini-1.5-pro")

	_, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support explicit Gemini model selection")
}

func TestGeminiAutoSelectedAntigravityExplicitModelFailsClearly(t *testing.T) {
	skipIfWindows(t)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
echo "unexpected antigravity invocation"
`)
	agyPath := filepath.Join(filepath.Dir(scriptPath), "agy")
	require.NoError(t, os.Rename(scriptPath, agyPath))

	a := NewGeminiAgent(agyPath)
	a.CommandAuto = true
	a = a.WithModel("gemini-1.5-pro").(*GeminiAgent)

	_, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support explicit Gemini model selection")
}

func TestGemini_Review_Integration(t *testing.T) {
	skipIfWindows(t)

	tests := []struct {
		name       string
		script     string
		wantResult string
		checkErr   func(t *testing.T, err error)
	}{
		{
			name: "PlainTextError",
			script: `#!/bin/sh
echo "Plain text review output"
echo "No issues found."
`,
			checkErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "stream-json")
				require.ErrorIs(t, err, errNoStreamJSON)
			},
		},
		{
			name: "PlainTextErrorWithStderr",
			script: `#!/bin/sh
echo "Plain text review output"
echo "Some stderr message" >&2
`,
			checkErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "Some stderr message")
			},
		},
		{
			name: "LargeStderrPreserved",
			script: `#!/bin/sh
echo "Plain text"
yes "This is a long stderr line that will contribute to the total size" | head -n 200 >&2
`,
			checkErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strings.Repeat("This is a long stderr line that will contribute to the total size\n", 200))
			},
		},
		{
			name: "StreamJSON_Success",
			script: `#!/bin/sh
echo '{"type":"message","role":"assistant","content":"Review complete. All good!"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`,
			wantResult: "Review complete. All good!",
		},
		{
			name: "StreamJSONNoResult",
			script: `#!/bin/sh
echo '{"type":"tool","name":"Read","input":{"path":"foo.go"}}'
echo '{"type":"tool_result","content":"file contents here"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`,
			wantResult: "No review output generated",
		},
		{
			name: "IOError",
			script: `#!/bin/sh
echo "Error message" >&2
exit 1
`,
			checkErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "gemini failed")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			scriptPath := writeGeminiTempCommand(t, tc.script)
			a := NewGeminiAgent(scriptPath)
			var output bytes.Buffer

			res, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", &output)

			if tc.checkErr != nil {
				tc.checkErr(t, err)
				return
			}
			require.NoError(err)
			assert.Equal(tc.wantResult, res)
		})
	}
}

func TestGeminiReview_ModelNotFoundFallback(t *testing.T) {
	skipIfWindows(t)

	// Script that fails with "model not found" when -m is passed,
	// and succeeds without it (simulating a retired default model).
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-m" ]; then
    echo "Error: model is not found for API version v1" >&2
    exit 1
  fi
done
echo '{"type":"message","role":"assistant","content":"Review from default model"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`)

	a := NewGeminiAgent(scriptPath)
	var output bytes.Buffer
	res, err := a.Review(
		t.Context(), t.TempDir(), "sha", "prompt", &output,
	)
	require.NoError(t, err)
	assert.Equal(t, "Review from default model", res)
}

func TestGeminiReview_ModelNotFoundLateStderr(t *testing.T) {
	skipIfWindows(t)

	// Regression: stderr emitted only at exit (after stdout closes).
	// Previously stderrStr was read before cmd.Wait(), racing with
	// the goroutine writing stderr.
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
for arg in "$@"; do
  if [ "$arg" = "-m" ]; then
    echo '{"type":"init","session_id":"retired-model-session"}'
    # Close stdout before writing stderr, simulating late error output.
    exec 1>&-
    sleep 0.05
    echo "Error: model is not found for API version v1" >&2
    exit 1
  fi
done
echo '{"type":"init","session_id":"fallback-session"}'
echo '{"type":"message","role":"assistant","content":"Review from default model"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`)

	a := NewGeminiAgent(scriptPath)
	var output bytes.Buffer
	result, err := a.Review(
		t.Context(), t.TempDir(), "sha", "prompt", &output,
	)
	require.NoError(t, err)
	assert.Equal(t, "Review from default model", result)
}

func TestGeminiReview_ModelNotFoundFallbackKeepsFinalSession(t *testing.T) {
	skipIfWindows(t)

	runsFile := filepath.Join(t.TempDir(), "runs")
	t.Setenv("RUNS_FILE", runsFile)
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
printf 'run\n' >> "$RUNS_FILE"
for arg in "$@"; do
  if [ "$arg" = "--resume" ]; then
    echo '{"type":"init","session_id":"resumed-session"}'
    echo '{"type":"result","status":"error"}'
    exit 0
  fi
done
for arg in "$@"; do
  if [ "$arg" = "-m" ]; then
    echo '{"type":"init","session_id":"retired-model-session"}'
    echo "Error: model is not found for API version v1" >&2
    exit 1
  fi
done
echo '{"type":"init","session_id":"fallback-session"}'
echo '{"type":"message","role":"assistant","content":"Review from default model"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`)

	savedSession := ""
	var output bytes.Buffer
	resetCalls := 0
	capture := NewSessionCaptureWriter(&output, func(sessionID string) {
		savedSession = sessionID
	}, func() error {
		savedSession = ""
		resetCalls++
		return nil
	})
	a := NewGeminiAgent(scriptPath).WithSessionID("resume-session").(*GeminiAgent)
	result, err := a.Review(t.Context(), t.TempDir(), "sha", "prompt", capture)

	require.NoError(t, err)
	assert.Equal(t, "Review from default model", result)
	assert.Equal(t, "fallback-session", capture.SessionID())
	assert.Equal(t, "fallback-session", savedSession)
	assert.Equal(t, 2, resetCalls)
	runs, readErr := os.ReadFile(runsFile)
	require.NoError(t, readErr)
	assert.Equal(t, "run\nrun\nrun\n", string(runs))
}

func TestGeminiReview_ModelNotFoundNoRetryWhenNoModel(t *testing.T) {
	skipIfWindows(t)

	// When Model is empty, don't retry on model-not-found.
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
echo "Error: model is not found" >&2
exit 1
`)

	a := NewGeminiAgent(scriptPath)
	a.Model = ""
	var output bytes.Buffer
	_, err := a.Review(
		t.Context(), t.TempDir(), "sha", "prompt", &output,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gemini failed")
}

func TestGeminiReview_ExplicitModelNoFallback(t *testing.T) {
	skipIfWindows(t)

	// When the model is not the built-in default, model-not-found
	// errors should fail fast with exactly one invocation.
	counterFile := filepath.Join(t.TempDir(), "invocations")
	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
echo "invoked" >> "$COUNTER_FILE"
echo "Error: model is not found for API version v1" >&2
exit 1
`)

	t.Setenv("COUNTER_FILE", counterFile)
	a := NewGeminiAgent(scriptPath).WithModel("user-specified-model")
	var output bytes.Buffer
	_, err := a.Review(
		t.Context(), t.TempDir(), "sha", "prompt", &output,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gemini failed")

	countBytes, readErr := os.ReadFile(counterFile)
	require.NoError(t, readErr)
	lines := strings.Count(string(countBytes), "invoked")
	assert.Equal(t, 1, lines,
		"explicit model should invoke exactly once, not retry")
}

func TestIsModelNotFoundError(t *testing.T) {
	tests := []struct {
		stderr string
		want   bool
	}{
		{"models/gemini-3.1-pro is not found", true},
		{"Error: model is not found for API version v1", true},
		{"Model not found: gemini-old", true},
		{"NOT_FOUND: model gemini-xyz not_found", true},
		{"quota exceeded for model gemini-2.5-pro", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range tests {
		got := isModelNotFoundError(tc.stderr)
		assert.Equal(t, tc.want, got, "isModelNotFoundError(%q)", tc.stderr)
	}
}

func TestGeminiReview_PromptDeliveredViaStdin(t *testing.T) {
	assert := assert.New(t)

	skipIfWindows(t)

	stdinFile := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("MOCK_STDIN_FILE", stdinFile)

	scriptPath := writeGeminiTempCommand(t, `#!/bin/sh
cat > "$MOCK_STDIN_FILE"
echo '{"type":"message","role":"assistant","content":"Done"}'
echo '{"type":"result","status":"success","stats":{"total_tokens":1}}'
`)

	a := NewGeminiAgent(scriptPath)
	var output bytes.Buffer
	expectedPrompt := "Please review this code for security issues"
	_, err := a.Review(t.Context(), t.TempDir(), "abc123", expectedPrompt, &output)
	require.NoError(t, err, "Review failed")

	// Verify the prompt was received
	receivedPrompt, err := os.ReadFile(stdinFile)
	require.NoError(t, err, "failed to read prompt file")
	assert.Equal(expectedPrompt, string(receivedPrompt), "prompt not delivered correctly")
}
