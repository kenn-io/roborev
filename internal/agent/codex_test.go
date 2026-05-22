package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupMockCodex(t *testing.T, unsafe bool, opts MockCLIOpts) (*CodexAgent, *MockCLIResult) {
	withUnsafeAgents(t, unsafe)
	mock := mockAgentCLI(t, opts)
	return NewCodexAgent(mock.CmdPath), mock
}

func buildStream(events ...string) string {
	return strings.Join(events, "\n") + "\n"
}

func TestCodex_buildArgs(t *testing.T) {
	a := NewCodexAgent("codex")

	tests := []struct {
		name             string
		agentic          bool
		autoApprove      bool
		sandboxBroken    bool
		wantFlags        []string
		wantMissingFlags []string
	}{
		{
			name:             "NonAgenticAutoApprove",
			agentic:          false,
			autoApprove:      true,
			wantFlags:        []string{"--sandbox", "read-only", "--json"},
			wantMissingFlags: []string{codexDangerousFlag, codexAutoApproveFlag},
		},
		{
			name:             "AgenticNoAutoApprove",
			agentic:          true,
			autoApprove:      false,
			wantFlags:        []string{codexDangerousFlag, "--json"},
			wantMissingFlags: []string{codexAutoApproveFlag},
		},
		{
			name:             "SandboxBrokenFallback",
			agentic:          false,
			autoApprove:      true,
			sandboxBroken:    true,
			wantFlags:        []string{codexDangerousFlag, "--json"},
			wantMissingFlags: []string{"--sandbox", codexAutoApproveFlag},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := a.buildArgs("/repo", tt.agentic, tt.autoApprove, tt.sandboxBroken)

			for _, flag := range tt.wantFlags {
				assert.Contains(t, args, flag, "buildArgs() missing expected flag %q, args: %v", flag, args)
			}

			for _, flag := range tt.wantMissingFlags {
				assert.NotContains(t, args, flag, "buildArgs() contains unexpected flag %q, args: %v", flag, args)
			}

			assert.Equal(t, "-", args[len(args)-1], "expected stdin marker '-' at end of args, got %v", args)
		})
	}
}

func TestCodexBuildArgsWithSessionResume(t *testing.T) {
	a := NewCodexAgent("codex").
		WithSessionID("session-123").
		WithModel("o4-mini").
		WithReasoning(ReasoningThorough).(*CodexAgent)

	args := a.buildArgs("/repo", false, true, false)

	assert.Equal(t, []string{
		"exec",
		"resume",
		"--json",
		"-c", codexReadOnlySandboxConfig,
		"-m", "o4-mini",
		"-c", `model_reasoning_effort="high"`,
		"session-123",
		"-",
	}, args)
	assert.NotContains(t, args, "--sandbox")
	assert.NotContains(t, args, "-C")
	assert.NotContains(t, args, "--add-dir")
	assert.Contains(t, args, codexReadOnlySandboxConfig)
}

func TestCodexBuildArgsCanDisableSkills(t *testing.T) {
	a := WithCodexSkillsDisabled(NewCodexAgent("codex"), true).(*CodexAgent)

	args := a.buildArgs("/repo", false, true, false)

	assert.Contains(t, args, "-c")
	assert.Contains(t, args, "skills.include_instructions=false")
}

func TestCodexBuildArgsLeavesSkillsEnabledByDefault(t *testing.T) {
	a := NewCodexAgent("codex")

	args := a.buildArgs("/repo", false, true, false)

	assert.NotContains(t, args, "skills.include_instructions=false")
}

func TestCodexBuildArgsCanIgnoreUserConfig(t *testing.T) {
	a := WithCodexUserConfigIgnored(NewCodexAgent("codex"), true).(*CodexAgent)

	args := a.buildArgs("/repo", false, true, false)

	assert.Contains(t, args, codexIgnoreUserConfigFlag)
}

func TestCodexBuildArgsLoadsUserConfigByDefault(t *testing.T) {
	a := NewCodexAgent("codex")

	args := a.buildArgs("/repo", false, true, false)

	assert.NotContains(t, args, codexIgnoreUserConfigFlag)
}

func TestCodexBuildArgsRejectsInvalidSessionResume(t *testing.T) {
	a := NewCodexAgent("codex").WithSessionID("-bad-session").(*CodexAgent)

	args := a.buildArgs("/repo", false, true, false)

	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, "exec", args[0])
	assert.NotEqual(t, "resume", args[1], "expected resume subcommand to be omitted for invalid session id, got %v", args)
	assertNotContainsArg(t, args, "-bad-session")
}

func TestCodexCommandLineOmitsRuntimeOnlyArgs(t *testing.T) {
	a := NewCodexAgent("codex").WithSessionID("session-123").WithModel("o4-mini").(*CodexAgent)

	cmdLine := a.CommandLine()

	assert.Contains(t, cmdLine, "exec resume --json")
	assert.Contains(t, cmdLine, "session-123")
	assert.NotContains(t, cmdLine, "--sandbox read-only")
	assert.Contains(t, cmdLine, codexReadOnlySandboxConfig)
	assert.NotContains(t, cmdLine, " -C ")
	assert.False(t, strings.HasSuffix(cmdLine, " -"), "command line should omit stdin marker: %q", cmdLine)
}

func TestCodexSupportsDangerousFlagAllowsNonZeroHelp(t *testing.T) {
	cmdPath := writeTempCommand(t, "#!/bin/sh\necho \"usage "+codexDangerousFlag+"\"; exit 1\n")

	supported, err := codexSupportsDangerousFlag(context.Background(), cmdPath, false)
	require.NoError(t, err)
	assert.True(t, supported, "expected dangerous flag support")
}

func TestCodexSupportsIgnoreUserConfigDetectsSupport(t *testing.T) {
	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$*" in
  "exec --ignore-user-config --help") echo "usage --ignore-user-config"; exit 0;;
esac
echo "unexpected args: $*" >&2
exit 1
`)

	supported, err := codexSupportsIgnoreUserConfig(context.Background(), cmdPath)
	require.NoError(t, err)
	assert.True(t, supported, "expected ignore-user-config support")
}

func TestCodexSupportsIgnoreUserConfigTreatsDirectProbeErrorsAsUnsupported(t *testing.T) {
	cases := []struct {
		name     string
		errorMsg string
	}{
		{"unknown flag", "error: unknown flag: --ignore-user-config"},
		{"no such option", "error: no such option: --ignore-user-config"},
		{"invalid option", "error: invalid option '--ignore-user-config'"},
		{"clap wasn't expected", "error: the argument '--ignore-user-config' wasn't expected"},
		{"bare bad option", "error: bad option --ignore-user-config"},
		{"echoes flag in plain help", "usage --ignore-user-config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "exec --ignore-user-config --help") echo %q >&2; exit 2;;
esac
echo "unexpected args: $*" >&2
exit 1
`, tc.errorMsg)
			cmdPath := writeTempCommand(t, script)

			supported, err := codexSupportsIgnoreUserConfig(context.Background(), cmdPath)
			require.NoError(t, err)
			assert.False(t, supported, "expected %q to be treated as unsupported", tc.errorMsg)
		})
	}
}

func TestCodexReviewUnsafeMissingFlagErrors(t *testing.T) {
	a, _ := setupMockCodex(t, true, MockCLIOpts{
		HelpOutput: "usage",
	})
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support")
}

func TestCodexReviewIncludesIgnoreUserConfigWhenSupported(t *testing.T) {
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox " + codexIgnoreUserConfigFlag,
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		},
	})
	a = WithCodexUserConfigIgnored(a, true).(*CodexAgent)

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assertContainsArg(t, args, codexIgnoreUserConfigFlag)
}

func TestCodexReviewUsesIgnoreUserConfigForPreflightChecks(t *testing.T) {
	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$*" in
  *--help*)
    case "$*" in
      *--ignore-user-config*) echo "usage --sandbox --ignore-user-config"; exit 0;;
    esac
    echo "broken config" >&2
    exit 1
    ;;
esac
echo '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
`)
	a := WithCodexUserConfigIgnored(NewCodexAgent(cmdPath), true).(*CodexAgent)

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)
}

func TestCodexReviewOmitsIgnoreUserConfigWhenUnsupported(t *testing.T) {
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox",
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		},
	})
	a = WithCodexUserConfigIgnored(a, true).(*CodexAgent)

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assertNotContainsArg(t, args, codexIgnoreUserConfigFlag)
}

func TestCodexReviewUsesSandboxNone(t *testing.T) {
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox",
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		},
	})

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err)
	argsStr := string(args)
	assert.Contains(t, argsStr, "--sandbox read-only",
		"expected --sandbox read-only in args, got: %s", strings.TrimSpace(argsStr))
	assert.NotContains(t, argsStr, codexAutoApproveFlag,
		"expected no %s in review mode, got: %s", codexAutoApproveFlag, strings.TrimSpace(argsStr))
}

func TestCodexReviewWithSessionResumePassesResumeArgs(t *testing.T) {
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox",
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		},
	})

	a = a.WithSessionID("session-123").(*CodexAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	require.GreaterOrEqual(t, len(args), 4)
	assert.Equal(t, "exec", args[0])
	assert.Equal(t, "resume", args[1])
	assertNotContainsArg(t, args, "--sandbox")
	assertNotContainsArg(t, args, "-C")
	assertContainsArg(t, args, codexReadOnlySandboxConfig)
	assertContainsArg(t, args, "session-123")
}

func TestCodexReviewTimeoutClosesStdoutPipe(t *testing.T) {
	skipIfWindows(t)

	prevWaitDelay := subprocessWaitDelay
	subprocessWaitDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		subprocessWaitDelay = prevWaitDelay
	})

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$*" in *--help*) echo "usage --sandbox"; exit 0;; esac
(sleep 0.2) &
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}'
exit 0
`)

	a := NewCodexAgent(cmdPath)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := a.Review(ctx, t.TempDir(), "deadbeef", "prompt", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.LessOrEqual(t, time.Since(start), time.Second, "Review hung after timeout")
}

func TestCodexParseStreamJSON(t *testing.T) {
	a := NewCodexAgent("codex")

	const (
		jsonThreadStarted = `{"type":"thread.started","thread_id":"abc123"}`
		jsonTurnStarted   = `{"type":"turn.started"}`
		jsonTurnCompleted = `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":50}}`
	)

	tests := []struct {
		name              string
		input             string
		want              string
		wantErr           error
		notWantErr        error
		wantWriterContent string
	}{
		{
			name: "AggregatesAgentMessages",
			input: buildStream(
				jsonThreadStarted,
				jsonTurnStarted,
				`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"First message"}}`,
				`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"Final review result"}}`,
				jsonTurnCompleted,
			),
			want: "First message\nFinal review result",
		},
		{
			name: "AggregatesByItemID",
			input: buildStream(
				`{"type":"item.updated","item":{"id":"item_1","type":"agent_message","text":"Draft"}}`,
				`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"Second message"}}`,
				`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Final first message"}}`,
			),
			want: "Final first message\nSecond message",
		},
		{
			name:  "EmptyStreamReturnsEmpty",
			input: buildStream(jsonThreadStarted, jsonTurnStarted, `{"type":"turn.completed","usage":{}}`),
			want:  "",
		},
		{
			name:       "TurnFailedReturnsError",
			input:      buildStream(`{"type":"turn.failed","error":{"message":"something broke"}}`),
			wantErr:    errCodexStreamFailed,
			notWantErr: errNoCodexJSON,
		},
		{
			name:       "ErrorEventReturnsError",
			input:      buildStream(`{"type":"error","message":"stream error"}`),
			wantErr:    errCodexStreamFailed,
			notWantErr: errNoCodexJSON,
		},
		{
			name: "IgnoresNonMessageItems",
			input: buildStream(
				`{"type":"item.started","item":{"id":"cmd1","type":"command_execution","command":"bash -lc ls"}}`,
				`{"type":"item.completed","item":{"id":"cmd1","type":"command_execution","command":"bash -lc ls","exit_code":0}}`,
				`{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"Done reviewing."}}`,
			),
			want: "Done reviewing.",
		},
		{
			name: "DropsNarrationBeforeToolCalls",
			input: buildStream(
				jsonThreadStarted,
				jsonTurnStarted,
				`{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"Checking the relevant files before I write the review."}}`,
				`{"type":"item.started","item":{"id":"cmd1","type":"command_execution","command":"sh -lc sed ..."}}`,
				`{"type":"item.completed","item":{"id":"cmd1","type":"command_execution","command":"sh -lc sed ...","exit_code":0}}`,
				`{"type":"item.completed","item":{"id":"msg2","type":"agent_message","text":"## Review Findings\n- **Severity**: Low; **Problem**: Final finding."}}`,
				jsonTurnCompleted,
			),
			want: "## Review Findings\n- **Severity**: Low; **Problem**: Final finding.",
		},
		{
			name: "PrefersFinalPostToolSegment",
			input: buildStream(
				jsonThreadStarted,
				jsonTurnStarted,
				`{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"## Review Findings\n- **Severity**: Low; **Problem**: Earlier provisional finding."}}`,
				`{"type":"item.started","item":{"id":"cmd1","type":"command_execution","command":"sh -lc rg ..."}}`,
				`{"type":"item.completed","item":{"id":"cmd1","type":"command_execution","command":"sh -lc rg ...","exit_code":0}}`,
				`{"type":"item.completed","item":{"id":"msg2","type":"agent_message","text":"## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding."}}`,
				jsonTurnCompleted,
			),
			want: "## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding.",
		},
		{
			name:              "StreamsToWriter",
			input:             buildStream(`{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`),
			want:              "hello",
			wantWriterContent: "agent_message",
		},
		{
			name:    "NoValidJSONReturnsError",
			input:   buildStream("plain text output", "another plain text line"),
			wantErr: errNoCodexJSON,
		},
		{
			name: "JSONWithoutCodexEventTypeReturnsError",
			input: buildStream(
				`{"foo":"bar"}`,
				`{"type":""}`,
				`{"type":"foo.bar"}`,
			),
			wantErr: errNoCodexJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			w := newSyncWriter(&buf) // Always initialize

			result, err := a.parseStreamJSON(strings.NewReader(tt.input), w)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, result, "parseStreamJSON() result = %q, want empty string on error", result)
			} else {
				require.NoError(t, err)
			}

			if tt.notWantErr != nil {
				require.Condition(t, func() bool { return !errors.Is(err, tt.notWantErr) }, "got unwanted error: %v", err)
			}

			if err == nil {
				assert.Equal(t, tt.want, result, "parseStreamJSON()")
			}

			if tt.wantWriterContent != "" {
				assert.Contains(t, buf.String(), tt.wantWriterContent, "writer output = %q, expected to contain %q", buf.String(), tt.wantWriterContent)
			}
		})
	}
}

func TestCodexReviewPipesPromptViaStdin(t *testing.T) {
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:   "usage --sandbox",
		CaptureStdin: true,
		StdoutLines: []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		},
	})

	testPrompt := "This is a test prompt with special chars: <>&\nand newlines"
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", testPrompt, nil)
	require.NoError(t, err)

	received, err := os.ReadFile(mock.StdinFile)
	require.NoError(t, err)
	assert.Equal(t, testPrompt, string(received), "prompt not piped correctly via stdin")
}

func TestCodexReviewNoValidJSONReturnsError(t *testing.T) {
	a, _ := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox",
		StdoutLines: []string{"plain text output"},
	})

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not emit valid --json events")
	assert.ErrorIs(t, err, errNoCodexJSON)
}
