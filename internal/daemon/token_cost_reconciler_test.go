package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

func TestDelayedTokenCostRetriesAfterImmediateCaptureMiss(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(
		t, tc, "delayed-session", `{"total_output_tokens":481}`,
	)
	tc.Pool.tokenCostScanInterval = time.Hour
	tc.Pool.tokenCostRetryInterval = 5 * time.Millisecond

	var indexed atomic.Bool
	var attempts atomic.Int32
	tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		attempts.Add(1)
		if !indexed.Load() {
			return nil, nil
		}
		return &tokens.Usage{HasCost: true, CostUSD: 0.17}, nil
	}

	tc.Pool.captureTokenUsageForSession(
		context.Background(), testWorkerID, job, "delayed-session",
	)
	indexed.Store(true)
	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(job.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)

	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Greater(t, attempts.Load(), int32(1))
	assert.Equal(t, int64(481), usage.OutputTokens)
	assert.InDelta(t, 0.17, usage.CostUSD, 1e-9)
}

func TestTokenCostReconcilerDiscoversPersistedCandidateAtStartup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(
		t, tc, "persisted-session", `{"total_output_tokens":812}`,
	)
	tc.Pool.tokenCostScanInterval = time.Hour
	tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		return &tokens.Usage{HasCost: true, CostUSD: 0.29}, nil
	}

	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(job.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)
}

func TestTokenCostReconcilerRecoversSessionFromJobLogAtStartup(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	require.NoError(t, tc.DB.CompleteJob(
		job.ID, "codex", "prompt", "No issues found.",
	))

	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"recovered-session"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":1024,`+
			`"output_tokens":64}}`+"\n",
	), 0o600))
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		assert.Equal(t, "recovered-session", sessionID)
		return &tokens.Usage{HasCost: true, CostUSD: 0.21}, nil
	}
	tc.Pool.recoverTokenUsageLogs(context.Background())
	recovered, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "recovered-session", recovered.SessionID)
	candidates, err := tc.DB.ListTokenCostCandidates(0, 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(job.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return updated.SessionID == "recovered-session" &&
			usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)
}

func TestTokenCostReconcilerRejectsJobLogFromPriorAttempt(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	require.NoError(t, tc.DB.CompleteJob(
		job.ID, "codex", "prompt", "Setup failed before agent invocation.",
	))

	completed, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, completed.StartedAt)
	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"prior-session"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":512,`+
			`"output_tokens":32}}`+"\n",
	), 0o600))
	priorAttempt := completed.StartedAt.Add(-time.Minute)
	require.NoError(t, os.Chtimes(logPath, priorAttempt, priorAttempt))

	tc.Pool.recoverTokenUsageLogs(context.Background())

	recovered, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, recovered.SessionID)
	assert.Empty(t, recovered.TokenUsage)
}

func TestTokenCostReconcilerAdvancesPastUnavailableCandidate(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	first := seedTokenCostCandidate(
		t, tc, "missing-session", `{"total_output_tokens":1}`,
	)
	second := seedTokenCostCandidate(t, tc, "priced-session", `{}`)
	tc.Pool.tokenCostScanInterval = 5 * time.Millisecond
	tc.Pool.tokenCostRetryInterval = time.Hour
	tc.Pool.tokenCostPageSize = 1
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		if sessionID == "missing-session" {
			return nil, nil
		}
		return &tokens.Usage{HasCost: true, CostUSD: 0.41}, nil
	}

	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(second.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)

	updated, err := tc.DB.GetJobByID(first.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.False(t, usage.HasCost)
}

func TestTokenCostReconcilerShutdownCancelsProviderLookup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	seedTokenCostCandidate(t, tc, "blocking-session", `{}`)

	started := make(chan struct{})
	tc.Pool.tokenUsageFetcher = func(ctx context.Context, _ string) (*tokens.Usage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	tc.Pool.Start()
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		tc.Pool.Stop()
		close(stopped)
	}()
	require.Eventually(t, func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
}

func TestTokenCostReconcilerMergesWithUsageSavedDuringLookup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(
		t, tc, "concurrent-session", `{"total_output_tokens":10}`,
	)

	started := make(chan struct{})
	release := make(chan struct{})
	tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		close(started)
		<-release
		return &tokens.Usage{HasCost: true, CostUSD: 0.33}, nil
	}
	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, tc.DB.SaveJobTokenUsage(
		job.ID,
		"concurrent-session",
		`{"total_output_tokens":20,"peak_context_tokens":200}`,
	))
	close(release)

	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(job.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)

	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(20), usage.OutputTokens)
	assert.Equal(t, int64(200), usage.PeakContextTokens)
	assert.InDelta(t, 0.33, usage.CostUSD, 1e-9)
}

func TestTokenCostRetryAdmissionIsBounded(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	tc.Pool.tokenCostPendingLimit = 2
	pending := make(map[int64]tokenCostRetryState)

	assert := assert.New(t)
	assert.True(tc.Pool.addTokenCostRetry(pending, 1))
	assert.True(tc.Pool.addTokenCostRetry(pending, 2))
	assert.True(tc.Pool.addTokenCostRetry(pending, 1), "duplicate remains admitted")
	assert.False(tc.Pool.addTokenCostRetry(pending, 3))
	assert.Len(pending, 2)
}

func seedTokenCostCandidate(
	t *testing.T, tc *workerTestContext, sessionID, tokenUsage string,
) *storage.ReviewJob {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	require.NoError(t, tc.DB.MarkJobAgentInvoked(job.ID, testWorkerID, "codex review"))
	require.NoError(t, tc.DB.SaveJobSessionID(job.ID, testWorkerID, sessionID))
	require.NoError(t, tc.DB.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	if tokenUsage != "" {
		updated, err := tc.DB.BackfillJobTokenUsageIfCurrent(storage.TokenUsageWrite{
			JobID:                job.ID,
			SessionID:            sessionID,
			TokenUsageJSON:       tokenUsage,
			RequireUniqueSession: true,
		})
		require.NoError(t, err)
		require.True(t, updated)
	}
	return job
}
