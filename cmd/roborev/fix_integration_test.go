//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestEnqueueIfNeeded(t *testing.T) {
	// Constants used across tests
	const sha = "abc123def456"

	tests := []struct {
		name             string
		jobResponses     []any
		expectedChecks   int32
		expectedEnqueues int32
		minChecks        int32
		checkExact       bool
	}{
		{
			name: "SkipsWhenJobAppearsAfterWait",
			jobResponses: []any{
				map[string]any{"jobs": []any{}},                         // First call
				map[string]any{"jobs": []any{map[string]any{"id": 42}}}, // Second call
			},
			expectedChecks:   2,
			expectedEnqueues: 0,
			checkExact:       true,
		},
		{
			name: "EnqueuesWhenNoJobExists",
			jobResponses: []any{
				map[string]any{"jobs": []any{}},
			},
			minChecks:        1,
			expectedEnqueues: 1,
			checkExact:       false,
		},
	}

	tmpDir := initTestGitRepo(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jobCheckCalls atomic.Int32
			var enqueueCalls atomic.Int32

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/jobs":
					n := jobCheckCalls.Add(1)
					if len(tt.jobResponses) == 0 {
						assert.Equal(t, false, len(tt.jobResponses) == 0, "jobResponses must not be empty")
						http.Error(w, "jobResponses empty", http.StatusInternalServerError)
						return
					}
					// Return the response corresponding to the call sequence, or the last one
					idx := int(n - 1)
					if idx >= len(tt.jobResponses) {
						idx = len(tt.jobResponses) - 1
					}
					json.NewEncoder(w).Encode(tt.jobResponses[idx])
				case "/api/enqueue":
					enqueueCalls.Add(1)
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"id": 99})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			ts := httptest.NewServer(handler)
			defer ts.Close()

			err := enqueueIfNeeded(context.Background(), ts.URL, tmpDir, sha)
			require.NoError(t, err, "enqueueIfNeeded: %v")

			if tt.checkExact {
				assert.Equal(t, false, jobCheckCalls.Load() != tt.expectedChecks)
			} else {
				assert.Equal(t, false, jobCheckCalls.Load() < tt.minChecks)
			}

			assert.Equal(t, false, enqueueCalls.Load() != tt.expectedEnqueues)
		})
	}
}

// withFreshTestAgent registers a brand-new *agent.TestAgent under the
// name "test", returning the instance so the test can read its
// recorded calls. Restores the default test agent in cleanup.
func withFreshTestAgent(t *testing.T) *agent.TestAgent {
	t.Helper()
	a := agent.NewTestAgent()
	agent.Register(a)
	t.Cleanup(func() {
		agent.Unregister("test")
		agent.Register(agent.NewTestAgent())
	})
	return a
}

func TestRunFixBatch_CountCapGroupsIntoTwoTwoOne(t *testing.T) {
	tester := withFreshTestAgent(t)

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	jobs := []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 2, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 3, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 4, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 5, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
	}
	_ = newMockDaemonBuilder(t).
		WithJobs(jobs).
		WithReview(1, "F1: missing error handling").
		WithReview(2, "F2: unused variable").
		WithReview(3, "F3: deadlock").
		WithReview(4, "F4: race").
		WithReview(5, "F5: leak").
		Build()

	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", quiet: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			base: base,
			out:  cmd.OutOrStdout(),
		}
		return runFixBatch(cmd, []int64{1, 2, 3, 4, 5}, "", false, false, false, 2, opts, tracker)
	})
	require.NoError(t, err)

	calls := tester.Calls()
	require.Len(t, calls, 3, "5 jobs / batch-size 2 = 3 invocations")
	assert.Contains(t, calls[0].Prompt, "F1")
	assert.Contains(t, calls[0].Prompt, "F2")
	assert.Contains(t, calls[1].Prompt, "F3")
	assert.Contains(t, calls[1].Prompt, "F4")
	assert.Contains(t, calls[2].Prompt, "F5")
	assert.NotContains(t, calls[2].Prompt, "F4")
}

func TestRunFix_ResumeChainsSessionAcrossSingleJobCalls(t *testing.T) {
	tester := withFreshTestAgent(t)

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	jobs := []storage.ReviewJob{
		{ID: 11, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 12, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		{ID: 13, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
	}
	_ = newMockDaemonBuilder(t).
		WithJobs(jobs).
		WithReview(11, "Issue 1").
		WithReview(12, "Issue 2").
		WithReview(13, "Issue 3").
		Build()

	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", quiet: true, resume: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			enabled: true,
			base:    base,
			quiet:   true,
			out:     cmd.OutOrStdout(),
		}
		return runFix(cmd, []int64{11, 12, 13}, opts, tracker)
	})
	require.NoError(t, err)

	calls := tester.Calls()
	require.Len(t, calls, 3)
	assert.Empty(t, calls[0].SessionID, "first call is fresh")
	assert.Equal(t, "test-session-1", calls[1].SessionID, "second call resumes session 1")
	assert.Equal(t, "test-session-1", calls[2].SessionID,
		"third call resumes session 1 — chain preserved because the test agent echoes incoming IDs")
}

func TestRunFix_BatchSizeAndResumeCombined(t *testing.T) {
	tester := withFreshTestAgent(t)

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	jobs := make([]storage.ReviewJob, 6)
	builder := newMockDaemonBuilder(t)
	for i := range 6 {
		jobs[i] = storage.ReviewJob{ID: int64(20 + i), Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"}
		builder = builder.WithReview(int64(20+i), fmt.Sprintf("issue %d", i))
	}
	_ = builder.WithJobs(jobs).Build()

	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", quiet: true, resume: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			enabled: true,
			base:    base,
			quiet:   true,
			out:     cmd.OutOrStdout(),
		}
		return runFixBatch(cmd, []int64{20, 21, 22, 23, 24, 25}, "", false, false, false, 3, opts, tracker)
	})
	require.NoError(t, err)

	calls := tester.Calls()
	require.Len(t, calls, 2, "6 jobs / batch-size 3 = 2 invocations")
	assert.Empty(t, calls[0].SessionID)
	assert.Equal(t, "test-session-1", calls[1].SessionID)
}

// failOnNthAgent wraps an Agent and returns an error on the failOn-th
// call (1-based). Other methods pass through.
type failOnNthAgent struct {
	inner  *agent.TestAgent
	failOn int
	calls  *int
}

func (a *failOnNthAgent) Name() string { return a.inner.Name() }
func (a *failOnNthAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	*a.calls++
	if *a.calls == a.failOn {
		// Still record the call so we can assert SessionID was passed in.
		_, _ = a.inner.Review(ctx, repoPath, commitSHA, prompt, io.Discard)
		return "", fmt.Errorf("simulated failure on call %d", a.failOn)
	}
	return a.inner.Review(ctx, repoPath, commitSHA, prompt, output)
}

func (a *failOnNthAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return &failOnNthAgent{inner: a.inner.WithReasoning(level).(*agent.TestAgent), failOn: a.failOn, calls: a.calls}
}

func (a *failOnNthAgent) WithAgentic(agentic bool) agent.Agent {
	return a
}

func (a *failOnNthAgent) WithModel(model string) agent.Agent {
	return a
}

func (a *failOnNthAgent) WithSessionID(id string) agent.Agent {
	return &failOnNthAgent{inner: a.inner.WithSessionID(id).(*agent.TestAgent), failOn: a.failOn, calls: a.calls}
}
func (a *failOnNthAgent) CommandLine() string { return a.inner.CommandLine() }

// namedAgent overrides Name() to keep the registry happy after
// substitution. It intentionally does NOT implement SessionAgent so
// wrapping a non-session inner agent doesn't accidentally advertise
// resume support to the tracker.
type namedAgent struct {
	name  string
	inner agent.Agent
}

func (a *namedAgent) Name() string { return a.name }
func (a *namedAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return a.inner.Review(ctx, repoPath, commitSHA, prompt, output)
}

func (a *namedAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return &namedAgent{name: a.name, inner: a.inner.WithReasoning(level)}
}

func (a *namedAgent) WithAgentic(agentic bool) agent.Agent {
	return &namedAgent{name: a.name, inner: a.inner.WithAgentic(agentic)}
}

func (a *namedAgent) WithModel(model string) agent.Agent {
	return &namedAgent{name: a.name, inner: a.inner.WithModel(model)}
}
func (a *namedAgent) CommandLine() string { return a.inner.CommandLine() }

// namedSessionAgent extends namedAgent with WithSessionID for cases
// where the inner agent supports session resume (e.g. the cascade test).
type namedSessionAgent struct {
	namedAgent
}

func (a *namedSessionAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return &namedSessionAgent{namedAgent: namedAgent{name: a.name, inner: a.inner.WithReasoning(level)}}
}

func (a *namedSessionAgent) WithAgentic(agentic bool) agent.Agent {
	return &namedSessionAgent{namedAgent: namedAgent{name: a.name, inner: a.inner.WithAgentic(agentic)}}
}

func (a *namedSessionAgent) WithModel(model string) agent.Agent {
	return &namedSessionAgent{namedAgent: namedAgent{name: a.name, inner: a.inner.WithModel(model)}}
}

func (a *namedSessionAgent) WithSessionID(id string) agent.Agent {
	if sa, ok := a.inner.(agent.SessionAgent); ok {
		return &namedSessionAgent{namedAgent: namedAgent{name: a.name, inner: sa.WithSessionID(id)}}
	}
	return a
}

func TestRunFix_ResumeCascadeBrokenOnError(t *testing.T) {
	tester := withFreshTestAgent(t)
	callIdx := 0

	// Replace the registered "test" agent with a wrapper that fails on call 2.
	failer := &failOnNthAgent{inner: tester, failOn: 2, calls: &callIdx}
	agent.Unregister("test")
	agent.Register(&namedSessionAgent{namedAgent: namedAgent{name: "test", inner: failer}})
	t.Cleanup(func() {
		agent.Unregister("test")
		agent.Register(agent.NewTestAgent())
	})

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	jobs := make([]storage.ReviewJob, 6)
	builder := newMockDaemonBuilder(t)
	for i := range 6 {
		jobs[i] = storage.ReviewJob{ID: int64(30 + i), Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"}
		builder = builder.WithReview(int64(30+i), fmt.Sprintf("issue %d", i))
	}
	_ = builder.WithJobs(jobs).Build()

	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", quiet: true, resume: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			enabled: true,
			base:    base,
			quiet:   true,
			out:     cmd.OutOrStdout(),
		}
		// Errors are expected mid-run; runFixBatch logs and continues.
		return runFixBatch(cmd, []int64{30, 31, 32, 33, 34, 35}, "", false, false, false, 2, opts, tracker)
	})
	require.NoError(t, err)

	calls := tester.Calls()
	require.GreaterOrEqual(t, len(calls), 3,
		"3 invocations expected: success, fail (still records), success-fresh")
	assert.Empty(t, calls[0].SessionID, "first call fresh")
	assert.Equal(t, "test-session-1", calls[1].SessionID, "second call attempted resume before failing")
	assert.Empty(t, calls[2].SessionID, "third call starts fresh after Reset()")
}

type nonSessionStub struct{ calls int }

func (a *nonSessionStub) Name() string { return "test" }
func (a *nonSessionStub) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	a.calls++
	if output != nil {
		_, _ = output.Write([]byte("plain output"))
	}
	return "ok", nil
}
func (a *nonSessionStub) WithReasoning(level agent.ReasoningLevel) agent.Agent { return a }
func (a *nonSessionStub) WithAgentic(agentic bool) agent.Agent                 { return a }
func (a *nonSessionStub) WithModel(model string) agent.Agent                   { return a }
func (a *nonSessionStub) CommandLine() string                                  { return "test" }

func TestRunFix_ResumeWithNonSessionAgentWarnsOnce(t *testing.T) {
	stub := &nonSessionStub{}
	agent.Unregister("test")
	agent.Register(&namedAgent{name: "test", inner: stub})
	t.Cleanup(func() {
		agent.Unregister("test")
		agent.Register(agent.NewTestAgent())
	})

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	_ = newMockDaemonBuilder(t).
		WithJobs([]storage.ReviewJob{
			{ID: 50, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
			{ID: 51, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		}).
		WithReview(50, "f").
		WithReview(51, "f").
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", resume: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			enabled: true,
			base:    base,
			out:     cmd.OutOrStdout(),
		}
		return runFix(cmd, []int64{50, 51}, opts, tracker)
	})
	require.NoError(t, err)

	assert.Equal(t, 2, stub.calls, "both jobs should reach the agent")
	count := strings.Count(out, "does not support session resume")
	assert.Equal(t, 1, count, "warning fires exactly once across multiple calls")
}

func TestRunFix_QuietSuppressesResumeAndUnsupportedWarnings(t *testing.T) {
	stub := &nonSessionStub{}
	agent.Unregister("test")
	agent.Register(&namedAgent{name: "test", inner: stub})
	t.Cleanup(func() {
		agent.Unregister("test")
		agent.Register(agent.NewTestAgent())
	})

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	_ = newMockDaemonBuilder(t).
		WithJobs([]storage.ReviewJob{
			{ID: 60, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
			{ID: 61, Status: storage.JobStatusDone, RepoPath: repo.Dir, GitRef: "abc123", Agent: "test"},
		}).
		WithReview(60, "f").
		WithReview(61, "f").
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		opts := fixOptions{agentName: "test", quiet: true, resume: true}
		base, err := resolveFixAgent(repo.Dir, opts)
		if err != nil {
			return err
		}
		tracker := &fixSessionTracker{
			enabled: true,
			base:    base,
			quiet:   true,
			out:     cmd.OutOrStdout(),
		}
		return runFix(cmd, []int64{60, 61}, opts, tracker)
	})
	require.NoError(t, err)

	assert.Equal(t, 2, stub.calls, "both jobs should reach the agent")
	assert.NotContains(t, out, "does not support session resume")
	assert.NotContains(t, out, "Resuming session")
}
