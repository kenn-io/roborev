package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestDroidBuildArgs(t *testing.T) {
	tests := []struct {
		name     string
		agentic  bool
		setup    func(*DroidAgent) *DroidAgent
		wantArgs []string
		dontWant []string
	}{
		{
			name:     "Non-agentic default",
			agentic:  false,
			wantArgs: []string{"exec", "--tag", "roborev", "--auto", "low"},
			dontWant: []string{"medium"},
		},
		{
			name:     "Agentic mode",
			agentic:  true,
			wantArgs: []string{"--auto", "medium"},
		},
		{
			name:     "Reasoning Thorough",
			setup:    func(a *DroidAgent) *DroidAgent { return a.WithReasoning(ReasoningThorough).(*DroidAgent) },
			wantArgs: []string{"--reasoning-effort", "high"},
		},
		{
			name:     "Reasoning Fast",
			setup:    func(a *DroidAgent) *DroidAgent { return a.WithReasoning(ReasoningFast).(*DroidAgent) },
			wantArgs: []string{"--reasoning-effort", "low"},
		},
		{
			name:     "Reasoning Standard",
			setup:    func(a *DroidAgent) *DroidAgent { return a.WithReasoning(ReasoningStandard).(*DroidAgent) },
			dontWant: []string{"--reasoning-effort"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewDroidAgent("droid")
			if tt.setup != nil {
				a = tt.setup(a)
			}

			args := a.buildArgs(tt.agentic, false)

			for _, want := range tt.wantArgs {
				assertContainsArg(t, args, want)
			}
			for _, dont := range tt.dontWant {
				assertNotContainsArg(t, args, dont)
			}
		})
	}
}

func TestDroidCommandLineUsesBuildArgs(t *testing.T) {
	a := NewDroidAgent("droid").WithReasoning(ReasoningThorough).WithAgentic(true).(*DroidAgent)

	want := strings.Join(a.buildArgs(true, false), " ")
	assert.Equal(t, "droid "+want, a.CommandLine())
}

func TestDroidPlanningCommandLine(t *testing.T) {
	withUnsafeAgents(t, true)
	a := NewDroidAgent("droid").WithAgentic(true).WithReasoning(ReasoningThorough).(*DroidAgent)

	assert.Equal(t, "droid exec --tag roborev --reasoning-effort high", a.PlanningCommandLine())
}

func TestDroidName(t *testing.T) {
	a := NewDroidAgent("")
	require.Equal(t, "droid", a.Name(), "expected name 'droid', got %s", a.Name())
	require.Equal(t, "droid", a.CommandName(), "expected command name 'droid', got %s", a.CommandName())
}

func TestDroidWithAgentic(t *testing.T) {
	a := NewDroidAgent("droid")
	require.False(t, a.Agentic, "expected non-agentic by default")

	a2 := a.WithAgentic(true).(*DroidAgent)
	require.True(t, a2.Agentic, "expected agentic after WithAgentic(true)")
	require.False(t, a.Agentic, "original should be unchanged")
}

func TestDroidReviewOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		mockOpts    MockCLIOpts
		wantError   bool
		errContains string
		wantResult  string
		exactMatch  bool
	}{
		{
			name:       "Success",
			mockOpts:   MockCLIOpts{StdoutLines: []string{"Review feedback from Droid"}},
			wantResult: "Review feedback from Droid\n",
			exactMatch: true,
		},
		{
			name:        "Failure",
			mockOpts:    MockCLIOpts{StderrLines: []string{"error: something went wrong"}, ExitCode: 1},
			wantError:   true,
			errContains: "droid failed",
		},
		{
			name:       "Empty Output",
			mockOpts:   MockCLIOpts{},
			wantResult: "No review output generated",
			exactMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, tt.mockOpts)
			a := NewDroidAgent(mock.CmdPath)

			result, err := a.Review(t.Context(), t.TempDir(), "HEAD", "review this commit", nil)

			if tt.wantError {
				require.Error(t, err)

				if !strings.Contains(err.Error(), tt.errContains) {
					require.ErrorContains(t, err, tt.errContains, "expected error to contain %q, got %v", tt.errContains, err)
				}
				return
			}
			require.NoError(t, err)

			if tt.exactMatch {
				require.Equal(t, tt.wantResult, result, "expected exact result %q, got %q", tt.wantResult, result)
			} else if !strings.Contains(result, tt.wantResult) {
				require.Contains(t, result, tt.wantResult, "expected result to contain %q, got %q", tt.wantResult, result)
			}
		})
	}
}

func TestDroidReviewWithProgress(t *testing.T) {
	skipIfWindows(t)
	tmpDir := t.TempDir()
	progressFile := filepath.Join(tmpDir, "progress.txt")

	mock := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{"Processing..."},
		StdoutLines: []string{"Done"},
	})
	a := NewDroidAgent(mock.CmdPath)

	f, err := os.Create(progressFile)
	require.NoError(t, err, "create progress file: %v")

	defer f.Close()

	_, err = a.Review(t.Context(), tmpDir, "deadbeef", "review this commit", f)
	require.NoError(t, err)

	progress, _ := os.ReadFile(progressFile)
	if !strings.Contains(string(progress), "Processing") {
		require.Contains(t, string(progress), "Processing", "expected progress output, got %q", string(progress))
	}
}

func TestDroidReviewPipesPromptViaStdin(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs:  true,
		CaptureStdin: true,
		StdoutLines:  []string{"ok"},
	})

	a := NewDroidAgent(mock.CmdPath)
	prompt := "Review this commit carefully"
	_, err := a.Review(
		t.Context(), t.TempDir(), "HEAD", prompt, nil,
	)
	require.NoError(t, err, "Review failed: %v")

	assertFileContent(t, mock.StdinFile, prompt)

	assertFileNotContains(t, mock.ArgsFile, prompt)
}

func TestDroidReviewAgenticModeFromGlobal(t *testing.T) {
	withUnsafeAgents(t, true)

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{"result"},
	})

	a := NewDroidAgent(mock.CmdPath)
	_, err := a.Review(t.Context(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assertArgsContainContiguous(t, args, []string{"--auto", "medium"})
}

func TestDroidReviewPassesModel(t *testing.T) {
	skipIfWindows(t)

	tests := []struct {
		name      string
		model     string
		wantModel bool
	}{
		{name: "configured model", model: "custom:My-Model-0", wantModel: true},
		{name: "no model", model: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, StdoutLines: []string{"ok"}})
			a := NewDroidAgent(mock.CmdPath).WithModel(tt.model)

			_, err := a.Review(t.Context(), t.TempDir(), "HEAD", "review this commit", nil)
			require.NoError(t, err)

			args := readMockArgs(t, mock.ArgsFile)
			if tt.wantModel {
				assertArgsContainContiguous(t, args, []string{"-m", tt.model})
				assert.Contains(t, a.CommandLine(), "-m "+tt.model)
				return
			}
			assertNotContainsArg(t, args, "-m")
		})
	}
}

func TestDroidCommandOverrideRunsConfiguredExecutable(t *testing.T) {
	skipIfWindows(t)

	mock := mockAgentCLI(t, MockCLIOpts{CaptureArgs: true, StdoutLines: []string{"ok"}})
	cfg := &config.Config{DroidCmd: mock.CmdPath}

	a, err := GetAvailableWithConfig(t.TempDir(), "droid", cfg)
	require.NoError(t, err)
	require.Equal(t, "droid", a.Name())
	assert.True(t, strings.HasPrefix(a.CommandLine(), mock.CmdPath+" exec "),
		"command line %q should start with the droid_cmd executable", a.CommandLine())

	_, err = a.Review(t.Context(), t.TempDir(), "HEAD", "review this commit", nil)
	require.NoError(t, err)
	assert.Equal(t, "exec", readMockArgs(t, mock.ArgsFile)[0])
}

func TestDroidReviewFailureClassification(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   LimitKind
	}{
		{
			// Provider wording from a real droid failure on an exhausted route.
			name: "subscription allowance used up",
			stderr: "Error during droid execution: 403 Your subscription allowance for this period " +
				"is used up, and your plan does not allow spending wallet balance on top of it. " +
				"It renews with your next billing period.",
			want: LimitKindQuota,
		},
		{
			// Droid reports this when the run ends with an error but no message.
			name:   "opaque exec failure",
			stderr: "Error during droid execution: Exec failed",
			want:   LimitKindNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, MockCLIOpts{StderrLines: []string{tt.stderr}, ExitCode: 1})
			a := NewDroidAgent(mock.CmdPath)

			_, err := a.Review(t.Context(), t.TempDir(), "HEAD", "review this commit", nil)
			require.Error(t, err)

			// The worker classifies the stored "agent: <err>" text.
			cls := ClassifyLimit("droid", "agent: "+err.Error())
			assert.Equal(t, tt.want, cls.Kind)
			assert.Contains(t, cls.Message, tt.stderr)
		})
	}
}

func TestDroidAllowanceRuleIsDroidScoped(t *testing.T) {
	msg := "403 Your subscription allowance for this period is used up"
	assert.Equal(t, LimitKindQuota, ClassifyLimit("droid", msg).Kind)
	assert.Equal(t, LimitKindNone, ClassifyLimit("codex", msg).Kind)
}
