package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

// nonSessionAgent implements agent.Agent but not agent.SessionAgent.
type nonSessionAgent struct{}

func (a *nonSessionAgent) Name() string { return "non-session" }
func (a *nonSessionAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return "", nil
}
func (a *nonSessionAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent { return a }
func (a *nonSessionAgent) WithAgentic(agentic bool) agent.Agent                 { return a }
func (a *nonSessionAgent) WithModel(model string) agent.Agent                   { return a }
func (a *nonSessionAgent) CommandLine() string                                  { return "non-session" }

func TestFixSessionTracker_DisabledAlwaysReturnsBase(t *testing.T) {
	base := agent.NewTestAgent()
	logged := &strings.Builder{}
	tr := &fixSessionTracker{
		enabled: false,
		base:    base,
		out:     logged,
	}

	a, resuming := tr.NextAgent()
	assert.Same(t, agent.Agent(base), a)
	assert.False(t, resuming)

	tr.Capture("test-session-1")
	a, resuming = tr.NextAgent()
	assert.Same(t, agent.Agent(base), a, "disabled tracker ignores Capture")
	assert.False(t, resuming)
	assert.Empty(t, logged.String(), "disabled tracker never logs")
}

func TestFixSessionTracker_EnabledFirstCallReturnsBase(t *testing.T) {
	base := agent.NewTestAgent()
	tr := &fixSessionTracker{enabled: true, base: base, out: io.Discard}

	a, resuming := tr.NextAgent()
	assert.Same(t, agent.Agent(base), a)
	assert.False(t, resuming)
}

func TestFixSessionTracker_AfterCaptureReturnsResumedAgent(t *testing.T) {
	base := agent.NewTestAgent()
	tr := &fixSessionTracker{enabled: true, base: base, out: io.Discard}

	tr.Capture("test-session-1")
	a, resuming := tr.NextAgent()
	require.True(t, resuming)
	resumed, ok := a.(*agent.TestAgent)
	require.True(t, ok)
	assert.Equal(t, "test-session-1", resumed.SessionID)
}

func TestFixSessionTracker_InvalidIDsDropped(t *testing.T) {
	base := agent.NewTestAgent()
	tr := &fixSessionTracker{enabled: true, base: base, out: io.Discard}

	tr.Capture("")
	tr.Capture("contains spaces")
	tr.Capture(strings.Repeat("x", 200)) // exceeds 128-char limit

	a, resuming := tr.NextAgent()
	assert.Same(t, agent.Agent(base), a)
	assert.False(t, resuming)
}

func TestFixSessionTracker_ResetClearsLast(t *testing.T) {
	base := agent.NewTestAgent()
	tr := &fixSessionTracker{enabled: true, base: base, out: io.Discard}

	tr.Capture("test-session-1")
	tr.Reset()

	a, resuming := tr.NextAgent()
	assert.Same(t, agent.Agent(base), a)
	assert.False(t, resuming)
}

func TestFixSessionTracker_NonSessionAgentWarnsOnce(t *testing.T) {
	base := &nonSessionAgent{}
	logged := &strings.Builder{}
	tr := &fixSessionTracker{
		enabled: true,
		base:    base,
		out:     logged,
	}

	for range 5 {
		a, resuming := tr.NextAgent()
		assert.Same(t, agent.Agent(base), a)
		assert.False(t, resuming)
	}

	out := logged.String()
	count := strings.Count(out, "does not support session resume")
	assert.Equal(t, 1, count, "warning fires exactly once across many calls")
}

func TestFixSessionTracker_NonSessionAgentWarningRespectsQuiet(t *testing.T) {
	base := &nonSessionAgent{}
	logged := &strings.Builder{}
	tr := &fixSessionTracker{
		enabled: true,
		base:    base,
		quiet:   true,
		out:     logged,
	}

	tr.NextAgent()
	tr.NextAgent()
	assert.Empty(t, logged.String(), "no warning when --quiet")
}
