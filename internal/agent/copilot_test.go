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

func TestCopilotSupportsAllowAllTools(t *testing.T) {
	skipIfWindows(t)

	t.Run("supported", func(t *testing.T) {
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput: "Usage: copilot [flags]\n\n  --allow-all-tools  Auto-approve all tool calls",
		})
		supported, err := copilotSupportsAllowAllTools(context.Background(), mock.CmdPath)
		require.NoError(t, err)
		assert.True(t, supported)
	})

	t.Run("not supported", func(t *testing.T) {
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput: "Usage: copilot [flags]\n\n  --model  Model to use",
		})
		supported, err := copilotSupportsAllowAllTools(context.Background(), mock.CmdPath)
		require.NoError(t, err)
		assert.False(t, supported)
	})
}

func TestCopilotSupportsStreamOff(t *testing.T) {
	skipIfWindows(t)

	t.Run("supported", func(t *testing.T) {
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput: "Usage: copilot [flags]\n\n  --stream string  Control streaming output",
		})
		supported, err := copilotSupportsStreamOff(context.Background(), mock.CmdPath)
		require.NoError(t, err)
		assert.True(t, supported)
	})

	t.Run("not supported", func(t *testing.T) {
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput: "Usage: copilot [flags]\n\n  --model  Model to use",
		})
		supported, err := copilotSupportsStreamOff(context.Background(), mock.CmdPath)
		require.NoError(t, err)
		assert.False(t, supported)
	})
}

func TestCopilotReviewParsesJSONOutput(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput:   "--allow-all-tools\n--stream\n--output-format\n--disable-builtin-mcps",
		CaptureArgs:  true,
		CaptureStdin: true,
		StdoutLines: []string{
			`{"type":"session.mcp_servers_loaded","data":{"servers":[{"name":"github-mcp-server","status":"disabled"}]}}`,
			`{"type":"assistant.message","data":{"messageId":"msg-1","content":"Full review output","toolRequests":[]}}`,
			`{"type":"result","exitCode":0,"sessionId":"session-1"}`,
		},
	})
	a := NewCopilotAgent(mock.CmdPath)

	var output bytes.Buffer
	res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", &output)

	require.NoError(t, err)
	assert.Equal(t, "Full review output", res)
	assert.Contains(t, output.String(), "Full review output")
	assert.NotContains(t, output.String(), "assistant.message")
	assertFileContent(t, mock.StdinFile, "prompt")

	args := readFileContent(t, mock.ArgsFile)
	assert.Contains(t, args, "--output-format json")
	assert.Contains(t, args, "--disable-builtin-mcps")
}

func TestCopilotReviewJSONOutputCapturesSessionIDWithoutTrailingNewline(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$*" in *--help*) echo "--allow-all-tools --stream --output-format"; exit 0;; esac
printf '%s\n%s' \
'{"type":"assistant.message","data":{"messageId":"msg-1","content":"No issues found.","toolRequests":[]}}' \
'{"type":"result","exitCode":0,"sessionId":"session-no-newline"}'
`)
	a := NewCopilotAgent(cmdPath)

	var output bytes.Buffer
	sessionWriter := NewSessionCaptureWriter(&output, nil)
	res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", sessionWriter)
	sessionWriter.Flush()

	require.NoError(t, err)
	assert.Equal(t, "No issues found.", res)
	assert.Equal(t, "session-no-newline", sessionWriter.SessionID())
	assert.Contains(t, output.String(), "No issues found.")
	assert.NotContains(t, output.String(), "assistant.message")
}

func TestCopilotReviewJSONOutputUsesLatestMessageContentByID(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput: "--allow-all-tools\n--stream\n--output-format",
		StdoutLines: []string{
			`{"type":"assistant.message","data":{"messageId":"msg-1","content":"Adds","toolRequests":[]}}`,
			`{"type":"assistant.message","data":{"messageId":"msg-1","content":"Adds a README update.\nNo issues found.","toolRequests":[]}}`,
			`{"type":"result","exitCode":0}`,
		},
	})
	a := NewCopilotAgent(mock.CmdPath)

	res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)

	require.NoError(t, err)
	assert.Equal(t, "Adds a README update.\nNo issues found.", res)
}

func TestCopilotReviewJSONOutputDropsToolRequestPreamble(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput: "--allow-all-tools\n--stream\n--output-format",
		StdoutLines: []string{
			`{"type":"assistant.message","data":{"messageId":"msg-1","content":"I'll inspect the files first.","toolRequests":[{"name":"read_file"}]}}`,
			`{"type":"assistant.message","data":{"messageId":"msg-2","content":"No issues found.\nSummary: Updates docs.","toolRequests":[]}}`,
			`{"type":"result","exitCode":0}`,
		},
	})
	a := NewCopilotAgent(mock.CmdPath)

	res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)

	require.NoError(t, err)
	assert.Equal(t, "No issues found.\nSummary: Updates docs.", res)
}

func TestCopilotBuildArgs(t *testing.T) {
	a := NewCopilotAgent("copilot")

	t.Run("review mode includes deny list", func(t *testing.T) {
		assert := assert.New(t)
		args := a.buildArgs(false)
		assert.Contains(args, "-s")
		assert.Contains(args, "--allow-all-tools")
		assertArgPair(t, args, "--stream", "off")
		// Verify deny-tool pairs exist for each denied tool
		for _, tool := range copilotReviewDenyTools {
			found := false
			for i, arg := range args {
				if arg == "--deny-tool" && i+1 < len(args) && args[i+1] == tool {
					found = true
					break
				}
			}
			assert.True(found, "missing --deny-tool %q", tool)
		}
	})

	t.Run("agentic mode has no deny list", func(t *testing.T) {
		assert := assert.New(t)
		args := a.buildArgs(true)
		assert.Contains(args, "-s")
		assert.Contains(args, "--allow-all-tools")
		assertArgPair(t, args, "--stream", "off")
		assert.NotContains(args, "--deny-tool")
	})

	t.Run("model flag included when set", func(t *testing.T) {
		withModel := NewCopilotAgent("copilot").WithModel("gpt-4o").(*CopilotAgent)
		args := withModel.buildArgs(false)
		found := false
		for i, arg := range args {
			if arg == "--model" && i+1 < len(args) && args[i+1] == "gpt-4o" {
				found = true
				break
			}
		}
		assert.True(t, found, "expected --model gpt-4o in args: %v", args)
	})
}

func TestCopilotCommandLineUsesPreviewArgs(t *testing.T) {
	a := NewCopilotAgent("copilot").WithModel("gpt-4o").(*CopilotAgent)

	cmdLine := a.CommandLine()

	assert.Contains(t, cmdLine, "--model gpt-4o")
	assert.NotContains(t, cmdLine, "--allow-all-tools")
	assert.NotContains(t, cmdLine, "--deny-tool")
}

func TestCopilotReview(t *testing.T) {
	skipIfWindows(t)

	tests := []struct {
		name       string
		prompt     string
		mockOpts   MockCLIOpts
		wantErr    bool
		wantErrStr string
		wantResult string
	}{
		{
			name:   "Pipes prompt via stdin",
			prompt: "Review this commit carefully",
			mockOpts: MockCLIOpts{
				CaptureArgs:  true,
				CaptureStdin: true,
				StdoutLines:  []string{"ok"},
			},
			wantErr:    false,
			wantResult: "ok\n",
		},
		{
			name:   "CLI failure (exit non-zero)",
			prompt: "Review this commit",
			mockOpts: MockCLIOpts{
				ExitCode:    1,
				StderrLines: []string{"error: failed to generate review"},
			},
			wantErr:    true,
			wantErrStr: "copilot failed",
		},
		{
			name:   "Empty output from CLI",
			prompt: "Review this commit",
			mockOpts: MockCLIOpts{
				CaptureArgs:  true,
				CaptureStdin: true,
				StdoutLines:  []string{},
			},
			wantErr:    false,
			wantResult: "No review output generated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, tt.mockOpts)
			a := NewCopilotAgent(mock.CmdPath)

			res, err := a.Review(
				context.Background(), t.TempDir(), "HEAD", tt.prompt, nil,
			)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrStr != "" {
					require.Contains(t, err.Error(), tt.wantErrStr, "Review() error = %v, want to contain %v", err, tt.wantErrStr)
				}
			} else {
				require.NoError(t, err, "Review failed")
				assert.Equal(t, tt.wantResult, res, "Review() result = %q, want %q", res, tt.wantResult)

				assertFileContent(t, mock.StdinFile, tt.prompt)

				assertFileNotContains(t, mock.ArgsFile, tt.prompt)
			}
		})
	}
}

func TestCopilotReviewPermissionFlags(t *testing.T) {
	skipIfWindows(t)

	t.Run("review mode passes permission and stream flags", func(t *testing.T) {
		assert := assert.New(t)
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput:   "--allow-all-tools\n--stream",
			CaptureArgs:  true,
			CaptureStdin: true,
			StdoutLines:  []string{"review output"},
		})
		a := NewCopilotAgent(mock.CmdPath)
		res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
		require.NoError(t, err)
		assert.Equal("review output\n", res)

		args := readFileContent(t, mock.ArgsFile)
		assert.Contains(args, "--allow-all-tools")
		assert.Contains(args, "--stream off")
		assert.Contains(args, "-s")
		assert.Contains(args, "--deny-tool")
		assertFileContent(t, mock.StdinFile, "prompt")
	})

	t.Run("agentic mode omits deny list", func(t *testing.T) {
		assert := assert.New(t)
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput:   "--allow-all-tools",
			CaptureArgs:  true,
			CaptureStdin: true,
			StdoutLines:  []string{"fix output"},
		})
		a := NewCopilotAgent(mock.CmdPath).WithAgentic(true)
		res, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
		require.NoError(t, err)
		assert.Equal("fix output\n", res)

		args := readFileContent(t, mock.ArgsFile)
		assert.Contains(args, "--allow-all-tools")
		assert.NotContains(args, "--deny-tool")
	})

	t.Run("AllowUnsafeAgents overrides to agentic", func(t *testing.T) {
		withUnsafeAgents(t, true)

		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput:  "--allow-all-tools",
			CaptureArgs: true,
			StdoutLines: []string{"output"},
		})
		a := NewCopilotAgent(mock.CmdPath) // not WithAgentic(true)
		_, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
		require.NoError(t, err)

		args := readFileContent(t, mock.ArgsFile)
		assert.Contains(t, args, "--allow-all-tools")
		assert.NotContains(t, args, "--deny-tool")
	})

	t.Run("falls back to no flags when unsupported", func(t *testing.T) {
		mock := mockAgentCLI(t, MockCLIOpts{
			HelpOutput:   "Usage: copilot [flags]", // no --allow-all-tools
			CaptureArgs:  true,
			CaptureStdin: true,
			StdoutLines:  []string{"output"},
		})
		a := NewCopilotAgent(mock.CmdPath)
		_, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)
		require.NoError(t, err)

		args := readFileContent(t, mock.ArgsFile)
		assert.NotContains(t, args, "--allow-all-tools")
		assert.NotContains(t, args, "--stream")
		assert.NotContains(t, args, "--deny-tool")
		assert.NotContains(t, args, "-s")
	})
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()

	pairs := make([]string, 0)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			pairs = append(pairs, args[i]+" "+args[i+1])
		}
	}
	assert.Contains(t, pairs, flag+" "+value)
}

// readFileContent reads a file and returns its trimmed content.
func readFileContent(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read %s", path)
	return strings.TrimSpace(string(content))
}
