package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ompVersionOutput = "omp/19.2.1"
	ompReviewStream  = `{"type":"session","version":3,"id":"omp-session-1"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"No issues found."}],"stopReason":"stop"}}`
)

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
			mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, Version: ompVersionOutput, StdoutLines: strings.Split(ompReviewStream, "\n")})

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
				assert.Equal("always-ask", argAfter(args, "--approval-mode"))
			} else {
				assert.NotContains(args, "--tools")
				assert.NotContains(args, "--no-tools")
				assert.Equal("yolo", argAfter(args, "--approval-mode"))
			}
		})
	}
}

// Before 19.2.0, --no-tools does not keep MCP, extension and custom tools
// out of a run, so reviews refuse those releases. Fix and refine still run.
func TestOmpReviewRequiresToolLimit(t *testing.T) {
	tests := []struct {
		name    string
		version string
		agentic bool
		wantErr string
	}{
		{name: "review on 19.1", version: "omp/19.1.1", wantErr: `reports "omp/19.1.1"; upgrade omp`},
		{name: "review without a version", version: "unknown", wantErr: `reports "unknown"; upgrade omp`},
		{name: "review on 19.2.0", version: "omp/19.2.0"},
		{name: "agentic on 19.1", version: "omp/19.1.1", agentic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withUnsafeAgents(t, false)
			mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, Version: tt.version, StdoutLines: strings.Split(ompReviewStream, "\n")})
			_, err := NewOmpAgent(mock.CmdPath).WithAgentic(tt.agentic).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.NotContains(t, readMockArgs(t, mock.ArgsFile), "--mode", "omp must not start")
				return
			}
			require.NoError(t, err)
			assert.Contains(t, readMockArgs(t, mock.ArgsFile), "--mode")
		})
	}
}

// A wrapper or a version manager shim can change the omp behind an unchanged
// omp_cmd, so each review checks the version again.
func TestOmpReviewChecksVersionEachRun(t *testing.T) {
	versionFile := filepath.Join(t.TempDir(), "version")
	require.NoError(t, os.WriteFile(versionFile, []byte("omp/19.2.1\n"), 0o600))
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; --version) cat '`+versionFile+`'; exit 0;; esac
cat <<'EOF'
`+ompReviewStream+`
EOF
`)
	a := NewOmpAgent(command)
	_, err := a.Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(versionFile, []byte("omp/19.1.1\n"), 0o600))
	_, err = a.Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, `reports "omp/19.1.1"`)
}

func TestOmpReviewReportsFailedVersionCheck(t *testing.T) {
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
echo 'unknown profile "work"' >&2
exit 1
`)
	_, err := NewOmpAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "check omp version: exit status 1")
	assert.Contains(t, err.Error(), `unknown profile "work"`)
	assert.NotContains(t, err.Error(), "upgrade omp")
}

// A relative omp_cmd such as ./bin/omp runs from the repository, so the
// version check must find the same file there.
func TestOmpReviewRelativeCommand(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{Version: ompVersionOutput, StdoutLines: strings.Split(ompReviewStream, "\n")})
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, "bin"), 0o755))
	require.NoError(t, os.Symlink(mock.CmdPath, filepath.Join(repo, "bin", "omp")))
	result, err := NewOmpAgent("./bin/omp").Review(t.Context(), repo, "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No issues found.", result)
}

func TestOmpReviewPromptAndOutput(t *testing.T) {
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; --version) echo `+ompVersionOutput+`; exit 0;; esac
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
	mock := mockAgentCLI(t, MockCLIOpts{Version: ompVersionOutput, StdoutLines: []string{piBedrockImageError}, ExitCode: 1})
	_, err := NewOmpAgent(mock.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "This model doesn't support the image field")
	assert.Equal(t, LimitKindPermanent, ClassifyLimit("omp", err.Error()).Kind)
}

// A cost cap or an abort ends the run with a non-zero exit. JSON mode reports
// why only in the event stream; a cost cap also has its own exit status.
func TestOmpReviewReportsStopReason(t *testing.T) {
	const costCap = "Cost cap reached: this session spent $1.02 of its $1.00 cap (--max-cost / session.maxCost), so the next model request was not sent."
	costCapNotice := `{"type":"notice","level":"error","message":"` + costCap + `","source":"cost-gate"}`
	tests := []struct {
		name     string
		exitCode int
		stdout   []string
		want     string
		kind     LimitKind
	}{
		{
			// A retry starts a new omp session that can spend up to the cap
			// again, so the job fails over instead.
			name:     "cost cap",
			exitCode: 3,
			stdout:   []string{costCapNotice},
			want:     "exit status 3: " + costCap,
			kind:     LimitKindPermanent,
		},
		{
			name:     "cost cap after a provider error",
			exitCode: 3,
			stdout:   []string{piBedrockImageError, costCapNotice},
			want:     "exit status 3: " + costCap,
			kind:     LimitKindPermanent,
		},
		{
			name:     "cost cap without a notice",
			exitCode: 3,
			stdout:   []string{`{"type":"session","id":"omp-session-1"}`},
			want:     "exit status 3: cost cap reached",
			kind:     LimitKindPermanent,
		},
		{
			name:     "aborted",
			exitCode: 1,
			stdout:   []string{`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"aborted","errorMessage":"Request was aborted"}}`},
			want:     "exit status 1: Request was aborted",
		},
		{
			name:     "error notice from another source",
			exitCode: 1,
			stdout:   []string{`{"type":"notice","level":"error","message":"Compaction failed","source":"compaction"}`},
			want:     "omp failed: exit status 1\nstderr:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, MockCLIOpts{Version: ompVersionOutput, StdoutLines: tt.stdout, ExitCode: tt.exitCode})
			_, err := NewOmpAgent(mock.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
			require.ErrorContains(t, err, tt.want)
			classification, _ := LimitClassificationFromError(err)
			assert.Equal(t, tt.kind, classification.Kind)
			// Synthesis and roborev fix classify by the error text alone.
			assert.Equal(t, tt.kind, ClassifyLimit("omp", err.Error()).Kind)
		})
	}
}

func TestOmpReviewRefusesUntrustedCheckout(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, Version: ompVersionOutput, StdoutLines: strings.Split(ompReviewStream, "\n")})
	ctx := WithUntrustedCheckout(t.Context())
	_, err := NewOmpAgent(mock.CmdPath).Review(ctx, t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "untrusted checkout")
	// The mock CLI also runs once at creation, so check for a review run.
	assert.NotContains(t, readMockArgs(t, mock.ArgsFile), "--mode", "omp must not start in an untrusted checkout")
}
