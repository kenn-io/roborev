package review

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	assert.Contains(t, s, substr)
}

// commonMockAgent provides default no-op chaining methods
type commonMockAgent struct {
	self agent.Agent
}

func (a *commonMockAgent) WithReasoning(_ agent.ReasoningLevel) agent.Agent { return a.self }
func (a *commonMockAgent) WithAgentic(_ bool) agent.Agent                   { return a.self }
func (a *commonMockAgent) WithModel(_ string) agent.Agent                   { return a.self }

// failingSynthAgent always returns an error from Review,
// used to force synthesis fallback deterministically.
type failingSynthAgent struct {
	commonMockAgent
}

func newFailingSynthAgent() *failingSynthAgent {
	a := &failingSynthAgent{}
	a.self = a
	return a
}

func (a *failingSynthAgent) Name() string { return "failing-synth" }
func (a *failingSynthAgent) Review(
	_ context.Context, _, _, _ string, _ io.Writer,
) (string, error) {
	return "", errors.New("agent exploded")
}
func (a *failingSynthAgent) CommandLine() string { return "failing-synth" }

// capturingAgent records the gitRef passed to Review.
type capturingAgent struct {
	commonMockAgent
	capturedGitRef string
}

func newCapturingAgent() *capturingAgent {
	a := &capturingAgent{}
	a.self = a
	return a
}

func (a *capturingAgent) Name() string { return "capture" }
func (a *capturingAgent) Review(
	_ context.Context, _, gitRef, _ string, _ io.Writer,
) (string, error) {
	a.capturedGitRef = gitRef
	return "synthesized output", nil
}
func (a *capturingAgent) CommandLine() string { return "capture" }

type synthesisEntrypointAgent struct {
	commonMockAgent
	synthPrompt  string
	reviewCalled bool
}

func newSynthesisEntrypointAgent() *synthesisEntrypointAgent {
	a := &synthesisEntrypointAgent{}
	a.self = a
	return a
}

func (a *synthesisEntrypointAgent) Name() string { return "synthesis-entrypoint" }
func (a *synthesisEntrypointAgent) Review(
	_ context.Context, _, _, _ string, _ io.Writer,
) (string, error) {
	a.reviewCalled = true
	return "", errors.New("review entrypoint should not be used for synthesis")
}

func (a *synthesisEntrypointAgent) Synthesize(
	_ context.Context, prompt string, _ io.Writer,
) (string, error) {
	a.synthPrompt = prompt
	return "synthesized output", nil
}
func (a *synthesisEntrypointAgent) CommandLine() string { return "synthesis-entrypoint" }

func TestSynthesize_Formatting(t *testing.T) {
	tests := []struct {
		name          string
		results       []ReviewResult
		expectedErr   error
		expectedTexts []string
	}{
		{
			name: "AllFailed",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "failed",
					Error:      "agent crashed",
				},
			},
			expectedErr: ErrAllFailed,
			expectedTexts: []string{
				"Review Failed",
			},
		},
		{
			name: "SingleSuccess",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "done",
					Output:     "No issues found.",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Passed",
				"No issues found.",
			},
		},
		{
			name: "SingleSeverityThresholdMet",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "review",
					Status:     "done",
					Output:     "SEVERITY_THRESHOLD_MET",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Passed",
			},
		},
		{
			name: "ThresholdMetWithFindings",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "review",
					Status:     "done",
					Output:     "SEVERITY_THRESHOLD_MET\n\n- High: critical bug found",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Complete",
			},
		},
		{
			name: "AllQuota",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "failed",
					Error:      QuotaErrorPrefix + "exhausted",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Skipped",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comment, err := Synthesize(
				context.Background(), tt.results, SynthesizeOpts{
					HeadSHA: "abc123456789",
				})
			if tt.expectedErr != nil {
				require.ErrorIs(t, err, tt.expectedErr)
			} else {
				require.NoError(t, err)
			}
			for _, text := range tt.expectedTexts {
				assertContains(t, comment, text)
			}
			assert.NotContains(t, comment, "Agent:")
			assert.NotContains(t, comment, "Type:")
		})
	}
}

func TestSynthesize_MultipleResults_FallsBackToRaw(t *testing.T) {
	// Register an agent that always fails so the test is
	// deterministic regardless of what other agents exist
	// in the global registry.
	ag := newFailingSynthAgent()
	agent.Register(ag)
	t.Cleanup(func() { agent.Unregister("failing-synth") })

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "Design looks good",
		},
	}

	comment, err := Synthesize(
		context.Background(), results, SynthesizeOpts{
			Agent:   "failing-synth",
			HeadSHA: "def456789012",
		})
	require.NoError(t, err)

	// Should fall back to raw format
	assertContains(t, comment, "Synthesis unavailable")
	assertContains(t, comment, "Found issue A")
	assertContains(t, comment, "Design looks good")
	assert.NotContains(t, comment, "codex —")
	assert.NotContains(t, comment, "security")
	assert.NotContains(t, comment, "design")
}

func TestSynthesize_MixedSuccessAndFailure(t *testing.T) {
	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found vulnerability",
		},
		{
			Agent:      "gemini",
			ReviewType: "security",
			Status:     "failed",
			Error:      "agent crashed",
		},
	}

	comment, err := Synthesize(
		context.Background(), results, SynthesizeOpts{
			Agent:   "nonexistent-synthesis-agent",
			HeadSHA: "abc123456789",
		})
	require.NoError(t, err)

	// Should fall back to raw format since synthesis agent
	// doesn't exist
	assertContains(t, comment, "Combined Review")
}

func TestSynthesize_PassesGitRefToAgent(t *testing.T) {
	cap := newCapturingAgent()
	agent.Register(cap)
	t.Cleanup(func() { agent.Unregister("capture") })

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "Design looks good",
		},
	}

	_, err := Synthesize(
		context.Background(), results, SynthesizeOpts{
			Agent:   "capture",
			GitRef:  "aaa111..bbb222",
			HeadSHA: "bbb222",
		})
	require.NoError(t, err)
	assert.Equal(t, "aaa111..bbb222", cap.capturedGitRef, "gitRef")
}

func TestSynthesize_UsesSynthesisEntrypoint(t *testing.T) {
	synth := newSynthesisEntrypointAgent()
	agent.Register(synth)
	t.Cleanup(func() { agent.Unregister("synthesis-entrypoint") })

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "default",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue B",
		},
	}

	comment, err := Synthesize(context.Background(), results, SynthesizeOpts{
		Agent:   "synthesis-entrypoint",
		GitRef:  "aaa111..bbb222",
		HeadSHA: "bbb222",
	})
	require.NoError(t, err)
	assertContains(t, comment, "synthesized output")
	assert.False(t, synth.reviewCalled, "standalone synthesis must not use code-review entrypoint")
	assertContains(t, synth.synthPrompt, "Found issue A")
	assert.NotContains(t, synth.synthPrompt, "Review the code changes in commit")
}

func TestSynthesize_EmptyAgentAutoSelectsAvailableAgent(t *testing.T) {
	t.Setenv("PATH", "")

	synth := newSynthesisEntrypointAgent()
	agent.Register(synth)
	t.Cleanup(func() { agent.Unregister("synthesis-entrypoint") })

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "default",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue B",
		},
	}

	comment, err := Synthesize(context.Background(), results, SynthesizeOpts{
		GitRef:  "aaa111..bbb222",
		HeadSHA: "bbb222",
	})
	require.NoError(t, err)
	assertContains(t, comment, "synthesized output")
	assert.False(t, synth.reviewCalled, "standalone synthesis must not use code-review entrypoint")
}

func TestSynthesize_PassesGlobalConfigToResolver(t *testing.T) {
	originalResolver := getAvailableWithConfig
	t.Cleanup(func() { getAvailableWithConfig = originalResolver })

	var seenAgent string
	var seenCfg *config.Config
	cap := newCapturingAgent()
	getAvailableWithConfig = func(repoPath string, agentName string, cfg *config.Config, backups ...string) (agent.Agent, error) {
		seenAgent = agentName
		seenCfg = cfg
		return cap, nil
	}

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "custom-acp",
			Command: "acp-agent",
		},
	}
	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "issue B",
		},
	}

	comment, err := Synthesize(context.Background(), results, SynthesizeOpts{
		Agent:        "custom-acp",
		GlobalConfig: cfg,
		HeadSHA:      "abc123",
		GitRef:       "abc123..def456",
	})
	require.NoError(t, err)
	require.Equal(t, "custom-acp", seenAgent, "resolver agent")
	require.Same(t, cfg, seenCfg, "resolver cfg pointer mismatch")
	assertContains(t, comment, "synthesized output")
}
