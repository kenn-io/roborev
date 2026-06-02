package daemon

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

// memberSpec describes one panel member to enqueue in a test run.
type memberSpec struct {
	name         string
	agent        string
	instructions string
}

// enqueuePanelRun builds and enqueues a panel run (members + gated synthesis)
// for the given members against the repo HEAD. It returns the run UUID, the
// member jobs, and the synthesis job.
func enqueuePanelRun(
	t *testing.T, tc *workerTestContext, panelName string, members []memberSpec,
) (string, []*storage.ReviewJob, *storage.ReviewJob) {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)

	runUUID := uuid.NewString()
	opts := make([]storage.EnqueueOpts, 0, len(members))
	for i, m := range members {
		cfgJSON, err := json.Marshal(config.ResolvedMember{
			Name:         m.name,
			Index:        i,
			Agent:        m.agent,
			Instructions: m.instructions,
		})
		require.NoError(t, err)
		opts = append(opts, storage.EnqueueOpts{
			RepoID:                tc.Repo.ID,
			CommitID:              commit.ID,
			GitRef:                sha,
			Agent:                 m.agent,
			JobType:               storage.JobTypeReview,
			PanelRunUUID:          runUUID,
			PanelRole:             storage.PanelRoleMember,
			PanelName:             panelName,
			PanelMemberName:       m.name,
			PanelMemberIndex:      i,
			PanelMemberConfigJSON: string(cfgJSON),
		})
	}
	synthesis := storage.EnqueueOpts{
		RepoID:       tc.Repo.ID,
		CommitID:     commit.ID,
		GitRef:       sha,
		Agent:        "test",
		PanelRunUUID: runUUID,
		PanelRole:    storage.PanelRoleSynthesis,
		PanelName:    panelName,
	}
	memberJobs, synthJob, err := tc.DB.EnqueuePanelRun(opts, synthesis)
	require.NoError(t, err)
	require.Len(t, memberJobs, len(members))
	require.NotNil(t, synthJob)
	return runUUID, memberJobs, synthJob
}

// registerCapturingAgent registers a FakeAgent that records the prompt it was
// handed into *captured and returns a clean "no issues" verdict.
func registerCapturingAgent(t *testing.T, name string, captured *string) {
	t.Helper()
	agent.Register(&agent.FakeAgent{
		NameStr: name,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, reviewPrompt string, output io.Writer) (string, error) {
			*captured = reviewPrompt
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(name) })
}

// registerPassingAgent registers a FakeAgent that always returns a clean verdict.
func registerPassingAgent(t *testing.T, name string) {
	t.Helper()
	agent.Register(&agent.FakeAgent{
		NameStr: name,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, reviewPrompt string, output io.Writer) (string, error) {
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(name) })
}

// claimNext claims the next queued job, asserting one is available. ClaimJob
// skips the claim_blocked synthesis row, so this yields members only.
func claimNext(t *testing.T, tc *workerTestContext) *storage.ReviewJob {
	t.Helper()
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed, "expected a claimable member job")
	return claimed
}

func TestMemberInstructionsAppended(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	var captured string
	const agentName = "panel-member-capture"
	registerCapturingAgent(t, agentName, &captured)

	_, _, _ = enqueuePanelRun(t, tc, "security-panel", []memberSpec{
		{name: "auth-reviewer", agent: agentName, instructions: "Focus on auth boundaries."},
	})

	claimed := claimNext(t, tc)
	tc.Pool.processJob(testWorkerID, claimed)

	assert.Contains(captured, "Focus on auth boundaries.")
	assert.Contains(captured, "auth-reviewer")
	assert.Contains(captured, "Additional reviewer instructions")
}

func TestMemberInstructionsNotAppendedForNonPanelJob(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	var captured string
	const agentName = "non-panel-capture"
	registerCapturingAgent(t, agentName, &captured)

	claimed := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agentName)
	tc.Pool.processJob(testWorkerID, claimed)

	assert.NotContains(t, captured, "Additional reviewer instructions")
}

func TestReleaseOnLastMemberDone(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const agentName = "panel-pass"
	registerPassingAgent(t, agentName)

	runUUID, _, _ := enqueuePanelRun(t, tc, "two-member-panel", []memberSpec{
		{name: "m0", agent: agentName},
		{name: "m1", agent: agentName},
	})

	// First member done — one member still queued, synthesis stays blocked.
	first := claimNext(t, tc)
	tc.Pool.processJob(testWorkerID, first)
	tc.assertJobStatus(t, first.ID, storage.JobStatusDone)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked, "synthesis must stay blocked until all members terminal")

	// Second (last) member done — synthesis releases.
	second := claimNext(t, tc)
	tc.Pool.processJob(testWorkerID, second)
	tc.assertJobStatus(t, second.ID, storage.JobStatusDone)
	synth, err = tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked, "synthesis must release after the last member is terminal")
}

func TestPanelMemberSavesTokenUsageBeforeRelease(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const agentName = "panel-member-token"
	agent.Register(&sessionStreamingTestAgent{
		name:       agentName,
		streamLine: `{"type":"thread.started","thread_id":"member-session-123"}`,
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	runUUID, _, _ := enqueuePanelRun(t, tc, "member-token-panel", []memberSpec{
		{name: "m0", agent: agentName},
	})

	var fetchedSession string
	tc.Pool.tokenUsageFetcher = func(ctx context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSession = sessionID
		synth, err := tc.DB.GetSynthesisJob(runUUID)
		require.NoError(t, err)
		assert.True(synth.ClaimBlocked, "synthesis must stay blocked while member token usage is captured")
		return &tokens.Usage{CostUSD: 0.11, HasCost: true}, nil
	}

	member := claimNext(t, tc)
	tc.Pool.processJob(testWorkerID, member)

	updated := tc.assertJobStatus(t, member.ID, storage.JobStatusDone)
	assert.Equal("member-session-123", fetchedSession)
	assert.Contains(updated.TokenUsage, `"cost_usd":0.11`)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked, "synthesis releases after member token usage is captured")
}

func TestReleaseOnAllMembersFailed(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	// Members use a FakeAgent so agent lookup succeeds; the failure is driven
	// through the real failOrRetry path, not the agent.
	const agentName = "panel-fail"
	registerPassingAgent(t, agentName)

	runUUID, _, _ := enqueuePanelRun(t, tc, "fail-panel", []memberSpec{
		{name: "m0", agent: agentName},
		{name: "m1", agent: agentName},
	})

	// Drive the first member to terminal failure via the real broadcastFailed
	// chokepoint: exhaust retries, then one non-retryable fail.
	first := claimNext(t, tc)
	first = tc.exhaustRetries(t, first, testWorkerID, agentName)
	tc.Pool.failOrRetry(testWorkerID, first, agentName, "terminal failure")
	tc.assertJobStatus(t, first.ID, storage.JobStatusFailed)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked, "synthesis stays blocked while a member is still alive")

	// Second member fails the same way — now all members terminal, release.
	second := claimNext(t, tc)
	second = tc.exhaustRetries(t, second, testWorkerID, agentName)
	tc.Pool.failOrRetry(testWorkerID, second, agentName, "terminal failure")
	tc.assertJobStatus(t, second.ID, storage.JobStatusFailed)
	synth, err = tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked, "synthesis releases after the last member fails")
}
