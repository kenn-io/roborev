package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"
)

// TestAgentCall records a single Review() invocation for assertion in tests.
type TestAgentCall struct {
	SessionID string // SessionID set on the agent at the time of the call ("" = fresh)
	Prompt    string
}

// TestAgent is a mock agent for testing that returns predictable output.
//
// Each Review() call emits a synthetic session event as the first line
// of streamed output so callers wired through SessionCaptureWriter pick
// up a session ID. Fresh calls (SessionID == "") emit a counter-based
// new ID like "test-session-1". Resumed calls (SessionID != "") echo
// the incoming ID. Counters are per-instance.
type TestAgent struct {
	Delay     time.Duration  // Simulated processing delay
	Output    string         // Fixed output to return
	Fail      bool           // If true, returns an error
	Reasoning ReasoningLevel // Reasoning level (for testing)
	SessionID string         // Incoming session ID for resume; "" = fresh

	// shared across clones produced by With* methods so the counter and
	// recording are consistent regardless of which clone Review() is called on.
	state *testAgentState
}

type testAgentState struct {
	mu      sync.Mutex
	counter int
	calls   []TestAgentCall
}

// NewTestAgent creates a new test agent with its own per-instance counter.
func NewTestAgent() *TestAgent {
	return &TestAgent{
		Delay:     100 * time.Millisecond,
		Output:    "Test review output: This commit looks good. No issues found.",
		Reasoning: ReasoningStandard,
		state:     &testAgentState{},
	}
}

func (a *TestAgent) clone() *TestAgent {
	return &TestAgent{
		Delay:     a.Delay,
		Output:    a.Output,
		Fail:      a.Fail,
		Reasoning: a.Reasoning,
		SessionID: a.SessionID,
		state:     a.state,
	}
}

// WithReasoning returns a copy of the agent with the specified reasoning level
func (a *TestAgent) WithReasoning(level ReasoningLevel) Agent {
	c := a.clone()
	c.Reasoning = level
	return c
}

// WithAgentic returns the agent unchanged (agentic mode not applicable for test agent)
func (a *TestAgent) WithAgentic(agentic bool) Agent {
	return a
}

// WithModel returns the agent unchanged (model selection not supported for test agent).
func (a *TestAgent) WithModel(model string) Agent {
	return a
}

// WithSessionID returns a copy configured to resume the given session.
// Implements SessionAgent.
func (a *TestAgent) WithSessionID(sessionID string) Agent {
	c := a.clone()
	c.SessionID = sessionID
	return c
}

func (a *TestAgent) CommandLine() string {
	return "test"
}

func (a *TestAgent) Name() string {
	return "test"
}

// Calls returns a copy of every Review invocation recorded by this
// agent (and any clones that share its state).
func (a *TestAgent) Calls() []TestAgentCall {
	a.state.mu.Lock()
	defer a.state.mu.Unlock()
	out := make([]TestAgentCall, len(a.state.calls))
	copy(out, a.state.calls)
	return out
}

func (a *TestAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// Respect context cancellation
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(a.Delay):
	}

	// Determine the session ID to advertise: echo incoming, or mint fresh.
	var sessionID string
	a.state.mu.Lock()
	if a.SessionID != "" {
		sessionID = a.SessionID
	} else {
		a.state.counter++
		sessionID = fmt.Sprintf("test-session-%d", a.state.counter)
	}
	a.state.calls = append(a.state.calls, TestAgentCall{
		SessionID: a.SessionID,
		Prompt:    prompt,
	})
	a.state.mu.Unlock()

	if a.Fail {
		return "", fmt.Errorf("test agent configured to fail")
	}

	shortSHA := gitrepo.ShortSHA(commitSHA)
	sessionLine := fmt.Sprintf(`{"type":"session","id":%q}`+"\n", sessionID)
	body := fmt.Sprintf("%s\n\nCommit: %s\nRepo: %s", a.Output, shortSHA, repoPath)
	streamed := sessionLine + body

	if output != nil {
		if _, err := output.Write([]byte(streamed)); err != nil {
			return "", fmt.Errorf("write output: %w", err)
		}
	}
	return body, nil
}

// FakeAgent implements Agent for tests outside the agent package.
type FakeAgent struct {
	NameStr  string
	ReviewFn func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error)
}

func (a *FakeAgent) Name() string { return a.NameStr }
func (a *FakeAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	if a.ReviewFn != nil {
		return a.ReviewFn(ctx, repoPath, commitSHA, prompt, output)
	}
	return "", nil
}
func (a *FakeAgent) WithReasoning(level ReasoningLevel) Agent { return a }
func (a *FakeAgent) WithAgentic(agentic bool) Agent           { return a }
func (a *FakeAgent) WithModel(model string) Agent             { return a }
func (a *FakeAgent) CommandLine() string                      { return "" }

func init() {
	Register(NewTestAgent())
}
