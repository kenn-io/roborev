package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorBuildArgs_Table(t *testing.T) {
	tests := []struct {
		name        string
		agentic     bool
		model       string
		wantArgs    []string
		excludeArgs []string
	}{
		{
			name:    "Review Mode (Default)",
			agentic: false,
			wantArgs: []string{
				"-p",
				"--output-format", "stream-json",
				"--model", "auto",
				"--mode", "plan",
			},
			excludeArgs: []string{"--force"},
		},
		{
			name:    "Agentic Mode",
			agentic: true,
			wantArgs: []string{
				"--force",
			},
			excludeArgs: []string{"--mode", "plan"},
		},
		{
			name:     "Custom Model",
			agentic:  false,
			model:    "gpt-5.2-codex-high",
			wantArgs: []string{"--model", "gpt-5.2-codex-high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewCursorAgent("agent")
			if tt.model != "" {
				var ok bool
				a, ok = a.WithModel(tt.model).(*CursorAgent)
				require.True(t, ok, "expected *CursorAgent")

			}

			args := a.buildArgs(tt.agentic)

			for _, want := range tt.wantArgs {
				assertContainsArg(t, args, want)
			}
			for _, exclude := range tt.excludeArgs {
				assertNotContainsArg(t, args, exclude)
			}
		})
	}
}

func TestCursorCommandLineUsesBuildArgs(t *testing.T) {
	a := NewCursorAgent("agent").WithModel("gpt-5.2-codex-high").WithAgentic(true).(*CursorAgent)

	want := strings.Join(a.buildArgs(true), " ")
	assert.Equal(t, "agent "+want, a.CommandLine())
}

func setupMockCursorAgent(t *testing.T, opts MockCLIOpts) (*CursorAgent, *MockCLIResult) {
	t.Helper()
	skipIfWindows(t)
	mock := mockAgentCLI(t, opts)
	return NewCursorAgent(mock.CmdPath), mock
}

func TestCursorReviewPassesModelFlag(t *testing.T) {
	a, mock := setupMockCursorAgent(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{`{"type":"result","result":"ok"}`},
	})

	b := a.WithModel("test-model")
	cursor, ok := b.(*CursorAgent)
	require.True(t, ok, "expected *CursorAgent, got %T", b)

	_, err := cursor.Review(context.Background(), t.TempDir(), "head", "test prompt", nil)
	require.NoError(t, err, "Review failed: %v")

	args := readMockArgs(t, mock.ArgsFile)
	assertContainsArg(t, args, "--model")
	assertContainsArg(t, args, "test-model")
}

func TestCursorParseStreamJSON(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedResult string
	}{
		{
			name: "ResultEvent",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"Reviewing..."}}
{"type":"result","result":"LGTM"}
`,
			expectedResult: "LGTM",
		},
		{
			name: "AssistantFallback",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"First"}}
{"type":"assistant","message":{"content":"Second"}}
`,
			expectedResult: "First\nSecond",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cursor := NewCursorAgent("dummy")
			res, err := cursor.parseStreamJSON(strings.NewReader(tt.input), nil)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedResult, res, "expected %q, got %q", tt.expectedResult, res)
		})
	}
}

func TestCursorWithChaining(t *testing.T) {
	a := NewCursorAgent("agent")

	b := a.WithModel("m1").WithReasoning(ReasoningThorough).WithAgentic(true)
	cursor, ok := b.(*CursorAgent)
	require.True(t, ok, "expected *CursorAgent, got %T", b)
	assert.Equal(t, "m1", cursor.Model, "expected model m1, got %q", cursor.Model)
	assert.Equal(t, ReasoningThorough, cursor.Reasoning, "expected thorough reasoning, got %q", cursor.Reasoning)

	assert.True(t, cursor.Agentic, "expected agentic true")
	assert.Equal(t, "agent", cursor.Command, "expected command 'agent', got %q", cursor.Command)
}

func TestCursorReviewPipesPromptViaStdin(t *testing.T) {
	a, mock := setupMockCursorAgent(t, MockCLIOpts{
		CaptureArgs:  true,
		CaptureStdin: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	prompt := "Review this commit carefully"
	_, err := a.Review(
		context.Background(), t.TempDir(), "HEAD", prompt, nil,
	)
	require.NoError(t, err, "Review failed: %v")

	assertFileContent(t, mock.StdinFile, prompt)

	assertFileNotContains(t, mock.ArgsFile, prompt)
}

func TestCursorReviewEmptyOutput(t *testing.T) {
	a, _ := setupMockCursorAgent(t, MockCLIOpts{
		StdoutLines: []string{`{"type":"system","subtype":"init"}`},
	})

	result, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.NoError(t, err)

	assert.Equal(t, "No review output generated", result)
}

func TestCursorReviewStreamsProgressToOutput(t *testing.T) {
	a, _ := setupMockCursorAgent(t, MockCLIOpts{
		StdoutLines: []string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Reviewing..."}]}}`,
			`{"type":"result","result":"ok"}`,
		},
	})

	var output strings.Builder
	result, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", &output,
	)
	require.NoError(t, err)

	assert.Equal(t, "ok", result)
	assert.Contains(t, output.String(), `"type":"assistant"`)
	assert.Contains(t, output.String(), `"Reviewing..."`)
}

func TestCursorReviewErrorResult(t *testing.T) {
	a, _ := setupMockCursorAgent(t, MockCLIOpts{
		StdoutLines: []string{
			`{"type":"system","subtype":"init"}`,
			`{"type":"result","is_error":true,"result":"","error":{"message":"Invalid API key"}}`,
		},
	})

	_, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "Invalid API key")
}

func TestCursorReviewErrorResultNonZeroExit(t *testing.T) {
	a, _ := setupMockCursorAgent(t, MockCLIOpts{
		StdoutLines: []string{
			`{"type":"system","subtype":"init"}`,
			`{"type":"result","is_error":true,"result":"","error":{"message":"quota exceeded"}}`,
		},
		ExitCode: 1,
	})

	_, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "quota exceeded")
	assert.Contains(t, errStr, "failed")
}

func TestCursorName(t *testing.T) {
	a := NewCursorAgent("")
	assert.Equal(t, "cursor", a.Name())
	assert.Equal(t, "agent", a.CommandName())
}
