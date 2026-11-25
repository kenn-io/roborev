package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ompReviewStream = `{"type":"session","version":3,"id":"omp-session-1"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"No issues found."}],"stopReason":"stop"}}`

func TestOmpReviewToolPermissions(t *testing.T) {
	tests := []struct {
		name      string
		agentic   bool
		unsafe    bool
		reasoning ReasoningLevel
		readOnly  bool
		thinking  string
	}{
		{name: "review", reasoning: ReasoningThorough, readOnly: true, thinking: "high"},
		{name: "agentic", agentic: true, reasoning: ReasoningStandard},
		{name: "unsafe review", unsafe: true, reasoning: ReasoningMaximum, thinking: "max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withUnsafeAgents(t, tt.unsafe)
			mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, StdoutLines: strings.Split(ompReviewStream, "\n")})

			a := NewOmpAgent(mock.CmdPath).WithModel("anthropic/claude-sonnet-4-5").WithReasoning(tt.reasoning).WithAgentic(tt.agentic)
			_, err := a.Review(t.Context(), t.TempDir(), "HEAD", "prompt", nil)
			require.NoError(t, err)

			args := readMockArgs(t, mock.ArgsFile)
			assert := assert.New(t)
			assertArgsContainContiguous(t, args, []string{"-p", "--mode", "json"})
			assert.Equal("anthropic/claude-sonnet-4-5", argAfter(args, "--model"))
			assert.Equal(tt.thinking, argAfter(args, "--thinking"))
			if tt.readOnly {
				assert.Equal(ompReadOnlyTools, argAfter(args, "--tools"))
				assert.Contains(args, "--no-tools")
				assert.Contains(args, "--no-lsp")
				assert.Equal("always-ask", argAfter(args, "--approval-mode"))
			} else {
				assert.NotContains(args, "--tools")
				assert.NotContains(args, "--no-tools")
				assert.Equal("yolo", argAfter(args, "--approval-mode"))
			}
		})
	}
}

func TestOmpReviewPromptAndOutput(t *testing.T) {
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
for arg in "$@"; do
  case "$arg" in
    @*) test "$(cat "${arg#@}")" = 'prompt body' || exit 2 ;;
  esac
done
test -z "$(cat)" || exit 3
cat <<'EOF'
`+ompReviewStream+`
EOF
echo 'status message' >&2
`)
	var output bytes.Buffer
	result, err := NewOmpAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "prompt body", &output)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("No issues found.", result)
	assert.Contains(output.String(), `{"type":"session","version":3,"id":"omp-session-1"}`+"\n")
	assert.Contains(output.String(), "status message\n")
}

// omp exits 1 when the provider ends the run with an error, and JSON mode
// writes the provider's message only to the event stream.
func TestOmpReviewReportsProviderError(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{StdoutLines: []string{piBedrockImageError}, ExitCode: 1})
	_, err := NewOmpAgent(mock.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "This model doesn't support the image field")
	assert.Equal(t, LimitKindPermanent, ClassifyLimit("omp", err.Error()).Kind)
}

func TestOmpReviewFailureReportsStderr(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		StdoutLines: []string{`{"type":"session","id":"omp-session-1"}`},
		StderrLines: []string{"Session cost cap reached"},
		ExitCode:    3,
	})
	_, err := NewOmpAgent(mock.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "omp failed: exit status 3")
	assert.Contains(t, err.Error(), "Session cost cap reached")
}

func TestOmpReviewRefusesUntrustedCheckout(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, StdoutLines: strings.Split(ompReviewStream, "\n")})
	ctx := WithUntrustedCheckout(t.Context())
	_, err := NewOmpAgent(mock.CmdPath).Review(ctx, t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "untrusted checkout")
	// The mock CLI also runs once at creation, so check for a review run.
	assert.NotContains(t, readMockArgs(t, mock.ArgsFile), "--mode", "omp must not start in an untrusted checkout")
}
