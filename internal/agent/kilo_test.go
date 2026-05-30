//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiloReviewModelFlag(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)
	tests := []struct {
		name      string
		model     string
		wantFlag  bool
		wantModel string
	}{
		{
			name:     "no model omits --model from args",
			model:    "",
			wantFlag: false,
		},
		{
			name:      "explicit model passes --model to subprocess",
			model:     "openai/gpt-4o",
			wantFlag:  true,
			wantModel: "openai/gpt-4o",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, args, _ := runKiloMockReview(t, tt.model, "review this", nil)
			args = strings.TrimSpace(args)
			if tt.wantFlag {
				assertContains(t, args, "--model")
				assertContains(t, args, tt.wantModel)
			} else {
				assertNotContains(t, args, "--model")
			}
		})
	}
}

func TestKiloReviewPipesPromptViaStdin(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	prompt := "Review this commit carefully"
	_, args, stdin := runKiloMockReview(t, "", prompt, nil)
	assert.Equal(t, prompt, strings.TrimSpace(stdin), "stdin content = %q, want %q", stdin, prompt)

	assertNotContains(t, args, prompt)
}

func TestKiloReviewSessionFlag(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs:  true,
		CaptureStdin: true,
		StdoutLines:  []string{kiloTextEvent("ok")},
	})

	a := NewKiloAgent(mock.CmdPath).WithSessionID("ses_123").(*KiloAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
	require.NoError(t, err)

	argsBytes, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err)
	args := string(argsBytes)
	assertContains(t, args, "--session")
	assertContains(t, args, "ses_123")
}

func TestKiloReviewExtractsTextFromJSONL(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		kiloTextEvent("**Review:** Fix the typo."),
		kiloTextEvent("Done."),
	}

	result, _, _ := runKiloMockReview(t, "", "prompt", stdoutLines)
	assertContains(t, result, "**Review:** Fix the typo.")
	assertContains(t, result, "Done.")
}

func TestKiloReviewStripsANSIFromJSONL(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stdoutLines := []string{
		kiloTextEvent("\x1b[31mred\x1b[0m text"),
	}

	result, _, _ := runKiloMockReview(t, "", "prompt", stdoutLines)
	assertContains(t, result, "red text")
	assertNotContains(t, result, "\x1b[")
}

func TestKiloReviewStderrOnExitZero(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{"Error: Model not found: z-ai/glm-5:free"},
		ExitCode:    0,
	})

	a := NewKiloAgent(mock.CmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.Error(t, err)

	assertContains(t, err.Error(), "Model not found")
}

func TestKiloReviewNonZeroExitSurfacesStderr(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{"Error: Model not found: z-ai/glm-5:free"},
		ExitCode:    1,
	})

	a := NewKiloAgent(mock.CmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.Error(t, err)

	assertContains(t, err.Error(), "Model not found")
}

func TestKiloReviewStderrStripsANSI(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{
			"\x1b[0m\x1b[91m\x1b[0m\x1b[1mError: \x1b[0m\x1b[0mModel not found",
		},
		ExitCode: 1,
	})

	a := NewKiloAgent(mock.CmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.Error(t, err)

	assertContains(t, err.Error(), "Error: Model not found")
	assertNotContains(t, err.Error(), "\x1b[")
}

func TestKiloReviewNonZeroExitFallsBackToStdout(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		StdoutLines: []string{"fatal: something went wrong"},
		ExitCode:    1,
	})

	a := NewKiloAgent(mock.CmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.Error(t, err)

	assertContains(t, err.Error(), "something went wrong")
}

func TestKiloReviewExitZeroNonJSONStdout(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		StdoutLines: []string{
			"Error: invalid configuration",
		},
		ExitCode: 0,
	})

	a := NewKiloAgent(mock.CmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.Error(t, err)

	assertContains(t, err.Error(), "invalid configuration")
}

func TestKiloReviewExitZeroJSONOnlyNoText(t *testing.T) {
	t.Parallel()
	skipIfWindows(t)

	stepEvent := `{"type":"step","part":{"type":"tool","name":"read"}}`
	mock := mockAgentCLI(t, MockCLIOpts{
		StdoutLines: []string{stepEvent},
		ExitCode:    0,
	})

	a := NewKiloAgent(mock.CmdPath)
	result, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", "prompt", nil,
	)
	require.NoError(t, err)

	assertEqual(t, result, "No review output generated")
}

func kiloTextEvent(text string) string {
	part := map[string]string{"type": "text", "text": text}
	partJSON, err := json.Marshal(part)
	if err != nil {
		panic(fmt.Sprintf("kiloTextEvent: %v", err))
	}
	ev := map[string]json.RawMessage{
		"type": json.RawMessage(`"text"`),
		"part": partJSON,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		panic(fmt.Sprintf("kiloTextEvent: %v", err))
	}
	return string(b)
}

func runKiloMockReview(
	t *testing.T, model, prompt string, stdoutLines []string,
) (output, args, stdin string) {
	t.Helper()

	if stdoutLines == nil {
		stdoutLines = []string{kiloTextEvent("ok")}
	}

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs:  true,
		CaptureStdin: true,
		StdoutLines:  stdoutLines,
	})

	a := NewKiloAgent(mock.CmdPath)
	if model != "" {
		a.Model = model
	}

	out, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", prompt, nil,
	)
	require.NoError(t, err, "Review failed: %v")

	argsBytes, err := os.ReadFile(mock.ArgsFile)
	require.NoError(t, err, "failed to read args file: %v")

	stdinBytes, err := os.ReadFile(mock.StdinFile)
	require.NoError(t, err, "failed to read stdin file: %v")

	return out, string(argsBytes), string(stdinBytes)
}
