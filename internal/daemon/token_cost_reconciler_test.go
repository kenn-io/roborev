package daemon

import (
	"context"
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
		require.NoError(t, tc.DB.BackfillJobTokenUsage(job.ID, sessionID, tokenUsage))
	}
	return job
}
