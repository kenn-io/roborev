package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
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
		t.Context(), testWorkerID, job, "delayed-session",
	)
	indexed.Store(true)
	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	// Wall-clock wait: SQLite-backed token reconciliation.
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

	// Wall-clock wait: SQLite-backed token reconciliation.
	require.Eventually(t, func() bool {
		updated, err := tc.DB.GetJobByID(job.ID)
		if err != nil {
			return false
		}
		usage := tokens.ParseJSON(updated.TokenUsage)
		return usage != nil && usage.HasCost
	}, time.Second, 5*time.Millisecond)
}

func TestTokenCostReconcilerAggregatesPlanPhaseSessions(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(t, tc, "implementation-session", `{"input_tokens":300,"total_output_tokens":40,"provider_session_ids":["planner-session","implementation-session"]}`)
	candidate, err := tc.DB.GetTokenCostCandidate(job.ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	var fetchedSessions []string
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSessions = append(fetchedSessions, sessionID)
		costs := map[string]float64{"planner-session": 0.13, "implementation-session": 0.29}
		return &tokens.Usage{CostUSD: costs[sessionID], HasCost: true}, nil
	}

	resolved, err := tc.Pool.reconcileTokenCostCandidate(t.Context(), *candidate, tc.Pool.fetchTokenUsage)
	require.NoError(t, err)
	assert.True(t, resolved)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, fetchedSessions)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.Equal(t, []string{"planner-session", "implementation-session"}, usage.ProviderSessionIDs)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestTokenCostReconcilerPreservesStoredCountsWhenPlanPhaseHasNoCounts(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(t, tc, "implementation-session", `{"input_tokens":300,"total_output_tokens":40,"provider_session_ids":["planner-session","implementation-session"],"expected_provider_sessions":2}`)
	candidate, err := tc.DB.GetTokenCostCandidate(job.ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		if sessionID == "planner-session" {
			return &tokens.Usage{CostUSD: 0.13, HasCost: true}, nil
		}
		return &tokens.Usage{InputTokens: 200, OutputTokens: 25, CostUSD: 0.29, HasCost: true}, nil
	}

	resolved, err := tc.Pool.reconcileTokenCostCandidate(t.Context(), *candidate, tc.Pool.fetchTokenUsage)
	require.NoError(t, err)
	assert.True(t, resolved)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestTokenCostReconcilerPricesAvailableSessionWithMissingPlanSession(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(t, tc, "implementation-session", `{"input_tokens":300,"total_output_tokens":40,"provider_session_ids":["implementation-session"],"expected_provider_sessions":2}`)
	candidate, err := tc.DB.GetTokenCostCandidate(job.ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	fetches := 0
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		fetches++
		assert.Equal(t, "implementation-session", sessionID)
		return &tokens.Usage{InputTokens: 200, OutputTokens: 25, CostUSD: 0.29, HasCost: true}, nil
	}

	resolved, err := tc.Pool.reconcileTokenCostCandidate(t.Context(), *candidate, tc.Pool.fetchTokenUsage)
	require.NoError(t, err)
	assert.True(t, resolved, "the available implementation session is priced")
	assert.Equal(t, 1, fetches)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost, "reconciliation recovers known implementation dollars")
	assert.InDelta(t, 0.29, usage.CostUSD, 1e-9)
	assert.EqualValues(t, 300, usage.InputTokens, "combined phase tokens stay unchanged")
	assert.EqualValues(t, 40, usage.OutputTokens, "combined phase tokens stay unchanged")
	assert.Equal(t, []string{"implementation-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 2, usage.ExpectedProviderSessions, "planner cost coverage stays partial")
}

func TestTokenCostReconcilerRecoversSessionFromJobLogAtStartup(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB,
		job.ID, "codex", "prompt", "No issues found.",
	))

	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"recovered-session"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":1024,`+
			`"output_tokens":64}}`+"\n",
	), 0o600))
	// Filesystem write timestamps can lag the nanosecond-precision job start.
	// Give this current-attempt fixture an explicit, whole-second timestamp.
	require.NotNil(t, job.StartedAt)
	logTime := job.StartedAt.Add(time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(logPath, logTime, logTime))

	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		assert.Equal(t, "recovered-session", sessionID)
		return &tokens.Usage{HasCost: true, CostUSD: 0.21}, nil
	}
	tc.Pool.recoverTokenUsageLogs(t.Context())
	recovered, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "recovered-session", recovered.SessionID)
	candidates, err := tc.DB.ListTokenCostCandidates(0, 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	tc.Pool.Start()
	t.Cleanup(tc.Pool.Stop)

	// Wall-clock wait: SQLite-backed token reconciliation retry.
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

func TestTokenCostReconcilerPreservesExpectedSessionsFromPlannerOnlyLog(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	saved, err := tc.DB.SaveRunningJobTokenUsage(
		job.ID, testWorkerID, job.StartedAtRaw,
		`{"thread_id":"planner-session","provider_session_ids":["planner-session"],`+
			`"expected_provider_sessions":2}`,
	)
	require.NoError(t, err)
	assert.True(t, saved)
	require.NoError(t, tc.DB.CancelJob(job.ID))
	released, err := tc.DB.ReleaseCanceledJob(job.ID, testWorkerID)
	require.NoError(t, err)
	assert.True(t, released)
	writeCodexUsageLog(t, job, "planner-session", 100, 0, 15)

	// Reconciliation's startup pass recovers the planner-only log after the
	// canceled attempt has been released from its former worker.
	tc.Pool.recoverTokenUsageLogs(t.Context())

	recovered, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "planner-session", recovered.SessionID)
	usage := tokens.ParseJSON(recovered.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, []string{"planner-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
}

func TestTokenCostReconcilerRejectsJobLogFromPriorAttempt(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB,
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

	tc.Pool.recoverTokenUsageLogs(t.Context())

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

	// Wall-clock wait: SQLite-backed token reconciliation.
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

func TestTokenCostReconcilerBacksOffRepeatedMisses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		job := seedTokenCostCandidate(t, tc, "late-session", `{}`)
		tc.Pool.tokenCostScanInterval = time.Minute
		tc.Pool.tokenCostRetryInterval = time.Hour
		// Keep worker lock retries and overlapping log scans from delaying cost
		// scans; SQLite retry sleeps on macOS let synctest.Wait return early.
		tc.Pool.numWorkers = 0
		tc.Pool.tokenUsageLogScanInterval = 24 * time.Hour

		var indexed atomic.Bool
		var lookups atomic.Int32
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			lookups.Add(1)
			if !indexed.Load() {
				return nil, nil
			}
			return &tokens.Usage{HasCost: true, CostUSD: 0.12}, nil
		}
		tc.Pool.Start()
		t.Cleanup(tc.Pool.Stop)
		synctest.Wait()
		require.Equal(t, int32(1), lookups.Load())

		// Misses wait 1, 2, 4, 8, and 16 minutes, so lookups run at
		// minutes 0, 1, 3, 7, 15, and 31 rather than every scan.
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		assert.Equal(t, int32(6), lookups.Load())

		// The next lookup is due at minute 63 and still prices the job.
		indexed.Store(true)
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(7), lookups.Load())
		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.InDelta(t, 0.12, usage.CostUSD, 1e-9)
	})
}

// startQuietTokenCostReconciler runs only the cost scan, once a minute, so a
// synctest bubble counts provider lookups without worker or log-scan noise.
func startQuietTokenCostReconciler(t *testing.T, pool *WorkerPool) {
	t.Helper()
	pool.tokenCostScanInterval = time.Minute
	pool.tokenCostRetryInterval = time.Hour
	// Keep worker lock retries and overlapping log scans from delaying cost
	// scans; SQLite retry sleeps on macOS let synctest.Wait return early.
	pool.numWorkers = 0
	pool.tokenUsageLogScanInterval = 24 * time.Hour
	pool.Start()
}

func TestTokenCostReconcilerStopsAskingAboutSessionWithoutUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		job := seedTokenCostCandidate(t, tc, "empty-session", `{}`)
		var lookups atomic.Int32
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			lookups.Add(1)
			return nil, tokens.ErrSessionWithoutUsage
		}
		startQuietTokenCostReconciler(t, tc.Pool)
		t.Cleanup(tc.Pool.Stop)

		// Lookups run at minutes 0, 1, 3, 7, and 15. The answer at minute 15
		// repeats the one before it and comes 15 minutes after the job
		// finished, so it is final.
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(t, int32(5), lookups.Load())

		time.Sleep(6 * 24 * time.Hour)
		synctest.Wait()
		assert.Equal(t, int32(5), lookups.Load())
		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Nil(t, tokens.ParseJSON(updated.TokenUsage))
	})
}

func TestTokenCostReconcilerPricesUsageThatArrivesAfterEmptyAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		job := seedTokenCostCandidate(t, tc, "arriving-session", `{}`)
		var indexed atomic.Bool
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			if !indexed.Load() {
				return nil, tokens.ErrSessionWithoutUsage
			}
			return &tokens.Usage{OutputTokens: 64, HasCost: true, CostUSD: 0.08}, nil
		}
		startQuietTokenCostReconciler(t, tc.Pool)
		t.Cleanup(tc.Pool.Stop)

		// Empty answers at minutes 0, 1, and 3 all come within 15 minutes of
		// the job finishing, so the lookup at minute 7 still runs.
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		indexed.Store(true)
		time.Sleep(3 * time.Minute)
		synctest.Wait()

		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.True(t, usage.HasCost)
		assert.InDelta(t, 0.08, usage.CostUSD, 1e-9)
	})
}

func TestTokenCostReconcilerKeepsAskingWhenEmptyAnswerMovesToAnotherPhase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		job := seedTokenCostCandidate(t, tc, "implementation-session", `{"input_tokens":300,"total_output_tokens":40,"provider_session_ids":["planner-session","implementation-session"],"expected_provider_sessions":2}`)
		var plannerIndexed, implementationIndexed atomic.Bool
		tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
			switch sessionID {
			case "planner-session":
				if !plannerIndexed.Load() {
					return nil, tokens.ErrSessionWithoutUsage
				}
				return &tokens.Usage{CostUSD: 0.13, HasCost: true}, nil
			case "implementation-session":
				if !implementationIndexed.Load() {
					return nil, tokens.ErrSessionWithoutUsage
				}
				return &tokens.Usage{CostUSD: 0.29, HasCost: true}, nil
			}
			return nil, fmt.Errorf("unexpected session %q", sessionID)
		}
		startQuietTokenCostReconciler(t, tc.Pool)
		t.Cleanup(tc.Pool.Stop)

		// The planner comes back empty at minutes 0, 1, 3, and 7.
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		// At minute 15 the planner has usage and the implementation session
		// is empty for the first time, so the lookup at minute 31 still runs.
		plannerIndexed.Store(true)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		implementationIndexed.Store(true)
		time.Sleep(15 * time.Minute)
		synctest.Wait()

		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.True(t, usage.HasCost)
		assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
	})
}

func TestTokenCostReconcilerBackoffSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		seedTokenCostCandidate(t, tc, "unindexed-session", `{}`)
		var lookups atomic.Int32
		fetcher := func(context.Context, string) (*tokens.Usage, error) {
			lookups.Add(1)
			return nil, nil
		}
		tc.Pool.tokenUsageFetcher = fetcher
		startQuietTokenCostReconciler(t, tc.Pool)

		// Lookups at minutes 0, 1, and 3 schedule the next one for minute 7.
		time.Sleep(3*time.Minute + 30*time.Second)
		synctest.Wait()
		require.Equal(t, int32(3), lookups.Load())
		tc.Pool.Stop()

		cfg := config.DefaultConfig()
		restarted := NewWorkerPool(tc.DB, NewStaticConfig(cfg), cfg.MaxWorkers, tc.Broadcaster, nil, nil)
		restarted.tokenUsageFetcher = fetcher
		startQuietTokenCostReconciler(t, restarted)
		t.Cleanup(restarted.Stop)

		time.Sleep(3*time.Minute + 30*time.Second)
		synctest.Wait()
		assert.Equal(t, int32(3), lookups.Load(), "restart must not reset the backoff")

		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(4), lookups.Load())
	})
}

func TestTokenCostReconcilerShutdownCancelsProviderLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		seedTokenCostCandidate(t, tc, "blocking-session", `{}`)

		started := make(chan struct{})
		tc.Pool.tokenUsageFetcher = func(ctx context.Context, _ string) (*tokens.Usage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		tc.Pool.Start()
		synctest.Wait()
		<-started

		stopped := make(chan struct{})
		go func() {
			tc.Pool.Stop()
			close(stopped)
		}()
		synctest.Wait()
		<-stopped
	})
}

func TestTokenCostReconcilerMergesWithUsageSavedDuringLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		<-started

		require.NoError(t, tc.DB.SaveJobTokenUsage(
			job.ID,
			"concurrent-session",
			`{"total_output_tokens":20,"peak_context_tokens":200}`,
		))
		close(release)

		synctest.Wait()

		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.True(t, usage.HasCost)
		assert.Equal(t, int64(20), usage.OutputTokens)
		assert.Equal(t, int64(200), usage.PeakContextTokens)
		assert.InDelta(t, 0.33, usage.CostUSD, 1e-9)
	})
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
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, job.ID, "codex", "prompt", "No issues found."))
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

func TestTokenCostPageBatchesPartialCandidate(t *testing.T) {
	worker := newWorkerTestContext(t, 1)
	job := seedTokenCostCandidate(t, worker, "implementation-session", `{"input_tokens":300,"total_output_tokens":40,"provider_session_ids":["implementation-session"],"expected_provider_sessions":2}`)
	var calls [][]string
	worker.Pool.tokenUsageBatchFetcher = func(_ context.Context, ids []string) (map[string]tokens.SessionUsageResult, error) {
		calls = append(calls, ids)
		return map[string]tokens.SessionUsageResult{"implementation-session": {Usage: &tokens.Usage{HasCost: true, CostUSD: 0.25}}}, nil
	}
	var individualCalls []string
	worker.Pool.tokenUsageFetcher = func(_ context.Context, id string) (*tokens.Usage, error) {
		individualCalls = append(individualCalls, id)
		return nil, nil
	}
	assert.Zero(t, worker.Pool.reconcileTokenCostPage(t.Context(), 0))
	assert.Equal(t, [][]string{{"implementation-session"}}, calls)
	assert.Empty(t, individualCalls)
	updated, err := worker.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.25, usage.CostUSD, 1e-9)
}

func tokenCostPageSessionIDs(count int) []string {
	ids := make([]string, count)
	for i := range count {
		ids[i] = fmt.Sprintf("session-%d", i)
	}
	return ids
}

func TestTokenCostPageHandlesBatchFailures(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		batchErr                error
		individualCalls         []string
		unpricedIDs             map[string]bool
		individualTimeoutID     string
		individualUnavailableID string
		logMessage              string
		cancelBatch             bool
		wantDue                 int
	}{
		{
			name:            "backend failure recovers individually",
			batchErr:        fmt.Errorf("archive read failed"),
			individualCalls: []string{"slow", "healthy"},
			logMessage:      "archive read failed",
			wantDue:         0,
		},
		{
			name:                "timeout retries sessions individually",
			batchErr:            context.DeadlineExceeded,
			individualCalls:     []string{"slow", "healthy"},
			unpricedIDs:         map[string]bool{"slow": true},
			individualTimeoutID: "slow",
			logMessage:          "batch usage lookup",
			wantDue:             0,
		},
		{
			name:            "unsupported batch recovers individually",
			batchErr:        tokens.ErrBatchUnsupported,
			individualCalls: []string{"slow", "healthy"},
		},
		{
			name:                    "provider unavailable stops page",
			batchErr:                tokens.ErrBatchUnsupported,
			individualCalls:         []string{"slow"},
			unpricedIDs:             map[string]bool{"slow": true, "healthy": true},
			individualUnavailableID: "slow",
			logMessage:              "token cost reconciliation: usage provider unavailable",
			wantDue:                 2,
		},
		{
			name:        "batch cancellation leaves candidates due",
			cancelBatch: true,
			wantDue:     2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := newWorkerTestContext(t, 1)
			slow := seedTokenCostCandidate(t, worker, "slow", `{}`)
			healthy := seedTokenCostCandidate(t, worker, "healthy", `{}`)
			ctx := t.Context()
			cancel := func() {}
			if tc.cancelBatch {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			var logOutput bytes.Buffer
			origOutput := log.Writer()
			log.SetOutput(&logOutput)
			t.Cleanup(func() { log.SetOutput(origOutput) })
			var batches [][]string
			worker.Pool.tokenUsageBatchFetcher = func(ctx context.Context, ids []string) (map[string]tokens.SessionUsageResult, error) {
				batches = append(batches, ids)
				if tc.cancelBatch {
					cancel()
					return nil, ctx.Err()
				}
				return nil, tc.batchErr
			}
			var individualCalls []string
			worker.Pool.tokenUsageFetcher = func(_ context.Context, id string) (*tokens.Usage, error) {
				individualCalls = append(individualCalls, id)
				if id == tc.individualUnavailableID {
					return nil, tokens.ErrUsageProviderUnavailable
				}
				if id == tc.individualTimeoutID {
					return nil, context.DeadlineExceeded
				}
				return &tokens.Usage{HasCost: true, CostUSD: 0.37}, nil
			}

			assert.Zero(t, worker.Pool.reconcileTokenCostPage(ctx, 0))
			if !tc.cancelBatch {
				for id, job := range map[string]*storage.ReviewJob{"slow": slow, "healthy": healthy} {
					updated, err := worker.DB.GetJobByID(job.ID)
					require.NoError(t, err)
					if tc.unpricedIDs[id] {
						assert.Equal(t, "{}", updated.TokenUsage)
						continue
					}
					usage := tokens.ParseJSON(updated.TokenUsage)
					require.NotNil(t, usage)
					assert.True(t, usage.HasCost)
					assert.InDelta(t, 0.37, usage.CostUSD, 1e-9)
				}
			}
			assert.Equal(t, [][]string{{"slow", "healthy"}}, batches)
			assert.Equal(t, tc.individualCalls, individualCalls)
			if tc.logMessage != "" {
				assert.Contains(t, logOutput.String(), tc.logMessage)
			}
			due, err := worker.DB.ListDueTokenCostCandidates(
				t.Context(), 0, 10,
				time.Now().Add(-worker.Pool.tokenCostMaxCandidateAge), time.Now(),
			)
			require.NoError(t, err)
			assert.Len(t, due, tc.wantDue)
		})
	}
}

func TestTokenCostPageRealBatchAndSlowRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}
	for _, slow := range []bool{false, true} {
		name := "two successful pages"
		if slow {
			name = "slow first session"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CALLS", calls)
			t.Setenv("SLOW", "0")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
if [ "$SLOW" = 1 ]; then
  if [ "$2" = usage-batch ] || [ "$3" = slow ]; then while :; do :; done; fi
  echo '{"has_cost":true,"cost_usd":0.37}'
  exit 0
fi
shift 2
printf '{"results":['
sep=''
while [ "$1" != --format ]; do
  printf '%s{"requested_id":"%s","exit_code":0,"usage":{"has_cost":true,"cost_usd":0.25}}' "$sep" "$1"
  sep=,
  shift
done
printf ']}'
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "agentsview"), []byte(script), 0o755))
			worker := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.Cost.Timeout = "1s"
			worker.reconfigurePool(cfg)
			ids := tokenCostPageSessionIDs(21)
			if slow {
				t.Setenv("SLOW", "1")
				ids = []string{"slow", "healthy"}
			}
			var jobs []*storage.ReviewJob
			for _, id := range ids {
				jobs = append(jobs, seedTokenCostCandidate(t, worker, id, `{"total_output_tokens":40}`))
			}
			start := time.Now()
			cursor := worker.Pool.reconcileTokenCostPage(t.Context(), 0)
			if !slow {
				require.Equal(t, jobs[19].ID, cursor)
				require.Zero(t, worker.Pool.reconcileTokenCostPage(t.Context(), cursor))
			} else {
				require.Zero(t, cursor)
				assert.Less(t, time.Since(start), 3500*time.Millisecond)
			}
			data, err := os.ReadFile(calls)
			require.NoError(t, err)
			if slow {
				assert.Equal(t, "session usage-batch slow healthy --format json --no-sync\nsession usage slow --format json --no-sync\nsession usage healthy --format json --no-sync\n", string(data))
				var misses int
				var next string
				require.NoError(t, worker.DB.QueryRowContext(t.Context(), "SELECT misses, next_check_at FROM token_cost_lookups WHERE job_id = ?", jobs[0].ID).Scan(&misses, &next))
				assert.Equal(t, 1, misses)
				assert.NotEmpty(t, next)
			} else {
				assert.Equal(t, "session usage-batch "+strings.Join(ids[:20], " ")+" --format json --no-sync\nsession usage-batch "+ids[20]+" --format json --no-sync\n", string(data))
			}
			for i, job := range jobs {
				updated, err := worker.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				usage := tokens.ParseJSON(updated.TokenUsage)
				require.NotNil(t, usage)
				assert.Equal(t, int64(40), usage.OutputTokens)
				if slow && i == 0 {
					assert.False(t, usage.HasCost)
				} else {
					assert.True(t, usage.HasCost)
					if slow {
						assert.InDelta(t, 0.37, usage.CostUSD, 1e-9)
					} else {
						assert.InDelta(t, 0.25, usage.CostUSD, 1e-9)
					}
				}
			}
		})
	}
}
