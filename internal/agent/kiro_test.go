package agent

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripKiroOutput(t *testing.T) {
	// Build the "footer in content" input: footer-like line followed by 10+ content lines.
	footerInContentLines := []string{"> ## Review", "some content", "▸ Time: 10s"}
	for range 10 {
		footerInContentLines = append(footerInContentLines, "more content")
	}
	footerInContentInput := strings.Join(footerInContentLines, "\n")

	// Build the "blockquote not stripped" input: 31 chrome lines then a blockquote.
	blockquoteLines := make([]string, 31)
	for i := range blockquoteLines {
		blockquoteLines[i] = "chrome line"
	}
	blockquoteLines = append(blockquoteLines, "> this is a blockquote in review content", "more content")
	blockquoteInput := strings.Join(blockquoteLines, "\n")

	type stripKiroTest struct {
		name          string
		input         string
		wantContains  []string
		wantMissing   []string
		exactMatch    string
		wantNotPrefix string // if non-empty, output must not start with this
	}

	tests := []stripKiroTest{
		{
			name: "full ANSI and chrome",
			input: "\x1b[38;5;141m⠀⠀logo⠀⠀\x1b[0m\n" +
				"\x1b[38;5;244m╭─── Did you know? ───╮\x1b[0m\n" +
				"\x1b[38;5;244m│ tip text            │\x1b[0m\n" +
				"\x1b[38;5;244m╰─────────────────────╯\x1b[0m\n" +
				"\x1b[38;5;244mModel: auto\x1b[0m\n" +
				"\n" +
				"\x1b[38;5;141m> \x1b[0m\x1b[1m## Summary\x1b[0m\n" +
				"This commit does something.\n" +
				"\n" +
				"## Issues Found\n" +
				"\n" +
				" \u25b8 Time: 21s\n",
			wantContains:  []string{"## Summary", "## Issues Found"},
			wantMissing:   []string{"\x1b[", "Did you know", "Model:", "Time:"},
			wantNotPrefix: "> ",
		},
		{
			name:       "no marker",
			input:      "\x1b[1msome output without marker\x1b[0m\n",
			exactMatch: "some output without marker",
		},
		{
			name:          "bare marker",
			input:         "chrome\n>\nreview content here\n",
			wantContains:  []string{"review content here"},
			wantMissing:   []string{"chrome"},
			wantNotPrefix: ">",
		},
		{
			name:         "footer in content body",
			input:        footerInContentInput,
			wantContains: []string{"▸ Time: 10s"},
		},
		{
			name:         "footer with trailing blanks",
			input:        "> ## Review\ncontent\n ▸ Time: 12s\n\n\n\n\n\n\n\n",
			wantContains: []string{"content"},
			wantMissing:  []string{"Time:"},
		},
		{
			name:         "blockquote not stripped",
			input:        blockquoteInput,
			wantContains: []string{"> this is a blockquote"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripKiroOutput(tt.input)
			if tt.exactMatch != "" {
				assert.Equal(t, tt.exactMatch, got)
				return
			}
			for _, want := range tt.wantContains {
				assert.Contains(t, got, want)
			}
			for _, miss := range tt.wantMissing {
				assert.NotContains(t, got, miss)
			}
			if tt.wantNotPrefix != "" {
				assert.False(t, strings.HasPrefix(got, tt.wantNotPrefix),
					"output should not start with %q, got: %q", tt.wantNotPrefix, got)
			}
		})
	}
}

func TestKiroBuildArgs(t *testing.T) {
	a := NewKiroAgent("kiro-cli")

	args := a.buildArgs(false)
	assertContainsArg(t, args, "chat")
	assertContainsArg(t, args, "--no-interactive")
	assertNotContainsArg(t, args, "--trust-all-tools")

	args = a.buildArgs(true)
	assertContainsArg(t, args, "chat")
	assertContainsArg(t, args, "--no-interactive")
	assertContainsArg(t, args, "--trust-all-tools")
}

func TestKiroName(t *testing.T) {
	a := NewKiroAgent("")
	require.Equal(t, "kiro", a.Name(), "expected name 'kiro', got %s", a.Name())
	require.Equal(t, "kiro-cli", a.CommandName(), "expected command name 'kiro-cli', got %s", a.CommandName())
}

func TestKiroCommandLine(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	cl := a.CommandLine()
	if !strings.Contains(cl, "-- <prompt>") {
		assert.Contains(t, cl, "-- <prompt>",
			"CommandLine should include -- separator, got: %q", cl,
		)
	}
	if !strings.Contains(cl, "--no-interactive") {
		assert.Contains(t, cl, "--no-interactive",
			"CommandLine should include --no-interactive, got: %q", cl,
		)
	}
}

func TestKiroWithAgentic(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	require.False(t, a.Agentic, "expected non-agentic by default")

	a2 := a.WithAgentic(true).(*KiroAgent)
	require.True(t, a2.Agentic, "expected agentic after WithAgentic(true)")
	require.False(t, a.Agentic, "original should be unchanged")
}

func TestKiroWithReasoning(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	b := a.WithReasoning(ReasoningThorough).(*KiroAgent)
	assert.Equal(t, ReasoningThorough, b.Reasoning, "expected thorough reasoning, got %q", b.Reasoning)
	assert.Equal(t, ReasoningStandard, a.Reasoning, "original reasoning should be unchanged")
}

func TestKiroWithModelIsNoop(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	b := a.WithModel("some-model")
	assert.Equal(t, a, b, "WithModel should return the same agent (kiro does not support model selection)")
}

func TestKiroReviewSuccess(t *testing.T) {
	skipIfWindows(t)

	output := "LGTM: looks good to me"
	script := NewScriptBuilder().AddOutput(output).Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "review this commit", nil)
	require.NoError(t, err)
	if !strings.Contains(result, output) {
		require.Contains(t, result, output, "expected result to contain %q, got %q", output, result)
	}
}

func TestKiroReviewWritesOutputWriter(t *testing.T) {
	skipIfWindows(t)

	output := "review findings here"
	script := NewScriptBuilder().AddOutput(output).Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	var buf bytes.Buffer
	result, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "review", &buf)
	require.NoError(t, err)
	if !strings.Contains(result, output) {
		require.Contains(t, result, output, "expected result to contain output, got: %q", result)
	}
	if !strings.Contains(buf.String(), output) {
		require.Contains(t, buf.String(), output, "expected output writer to contain %q, got %q", output, buf.String())
	}
}

func TestKiroReviewFailure(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo "error: auth failed" >&2`).
		AddRaw("exit 1").
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "review this commit", nil)
	require.Error(t, err)

	if !strings.Contains(err.Error(), "kiro failed") {
		require.Contains(t, err.Error(), "kiro failed", "unexpected error: %v", err)
	}
}

func TestKiroReviewEmptyOutput(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().AddRaw("exit 0").Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "review this commit", nil)
	require.NoError(t, err)
	require.Equal(t, "No review output generated", result, "unexpected result: %q", result)
}

func TestKiroPassesPromptAsArg(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{"review complete"},
	})

	a := NewKiroAgent(mock.CmdPath)
	prompt := "Review this commit for issues"
	_, err := a.Review(context.Background(), t.TempDir(), "HEAD", prompt, nil)
	require.NoError(t, err, "Review failed")

	args, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err, "read args capture")
	if !strings.Contains(string(args), prompt) {
		assert.Contains(t, string(args), prompt, "expected prompt in args, got: %s", string(args))
	}
	if !strings.Contains(string(args), "--no-interactive") {
		assert.Contains(t, string(args), "--no-interactive", "expected --no-interactive in args, got: %s", string(args))
	}
	if !strings.Contains(string(args), " -- ") {
		assert.Contains(t, string(args), " -- ", "expected -- separator before prompt, got: %s", string(args))
	}
}

func TestKiroReviewAgenticMode(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{"review complete"},
	})

	a := NewKiroAgent(mock.CmdPath)
	a2 := a.WithAgentic(true).(*KiroAgent)

	_, err := a2.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
	require.NoError(t, err, "Review failed")

	args, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err, "read args capture")
	if !strings.Contains(string(args), "--trust-all-tools") {
		assert.Contains(t, string(args), "--trust-all-tools", "expected --trust-all-tools in args, got: %s", string(args))
	}
}

func TestKiroReviewAgenticModeFromGlobal(t *testing.T) {
	withUnsafeAgents(t, true)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{"review complete"},
	})

	a := NewKiroAgent(mock.CmdPath)
	_, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
	require.NoError(t, err, "Review failed")

	args, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err, "read args capture")
	if !strings.Contains(string(args), "--trust-all-tools") {
		require.Contains(t, string(args), "--trust-all-tools", "expected --trust-all-tools when global unsafe enabled, got: %s", strings.TrimSpace(string(args)))
	}
}

func TestKiroReviewStderrFallback(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo "Model: auto" >&2`).
		AddRaw(`echo "> review on stderr" >&2`).
		AddRaw(`echo " ▸ Time: 5s" >&2`).
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "review", nil)
	require.NoError(t, err)
	if !strings.Contains(result, "review on stderr") {
		require.Contains(t, result, "review on stderr", "expected stderr fallback, got: %q", result)
	}
	if strings.Contains(result, "Model:") {
		assert.NotContains(t, result, "Model:", "Kiro chrome should be stripped from stderr")
	}
	if strings.Contains(result, "Time:") {
		assert.NotContains(t, result, "Time:", "timing footer should be stripped from stderr")
	}
}

func TestKiroReviewStderrFallbackMarkerOnlyStdout(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo ">"`).
		AddRaw(`echo "> review from stderr" >&2`).
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(
		context.Background(), t.TempDir(),
		"deadbeef", "review", nil,
	)
	require.NoError(t, err)
	if !strings.Contains(result, "review from stderr") {
		require.Contains(t, result, "review from stderr", "expected stderr when stdout is marker-only, got: %q", result)
	}
}

func TestKiroReviewStderrPreferredOverStdoutNoise(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo "Model: auto"`).
		AddRaw(`echo "Loading..."`).
		AddRaw(`echo "> actual review content" >&2`).
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(
		context.Background(), t.TempDir(),
		"deadbeef", "review", nil,
	)
	require.NoError(t, err)
	if !strings.Contains(result, "actual review content") {
		require.Contains(t, result, "actual review content", "expected stderr review over stdout noise, got: %q", result)
	}
	if strings.Contains(result, "Loading") {
		assert.NotContains(t, result, "Loading", "stdout noise should not appear in result")
	}
}

func TestKiroReviewStderrFallbackNoMarker(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo "review text without marker" >&2`).
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(
		context.Background(), t.TempDir(),
		"deadbeef", "review", nil,
	)
	require.NoError(t, err)
	if !strings.Contains(result, "review text without marker") {
		require.Contains(t, result, "review text without marker", "expected stderr fallback even without marker, got: %q", result)
	}
}

func TestKiroReviewStdoutPreservedOverStderrNoise(t *testing.T) {
	skipIfWindows(t)

	script := NewScriptBuilder().
		AddRaw(`echo "plain review on stdout"`).
		AddRaw(`echo "warning: something" >&2`).
		Build()
	cmdPath := writeTempCommand(t, script)
	a := NewKiroAgent(cmdPath)

	result, err := a.Review(
		context.Background(), t.TempDir(),
		"deadbeef", "review", nil,
	)
	require.NoError(t, err)
	if !strings.Contains(result, "plain review on stdout") {
		require.Contains(t, result, "plain review on stdout", "expected stdout to be kept, got: %q", result)
	}
	if strings.Contains(result, "warning") {
		assert.NotContains(t, result, "warning", "stderr noise should not replace stdout content")
	}
}

func TestKiroReviewPromptTooLarge(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	bigPrompt := strings.Repeat("x", maxPromptArgLen+1)
	_, err := a.Review(context.Background(), t.TempDir(), "HEAD", bigPrompt, nil)
	require.Error(t, err)

	if !strings.Contains(err.Error(), "too large") {
		require.Contains(t, err.Error(), "too large", "unexpected error: %v", err)
	}
}

func TestKiroWithChaining(t *testing.T) {
	a := NewKiroAgent("kiro-cli")
	b := a.WithReasoning(ReasoningThorough).WithAgentic(true)
	kiro := b.(*KiroAgent)
	require.Equal(t, ReasoningThorough, kiro.Reasoning, "expected thorough reasoning, got %q", kiro.Reasoning)

	require.True(t, kiro.Agentic, "expected agentic true")
	require.Equal(t, "kiro-cli", kiro.Command, "expected command 'kiro-cli', got %q", kiro.Command)
}
